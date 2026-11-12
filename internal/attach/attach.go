// Package attach is the "live" engine: it dials the unix sockets that the maki
// Lua plugin serves (docs/WIRE.md) and maps the v2 attach protocol onto
// model.Session entries and hub events. One connection per running TUI; the
// gateway owns nothing on that side and never writes to a socket dir.
//
// The wire is a subset of ACP v1 with maki extensions, so the codec is the one
// this repository uses for `maki acp` children too: internal/acpwire. The plugin
// is the agent and the gateway is the client, and the only request that runs the
// other way is a permission ask (WIRE 10).
//
// Two parts of the engine surface have no method behind them, and both are
// decisions of the spec rather than gaps: no session/new opens a tab (Appendix
// B.1), and a live tab is never cancelled from here (WIRE 7.3).
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

// Discovery and dialing, unchanged from v1 (WIRE 1.1).
const (
	sockSuffix  = ".sock"
	dialTimeout = 3 * time.Second
)

// Event types and dirty reasons of CONTRACTS 2. The SSE surface is frozen, so
// these names and their fields stay exactly as the bridge published them in v1.
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
	// reasonStatus rides on the dirty that WIRE 11.2 pairs with a status change,
	// which is where v1's turn_start nudge lives now: CONTRACTS 2 lists no
	// turn_start reason, and "status" is the reason that also means "the tab
	// changed, refetch it".
	reasonStatus = "status"

	questionNotice = "question pending in terminal"
)

// noUI is the plugin's fixed answer when its process has no terminal to drive
// (WIRE 13.1 keeps the words). It is not a mapping of the table: a caller reads
// it as "no screen to move", which is a 501 here.
const noUI = "no interactive UI"

// timings are the engine clocks. New uses the WIRE values; tests shorten them so
// a suite does not sleep for minutes.
type timings struct {
	scanEvery  time.Duration
	timeout    time.Duration
	tryAll     time.Duration
	cancelWait time.Duration
	missLimit  int
	maxLine    int
}

func defaultTimings() timings {
	return timings{
		scanEvery:  3 * time.Second,
		timeout:    10 * time.Second,
		tryAll:     3 * time.Second,
		cancelWait: 2 * time.Second,
		missLimit:  2,
		maxLine:    acpwire.MaxLine,
	}
}

// Engine implements model.Engine for kind "live".
type Engine struct {
	sockDir string
	hub     *model.Hub
	t       timings

	// nextRequestID mints the permission request ids the hub and the HTTP route
	// carry. WIRE 10 puts the plugin's own id on the socket and leaves this side
	// to number its dialogs, which is what CONTRACTS 2 means by per-engine ids.
	nextRequestID atomic.Int64

	mu     sync.Mutex
	conns  map[string]*conn // socket path -> connection
	routes map[string]*conn // session id -> connection that owns it
	asks   map[int64]*ask   // request id -> permission prompt held open
	closed bool
	quit   chan struct{}
	wake   chan struct{}
}

// ask is one permission prompt this gateway holds open for the browser.
type ask struct {
	c         *conn
	wireID    string // the id the plugin put on its own request
	requestID int64  // the id this gateway published for it
	req       *acpwire.Request
	params    permissionRequest
}

// New starts watching sockDir for plugin sockets and returns immediately; the
// engine is usable even when the directory does not exist yet.
func New(sockDir string, hub *model.Hub) *Engine {
	return newEngine(sockDir, hub, defaultTimings())
}

func newEngine(sockDir string, hub *model.Hub, t timings) *Engine {
	e := &Engine{
		sockDir: sockDir,
		hub:     hub,
		t:       t,
		conns:   map[string]*conn{},
		routes:  map[string]*conn{},
		asks:    map[int64]*ask{},
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
		c.drop()
	}
	return nil
}

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

// scan dials every socket it does not already hold. A stale socket file (the TUI
// died without unlinking) simply fails the dial and is tried again on the next
// tick, which is also the retry path for a dead connection.
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

