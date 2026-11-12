package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

// fakeTUI is the plugin side of docs/WIRE.md: a v2 agent that serves one unix
// socket in a temp dir on the same codec the gateway drives, answers the requests
// of the method index, pushes the two notifications, and sends the one request a
// plugin sends. Everything it asserts is a contract assertion.
//
// A fake also has to repeat the abuses of the wire, so it can leave a request
// unanswered
// (frozen), answer it with neither reply field (bare), or answer with a canned
// error of a given code (fail).
type fakeTUI struct {
	t    *testing.T
	path string
	pid  int

	mu      sync.Mutex
	ln      *net.UnixListener
	agents  []*agent
	frames  []request // every request the gateway sent
	closed  bool
	proto   any                 // what _meta reports for attach_proto
	noProto bool                // omit the attach_proto key entirely
	makiVer string              // what _meta reports for maki_version
	tabs    []map[string]any    // what session/list answers
	tasks   []map[string]any    // what _sh.maki/tasks answers
	focus   string              // the task the last follow moved on screen
	noUI    bool                // answer every follow like a headless maki
	frozen  bool                // stop answering session/list, the liveness clock
	fail    map[string]string   // method -> canned error message
	codes   map[string]int      // method -> canned error code
	bare    map[string]bool     // method -> answer with neither reply field
	hang    map[string]bool     // method -> take the ask and never answer it
	state   string              // what _sh.maki/queue answers
	asks    map[string]*fakeAsk // plugin request id -> open ask
	answers map[string]wirePermission
	nextID  int64
}

// The keys of what a plugin writes back. The gateway forwards or decodes these
// through the shapes of protocol.go, and never sends them.
const (
	keySessions     = "sessions"
	keyMessages     = "messages"
	keyState        = "state"
	keyMode         = "mode"
	keyPath         = "path"
	keyReady        = "ready"
	keyContent      = "content"
	keyToolCall     = "toolCall"
	keyToolCallID   = "toolCallId"
	keyRawInput     = "rawInput"
	keyOptions      = "options"
	keyOptionID     = "optionId"
	keyName         = "name"
	keyKind         = "kind"
	keyStatus       = "status"
	keyUpdate       = "update"
	keySessionUpd   = "sessionUpdate"
	upUserChunk     = "user_message_chunk"
	upThoughtChunk  = "agent_thought_chunk"
	upMessageChunk  = "agent_message_chunk"
	upModeUpdate    = "current_mode_update"
	keyOutcome      = "outcome"
	keyRowID        = "id"
	keyCurrentMode  = "currentModeId"
	keyAvailable    = "availableModes"
	keyModes        = "modes"
	keyPluginBuild  = "agentCapabilities"
	keyLoadSession  = "loadSession"
	keyPromptCaps   = "promptCapabilities"
	keySessionCaps  = "sessionCapabilities"
	keyListCap      = "list"
	keyMeta         = "_meta"
	keyClock        = "updatedAt"
	keyFocused      = "focused"
	keyTool         = "tool"
	keyTaskSession  = "session"
	keyContentBlock = "content"
	keyType         = "type"
	keyPrompt       = "prompt"
	keyUsed         = "used"
	keySize         = "size"
)

// request is one recorded call frame: the method, its params, and the request
// number the gateway put on it.
// rawParams keeps the params frame exactly as it arrived, so a test can tell an
// empty object from an absent field (WIRE 1.3).
type request struct {
	method    string
	params    map[string]any
	id        json.RawMessage
	rawParams json.RawMessage
}

// agent is one accepted gateway connection, seen from the plugin side.
type agent struct {
	f    *fakeTUI
	p    *plug
	wire *acpwire.Conn
}

// fakeAsk is one session/request_permission the plugin sent and still owes an
// answer to.
type fakeAsk struct {
	id   json.RawMessage // the plugin's own number: 1000 up (WIRE 1.4)
	wire string          // the same id, canonically spelled
	ag   *agent
}

// wirePermission is the answer of WIRE 10.1, read off the wire.
type wirePermission struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	} `json:"outcome"`
	Meta map[string]any `json:"_meta"`
}

