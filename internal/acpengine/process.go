package acpengine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

// errGone answers a request this side will never see replied. It is also a
// model.ErrNotFound, so a call that races with the child's death reports the
// session as gone, and Prompt, which is allowed to lose that race, can tell a
// death from a rejection.
var errGone = fmt.Errorf("%w: process exited", model.ErrNotFound)

var errTimeout = errors.New("acp request timed out")

const (
	callTimeout   = 30 * time.Second
	handshakeWait = 60 * time.Second
	killGrace     = 3 * time.Second

	// A yolo respawn waits longer than Kill does: the transcript of an in-flight
	// turn is written as the child sees stdin close, and losing that is worse
	// than waiting. haltGone is how long it waits for the death before sending a
	// signal, and haltTerm how long after the signal.
	haltGone = 5 * time.Second
	haltTerm = 2 * time.Second
	exitPoll = 20 * time.Millisecond

	// promptAdmission bounds how long Prompt waits for a rejection the agent can
	// only answer with right away. A turn that keeps running is not delayed by
	// more than this.
	promptAdmission = 400 * time.Millisecond
)

// process is one web session, and it outlives the `maki acp` child it drives. A
// yolo toggle swaps the child under the record (see install), so the session id,
// the cached transcript and the spawn inputs survive while the process behind them
// is replaced. The server holds exactly one session per process (server.rs:101).
type process struct {
	eng *Engine
	// log names the pid the session started with and is never reassigned: the
	// pump goroutines of one child can still be running when the next one is
	// armed, so a mutable logger would be a data race.
	log *slog.Logger

	// modelSpec is the model the user last asked this session to run: the -m of
	// the create request, or the value of the newest SetModel. It outlives one
	// child, so a respawn can repeat the spawn with the permission answer moved.
	// The model a child reports for itself is not asked for here, because that is
	// an answer about the process that is going away.
	modelSpec string
	yolo      bool
	cwd       string

	wmu    sync.Mutex
	stdin  io.WriteCloser
	closed bool

	// wire is the connection of the child this record talks to now, and nil
	// before the first one. A pointer rather than a field under mu because the
	// list row reads it while holding mu and every call path takes wmu: a lock
	// here would invert that order. A yolo toggle replaces it with install.
	wire atomic.Pointer[acpwire.Conn]

	mu         sync.Mutex
	cmd        *exec.Cmd
	tr         *transcript
	busy       bool
	restarting bool
	// loading mutes the transcript reducer and the update passthrough while a
	// respawn replays the stored history. See reload.
	loading   bool
	session   string
	modelName string
	modes     []string
	title     string
	updatedAt int64
}

// spawn starts the child and runs the handshake, so the caller gets a session id
// it can prompt immediately.
func spawn(eng *Engine, cwd, m string, yolo bool) (*process, error) {
	p := &process{
		eng:       eng,
		tr:        newTranscript(),
		cwd:       cwd,
		modelSpec: m,
		yolo:      yolo,
		modelName: m,
	}
	c, err := launch(eng.makiPath, cwd, childArgv(m, yolo))
	if err != nil {
		return nil, err
	}
	p.log = eng.log.With("pid", c.cmd.Process.Pid)
	conn := p.install(c)
	p.touch()
	go p.pumpStderr(c.stderr)
	conn.Start()
	if err := p.handshake(); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// childArgv is the argv of one web child: the `acp` subcommand, then its own
// `--yolo`, which is where maki reads the permission answer (src/cli.rs:428-435,
// src/cmd/acp.rs:108), then the model the user asked this session for. maki reads
// both once, at startup, so a running child cannot change its mind and the argv of
// its successor is the only place either choice is written down.
//
// The order matters: `--yolo` is the flag of the `acp` subcommand, and the global
// flag of the same name (src/cli.rs:145) reaches the tui and print paths only, so
// `maki --yolo acp` starts a session that still asks.
func childArgv(modelSpec string, yolo bool) []string {
	args := make([]string, 0, 4)
	args = append(args, argACP)
	if yolo {
		args = append(args, argYolo)
	}
	if modelSpec != "" {
		args = append(args, argModel, modelSpec)
	}
	return args
}

// child is one started process with the pipes the gateway drives it on.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// transport is one child's stdio seen as a single byte stream, which is all
// acpwire asks of a transport. It has no Close on purpose: the gateway ends a
// child by closing its input, and the codec must not do that behind its back.
type transport struct {
	r io.Reader
	w io.Writer
}

func (t transport) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t transport) Write(p []byte) (int, error) { return t.w.Write(p) }

