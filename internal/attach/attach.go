// Package attach is the "live" engine: it dials the unix sockets that the maki
// Lua plugin serves (CONTRACTS 1) and maps plugin traffic onto model.Session
// entries and hub events. One connection per running TUI; the gateway owns
// nothing on that side and never writes to a socket dir.
package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maki-gateway/internal/model"
)

// Plugin-side limits and clocks, all from CONTRACTS 1.
const (
	maxFrame       = 8 << 20 // a bigger frame ends the connection
	readBufferSize = 64 << 10
	firstLineStep  = 4 << 10
	dialTimeout    = 3 * time.Second
	sockSuffix     = ".sock"

	// pluginVersion is the value hello reports for its protocol build.
	pluginVersion = "1"
)

// Event types and dirty reasons of CONTRACTS 2.
const (
	evStatus   = "status"
	evDirty    = "dirty"
	evNotice   = "notice"
	evSessions = "sessions"
	// evTasks tells the browser a live tab's task list changed, so it refetches
	// GET /api/sessions/{id}/tasks (CONTRACTS 2).
	evTasks = "tasks"
	// evTaskFocus tells the browser the followed chat of a live tab changed and
	// names the new one, which is the plugin's own task_focus event relayed
	// (CONTRACTS 2).
	evTaskFocus = "task_focus"

	evPermission       = "permission"
	evPermissionCancel = "permission_cancel"

	reasonTurnEnd   = "turn_end"
	reasonToolDone  = "tool_done"
	reasonToolStart = "tool_start"
	// reasonStatus is what a turn_start maps to: CONTRACTS 2 lists no
	// turn_start reason, and "status" is the reason that also means "the tab
	// changed, refetch it".
	reasonStatus = "status"

	questionNotice = "question pending in terminal"
)

// Op names of CONTRACTS 1.
const (
	opHello     = "hello"
	opPing      = "ping"
	opList      = "list"
	opMessages  = "messages"
	opPrompt    = "prompt"
	opNew       = "new"
	opSetMode   = "set_mode"
	opSetTitle  = "set_title"
	opRespond   = "respond"
	opPlan      = "plan"
	opTasks     = "tasks"
	opTaskFocus = "task_focus"
	opPerms     = "permissions"
	argSession  = "session"
	argTaskID   = "task_id"
	argText     = "text"
	argLast     = "last"
	argCwd      = "cwd"
	argMode     = "mode"
	argTitle    = "title"
	argPrompt   = "prompt"
	argRequest  = "request_id"
	argDecision = "decision"
	argGuidance = "guidance"
	argOn       = "on"
	argYolo     = "yolo"
)

// noUI is the plugin's fixed answer when its process has no terminal to drive
// (lua/gateway/ops.lua NO_UI). CONTRACTS 1 keeps the words.
const noUI = "no interactive UI"

// timings are the engine clocks. New uses the contract values; tests shorten
// them so a suite does not sleep for minutes.
type timings struct {
	scanEvery time.Duration
	pingEvery time.Duration
	timeout   time.Duration
	tryAll    time.Duration
	missLimit int
	maxLine   int
}

func defaultTimings() timings {
	return timings{
		scanEvery: 3 * time.Second,
		pingEvery: 15 * time.Second,
		timeout:   10 * time.Second,
		tryAll:    3 * time.Second,
		missLimit: 2,
		maxLine:   maxFrame,
	}
}

// Engine implements model.Engine for kind "live".
type Engine struct {
	sockDir string
	hub     *model.Hub
	t       timings

	nextID atomic.Int64

	mu      sync.Mutex
	conns   map[string]*conn // socket path -> connection
	routes  map[string]*conn // session id -> connection that owns it
	tickets map[int64]*conn  // plugin permission id -> connection
	closed  bool
	quit    chan struct{}
	wake    chan struct{}
}

// New starts watching sockDir for plugin sockets and returns immediately; the
// engine is usable even when the directory does not exist yet.
func New(sockDir string, hub *model.Hub) *Engine {
	e := newEngine(sockDir, hub, defaultTimings())
	return e
}