// plug is one socket from the plugin side. Writes are one frame at a time,
// whether they leave through the codec or through a raw wire-abuse frame.
type plug struct {
	nc net.Conn
	mu sync.Mutex
}

func (p *plug) Read(b []byte) (int, error) { return p.nc.Read(b) }

func (p *plug) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nc.Write(b)
}

func (p *plug) raw(line string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := io.WriteString(p.nc, line)
	return err
}

func (p *plug) Close() error { return p.nc.Close() }

func fastTimings() timings {
	return timings{
		scanEvery:  25 * time.Millisecond,
		timeout:    250 * time.Millisecond,
		tryAll:     250 * time.Millisecond,
		cancelWait: 100 * time.Millisecond,
		missLimit:  2,
		maxLine:    acpwire.MaxLine,
	}
}

// startEngine returns a watching engine over dir plus its event stream.
func startEngine(t *testing.T, dir string, tt timings) (*Engine, chan model.Event) {
	t.Helper()
	hub := model.NewHub()
	ch := hub.Subscribe()
	e := newEngine(dir, hub, tt)
	t.Cleanup(func() { e.Close() })
	return e, ch
}

func newFakeTUI(t *testing.T, dir, name string, pid int, tabs ...map[string]any) *fakeTUI {
	t.Helper()
	f := &fakeTUI{
		t: t, path: filepath.Join(dir, name),
		proto: float64(minAttachProto), makiVer: "0.7.1",
		fail: map[string]string{}, codes: map[string]int{}, bare: map[string]bool{},
		hang:  map[string]bool{},
		state: "started", asks: map[string]*fakeAsk{}, answers: map[string]wirePermission{},
	}
	if len(tabs) > 0 {
		f.tabs = tabs
	} else {
		f.tabs = []map[string]any{liveTab("s1", model.StatusIdle)}
	}
	f.tasks = fakeTaskRows()
	f.listen(pid)
	t.Cleanup(f.stop)
	return f
}

// liveTab is one row of session/list as WIRE 3 writes it: ACP's SessionInfo, the
// maki facts in _meta, and the clock in the one format v2 has.
func liveTab(id, status string) map[string]any {
	return map[string]any{
		argSessionID: id,
		argCwd:       "/work",
		argTitle:     "tab " + id,
		keyClock:     tabClock,
		keyMeta: map[string]any{
			metaStatus:  status,
			metaModel:   "m-" + id,
			metaCwd:     "/work",
			metaMode:    "build",
			metaFocused: id == "s1",
		},
	}
}

const tabClock = "2026-11-12T09:14:03Z"

// tabClockEpoch is the same instant in the seconds the registry works in, spelled
// without the code under test.
var tabClockEpoch = time.Date(2026, 11, 12, 9, 14, 3, 0, time.UTC).Unix()

// One row of maki.task.list() beyond the fields the gateway reads.
const (
	taskStartedAt = 1700000000
	taskCost      = 0.02
)

func (f *fakeTUI) listen(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.ln = ln.(*net.UnixListener)
	f.agents, f.closed = nil, false
	f.pid = pid
	go f.accept(f.ln)
}

// crash is a killed TUI: the connections die and the socket file stays behind.
func (f *fakeTUI) crash() {
	f.mu.Lock()
	ln, agents := f.ln, f.agents
	f.ln, f.agents, f.closed = nil, nil, true
	f.mu.Unlock()
	if ln != nil {
		ln.SetUnlinkOnClose(false)
		_ = ln.Close()
	}
	for _, ag := range agents {
		_ = ag.p.Close()
	}
}

// restart is the same path coming back to life.
func (f *fakeTUI) restart() {
	f.mu.Lock()
	f.answers, f.frozen = map[string]wirePermission{}, false
	f.fail, f.codes, f.bare = map[string]string{}, map[string]int{}, map[string]bool{}
	f.hang = map[string]bool{}
	f.asks = map[string]*fakeAsk{}
	f.noUI, f.focus, f.closed = false, "", false
	pid := f.pid
	f.mu.Unlock()
	f.listen(pid)
}