// launch starts a child without touching the session record, so its pipes reach
// the pump goroutines only once the record is ready to hand over the wire.
func launch(makiPath, cwd string, args []string) (*child, error) {
	cmd := exec.Command(makiPath, args...)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", makiPath, err)
	}
	return &child{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// install makes a freshly launched child the one this record talks to. The record
// keeps everything else: transcript cache, session id, title, model. Nothing of
// the old child is still owed an answer, because its connection failed every open
// request when its stdout closed, and a respawn waits for that.
func (p *process) install(c *child) *acpwire.Conn {
	conn := acpwire.New(transport{r: c.stdout, w: c.stdin}, acpwire.Options{
		// The death frame keeps the marker name a caller reads a lost child by,
		// so a request that raced the exit answers the same as a closed one.
		Death:          &wireError{Code: codeGone, Message: "process exited"},
		Logger:         p.log,
		OnRequest:      p.onAsk,
		OnNotification: p.onNote,
		OnClose:        p.die,
	})

	p.mu.Lock()
	p.cmd = c.cmd
	p.mu.Unlock()

	p.wmu.Lock()
	p.stdin = c.stdin
	p.closed = false
	p.wmu.Unlock()

	p.wire.Store(conn)
	return conn
}

// respawnChild starts the next child of this session, asked to answer permissions
// the way yolo says and to run the model the user last chose for it.
func (p *process) respawnChild(yolo bool) error {
	p.mu.Lock()
	args := childArgv(p.modelSpec, yolo)
	p.mu.Unlock()
	c, err := launch(p.eng.makiPath, p.cwd, args)
	if err != nil {
		return err
	}
	conn := p.install(c)
	p.log.Info("acp child respawned", "pid", c.cmd.Process.Pid, "yolo", yolo)
	go p.pumpStderr(c.stderr)
	conn.Start()
	// Second half of the Kill check: it can also land between install and here, and
	// then this child was started for a session nothing owns. The pumps are already
	// running, so ending it reaps it like any other death.
	if !p.eng.owns(p) {
		p.stop()
		return errGone
	}
	return nil
}

// initialize is the once per process handshake every ACP conversation starts with.
func (p *process) initialize() error {
	if _, err := p.call(methodInitialize, map[string]any{
		"protocolVersion": acpProtocol,
		"clientCapabilities": map[string]any{
			"fs":          map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal":    false,
			"elicitation": map[string]any{"form": map[string]any{}},
		},
		"clientInfo": map[string]any{"name": "maki-gateway", "version": "1"},
	}, handshakeWait); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return nil
}

// handshake is `initialize`, then `session/new`, which is what names the session.
func (p *process) handshake() error {
	if err := p.initialize(); err != nil {
		return err
	}
	raw, err := p.call(methodSessionNew, map[string]any{"cwd": p.cwd, "mcpServers": []any{}}, handshakeWait)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var res struct {
		SessionID     string         `json:"sessionId"`
		Modes         modeState      `json:"modes"`
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("session/new result: %w", err)
	}
	if res.SessionID == "" {
		return errors.New("session/new returned no sessionId")
	}
	p.mu.Lock()
	p.session = res.SessionID
	p.modes = res.Modes.ids()
	p.mu.Unlock()
	p.readModel(res.ConfigOptions)
	return nil
}

// reload points a freshly spawned child at the session the previous one held, so a
// toggle continues the same conversation rather than opening a blank one.
//
// The load is the one place the update stream repeats itself: the server replays
// every stored turn as session/update notifications before it answers (server.rs
// load_session, translate::replay_history), and both the cached transcript and the
// browser already hold that history. So loading mutes the reducer and the hub
// passthrough for the call, and nothing of the replay reaches either. The session
// fields an update carries are still read, because a replayed config option names
// the loaded session's own model.
//
// A session the dying child still holds answers -32000, which mapWireError turns
// into model.ErrOpenElsewhere and the caller retries.
func (p *process) reload() error {
	if err := p.initialize(); err != nil {
		return err
	}
	id := p.Session()
	p.setLoading(true)
	defer p.setLoading(false)
	raw, err := p.call(methodSessionLoad, map[string]any{
		"sessionId": id, "cwd": p.cwd, "mcpServers": []any{},
	}, handshakeWait)
	if err != nil {
		return fmt.Errorf("session/load: %w", err)
	}
	var res struct {
		Modes         modeState      `json:"modes"`
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("session/load result: %w", err)
	}
	p.mu.Lock()
	p.modes = res.Modes.ids()
	p.mu.Unlock()
	p.readModel(res.ConfigOptions)
	return nil
}

// call sends a request and waits for its answer, no longer than timeout.
func (p *process) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	conn, err := p.link()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, err := conn.Call(ctx, method, params)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errTimeout
	}
	return raw, mapError(err)
}