func newEngine(sockDir string, hub *model.Hub, t timings) *Engine {
	e := &Engine{
		sockDir: sockDir,
		hub:     hub,
		t:       t,
		conns:   map[string]*conn{},
		routes:  map[string]*conn{},
		tickets: map[int64]*conn{},
		quit:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
	go e.watch()
	return e
}

var _ model.Engine = (*Engine)(nil)

func (e *Engine) Kind() string { return model.KindLive }

// Close stops the watch loop and hangs up every plugin. It publishes nothing:
// the gateway is going away, not the TUI.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	conns := slices.Collect(maps.Values(e.conns))
	e.mu.Unlock()
	close(e.quit)
	for _, c := range conns {
		c.drop(errEngineClosed)
	}
	return nil
}

// errEngineClosed marks a shutdown, not a plugin fault.
var errEngineClosed = errors.New("attach: engine closed")

func (e *Engine) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// ScanNow rediscovers plugin sockets without waiting for the next tick, so a
// caller that just launched a TUI can attach on demand.
func (e *Engine) ScanNow() { e.scan() }

// watch scans on a slow timer, plus whenever a caller asks for a scan.
func (e *Engine) watch() {
	timer := time.NewTimer(e.t.scanEvery)
	defer timer.Stop()
	for {
		select {
		case <-e.quit:
			return
		case <-timer.C:
		case <-e.wake:
		}
		e.scan()
		timer.Reset(e.t.scanEvery)
	}
}

// scan dials every socket it does not already hold. A stale socket file (the
// TUI died without unlinking) simply fails the dial and is tried again on the
// next tick, which is also the retry path for a dead connection.
func (e *Engine) scan() {
	entries, err := os.ReadDir(e.sockDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Debug("attach: scan socket dir", "dir", e.sockDir, "error", err)
		}
		return
	}
	held := map[string]bool{}
	e.mu.Lock()
	for path := range e.conns {
		held[path] = true
	}
	e.mu.Unlock()
	for _, en := range entries {
		path := filepath.Join(e.sockDir, en.Name())
		if en.IsDir() || !strings.HasSuffix(en.Name(), sockSuffix) || held[path] {
			continue
		}
		e.dial(path)
	}
}

// dial opens one socket, introduces itself, and only then takes ownership of
// the connection: an untracked failure must not raise a disconnect notice.
func (e *Engine) dial(path string) {
	nc, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		slog.Debug("attach: dial", "socket", path, "error", err)
		return
	}
	c := newConn(e, path, nc)
	go c.read()
	go c.keepalive()
	ctx, cancel := e.context()
	h, err := c.hello(ctx)
	cancel()
	if err != nil {
		slog.Warn("attach: hello refused", "socket", path, "error", err)
		c.drop(err)
		return
	}
	if h.Plugin != "" && h.Plugin != pluginVersion {
		slog.Warn("attach: unexpected plugin protocol build", "socket", path, "plugin", h.Plugin)
	}
	e.mu.Lock()
	tracked := false
	if !e.closed {
		e.conns[path] = c
		tracked = true
	}
	e.mu.Unlock()
	if !tracked {
		c.drop(errEngineClosed)
		return
	}
	// Arm the permission bridge: the plugin only pushes permission events while
	// it is on, so a gateway that never asks would never see a prompt. A plugin
	// without the op answers an error, which costs the bridge and nothing else.
	permsCtx, permsCancel := e.context()
	err = c.call(permsCtx, opPerms, map[string]any{argOn: true}, nil)
	permsCancel()
	if err != nil {
		slog.Debug("attach: permission bridge unavailable", "socket", path, "error", err)
	}
	slog.Debug("attach: tui connected", "socket", path, "pid", h.PID, "sessions", h.Sessions)
	e.publish(model.Event{Type: evSessions})
}

// forgetConn drops a connection's bookkeeping and reports whether it was ever
// tracked. Routes and tickets owned by it are void: they name a TUI that is gone.
func (e *Engine) forgetConn(dead *conn) bool {
	e.mu.Lock()
	tracked := false
	if c, ok := e.conns[dead.path]; ok && c == dead {
		delete(e.conns, dead.path)
		tracked = true
	}
	for id, c := range e.routes {
		if c == dead {
			delete(e.routes, id)
		}
	}
	for rid, c := range e.tickets {
		if c == dead {
			delete(e.tickets, rid)
		}
	}
	e.mu.Unlock()
	return tracked
}