func (f *fakeTUI) stop() {
	f.mu.Lock()
	ln, agents := f.ln, f.agents
	f.ln, f.agents = nil, nil
	f.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, ag := range agents {
		_ = ag.p.Close()
	}
}

func (f *fakeTUI) accept(ln *net.UnixListener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		ag := &agent{f: f, p: &plug{nc: nc}}
		f.mu.Lock()
		stale := f.closed || f.ln != ln
		if !stale {
			f.agents = append(f.agents, ag)
		}
		f.mu.Unlock()
		if stale {
			_ = nc.Close()
			return
		}
		ag.wire = acpwire.New(ag.p, acpwire.Options{
			OnRequest: ag.onRequest,
			OnClose:   func(error) { f.hangUp(ag) },
		})
		ag.wire.Start()
	}
}

// onRequest is the agent half of the codec: read the call, answer it, or take
// ownership and stay silent, which is how a stalled plugin is faked.
func (ag *agent) onRequest(_ context.Context, r *acpwire.Request) error {
	params, err := decodeParams(r.Params)
	if err != nil {
		return err
	}
	ag.f.record(request{method: r.Method, params: params, id: r.ID, rawParams: bytes.Clone(r.Params)})

	f := ag.f
	f.mu.Lock()
	bare, msg, code := f.bare[r.Method], f.fail[r.Method], f.codes[r.Method]
	hung := f.hang[r.Method]
	frozen, pid := f.frozen, f.pid
	proto, noProto, maki := f.proto, f.noProto, f.makiVer
	state, headless := f.state, f.noUI
	f.mu.Unlock()

	if bare {
		// A plugin that broke WIRE 1.3: a response carries exactly one reply field.
		_ = ag.p.raw(`{"jsonrpc":"2.0","id":` + string(r.ID) + "}\n")
		return nil
	}
	if msg != "" {
		if code == 0 {
			code = acpwire.CodeInternal
		}
		return &acpwire.Error{Code: code, Message: msg}
	}
	if hung {
		// A stalled plugin: it read the request and has nothing left to say.
		return nil
	}

	switch r.Method {
	case mInitialize:
		declared := meta{metaPID: jsonNumber(pid), metaCwd: jsonString("/work"),
			metaMakiVersion: jsonString(maki)}
		if !noProto {
			declared[metaAttachProto] = jsonAny(proto)
		}
		return ag.answer(r, map[string]any{
			argProtocolVersion: acpProtocol,
			keyPluginBuild: map[string]any{
				keyLoadSession: true,
				keyPromptCaps:  map[string]any{},
				keySessionCaps: map[string]any{keyListCap: map[string]any{}},
			},
			keyMeta: declared,
		})
	case mSessionList:
		if frozen {
			return nil
		}
		return ag.answer(r, map[string]any{keySessions: f.tabsSnapshot()})
	case mSessionLoad:
		if err := f.requireLive(params); err != nil {
			return ag.fail(r, err)
		}
		// WIRE 4: the replay goes out first, and the answer follows the last one.
		f.pushReplay(strParam(params, argSessionID))
		return ag.answer(r, map[string]any{keyModes: map[string]any{
			keyCurrentMode: "build",
			keyAvailable: []any{
				map[string]any{keyRowID: "build", keyName: "Build"},
				map[string]any{keyRowID: "plan", keyName: "Plan"},
			},
		}})
	case mMessages:
		if err := f.requireLive(params); err != nil {
			return ag.fail(r, err)
		}
		return ag.answer(r, map[string]any{keyMessages: f.messagesFor(strParam(params, argSessionID))})
	case mQueue:
		if err := f.requireLive(params); err != nil {
			return ag.fail(r, err)
		}
		if strParam(params, argText) == "" {
			return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInvalidParams, Message: "text is required"})
		}
		return ag.answer(r, map[string]any{keyState: state})
	case mNotify:
		if strParam(params, argText) == "" {
			return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInvalidParams, Message: "text is required"})
		}
		if s := strParam(params, argSessionID); s != "" {
			if err := f.requireLive(params); err != nil {
				return ag.fail(r, err)
			}
		}
		return ag.answer(r, true)
	case mPlan:
		if err := f.requireLive(params); err != nil {
			return ag.fail(r, err)
		}
		return ag.answer(r, map[string]any{keyMode: "plan", keyPath: "/tmp/plan.md",
			keyReady: true, keyContent: "# plan"})
	case mTasks:
		return ag.answer(r, f.tasksSnapshot())
	case mTaskFocus:
		return ag.follow(r, strParam(params, argTaskID), headless)
	case mSessionSetMode:
		m := strParam(params, argModeID)
		if m != "build" && m != "plan" {
			return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInvalidParams,
				Message: `mode must be "build" or "plan"`})
		}
		// WIRE 8.1: the mode moved, so the update stream says so too.
		f.pushUpdate(strParam(params, argSessionID), map[string]any{keySessionUpd: upModeUpdate})
		return ag.answer(r, map[string]any{})
	case mSessionSetTitle:
		if err := f.requireLive(params); err != nil {
			return ag.fail(r, err)
		}
		// WIRE 8.2: the title reaches the gateway as an event, not in the answer.
		f.pushEvent(map[string]any{keyKind: kindTitle, argSessionID: strParam(params, argSessionID),
			argTitle: strParam(params, argTitle)})
		return ag.answer(r, map[string]any{})
	case mPopQueue, mRunCommand:
		return ag.answer(r, true)
	case mSessionCancel:
		// WIRE 7.3: no live tab is cancelled over this socket. This is the fixed
		// answer, not a gap in the fake.
		return ag.fail(r, &acpwire.Error{Code: acpwire.CodeMethodNotFound, Message: "method not found"})
	case mSessionNew:
		// WIRE Appendix B.1: a plugin answers session/new with -32601.
		return ag.fail(r, &acpwire.Error{Code: acpwire.CodeMethodNotFound, Message: "method not found"})
	}
	return ag.fail(r, &acpwire.Error{Code: acpwire.CodeMethodNotFound, Message: "method not found"})
}

