// Package acpengine implements model.Engine for web sessions: one `maki acp`
// child process per session, driven over ACP JSON-RPC v1 on newline-delimited
// JSON on its stdio. No LSP framing, no Content-Length headers.
//
// Ground truth for every wire shape is the Rust server in
// maki-acp/src/{server,methods,permissions,elicitation,translate}.rs; the
// quotes and file:line refs live in docs/ACP_MIN.md, which doubles as the
// contract the fake in fake.go obeys.
package acpengine

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

const (
	// A respawn can lose the claim on the session file to the child it is
	// replacing, since an flock is only free once that process is gone. So the
	// whole spawn-and-load starts over, this many times, with this long between
	// attempts.
	defaultYoloRetries   = 10
	defaultYoloRetryWait = 300 * time.Millisecond
)

// Engine is the web engine. It is safe for concurrent use.
type Engine struct {
	makiPath string
	hub      *model.Hub
	log      *slog.Logger

	mu    sync.Mutex
	procs map[string]*process

	// How many respawns a held session claim buys, and how long between them.
	// Fields, so a test can bound the loop it drives.
	yoloRetries   int
	yoloRetryWait time.Duration

	parkMu sync.Mutex
	parkID int64
	parked map[int64]*parkedAsk
}

// parkedAsk is one agent->client permission request this side still owes an
// answer to. The Request is the codec's own handle: it carries the JSON-RPC id,
// which may be a number or a string (server.rs:880), and answers once. id is what
// this engine publishes, so the two ids must travel together.
type parkedAsk struct {
	id      int64
	req     *acpwire.Request
	proc    *process
	session string
	options []permissionOption
}

var _ model.Engine = (*Engine)(nil)

// New returns an engine that spawns maki at makiPath. The path comes from the
// gateway's -maki flag, so tests point it at the fake.
func New(makiPath string, hub *model.Hub) *Engine {
	return &Engine{
		makiPath:      makiPath,
		hub:           hub,
		log:           slog.Default().With("engine", model.EngineACP),
		procs:         map[string]*process{},
		yoloRetries:   defaultYoloRetries,
		yoloRetryWait: defaultYoloRetryWait,
		parked:        map[int64]*parkedAsk{},
	}
}

// Kind is model.KindWeb: these sessions exist only while this process lives.
func (e *Engine) Kind() string { return model.KindWeb }

// Create spawns the child, handshakes, and returns the sessionId the agent
// chose. A non-empty prompt starts the first turn. req.Yolo is the permission
// answer given at creation: it goes to the child as `--yolo`, and the record keeps
// it, so List reports the same answer SetYolo moves.
func (e *Engine) Create(req model.CreateRequest) (string, error) {
	cwd := req.Cwd
	// session/new demands an absolute cwd (agent.rs:1225), and the browser can
	// post none at all, so an empty one means "where the gateway runs".
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("%w: cwd %q is not absolute", model.ErrUnsupported, cwd)
	}
	p, err := spawn(e, cwd, req.Model, req.Yolo)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.procs[p.Session()] = p
	e.mu.Unlock()
	p.setStatus(model.StatusIdle)
	if req.Prompt != "" {
		if _, err := e.Prompt(p.Session(), req.Prompt); err != nil {
			e.log.Warn("initial prompt failed", "session", p.Session(), "error", err)
		}
	}
	return p.Session(), nil
}