// begin writes a request whose answer the caller waits for itself. session/prompt
// answers only at end of turn, so its owner reads Call.Resp instead of blocking
// here.
func (p *process) begin(method string, params any) (*acpwire.Call, error) {
	conn, err := p.link()
	if err != nil {
		return nil, err
	}
	call, err := conn.Send(method, params)
	if err != nil {
		return nil, mapError(err)
	}
	return call, nil
}

func (p *process) notify(method string, params any) error {
	conn, err := p.link()
	if err != nil {
		return err
	}
	return mapError(conn.Notify(method, params))
}

// link is the connection to talk on. A record whose input is closed or whose child
// stopped answering has no connection left to use, so a call on it reports the
// session as gone rather than faulting this one request.
func (p *process) link() (*acpwire.Conn, error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	conn := p.wire.Load()
	if p.closed || conn == nil || conn.Closed() {
		return nil, errGone
	}
	return conn, nil
}

// onNote forwards the one notification stream the server pushes. A method with no
// meaning here is dropped rather than treated as a fault: an agent may announce
// anything, and the gateway passes on what the browser can use.
func (p *process) onNote(method string, params json.RawMessage) {
	if method == methodSessionUpdate {
		p.handleUpdate(params)
	}
}

// onAsk answers the agent->client requests the server can send. Anything else is
// refused on the spot, so the child never waits on an ask nobody read.
func (p *process) onAsk(_ context.Context, r *acpwire.Request) error {
	switch r.Method {
	case methodRequestPermission:
		return p.askPermission(r)
	case methodElicitationCreate:
		return p.askElicitation(r)
	}
	p.logger().Debug("unhandled agent request", "method", r.Method)
	return &wireError{Code: codeMethodNotFound, Message: "method not found"}
}

func (p *process) handleUpdate(params json.RawMessage) {
	var u struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &u); err != nil || len(u.Update) == 0 {
		p.logger().Debug("unreadable session/update", "error", err)
		return
	}
	p.mu.Lock()
	id := p.session
	loading := p.loading
	p.mu.Unlock()
	if u.SessionID != "" {
		id = u.SessionID
	}
	p.touch()
	p.note(u.Update)
	if loading {
		// Part of a respawn's history replay: the cache has these turns already,
		// and so does the browser. Feeding either would paint the history twice.
		return
	}
	p.eng.hub.Publish(model.Event{
		Type:    "update",
		Engine:  model.EngineACP,
		Session: id,
		ACP:     u.Update,
	})
	p.mu.Lock()
	p.tr.feed(u.Update)
	p.mu.Unlock()
}

// note keeps the session fields an update reports: the selected model and the
// title. The raw update still reaches the browser, so nothing is lost here.
func (p *process) note(raw json.RawMessage) {
	var head struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return
	}
	switch head.SessionUpdate {
	case updateConfigOption:
		var update struct {
			ConfigOptions []configOption `json:"configOptions"`
		}
		if json.Unmarshal(raw, &update) == nil {
			p.readModel(update.ConfigOptions)
		}
	case updateSessionInfo:
		var update struct {
			Title *string `json:"title"`
		}
		if json.Unmarshal(raw, &update) == nil && update.Title != nil {
			p.mu.Lock()
			p.title = *update.Title
			p.mu.Unlock()
		}
	}
}

// askPermission parks a session/request_permission for the browser to answer. The
// ask stays unanswered until someone replies, which is what the child waits on.
func (p *process) askPermission(r *acpwire.Request) error {
	var req permissionRequest
	if err := json.Unmarshal(r.Params, &req); err != nil {
		p.logger().Debug("bad request_permission", "error", err)
		return &wireError{Code: codeInvalidRequest, Message: "invalid params"}
	}
	id := p.Session()
	requestID := p.eng.park(p, id, r, req.Options)
	p.setStatus(model.StatusNeedsInput)
	p.logger().Debug("parked permission", "request_id", requestID, "tool", req.ToolCall.Title)
	p.eng.hub.Publish(model.Event{
		Type:      "permission",
		Engine:    model.EngineACP,
		Session:   id,
		RequestID: requestID,
		Tool:      req.ToolCall.Title,
		Input:     req.ToolCall.RawInput,
	})
	return nil
}