// answer writes one result, and reports to the codec that the ask is closed.
func (ag *agent) answer(r *acpwire.Request, result any) error {
	if err := r.Answer(result); err != nil {
		return err
	}
	return nil
}

// fail hands an error to the codec, which writes it as the answer.
func (ag *agent) fail(r *acpwire.Request, err error) error {
	return err
}

// follow is the host side of _sh.maki/task_focus (WIRE 9.4): the task rides in
// `taskId`, it must be a row of this tab, and following it raises the task_focus
// event the way the host's TaskFocusChanged does. The v1 fallback that took the
// name from the frame's own request number is gone with the collision that made
// it necessary, so a follow that names no task is a bug in the gateway.
func (ag *agent) follow(r *acpwire.Request, taskID string, headless bool) error {
	f := ag.f
	if taskID == "" {
		return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInvalidParams, Message: "taskId is required"})
	}
	if headless {
		return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInternal, Message: refuseUI})
	}
	found := false
	for _, row := range f.tasksSnapshot() {
		if strParam(row, keyRowID) == taskID {
			found = true
		}
	}
	if !found {
		return ag.fail(r, &acpwire.Error{Code: acpwire.CodeInvalidParams,
			Message: "no task with id " + taskID})
	}
	f.mu.Lock()
	f.focus = taskID
	f.mu.Unlock()
	f.pushEvent(map[string]any{keyKind: kindTaskFocus, argSessionID: "s1", argTaskID: taskID})
	return ag.answer(r, true)
}

// refuseUI is the words a headless maki answers a follow with (WIRE 13.1).
const refuseUI = noUI + " attached"

// push sends one notification to every live connection, as the plugin does: every
// connection gets the same events (WIRE 1.1).
func (f *fakeTUI) push(method string, params any) {
	f.mu.Lock()
	agents := slices.Clone(f.agents)
	f.mu.Unlock()
	for _, ag := range agents {
		if err := ag.wire.Notify(method, params); err != nil {
			f.t.Logf("fake: push %s: %v", method, err)
		}
	}
}