// List reports live web sessions. An exited one stays out of the list so the
// store engine can answer for it, while its cached transcript still serves
// Messages until Kill drops it. A session whose child is being replaced by a yolo
// toggle keeps its row: the same conversation continues on the next process.
func (e *Engine) List() ([]model.Session, error) {
	e.mu.Lock()
	procs := make([]*process, 0, len(e.procs))
	for _, p := range e.procs {
		procs = append(procs, p)
	}
	e.mu.Unlock()

	out := make([]model.Session, 0, len(procs))
	for _, p := range procs {
		if s, ok := p.entry(); ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Messages returns the cached transcript tail built from prompts and updates.
func (e *Engine) Messages(id string, last int) ([]model.Message, error) {
	p, ok := e.lookup(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", model.ErrNotFound, id)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tr.msgs) == 0 && p.isExited() {
		return nil, fmt.Errorf("%w: %s", model.ErrNotFound, id)
	}
	return p.tr.tail(last), nil
}

// Prompt sends session/prompt and returns "started" once the frame is written.
// The agent answers that request only at end of turn, so the wait does not block
// the handler: a background goroutine completes the pending request, and turn
// end reaches the browser as update events. The one thing the caller must not
// learn late is a rejection, and the server answers those in the same breath it
// received the request, so a short admission wait turns a -32603 into 409.
func (e *Engine) Prompt(id, text string) (string, error) {
	p, err := e.live(id)
	if err != nil {
		return "", err
	}
	call, err := p.beginPrompt(text)
	if err != nil {
		return "", err
	}
	p.setStatus(model.StatusWorking)
	done := make(chan error, 1)
	go func() { done <- p.endPrompt(<-call.Resp) }()
	select {
	case err := <-done:
		// Losing the child after the frame went out is not a rejection. The
		// caller gets 200 and the browser learns about it from the exit event.
		if err != nil && !errors.Is(err, errGone) {
			return "", err
		}
	case <-time.After(promptAdmission):
	}
	return "started", nil
}

// Cancel sends the session/cancel notification and withdraws every ask the agent
// still holds for that session, which is what ACP requires of a client that
// cancels a turn (client.rs:2440). The agent then answers the open prompt with
// the cancelled stop reason.
func (e *Engine) Cancel(id string) error {
	p, err := e.live(id)
	if err != nil {
		return err
	}
	e.withdrawAsks(p)
	return p.notify(methodSessionCancel, map[string]string{"sessionId": id})
}

// withdrawAsks drops every open ask of one session, answers the agent when the
// child can still read, and publishes permission_cancel so the dialog closes.
// The asks must be read, not just deleted, or the written answer would have no
// id to echo and the agent would stay parked.
func (e *Engine) withdrawAsks(p *process) {
	for _, ask := range e.takeAll(p.Session()) {
		_ = ask.req.Answer(map[string]any{"outcome": map[string]string{"outcome": outcomeCancelled}})
		e.hub.Publish(model.Event{
			Type:      "permission_cancel",
			Engine:    model.EngineACP,
			Session:   ask.session,
			RequestID: ask.id,
		})
	}
}

// takeAll drops and reports every open ask of one session, by id.
func (e *Engine) takeAll(session string) []*parkedAsk {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	var got []*parkedAsk
	for id, ask := range e.parked {
		if ask.session == session {
			got = append(got, ask)
			delete(e.parked, id)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].id < got[j].id })
	return got
}

// SetMode sends session/set_mode, which maki-acp handles for build and plan
// (server.rs:785). An agent that advertised no modes cannot be asked for one.
func (e *Engine) SetMode(id, mode string) error {
	if mode != modeBuild && mode != modePlan {
		return fmt.Errorf("%w: mode %q", model.ErrUnsupported, mode)
	}
	p, err := e.live(id)
	if err != nil {
		return err
	}
	if !p.supportsMode(mode) {
		return fmt.Errorf("%w: mode %q", model.ErrUnsupported, mode)
	}
	_, err = p.call(methodSessionSetMode, map[string]string{"sessionId": id, "modeId": mode}, callTimeout)
	return err
}

// SetModel switches the model of a live session with session/set_config_option,
// the config option maki-acp publishes under the id "model" (methods.rs:14). It
// is not part of model.Engine, because CONTRACTS 2 has no route for it yet.
//
// The value is also remembered as the model of the session, which is what the next
// child of a yolo toggle is started with: maki reads -m at startup only, so a
// respawn that repeated the create request alone would quietly undo this change.
func (e *Engine) SetModel(id, m string) error {
	p, err := e.live(id)
	if err != nil {
		return err
	}
	_, err = p.call(methodSessionSetConfig, map[string]any{
		"sessionId": id, "configId": optionModel, "value": m,
	}, callTimeout)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.modelName = m
	p.modelSpec = m
	p.mu.Unlock()
	return nil
}

// SetYolo answers a mid-session yolo toggle for one web session.
//
// maki reads the permission answer from the `acp --yolo` flag once, at startup, so
// the child a session runs on cannot be talked into another mode. What works
// instead is a new child on the same session: this ends the current one, spawns
// another with the flag set to match `on`, and asks it to load the session, which
// replays the stored history and continues the conversation from there. The
// transcript cache is kept as it was, so the browser sees no gap and no line twice.
//
// A session whose child died is revived by the same call, since the record still
// holds its id and cwd: `on` is applied to the child that loads it back.
//
// What it costs the session: every permission dialog the browser still holds is
// withdrawn, background work of the old child dies with it, and a prompt in flight
// blocks the toggle with model.ErrBusy rather than being cut short. The model and
// the working directory are not lost: the next child is started with the model the
// user last chose for this session, and loads the same session id in the same cwd.
func (e *Engine) SetYolo(id string, on bool) error {
	e.mu.Lock()
	p, ok := e.procs[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", model.ErrNotFound, id)
	}
	if reason := p.claimRestart(); reason != "" {
		return fmt.Errorf("%w: %s", model.ErrBusy, reason)
	}
	defer p.releaseRestart()
	if p.permissionAnswer() == on && !p.isExited() {
		return nil
	}
	if err := e.respawn(p, on); err != nil {
		// The record stays, and stays out of the list, with its transcript for
		// whoever reads the session next. Another toggle can retry from here.
		return err
	}
	p.setPermissionAnswer(on)
	p.setStatus(model.StatusIdle)
	return nil
}

// respawn restarts one session's child and reloads the session into it, retrying
// the whole attempt while the previous child still holds the claim. A failure that
// is not that race is returned at once: no number of tries fixes a binary that will
// not start.
func (e *Engine) respawn(p *process, on bool) error {
	var last error
	for attempt := 1; ; attempt++ {
		last = e.respawnOnce(p, on)
		if last == nil || !errors.Is(last, model.ErrOpenElsewhere) {
			return last
		}
		if attempt >= e.yoloRetries {
			break
		}
		e.log.Info("session still claimed, retrying", "session", p.Session(),
			"attempt", attempt, "error", last)
		time.Sleep(e.yoloRetryWait)
	}
	return fmt.Errorf("%w: %s still claimed after %d respawn attempts: %w",
		model.ErrOpenElsewhere, p.Session(), e.yoloRetries, last)
}

// respawnOnce is one attempt: end the child, start the next one, load the session.
// An attempt that cannot load what it started ends that child again, so the next
// one faces the same clean state, and a session nobody else holds.
func (e *Engine) respawnOnce(p *process, on bool) error {
	if !p.halt() {
		return fmt.Errorf("the acp child of %s would not exit", p.Session())
	}
	// Kill deletes the record while the child is going down. Then no new child may
	// start behind it, and the person who asked for the toggle learns the session is
	// gone rather than finding a process nothing owns.
	if !e.owns(p) {
		return fmt.Errorf("%w: %s", model.ErrNotFound, p.Session())
	}
	if err := p.respawnChild(on); err != nil {
		return err
	}
	if err := p.reload(); err != nil {
		p.stop()
		return err
	}
	return nil
}

// owns reports whether the engine still maps this session to this record.
func (e *Engine) owns(p *process) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.procs[p.Session()] == p
}