// dial opens one socket, runs the handshake of WIRE 2, and only then takes
// ownership of the connection: an untracked failure must not raise a disconnect
// notice, and a refused plugin must never reach the list.
func (e *Engine) dial(path string) {
	nc, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		slog.Debug("attach: dial", "socket", path, "error", err)
		return
	}
	c := newConn(e, path, nc)
	c.serve()
	ctx, cancel := e.context()
	info, err := c.initialize(ctx)
	cancel()
	if err != nil {
		slog.Warn("attach: initialize refused", "socket", path, "error", err)
		c.drop()
		return
	}
	if !info.protoSeen || info.proto < minAttachProto {
		e.refuse(path, c, info)
		return
	}
	e.mu.Lock()
	tracked := false
	if !e.closed {
		e.conns[path] = c
		tracked = true
	}
	e.mu.Unlock()
	if !tracked {
		c.drop()
		return
	}
	slog.Debug("attach: tui connected", "socket", path, "pid", info.pid,
		"cwd", info.cwd, "maki", info.makiVer, "attach_proto", info.proto)
	e.publish(model.Event{Type: evSessions})
}

// refuse ends a connection whose plugin cannot speak v2, and says so twice: in
// one error log line, and in a notice the browser shows. A silent refusal is
// what left a person staring at an empty list with no live rows and no reason
// (WIRE 2.2).
func (e *Engine) refuse(path string, c *conn, info agentInfo) {
	seen := "none"
	if info.protoSeen {
		seen = strconv.FormatFloat(info.proto, 'f', -1, 64)
	}
	slog.Error("attach: "+tooOld, "socket", path, "pid", info.pid,
		"attach_proto", seen, "want", minAttachProto, "maki_version", info.makiVer)
	e.publish(model.Event{Type: evNotice, Text: fmt.Sprintf(refusalNotice, info.pid)})
	c.drop()
}