// pushEvent sends one _sh.maki/session_event of the given kind (WIRE 11.2).
func (f *fakeTUI) pushEvent(params map[string]any) {
	f.push(mSessionEvent, params)
}

// pushUpdate sends one session/update of a live tab (WIRE 11.1).
func (f *fakeTUI) pushUpdate(session string, update map[string]any) {
	f.push(mSessionUpdate, map[string]any{argSessionID: session, keyUpdate: update})
}

// pushReplay is the history stream of WIRE 4: chunks, one tool card and its end,
// then the answer of the load. A plugin pushes it on the same connection.
func (f *fakeTUI) pushReplay(session string) {
	f.pushUpdate(session, map[string]any{keySessionUpd: upUserChunk,
		keyContentBlock: map[string]any{keyType: "text", argText: "fix the tests"}})
	f.pushUpdate(session, map[string]any{keySessionUpd: upThoughtChunk,
		keyContentBlock: map[string]any{keyType: "text", argText: "the failing case is here"}})
	f.pushUpdate(session, map[string]any{keySessionUpd: upToolCall, keyToolCallID: "tc9",
		keyName: "read", argTitle: "read file", keyStatus: statusPending})
	f.pushUpdate(session, map[string]any{keySessionUpd: upToolUpdate, keyToolCallID: "tc9",
		keyStatus: statusCompleted})
	f.pushUpdate(session, map[string]any{keySessionUpd: upMessageChunk,
		keyContentBlock: map[string]any{keyType: "text", argText: "done"}})
}

// writeRaw sends arbitrary bytes at the gateway: oversized and malformed frames.
func (f *fakeTUI) writeRaw(s string) {
	f.mu.Lock()
	agents := slices.Clone(f.agents)
	f.mu.Unlock()
	for _, ag := range agents {
		if err := ag.p.raw(s); err != nil {
			f.t.Logf("fake: write failed: %v", err)
		}
	}
}

// askPermission sends one session/request_permission (WIRE 10) and keeps it open
// until the gateway answers. It returns the plugin's own request id, which is the
// only thing that ties a later answer or withdrawal back to this ask.
func (f *fakeTUI) askPermission(session, title string, input map[string]any) string {
	f.mu.Lock()
	ag := f.firstAgentLocked()
	f.nextID++
	if f.nextID < pluginIDFloor {
		f.nextID = pluginIDFloor
	}
	f.mu.Unlock()
	if ag == nil {
		f.t.Fatalf("fake: no gateway connection to ask on")
	}
	cl, err := ag.wire.Send(mRequestPermission, map[string]any{
		argSessionID: session,
		keyToolCall: map[string]any{keyToolCallID: "tc-" + title,
			keyName: strings.SplitN(title, ": ", 2)[0], argTitle: title, keyRawInput: input},
		keyOptions: []any{
			map[string]any{keyOptionID: optionAllowOnce, keyName: "Allow once", keyKind: optionAllowOnce},
			map[string]any{keyOptionID: optionReject, keyName: "Reject once", keyKind: optionReject},
		},
	})
	if err != nil {
		f.t.Fatalf("fake: send ask: %v", err)
	}
	a := &fakeAsk{id: cl.ID, wire: canonID(cl.ID), ag: ag}
	f.mu.Lock()
	f.asks[a.wire] = a
	f.mu.Unlock()
	go func() {
		m := <-cl.Resp
		var got wirePermission
		if json.Unmarshal(m.Result, &got) == nil {
			f.mu.Lock()
			f.answers[a.wire] = got
			f.mu.Unlock()
		}
	}()
	return a.wire
}

// withdraw is the 55 second fallthrough of WIRE 10.2: the plugin ends its own ask
// with the cancelled outcome, in a frame that answers the request it sent.
func (f *fakeTUI) withdraw(wireID string) {
	f.mu.Lock()
	a, ok := f.asks[wireID]
	f.mu.Unlock()
	if !ok {
		f.t.Fatalf("fake: no ask with id %s to withdraw", wireID)
	}
	line := `{"jsonrpc":"2.0","id":` + string(a.id) +
		`,"result":{"outcome":{"outcome":"cancelled"}}}` + "\n"
	if err := a.ag.p.raw(line); err != nil {
		f.t.Fatalf("fake: withdraw: %v", err)
	}
}