// askElicitation dismisses an ask this side has no form for.
// elicitation.rs:130 turns any action other than "accept" into its DISMISSED text,
// and "cancel" is the spelling its own test at elicitation.rs:255 uses.
func (p *process) askElicitation(r *acpwire.Request) error {
	if err := r.Answer(map[string]string{"action": actionCancel}); err != nil {
		p.logger().Debug("elicitation cancel failed", "error", err)
	}
	p.eng.hub.Publish(model.Event{
		Type:    "notice",
		Engine:  model.EngineACP,
		Session: p.Session(),
		Text:    noticeQuestion,
	})
	return nil
}

// pumpStderr keeps child noise out of the gateway's own stderr.
func (p *process) pumpStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<14)
	for {
		line, err := br.ReadString('\n')
		if trimmed := trimEnd(line); trimmed != "" {
			p.logger().Debug("acp", "line", trimmed)
		}
		if err != nil {
			return
		}
	}
}

func trimEnd(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// die runs once when the child's stdout ends, reported by its connection. It drops
// parked asks and publishes exit. There is no ping and no restart: an open request
// was already failed by the connection when the stream stopped.
func (p *process) die(readErr error) {
	p.mu.Lock()
	p.busy = false
	// The handle is taken here, not read later: a respawn swaps p.cmd once this
	// record is marked gone, and waiting on the wrong child would reap the new one.
	cmd := p.cmd
	id := p.session
	p.mu.Unlock()

	for _, ask := range p.eng.takeAll(id) {
		p.eng.hub.Publish(model.Event{
			Type:      "permission_cancel",
			Engine:    model.EngineACP,
			Session:   id,
			RequestID: ask.id,
		})
	}
	if err := cmd.Wait(); err != nil && readErr == nil {
		p.logger().Debug("wait", "error", err)
	}
	code := exitCode(cmd)
	if code == nil {
		p.logger().Info("acp process exited", "read", readErr)
	} else {
		p.logger().Info("acp process exited", "code", *code, "read", readErr)
	}
	p.eng.hub.Publish(model.Event{Type: "exit", Engine: model.EngineACP, Session: id, Code: code})
}

func exitCode(cmd *exec.Cmd) *int {
	if cmd == nil || cmd.ProcessState == nil {
		return nil
	}
	code := cmd.ProcessState.ExitCode()
	return &code
}

// stop closes stdin, waits killGrace, then sends SIGTERM.
func (p *process) stop() {
	p.closeStdin()
	if !p.exitedWithin(killGrace) {
		p.term()
	}
}

// halt ends the child so a respawn can take its place, and reports whether the
// child is gone. stdin closes first, and nothing harsher follows for a while,
// because the child writes the transcript of an in-flight turn as it sees that
// EOF: a signal that caught it mid-turn would lose the turn, and a kill would lose
// the session. Waiting for the death is also what frees the claim on the session
// file, which is the reason a respawn cannot start early.
func (p *process) halt() bool {
	p.closeStdin()
	if p.exitedWithin(haltGone) {
		return true
	}
	p.logger().Warn("acp child ignored stdin close, signalling it", "waited", haltGone.String())
	p.term()
	if p.exitedWithin(haltTerm) {
		return true
	}
	// Nothing is left that costs less. Starting a second child on the same session
	// while this one still holds the claim would only fail at the load, so the
	// toggle is called off and the stuck child is left to report its own death.
	p.logger().Error("acp child will not die, leaving the session alone", "waited", haltTerm.String())
	return false
}

// closeStdin ends the child's input once. A child that is already gone answers
// with an error nobody reads, so the flag is the only bookkeeping.
func (p *process) closeStdin() {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.closed {
		p.closed = true
		_ = p.stdin.Close()
	}
}

// term signals the child this record holds now.
func (p *process) term() {
	if cmd := p.handle(); cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}

// handle is the child this record owns right now, or nil before the first one.
func (p *process) handle() *exec.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd
}

// exitedWithin waits for the child's connection to report the stream ended.
func (p *process) exitedWithin(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if p.isExited() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(exitPoll)
	}
}

// setLoading opens or closes the history replay window of a respawn. Updates that
// arrive inside it reach neither the transcript cache nor the browser. See reload.
func (p *process) setLoading(on bool) {
	p.mu.Lock()
	p.loading = on
	p.mu.Unlock()
}

