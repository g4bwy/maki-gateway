package acpengine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

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

	// args is the argv the session was created with, without --yolo, and yolo is
	// the permission answer it was created with. Both outlive one process, so a
	// respawn can repeat the spawn with the flag moved. cwd never changes, so it
	// needs no lock.
	args []string
	yolo bool
	cwd  string

	wmu    sync.Mutex
	stdin  io.WriteCloser
	enc    *json.Encoder
	closed bool

	mu         sync.Mutex
	cmd        *exec.Cmd
	seq        int64
	calls      map[int64]chan wireMessage
	tr         *transcript
	busy       bool
	exited     bool
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
		calls:     map[int64]chan wireMessage{},
		tr:        newTranscript(),
		cwd:       cwd,
		args:      baseArgv(m),
		yolo:      yolo,
		modelName: m,
	}
	c, err := launch(eng.makiPath, cwd, childArgv(p.args, yolo))
	if err != nil {
		return nil, err
	}
	p.log = eng.log.With("pid", c.cmd.Process.Pid)
	p.install(c)
	p.touch()
	go p.pumpStderr(c.stderr)
	go p.readLoop(c.stdout)
	if err := p.handshake(); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// baseArgv is the argv of one web child without the permission answer: the `acp`
// subcommand, then the model spec when the session named one.
func baseArgv(m string) []string {
	args := []string{argACP}
	if m != "" {
		args = append(args, argModel, m)
	}
	return args
}

// childArgv adds or drops the subcommand's own `--yolo`, which is where maki reads
// the permission answer (src/cli.rs:428-435, src/cmd/acp.rs:108). It reads it once
// at startup, so a running child cannot change its mind and the flag is the whole
// of a yolo toggle.
func childArgv(base []string, yolo bool) []string {
	args := make([]string, 0, len(base)+1)
	if !yolo {
		return append(args, base...)
	}
	args = append(args, base[0], argYolo)
	return append(args, base[1:]...)
}

// child is one started process with the pipes the gateway drives it on.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

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
// the old child is still owed an answer, because die failed every open request
// when its stdout closed, and a respawn waits for that.
func (p *process) install(c *child) {
	p.mu.Lock()
	p.cmd = c.cmd
	p.calls = map[int64]chan wireMessage{}
	p.exited = false
	p.mu.Unlock()

	p.wmu.Lock()
	p.stdin = c.stdin
	p.enc = json.NewEncoder(c.stdin)
	p.closed = false
	p.wmu.Unlock()
}

// respawnChild starts the next child of this session, asked to answer permissions
// the way yolo says, and makes it the one the wire methods talk to.
func (p *process) respawnChild(yolo bool) error {
	c, err := launch(p.eng.makiPath, p.cwd, childArgv(p.args, yolo))
	if err != nil {
		return err
	}
	p.install(c)
	p.log.Info("acp child respawned", "pid", c.cmd.Process.Pid, "yolo", yolo)
	go p.pumpStderr(c.stderr)
	go p.readLoop(c.stdout)
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

// call sends a request and waits for its answer.
func (p *process) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	key, ch, err := p.request(method, params)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-ch:
		return m.result()
	case <-timer.C:
		p.forget(key)
		return nil, errTimeout
	}
}

// request registers a pending call and writes its frame. session/prompt answers
// only at end of turn, so that caller waits on the channel instead of here.
func (p *process) request(method string, params any) (int64, chan wireMessage, error) {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return 0, nil, errGone
	}
	p.seq++
	key := p.seq
	ch := make(chan wireMessage, 1)
	p.calls[key] = ch
	frame := wireRequest{JSONRPC: jsonrpcVersion, ID: json.RawMessage(strconv.FormatInt(key, 10)), Method: method, Params: params}
	p.mu.Unlock()

	if err := p.send(frame); err != nil {
		p.forget(key)
		return 0, nil, err
	}
	return key, ch, nil
}

func (p *process) forget(key int64) {
	p.mu.Lock()
	delete(p.calls, key)
	p.mu.Unlock()
}

func (p *process) notify(method string, params any) error {
	return p.send(wireRequest{JSONRPC: jsonrpcVersion, Method: method, Params: params})
}

func (p *process) send(frame any) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.closed {
		return errGone
	}
	return p.enc.Encode(frame)
}

// respond answers an agent->client request, echoing its raw id. server.rs:851
// reads `id` plus either `result` or `error` off the frame.
func (p *process) respond(rawID json.RawMessage, result any) error {
	return p.send(map[string]any{"jsonrpc": jsonrpcVersion, "id": rawID, "result": result})
}