// forgetConn drops a connection's bookkeeping and reports whether it was ever
// tracked. Routes and parked asks owned by it are void: they name a TUI that is
// gone, and nobody can answer them any more.
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
	for rid, a := range e.asks {
		if a.c == dead {
			delete(e.asks, rid)
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

// nextAskID mints one permission request id.
func (e *Engine) nextAskID() int64 { return e.nextRequestID.Add(1) }

// armAsk parks one ask and turns on the sniffer that watches for its withdrawal.
func (e *Engine) armAsk(a *ask) {
	e.mu.Lock()
	if !e.closed {
		e.asks[a.requestID] = a
	}
	e.mu.Unlock()
	a.c.sniff.on.Store(true)
}

// takeAsk removes the ask of one request id, or returns nil when this side holds
// no such dialog.
func (e *Engine) takeAsk(requestID int64) *ask {
	e.mu.Lock()
	a := e.asks[requestID]
	delete(e.asks, requestID)
	e.mu.Unlock()
	if a != nil {
		a.c.armSniffer()
	}
	return a
}

// removeAskByWire takes the ask a plugin ended on its own. The scan is the whole
// cost: a connection holds at most a few dialogs at a time.
func (e *Engine) removeAskByWire(c *conn, wireID string) *ask {
	e.mu.Lock()
	var found *ask
	for _, a := range e.asks {
		if a.c == c && a.wireID == wireID {
			found = a
			delete(e.asks, a.requestID)
			break
		}
	}
	e.mu.Unlock()
	if found != nil {
		c.armSniffer()
	}
	return found
}

// armSniffer keeps the withdrawal sniffer of this connection on only while it
// still parks an ask, so a plugin that never asks costs one atomic load per read.
func (c *conn) armSniffer() {
	on := c.e.asksOpenOn(c)
	c.sniff.on.Store(on)
}

func (e *Engine) asksOpenOn(c *conn) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.asks {
		if a.c == c {
			return true
		}
	}
	return false
}

// owner resolves a session to its connection. The map is built from list
// results and from every event that names a session: WIRE 3 makes session/list
// the only way to learn what a TUI holds.
func (e *Engine) owner(session string) (*conn, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.routes[session]
	return c, ok && !c.wire.Closed()
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

// op runs one request against the connection owning session, or against every
// live connection when no route claims it, and decodes the result into out.
func (e *Engine) op(session, method string, args any, out any) error {
	raw, err := e.raw(session, method, args)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("attach: %s: %w", method, errNoBody)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("attach: %s result: %w", method, err)
	}
	return nil
}

// raw runs one request and hands back its result untouched, which is what the
// routes that forward a plugin answer verbatim need (WIRE 6, 9.2, 9.3).
func (e *Engine) raw(session, method string, args any) (json.RawMessage, error) {
	if c, ok := e.owner(session); ok {
		ctx, cancel := e.context()
		raw, err := c.request(ctx, method, args)
		cancel()
		if !stale(err) {
			return raw, nameMethod(method, MapError(err))
		}
		// The route claimed a tab the plugin no longer has: the TUI closed it.
		// Re-resolve through every connection.
		e.mu.Lock()
		if e.routes[session] == c {
			delete(e.routes, session)
		}
		e.mu.Unlock()
	}
	raw, err := e.fanOut(method, args)
	return raw, nameMethod(method, MapError(err))
}

// fanOut asks every live connection and keeps the first answer. The budget is
// shorter than a normal request: nobody should wait for the slowest TUI.
func (e *Engine) fanOut(method string, args any) (json.RawMessage, error) {
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
			raw, err := c.request(ctx, method, args)
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

// stale reports a plugin answer that means "not my tab", so an outdated route can
// be re-resolved instead of failing the call. WIRE 13 gives that fact a code of
// its own (-32002) and the words of WIRE 13.1 to carry it.
func stale(err error) bool {
	var we *acpwire.Error
	if !errors.As(err, &we) {
		return false
	}
	return we.Code == codeNotLive || matchAny(we.Message, notLive, unknownID)
}

// Error fragments the plugin writes (WIRE 13.1). They are matched, not compared,
// because the text carries the session id.
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

// MapError turns a plugin error into the sentinel errors the HTTP layer already
// speaks, by the substring table of WIRE 13.1. A transport fault is not a plugin
// answer, so it passes through untouched.
func MapError(err error) error {
	var we *acpwire.Error
	if !errors.As(err, &we) {
		return err
	}
	msg := we.Message
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

// nameMethod keeps the promise of WIRE 12.2: a -32601 for any method but
// session/cancel reaches a caller as an error that names the method.
func nameMethod(method string, err error) error {
	var we *acpwire.Error
	if errors.As(err, &we) && we.Code == acpwire.CodeMethodNotFound {
		return fmt.Errorf("attach: %s: %w", method, err)
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

// List asks every live TUI for its tabs, which is also the liveness clock of
// WIRE 1.5: two polls in a row that fail or time out end that connection, and
// its tabs leave the list. There is no ping on this wire.
func (e *Engine) List() ([]model.Session, error) {
	conns := e.live()
	out := make([]model.Session, 0, len(conns))
	for _, c := range conns {
		ctx, cancel := e.context()
		var res listResult
		err := c.call(ctx, mSessionList, noArgs, &res)
		cancel()
		if err != nil {
			slog.Debug("attach: list failed", "socket", c.path, "error", err)
			if misses := c.countMiss(); misses >= e.t.missLimit {
				slog.Warn("attach: plugin stopped answering the list poll, dropping it",
					"socket", c.path, "pid", c.pluginPID(), "misses", misses)
				c.drop()
			}
			continue
		}
		c.resetMisses()
		pid := c.pluginPID()
		for _, row := range res.Sessions {
			e.routeTo(row.SessionID, c)
			cwd := row.Meta.str(metaCwd)
			if cwd == "" {
				cwd = row.Cwd
			}
			out = append(out, model.Session{
				ID:        row.SessionID,
				Kind:      model.KindLive,
				Title:     row.Title,
				Status:    row.Meta.str(metaStatus),
				Cwd:       cwd,
				Model:     row.Meta.str(metaModel),
				UpdatedAt: epochSeconds(row.UpdatedAt),
				PluginPID: pid,
			})
		}
	}
	return out, nil
}

// Messages reads a tab's transcript through _sh.maki/messages, the one method
// WIRE 6 keeps bespoke because an ACP update stream cannot rebuild a native
// record. The records pass through verbatim.
func (e *Engine) Messages(id string, last int) ([]model.Message, error) {
	var res messagesResult
	args := map[string]any{argSessionID: id, argLast: last}
	if err := e.op(id, mMessages, args, &res); err != nil {
		return nil, err
	}
	return res.Messages, nil
}

// Prompt puts text in front of a live tab and reports "started" or "queued" at
// once. session/prompt cannot answer before the turn ends (WIRE 7.1), and the
// HTTP contract answers with the queue state immediately, so the route calls
// _sh.maki/queue (WIRE 7.2).
func (e *Engine) Prompt(id, text string) (string, error) {
	var res queueResult
	args := map[string]any{argSessionID: id, argText: text}
	if err := e.op(id, mQueue, args, &res); err != nil {
		return "", err
	}
	return res.State, nil
}

// Cancel asks a live tab to stop, and WIRE 7.3 says it cannot. The request goes
// out so a plugin can answer: a -32601 or no answer at all both mean the same
// thing here, and a person stops a turn at the terminal.
func (e *Engine) Cancel(id string) error {
	c, ok := e.owner(id)
	if !ok {
		return model.ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.t.cancelWait)
	defer cancel()
	_, err := c.request(ctx, mSessionCancel, map[string]any{argSessionID: id})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || isMethodNotFound(err) {
		return model.ErrUnsupported
	}
	return MapError(err)
}

func isMethodNotFound(err error) bool {
	var we *acpwire.Error
	return errors.As(err, &we) && we.Code == acpwire.CodeMethodNotFound
}

// Kill has no method on this socket: the TUI owns its process and outlives the
// gateway by design, so DELETE answers 400 for a live row. What this side can do
// is hang up, which drops that TUI's tabs from the list until the next scan dials
// it again.
func (e *Engine) Kill(id string) error {
	c, ok := e.owner(id)
	if !ok {
		return model.ErrUnsupported
	}
	c.drop()
	return nil
}

// SetMode switches a tab between build and plan (WIRE 8.1).
func (e *Engine) SetMode(id, mode string) error {
	args := map[string]any{argSessionID: id, argModeID: mode}
	return e.op(id, mSessionSetMode, args, nil)
}

// SetTitle renames a tab. An empty title clears it, which is the one spelling
// WIRE 8.2 keeps: v1's null has no empty value to echo in JSON.
func (e *Engine) SetTitle(id, title string) error {
	args := map[string]any{argSessionID: id, argTitle: title}
	return e.op(id, mSessionSetTitle, args, nil)
}

// Create cannot open a tab. WIRE Appendix B.1 fixes that no session/new exists on
// this socket: a live tab is opened by a person at the terminal, and a plugin
// answers session/new with -32601. The request still goes out to the lowest
// socket, so a build that does answer one is read, and a refusal is reported as
// what it is. A caller that must create a session spawns maki acp, which is what
// the HTTP route does.
func (e *Engine) Create(req model.CreateRequest) (string, error) {
	conns := e.live()
	if len(conns) == 0 {
		return "", fmt.Errorf("%w: no live tui", model.ErrNotFound)
	}
	args := map[string]any{argMCPServers: []any{}}
	if req.Cwd != "" {
		args[argCwd] = req.Cwd
	}
	ctx, cancel := e.context()
	raw, err := conns[0].request(ctx, mSessionNew, args)
	cancel()
	if err != nil {
		if isMethodNotFound(err) {
			return "", model.ErrUnsupported
		}
		return "", MapError(err)
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("attach: %s result: %w", mSessionNew, err)
	}
	e.routeTo(body.SessionID, conns[0])
	e.publish(model.Event{Type: evSessions})
	return body.SessionID, nil
}

// Notify writes one line into a live tab (WIRE 9.1). Without a session it goes to
// the focused tab, so the call is fanned out and the first TUI that has one
// answers.
func (e *Engine) Notify(session, text string) error {
	args := map[string]any{argText: text}
	if session == "" {
		_, err := e.fanOut(mNotify, args)
		return nameMethod(mNotify, MapError(err))
	}
	args[argSessionID] = session
	return e.op(session, mNotify, args, nil)
}

// Plan returns the socket plan answer for a live tab: {"mode","path","ready",
// "content"}, forwarded as raw JSON because the browser renders it verbatim
// (WIRE 9.2, GET /api/plan/{id}).
func (e *Engine) Plan(id string) (json.RawMessage, error) {
	return e.raw(id, mPlan, map[string]any{argSessionID: id})
}

// Tasks returns the socket tasks answer for a live tab, raw JSON of the
// maki.task.list() rows ({"id","session","name","status","focused",...}). The
// method names no session, because maki only lists the focused tab's chats, so
// the id here says which TUI to ask and the rows are that TUI's focused tab
// (WIRE 9.3). The result is a bare array: the {"tasks":...} wrapper the HTTP
// route answers is the gateway's own, and stays in httpapi.
func (e *Engine) Tasks(id string) (json.RawMessage, error) {
	return e.raw(id, mTasks, noArgs)
}

// FocusTask moves the followed chat of a live tab, which is the terminal side of
// POST /api/sessions/{id}/tasks/{tid}/focus (WIRE 9.4). The method names no
// session, because the host moves a chat of the tab the terminal already has
// focused, so the id here only says which TUI to ask. The task it moves goes in
// `taskId`, which is the whole argument: v1's habit of taking the same name in
// the frame's own request number is gone with the collision that made it
// necessary.
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
	// Only an answer from the plugin earns the treatment of its own words. The
	// death of the socket is a transport fault, and its text says nothing about
	// this click.
	err := c.call(ctx, mTaskFocus, map[string]any{argTaskID: taskID}, nil)
	if err == nil {
		return nil
	}
	var we *acpwire.Error
	if errors.Is(err, errGone) || !errors.As(err, &we) {
		return MapError(err)
	}
	if matchAny(we.Message, noUI) {
		return Refusal{Err: model.ErrUnsupported, Words: we.Message}
	}
	return Refusal{Err: model.ErrNotFound, Words: we.Message}
}

// Refusal is one plugin answer the browser reads as written. Words are the
// plugin's own, which WIRE 13.1 fixes, and Err is the sentinel that picks the
// HTTP status. Wrapping with %w would put that sentinel's text in front of words
// that are already a complete sentence about the user's own click.
type Refusal struct {
	Err   error
	Words string
}

func (r Refusal) Error() string { return r.Words }

func (r Refusal) Unwrap() error { return r.Err }

// RespondPermission answers a parked permission ask. The id is the one this
// gateway published on the permission event, so the ask map names the connection
// and the request it still owes.
//
// allow_always degrades here rather than in the plugin: the permission.prompt
// slot a live tab wraps answers allow or deny and nothing else, so the wire
// offers two options and "always" is dropped (WIRE 10.1). A live tab therefore
// asks again on the next call, which is the same answer an allow already gives.
// Guidance has no ACP field, so it travels in the _meta of the result.
func (e *Engine) RespondPermission(requestID int64, decision, guidance string) error {
	if decision == model.DecisionAllowAlways {
		decision = model.DecisionAllow
	}
	want := optionReject
	if decision == model.DecisionAllow {
		want = optionAllowOnce
	}
	a := e.takeAsk(requestID)
	if a == nil {
		// WIRE 13.1: this side writes the word itself, and the mapping reads it
		// back as the 404 for a dialog that is no longer held.
		return fmt.Errorf("%w: permission %d expired", model.ErrNotFound, requestID)
	}
	if err := a.req.Answer(newPermissionResult(a.params.optionID(want), guidance)); err != nil {
		return fmt.Errorf("attach: answer permission: %w", err)
	}
	return nil
}