// claimRestart takes the one respawn a session may run at a time. A respawn holds
// it, and live() turns every other call away busy while it is held, so nothing can
// be sent to a child that has not finished loading its session. It answers the
// reason it refused, which is also what the caller reports.
func (p *process) claimRestart() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.restarting:
		return "a respawn is already running"
	case p.busy:
		// The one open prompt of a session: replacing the child would lose the turn
		// it is running, so the toggle has to wait for the person to end it.
		return "a prompt is running"
	}
	p.restarting = true
	return ""
}

func (p *process) releaseRestart() {
	p.mu.Lock()
	p.restarting = false
	p.mu.Unlock()
}

func (p *process) isRestarting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restarting
}

// permissionAnswer is what this session's child was started with, which is the
// same answer its row reports and the flag a toggle moves.
func (p *process) permissionAnswer() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.yolo
}

func (p *process) setPermissionAnswer(on bool) {
	p.mu.Lock()
	p.yolo = on
	p.mu.Unlock()
}

// isExited reports that this record has no live child: no connection yet, or one
// whose stream ended. The connection owns that fact, so a respawn that arms a new
// child is live again without any bookkeeping here.
func (p *process) isExited() bool {
	conn := p.wire.Load()
	return conn == nil || conn.Closed()
}

func (p *process) Session() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session
}

// logger carries the session id on every line once the handshake has one. The
// field itself is never reassigned: the read and stderr goroutines start before
// session/new answers, so a mutable logger would be a data race.
func (p *process) logger() *slog.Logger {
	if id := p.Session(); id != "" {
		return p.log.With("session", id)
	}
	return p.log
}

func (p *process) touch() {
	p.mu.Lock()
	p.updatedAt = time.Now().Unix()
	p.mu.Unlock()
}

// setStatus publishes a status event so the registry refetches the list. A dead
// child has no status to report: it is out of the list, and `exit` said it.
func (p *process) setStatus(status string) {
	if p.isExited() {
		return
	}
	p.touch()
	p.eng.hub.Publish(model.Event{
		Type:    "status",
		Engine:  model.EngineACP,
		Session: p.Session(),
		Status:  status,
		Title:   p.name(),
	})
}

func (p *process) name() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.title
}

// beginPrompt takes the one open turn this process may have and writes the
// request. The answer lands at end of turn, so the caller owns the call.
func (p *process) beginPrompt(text string) (*acpwire.Call, error) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return nil, model.ErrBusy
	}
	p.busy = true
	p.tr.user(text)
	session := p.session
	p.mu.Unlock()

	call, err := p.begin(methodSessionPrompt, map[string]any{
		"sessionId": session,
		"prompt":    []map[string]string{{"type": blockText, "text": text}},
	})
	if err != nil {
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
		return nil, err
	}
	return call, nil
}

// endPrompt releases the turn and reports how the agent answered it. A failed
// turn is not a failed request: the caller may hold a 200 already, because the
// browser learns about a turn from its update stream, so the reason also travels
// as a notice, in the agent's own words, which the mapped error drops.
func (p *process) endPrompt(m wireMessage) error {
	p.mu.Lock()
	p.busy = false
	p.mu.Unlock()
	_, err := payload(&m)
	if m.Error != nil && m.Error.Code != codeGone {
		p.logger().Warn("turn answered with an error", "error", err)
		p.eng.hub.Publish(model.Event{
			Type:    "notice",
			Engine:  model.EngineACP,
			Session: p.Session(),
			Text:    "turn failed: " + m.Error.Error(),
		})
	}
	p.resumeAfterAsk()
	return err
}

// configOption is the subset of a session config option this engine reads.
type configOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
}

type modeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []struct {
		ID string `json:"id"`
	} `json:"availableModes"`
}

func (m modeState) ids() []string {
	ids := make([]string, 0, len(m.AvailableModes))
	for _, mode := range m.AvailableModes {
		ids = append(ids, mode.ID)
	}
	return ids
}

// readModel caches the model the agent reports, so List shows what a turn ran. It
// never touches modelSpec: what a child says it loaded is not what the user asked
// this session for, and the next child has to be started with the second.
func (p *process) readModel(options []configOption) {
	for _, o := range options {
		if o.ID == optionModel && o.CurrentValue != "" {
			p.mu.Lock()
			p.modelName = o.CurrentValue
			p.mu.Unlock()
			return
		}
	}
}

// supportsMode reads the mode list session/new advertised. An empty list means
// the agent sent no modes, so no set_mode call can be predicted.
func (p *process) supportsMode(mode string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.modes {
		if m == mode {
			return true
		}
	}
	return false
}