// routeTo records that session lives on c.
func (e *Engine) routeTo(session string, c *conn) {
	if session == "" {
		return
	}
	e.mu.Lock()
	if !e.closed {
		e.routes[session] = c
	}
	e.mu.Unlock()
}

func (e *Engine) ticketOwner(requestID int64) (*conn, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.tickets[requestID]
	return c, ok && !c.closed.Load()
}

func (e *Engine) noteTicket(requestID int64, c *conn) {
	e.mu.Lock()
	if !e.closed {
		e.tickets[requestID] = c
	}
	e.mu.Unlock()
}

func (e *Engine) dropTicket(requestID int64) {
	e.mu.Lock()
	delete(e.tickets, requestID)
	e.mu.Unlock()
}

// owner resolves a session to its connection. The map is built from list
// results and from every event that names a session: CONTRACTS 1 has no op
// that finds a session across plugins.
func (e *Engine) owner(session string) (*conn, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.routes[session]
	return c, ok && !c.closed.Load()
}

func (e *Engine) live() []*conn {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*conn, 0, len(e.conns))
	for _, c := range e.conns {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *conn) int { return strings.Compare(a.path, b.path) })
	return out
}

// context is the per-request budget: a stuck plugin gets 10 s, then the caller
// sees a timeout rather than hanging the HTTP handler.
func (e *Engine) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), e.t.timeout)
}

// op runs one op against the connection owning session, or against every live
// connection when no route claims it.
func (e *Engine) op(session, opName string, args map[string]any, out any) error {
	raw, err := e.raw(session, opName, args)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return fmt.Errorf("attach: %s: %w", opName, errProtocol)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("attach: %s reply: %w", opName, err)
	}
	return nil
}

func (e *Engine) raw(session, opName string, args map[string]any) (json.RawMessage, error) {
	if c, ok := e.owner(session); ok {
		ctx, cancel := e.context()
		raw, err := c.request(ctx, opName, args)
		cancel()
		if !stale(err) {
			return raw, MapError(err)
		}
		// The route claimed a session the plugin no longer has: the TUI
		// closed the tab. Re-resolve through every connection.
		e.mu.Lock()
		if e.routes[session] == c {
			delete(e.routes, session)
		}
		e.mu.Unlock()
	}
	raw, err := e.fanOut(opName, args)
	return raw, MapError(err)
}