// waitForAnswer waits until the gateway has answered one ask and hands back what
// it wrote.
func (f *fakeTUI) waitForAnswer(t *testing.T, wireID string) wirePermission {
	t.Helper()
	var got wirePermission
	waitFor(t, "the permission answer to reach the plugin", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, ok := f.answers[wireID]
		got = g
		return ok
	})
	return got
}

func (f *fakeTUI) firstAgentLocked() *agent {
	if len(f.agents) == 0 {
		return nil
	}
	return f.agents[0]
}

// hangUp drops an agent the gateway closed, so connCount reads the sockets that
// are still up.
func (f *fakeTUI) hangUp(dead *agent) {
	f.mu.Lock()
	kept := make([]*agent, 0, len(f.agents))
	for _, a := range f.agents {
		if a != dead {
			kept = append(kept, a)
		}
	}
	f.agents = kept
	f.mu.Unlock()
}

// isAnswered reports whether the gateway has answered one ask.
func (f *fakeTUI) isAnswered(wireID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.answers[wireID]
	return ok
}

func (f *fakeTUI) record(r request) {
	f.mu.Lock()
	f.frames = append(f.frames, r)
	f.mu.Unlock()
}

func (f *fakeTUI) tabsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A plugin with no tabs answers an empty list, not a JSON null.
	out := make([]map[string]any, 0, len(f.tabs))
	return append(out, f.tabs...)
}

func (f *fakeTUI) has(id string) bool {
	for _, row := range f.tabsSnapshot() {
		if strParam(row, argSessionID) == id {
			return true
		}
	}
	return false
}

// requireLive is the -32002 of WIRE 13: a sessionId this TUI does not hold.
func (f *fakeTUI) requireLive(params map[string]any) error {
	id := strParam(params, argSessionID)
	if f.has(id) {
		return nil
	}
	return &acpwire.Error{Code: codeNotLive, Message: notLive + ": " + id}
}

// followTaskName is the task this fake last moved onto the screen.
func (f *fakeTUI) followedTask() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focus
}

// messagesFor is the transcript of WIRE 6: native maki records, with no field
// renamed, dropped, or added.
func (f *fakeTUI) messagesFor(session string) []map[string]any {
	if !f.has(session) {
		return nil
	}
	return []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello " + session}}},
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "tu1", "name": "bash", "input": map[string]any{"command": "ls"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "tu1", "content": "file.c", "is_error": false},
		}},
	}
}

// fakeTaskRows is maki.task.list() as the host writes it: the main chat first,
// with no status, then one working subagent (WIRE 9.3).
func fakeTaskRows() []map[string]any {
	return []map[string]any{
		{keyRowID: "main", keyTaskSession: "s1", keyName: "main", keyFocused: true},
		{keyRowID: "tu1", keyTaskSession: "s1", keyName: "explore the repo",
			keyStatus: "working", keyTool: "task", keyFocused: false,
			"started_at": taskStartedAt, "cost": taskCost},
	}
}

func (f *fakeTUI) tasksSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.tasks))
	return append(out, f.tasks...)
}

// setTasks replaces what the tasks method answers.
func (f *fakeTUI) setTasks(rows ...map[string]any) {
	f.mu.Lock()
	f.tasks = rows
	f.mu.Unlock()
}

// setTabs replaces what session/list answers.
func (f *fakeTUI) setTabs(tabs ...map[string]any) {
	f.mu.Lock()
	f.tabs = tabs
	f.mu.Unlock()
}

// failWith makes one method answer an error, which is how the mapping of WIRE 13.1
// is exercised. The code is the one that fits the message in the table.
func (f *fakeTUI) failWith(method, msg string) {
	f.mu.Lock()
	f.fail[method] = msg
	f.mu.Unlock()
}