// SetTitle is unsupported: maki-acp answers no session/set_title, so the title
// only ever arrives from the agent as a session_info_update.
func (e *Engine) SetTitle(string, string) error {
	return fmt.Errorf("%w: session/set_title", model.ErrUnsupported)
}

// Kill ends the child and drops its record, transcript included. A session whose
// process already died is dropped too, which is the only way its cache is freed.
func (e *Engine) Kill(id string) error {
	p, ok := e.lookup(id)
	if !ok {
		return fmt.Errorf("%w: %s", model.ErrNotFound, id)
	}
	e.mu.Lock()
	delete(e.procs, id)
	e.mu.Unlock()
	// Withdraw the dialogs before the child goes: no answer can reach it after.
	e.withdrawAsks(p)
	p.stop()
	return nil
}

// RespondPermission answers a parked session/request_permission with the option
// id the decision maps to (optionFor). ACP has no field for guidance, so it is
// dropped.
func (e *Engine) RespondPermission(requestID int64, decision, guidance string) error {
	e.parkMu.Lock()
	ask, ok := e.parked[requestID]
	delete(e.parked, requestID)
	e.parkMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: permission %d expired", model.ErrNotFound, requestID)
	}
	option, err := optionFor(ask.options, decision)
	if err != nil {
		e.parkMu.Lock()
		e.parked[requestID] = ask
		e.parkMu.Unlock()
		return err
	}
	if err := ask.req.Answer(map[string]any{
		"outcome": map[string]any{"outcome": outcomeSelected, "optionId": option},
	}); err != nil {
		return mapError(err)
	}
	ask.proc.resumeAfterAsk()
	return nil
}