// fanOut asks every live connection and keeps the first answer. The budget is
// shorter than a normal request: nobody should wait for the slowest TUI.
func (e *Engine) fanOut(opName string, args map[string]any) (json.RawMessage, error) {
	conns := e.live()
	if len(conns) == 0 {
		return nil, fmt.Errorf("%w: no live tui", model.ErrNotFound)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.t.tryAll)
	defer cancel()

	var (
		wg      sync.WaitGroup
		answers = make(chan json.RawMessage, len(conns))
		fails   = make(chan error, len(conns))
	)
	for _, c := range conns {
		wg.Add(1)
		go func(c *conn) {
			defer wg.Done()
			raw, err := c.request(ctx, opName, args)
			if err != nil {
				fails <- err
				return
			}
			answers <- raw
		}(c)
	}
	go func() { wg.Wait(); close(answers); close(fails) }()

	var firstErr error
	for range len(conns) {
		select {
		case raw, ok := <-answers:
			if ok {
				return raw, nil
			}
		case err, ok := <-fails:
			if ok && firstErr == nil {
				firstErr = err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if firstErr == nil {
		firstErr = model.ErrNotFound
	}
	return nil, firstErr
}

// stale reports a plugin error that means "not my session", so an outdated
// route can be re-resolved instead of failing the call.
func stale(err error) bool {
	var pe pluginError
	if !errors.As(err, &pe) {
		return false
	}
	return matchAny(string(pe), notLive, unknownID)
}

// Error fragments the plugin writes (CONTRACTS 1). They are matched, not
// compared, because the text carries the session id.
const (
	notLive       = "session not live"
	openElsewhere = "another"
	expired       = "expired"
	unknownID     = "unknown"
	noSuch        = "no such"
	busyText      = "busy"
	elsewhereAlt  = "elsewhere"
)

func matchAny(msg string, frags ...string) bool {
	for _, f := range frags {
		if strings.Contains(msg, f) {
			return true
		}
	}
	return false
}

// MapError turns a plugin error string into the sentinel errors the HTTP layer
// already speaks. Transport faults are passed through untouched.
func MapError(err error) error {
	var pe pluginError
	if !errors.As(err, &pe) {
		return err
	}
	msg := string(pe)
	switch {
	case matchAny(msg, notLive, expired, unknownID, noSuch):
		return fmt.Errorf("%w: %s", model.ErrNotFound, msg)
	case matchAny(msg, openElsewhere, elsewhereAlt):
		return fmt.Errorf("%w: %s", model.ErrOpenElsewhere, msg)
	case matchAny(msg, busyText):
		return fmt.Errorf("%w: %s", model.ErrBusy, msg)
	}
	return err
}

func (e *Engine) publish(ev model.Event) {
	if e.hub != nil {
		e.hub.Publish(ev)
	}
}

// disconnected is the only notice a dying TUI earns (CONTRACTS 2).
func (e *Engine) disconnected(pid int) {
	e.publish(model.Event{Type: evNotice, Text: fmt.Sprintf("tui disconnected (pid %d)", pid)})
	e.publish(model.Event{Type: evSessions})
}

// List asks every live TUI for its tabs. A connection that fails is left out:
// its sessions disappear from the merged list, which is the truth, and its
// reader drops it a moment later.
func (e *Engine) List() ([]model.Session, error) {
	conns := e.live()
	out := make([]model.Session, 0, len(conns))
	for _, c := range conns {
		ctx, cancel := e.context()
		var items []liveSession
		err := c.call(ctx, opList, nil, &items)
		cancel()
		if err != nil {
			slog.Debug("attach: list failed", "socket", c.path, "error", err)
			continue
		}
		pid := c.pluginPID()
		for _, s := range items {
			e.routeTo(s.ID, c)
			out = append(out, model.Session{
				ID:        s.ID,
				Kind:      model.KindLive,
				Title:     s.Title,
				Status:    s.Status,
				Cwd:       s.Cwd,
				Model:     s.Model,
				UpdatedAt: s.UpdatedAt,
				PluginPID: pid,
			})
		}
	}
	return out, nil
}

// Messages reads a tab's transcript. CONTRACTS 1 always wants session and last.
func (e *Engine) Messages(id string, last int) ([]model.Message, error) {
	var body messagesBody
	args := map[string]any{argSession: id, argLast: last}
	if err := e.op(id, opMessages, args, &body); err != nil {
		return nil, err
	}
	return body.Messages, nil
}

// Prompt injects a line and reports "started" or "queued".
func (e *Engine) Prompt(id, text string) (string, error) {
	var state string
	if err := e.op(id, opPrompt, map[string]any{argSession: id, argText: text}, &state); err != nil {
		return "", err
	}
	return state, nil
}

// Cancel has no plugin op: the contract stops at prompt, and CONTRACTS 2 answers
// 501 for a live session. Stop the turn in the terminal instead.
func (e *Engine) Cancel(string) error { return model.ErrUnsupported }

// Kill has no plugin op either: the TUI outlives the gateway by design, so the
// HTTP layer refuses DELETE for a live session.
func (e *Engine) Kill(string) error { return model.ErrUnsupported }

// SetMode switches a tab between build and plan.
func (e *Engine) SetMode(id, mode string) error {
	return e.op(id, opSetMode, map[string]any{argSession: id, argMode: mode}, nil)
}

// SetTitle renames a tab. An empty title clears it, which the plugin spells
// null (CONTRACTS 1).
func (e *Engine) SetTitle(id, title string) error {
	var value any
	if title != "" {
		value = title
	}
	return e.op(id, opSetTitle, map[string]any{argSession: id, argTitle: value}, nil)
}

// Create opens a background tab in the first live TUI, lowest socket path
// winning so repeated calls land on one process. m is dropped: the new op takes
// no model, so the tab keeps that TUI's current model. Yolo travels to the op as
// `yolo:true`, and the plugin ignores it: a tab of a running TUI cannot skip the
// permission prompts of a process that was started without it.
func (e *Engine) Create(req model.CreateRequest) (string, error) {
	conns := e.live()
	if len(conns) == 0 {
		return "", fmt.Errorf("%w: no live tui", model.ErrNotFound)
	}
	args := map[string]any{}
	if req.Cwd != "" {
		args[argCwd] = req.Cwd
	}
	if req.Prompt != "" {
		args[argPrompt] = req.Prompt
	}
	if req.Yolo {
		args[argYolo] = true
	}
	ctx, cancel := e.context()
	raw, err := conns[0].request(ctx, opNew, args)
	cancel()
	if err != nil {
		return "", MapError(err)
	}
	var body newBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("attach: %s reply: %w", opNew, err)
	}
	e.routeTo(body.Session, conns[0])
	e.publish(model.Event{Type: evSessions})
	return body.Session, nil
}

// Plan returns the socket plan op answer for a live tab: {"mode","path","ready",
// "content"}, forwarded as raw JSON because the browser renders it verbatim
// (CONTRACTS 1, GET /api/plan/{id}).
func (e *Engine) Plan(id string) (json.RawMessage, error) {
	return e.raw(id, opPlan, map[string]any{argSession: id})
}

// Tasks returns the socket tasks op answer for a live tab, raw JSON of the
// maki.task.list() rows ({"id","session","name","status","focused",...}). The op
// takes no session argument, because maki only lists the focused tab's chats, so
// the id says which TUI to ask and the rows are that TUI's focused tab.
func (e *Engine) Tasks(id string) (json.RawMessage, error) {
	return e.raw(id, opTasks, nil)
}

// FocusTask moves the followed chat of a live tab, which is the terminal side of
// GET /api/sessions/{id}/tasks/{tid}/focus (CONTRACTS 2). The op names no
// session, because the host moves a chat of the tab the terminal already has
// focused, so the id here only says which TUI to ask. The task it moves goes in
// `task_id`, which leaves the frame's own `id` free for its request number.
//
// It asks that TUI alone rather than every live one: following is a change to a
// screen, and a browser that follows one terminal must not move another's.
//
// A refusal keeps the plugin's own words, which are the answer a person should
// read: "no task with id …" says that id is not a chat of this tab, and
// "no interactive UI attached" says this maki has no terminal behind it. The
// first is a 404 and the second a 501 (CONTRACTS 3).
func (e *Engine) FocusTask(session, taskID string) error {
	c, ok := e.owner(session)
	if !ok {
		return fmt.Errorf("%w: %s: %s", model.ErrNotFound, notLive, session)
	}
	ctx, cancel := e.context()
	defer cancel()
	var pe pluginError
	if err := c.call(ctx, opTaskFocus, map[string]any{argTaskID: taskID}, nil); err != nil {
		if !errors.As(err, &pe) {
			return MapError(err)
		}
		words := string(pe)
		if matchAny(words, noUI) {
			return Refusal{Err: model.ErrUnsupported, Words: words}
		}
		return Refusal{Err: model.ErrNotFound, Words: words}
	}
	return nil
}

// Refusal is one plugin answer the browser reads as written. Words are the
// plugin's own, which CONTRACTS 1 fixes, and Err is the sentinel that picks the
// HTTP status. Wrapping with %w would put that sentinel's text in front of words
// that are already a complete sentence about the user's own click.
type Refusal struct {
	Err   error
	Words string
}

func (r Refusal) Error() string { return r.Words }

func (r Refusal) Unwrap() error { return r.Err }

// RespondPermission answers a parked permission prompt. The plugin request id
// arrives with the permission event, so the ticket map knows who parked it; a
// gateway that never saw the event asks everyone.
//
// allow_always degrades here rather than in the plugin: the permission.prompt
// slot a live tab wraps answers allow or deny and nothing else, so the Lua side
// keeps its binary verdict (lua/gateway/permission.lua) and "always" is dropped.
// A live tab therefore asks again on the next call, which is the same answer an
// allow already gives today.
func (e *Engine) RespondPermission(requestID int64, decision, guidance string) error {
	if decision == model.DecisionAllowAlways {
		decision = model.DecisionAllow
	}
	args := map[string]any{argRequest: requestID, argDecision: decision}
	if guidance != "" {
		args[argGuidance] = guidance
	}
	var err error
	if c, ok := e.ticketOwner(requestID); ok {
		ctx, cancel := e.context()
		_, err = c.request(ctx, opRespond, args)
		cancel()
		e.dropTicket(requestID)
	} else {
		_, err = e.fanOut(opRespond, args)
		err = MapError(err)
	}
	return MapError(err)
}