// failCode makes one method answer an error of one code.
func (f *fakeTUI) failCode(method string, code int, msg string) {
	f.mu.Lock()
	f.fail[method], f.codes[method] = msg, code
	f.mu.Unlock()
}

// hangOn makes one method read like a stock agent that has no answer for it: the
// request is taken and never answered, so only the caller's deadline ends the wait
// (WIRE 1.5, 7.3).
func (f *fakeTUI) hangOn(method string) {
	f.mu.Lock()
	f.hang[method] = true
	f.mu.Unlock()
}

// replyBare makes one method answer a frame with neither result nor error, which
// WIRE 1.3 forbids.
func (f *fakeTUI) replyBare(method string) {
	f.mu.Lock()
	f.bare[method] = true
	f.mu.Unlock()
}

// setProto changes what initialize reports for attach_proto, which is the whole
// version gate of WIRE 2.2. A nil value omits the key.
func (f *fakeTUI) setProto(v any) {
	f.mu.Lock()
	f.proto, f.noProto = v, v == nil
	f.mu.Unlock()
}

// answerQueueWith changes what _sh.maki/queue answers.
func (f *fakeTUI) answerQueueWith(state string) {
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
}

// refuseFollow makes this plugin answer every follow like a maki with no terminal.
func (f *fakeTUI) refuseFollow() {
	f.mu.Lock()
	f.noUI = true
	f.mu.Unlock()
}

// freeze makes the plugin stop answering the list poll, the liveness clock of
// WIRE 1.5.
func (f *fakeTUI) freeze() {
	f.mu.Lock()
	f.frozen = true
	f.mu.Unlock()
}

// methodsSeen is the list of methods the gateway called, in order.
func (f *fakeTUI) methodsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.frames))
	for _, r := range f.frames {
		out = append(out, r.method)
	}
	return out
}

func (f *fakeTUI) frameWithMethod(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.frames) - 1; i >= 0; i-- {
		if f.frames[i].method == method {
			return f.frames[i].params
		}
	}
	return nil
}

// framesOf counts the requests naming one method, so a test can say every call
// reached the plugin rather than only the last one.
func (f *fakeTUI) framesOf(method string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.frames {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// framesWithoutNumber are the recorded requests that carry no JSON number in
// `id` (WIRE 1.4). A frame that spent that field on anything else gets its answer
// dropped, so a test can name the shape the engine must never send.
func (f *fakeTUI) framesWithoutNumber() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.frames {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(r.id)), 10, 64); err != nil || n == 0 {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeTUI) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.agents)
}

// decodeParams reads one params object into a map, so a test can name a field
// without a shape of its own. A method that takes no argument is sent {} (WIRE
// 1.3), and anything else is a frame no plugin can read.
func decodeParams(raw json.RawMessage) (map[string]any, error) {
	params := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return params, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, &acpwire.Error{Code: acpwire.CodeInvalidRequest, Message: "params must be an object"}
	}
	for k, v := range fields {
		var val any
		if json.Unmarshal(v, &val) == nil {
			params[k] = val
		}
	}
	return params, nil
}

func strParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

func jsonString(s string) json.RawMessage { return json.RawMessage(strconv.Quote(s)) }

func jsonNumber(n int) json.RawMessage { return json.RawMessage(strconv.Itoa(n)) }

// jsonAny spells one _meta value the way a Lua encoder may: a number stays a
// number, a string is quoted, because WIRE 2.3 says a reader takes both.
func jsonAny(v any) json.RawMessage {
	if s, ok := v.(string); ok {
		return jsonString(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// wantEvent returns the next event of one type, skipping incidental ones.
func wantEvent(t *testing.T, ch chan model.Event, typ string) model.Event {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-deadline.C:
			t.Fatalf("no %q event received", typ)
		}
	}
}

// safeBuffer is a log sink a plugin reader goroutine can write to while the test
// reads it back.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// warnLog routes the package logger's warnings and errors into buf until the
// returned func runs. Tests that assert a refusal is visible need it: the default
// handler drops everything below Error.
func warnLog(t *testing.T, buf *safeBuffer) func() {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return func() { slog.SetDefault(prev) }
}
