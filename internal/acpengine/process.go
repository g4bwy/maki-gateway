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

	// promptAdmission bounds how long Prompt waits for a rejection the agent can
	// only answer with right away. A turn that keeps running is not delayed by
	// more than this.
	promptAdmission = 400 * time.Millisecond
)

// process is one `maki acp` child. The server holds exactly one session per
// process (server.rs:101), so this is also the session record.
type process struct {
	eng *Engine
	log *slog.Logger

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	enc    *json.Encoder
	wmu    sync.Mutex
	closed bool

	mu        sync.Mutex
	seq       int64
	calls     map[int64]chan wireMessage
	tr        *transcript
	busy      bool
	exited    bool
	session   string
	cwd       string
	modelName string
	modes     []string
	title     string
	updatedAt int64
}

// spawn starts the child and runs the handshake, so the caller gets a session id
// it can prompt immediately.
func spawn(eng *Engine, cwd, m string) (*process, error) {
	args := []string{argACP}
	if m != "" {
		args = append(args, argModel, m)
	}
	cmd := exec.Command(eng.makiPath, args...)
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
		return nil, fmt.Errorf("start %s: %w", eng.makiPath, err)
	}

	p := &process{
		eng:       eng,
		cmd:       cmd,
		stdin:     stdin,
		enc:       json.NewEncoder(stdin),
		calls:     map[int64]chan wireMessage{},
		tr:        newTranscript(),
		cwd:       cwd,
		modelName: m,
	}
	p.log = eng.log.With("pid", cmd.Process.Pid)
	p.touch()
	go p.pumpStderr(stderr)
	go p.readLoop(stdout)
	if err := p.handshake(); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// handshake is `initialize` once per process, then `session/new`.
func (p *process) handshake() error {
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
	p.mu.Unlock()
	if u.SessionID != "" {
		id = u.SessionID
	}
	p.touch()
	p.eng.hub.Publish(model.Event{
		Type:    "update",
		Engine:  model.EngineACP,
		Session: id,
		ACP:     u.Update,
	})
	p.note(u.Update)
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
	if err := p.cmd.Wait(); err != nil && readErr == nil {
		p.logger().Debug("wait", "error", err)
	}
	code := p.exitCode()
	if code == nil {
		p.logger().Info("acp process exited", "read", readErr)
	} else {
		p.logger().Info("acp process exited", "code", *code, "read", readErr)
	}
	p.eng.hub.Publish(model.Event{Type: "exit", Engine: model.EngineACP, Session: id, Code: code})
}

func (p *process) exitCode() *int {
	if p.cmd.ProcessState == nil {
		return nil
	}
	code := p.cmd.ProcessState.ExitCode()
	return &code
}

// stop closes stdin, waits killGrace, then sends SIGTERM.
func (p *process) stop() {
	p.wmu.Lock()
	if !p.closed {
		p.closed = true
		_ = p.stdin.Close()
	}
	p.wmu.Unlock()

	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		if p.isExited() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if proc := p.cmd.Process; proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
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