func (p *process) respondError(rawID json.RawMessage, code int, message string) error {
	return p.send(map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      rawID,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// readLoop decodes the stdout stream. A json.Decoder is used rather than a line
// scanner because one tool result can outgrow a 64 KiB line cap.
func (p *process) readLoop(r io.Reader) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<16))
	for {
		var m wireMessage
		if err := dec.Decode(&m); err != nil {
			p.die(err)
			return
		}
		p.dispatch(&m)
	}
}

func (p *process) dispatch(m *wireMessage) {
	switch m.kind() {
	case "response":
		p.answer(m)
	case "request":
		p.handleAsk(m)
	default:
		if m.Method == methodSessionUpdate {
			p.handleUpdate(m.Params)
		}
	}
}

// answer hands a response to whoever sent its request. Ids this side sends are
// always numbers, so anything else has no owner here.
func (p *process) answer(m *wireMessage) {
	var key int64
	if err := json.Unmarshal(m.ID, &key); err != nil {
		p.logger().Debug("response with an unreadable id", "id", string(m.ID))
		return
	}
	p.mu.Lock()
	ch, ok := p.calls[key]
	delete(p.calls, key)
	p.mu.Unlock()
	if !ok {
		p.logger().Debug("response for a forgotten request id", "id", key)
		return
	}
	ch <- *m
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

// handleAsk answers the two agent->client requests the server can send.
func (p *process) handleAsk(m *wireMessage) {
	switch m.Method {
	case methodRequestPermission:
		p.askPermission(m)
	case methodElicitationCreate:
		p.askElicitation(m)
	default:
		p.logger().Debug("unhandled agent request", "method", m.Method)
		_ = p.respondError(m.ID, codeMethodNotFound, "method not found")
	}
}

func (p *process) askPermission(m *wireMessage) {
	var req permissionRequest
	if err := json.Unmarshal(m.Params, &req); err != nil {
		p.logger().Debug("bad request_permission", "error", err)
		_ = p.respondError(m.ID, codeInvalidRequest, "invalid params")
		return
	}
	id := p.Session()
	requestID := p.eng.park(p, id, m.ID, req.Options)
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
}

func (p *process) askElicitation(m *wireMessage) {
	// No web form exists yet, so dismiss the ask the way a closed dialog reads.
	// elicitation.rs:130 turns any action other than "accept" into its DISMISSED
	// text, and "cancel" is the spelling its own test at elicitation.rs:255 uses.
	if err := p.respond(m.ID, map[string]string{"action": actionCancel}); err != nil {
		p.logger().Debug("elicitation cancel failed", "error", err)
	}
	p.eng.hub.Publish(model.Event{
		Type:    "notice",
		Engine:  model.EngineACP,
		Session: p.Session(),
		Text:    noticeQuestion,
	})
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

// die runs once when stdout closes. It fails every open request, drops parked
// asks, and publishes exit. There is no ping and no restart.
func (p *process) die(readErr error) {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return
	}
	p.exited = true
	p.busy = false
	// The handle is taken here, not read later: a respawn swaps p.cmd once this
	// record is marked gone, and waiting on the wrong child would reap the new one.
	cmd := p.cmd
	waiting := make([]chan wireMessage, 0, len(p.calls))
	for key, ch := range p.calls {
		waiting = append(waiting, ch)
		delete(p.calls, key)
	}
	id := p.session
	p.mu.Unlock()

	gone := wireMessage{Error: &wireError{Code: codeGone, Message: "process exited"}}
	for _, ch := range waiting {
		ch <- gone
	}
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

// exitedWithin waits for the die path to report the child gone.
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

func (p *process) isExited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
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
// request. The answer lands at end of turn, so the caller owns the channel.
func (p *process) beginPrompt(text string) (chan wireMessage, error) {
	p.mu.Lock()
	switch {
	case p.exited:
		p.mu.Unlock()
		return nil, errGone
	case p.busy:
		p.mu.Unlock()
		return nil, model.ErrBusy
	}
	p.busy = true
	p.tr.user(text)
	session := p.session
	p.mu.Unlock()

	_, ch, err := p.request(methodSessionPrompt, map[string]any{
		"sessionId": session,
		"prompt":    []map[string]string{{"type": blockText, "text": text}},
	})
	if err != nil {
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

// endPrompt releases the turn and reports how the agent answered it. A failed
// turn is not a failed request: the caller may hold a 200 already, because the
// browser learns about a turn from its update stream, so the reason also travels
// as a notice, in the agent's own words, which the mapped error drops.
func (p *process) endPrompt(m wireMessage) error {
	p.mu.Lock()
	p.busy = false
	p.mu.Unlock()
	_, err := m.result()
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

// readModel caches the model the agent reports, so List shows what a turn ran.
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