func (e *Engine) lookup(id string) (*process, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.procs[id]
	return p, ok
}

// live is the guard every call that needs the child takes. A dead one is gone
// from the caller's point of view even while its transcript cache stays, and one
// that is respawning has a child that has not finished loading its session, so
// nothing may be sent to it yet.
func (e *Engine) live(id string) (*process, error) {
	p, ok := e.lookup(id)
	if !ok || p.isExited() {
		return nil, fmt.Errorf("%w: %s", model.ErrNotFound, id)
	}
	if p.isRestarting() {
		return nil, fmt.Errorf("%w: session is respawning", model.ErrBusy)
	}
	return p, nil
}

// park records an open ask and returns the id this engine publishes for it.
// Ids are per-engine counters, never the agent's, so two children that both
// start at 1 cannot answer each other.
func (e *Engine) park(p *process, session string, req *acpwire.Request, options []permissionOption) int64 {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	e.parkID++
	e.parked[e.parkID] = &parkedAsk{id: e.parkID, req: req, proc: p, session: session, options: options}
	return e.parkID
}

// resumeAfterAsk puts the session status back the way the turn left it.
func (p *process) resumeAfterAsk() {
	p.mu.Lock()
	busy := p.busy
	p.mu.Unlock()
	if busy {
		p.setStatus(model.StatusWorking)
		return
	}
	p.setStatus(model.StatusIdle)
}

// entry renders the list row, or false once the child is gone and nothing is
// taking its place. A child that is being replaced still has a row: the session
// continues on the next process, and dropping the row for the moment between them
// would tell the browser the chat ended.
func (p *process) entry() (model.Session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == "" || (p.isExited() && !p.restarting) {
		return model.Session{}, false
	}
	return model.Session{
		ID:        p.session,
		Kind:      model.KindWeb,
		Title:     p.title,
		Status:    p.statusLocked(),
		Cwd:       p.cwd,
		Model:     p.modelName,
		UpdatedAt: p.updatedAt,
		Yolo:      p.yolo,
	}, true
}

func (p *process) statusLocked() string {
	switch {
	case p.eng.hasParked(p.session):
		return model.StatusNeedsInput
	case p.busy:
		return model.StatusWorking
	default:
		return model.StatusIdle
	}
}

// hasParked is read with the process lock held, so it must not call back into
// the process.
func (e *Engine) hasParked(session string) bool {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	for _, ask := range e.parked {
		if ask.session == session {
			return true
		}
	}
	return false
}
