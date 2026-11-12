package acpengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"maki-gateway/internal/acpwire"
)

// Fake is a stand-in `maki acp` agent. It speaks the same ACP v1 subset written
// in docs/ACP_MIN.md, so the engine can be tested without a model, an API key,
// or a network. It is the codec's second user: the engine drives it as a client,
// and this side answers as the agent.
//
// A prompt text picks what the turn does:
//
//	(default)   stream thought, text, a tool call and its result, usage, end_turn
//	@permission park a session/request_permission, then stream the chosen option
//	@permission @noalways
//	            the same ask, with no allow_always option to pick
//	@ask        park an elicitation/create, then stream the action that answered it
//	@reject     answer with -32603 "a prompt is already running", like a busy agent
//	@hold       stream one chunk and never answer, until session/cancel
//	@exit       die mid-turn without answering, exit status 3
//	@exitask    park a session/request_permission, then die without answering it
//	@giant      stream one chunk well past 64 KiB, then end the turn
//
// session/load has markers of its own, because a load carries no text: a file
// named @busyload in the session cwd makes the next load answer -32000 and is
// removed on the way, standing for the claim a dying child has not released yet.
// With the content "always" it keeps answering that way.
type Fake struct {
	conn *acpwire.Conn

	mu         sync.Mutex
	openPrompt *acpwire.Request
}

// Markers the fake reads. All but the two load markers arrive in prompt text; a
// load carries no text, so those are files inside the session cwd.
const (
	FakePermission  = "@permission"
	FakeAsk         = "@ask"
	FakeReject      = "@reject"
	FakeHold        = "@hold"
	FakeExit        = "@exit"
	FakeExitAsk     = "@exitask"
	FakeGiant       = "@giant"
	FakeElsewhere   = "@elsewhere"
	FakeDontAsk     = "@dontask"
	FakeNoAlways    = "@noalways"
	FakeSessionStem = "fake-"

	// FakeBusyLoad names the file that makes session/load answer -32000 once,
	// which is what a session whose previous child has not released its claim
	// yet looks like to a respawning client. FakeBusyLoadAlways is the file
	// content that keeps refusing.
	FakeBusyLoad       = "@busyload"
	FakeBusyLoadAlways = "always"

	fakeRejectDetail  = "a prompt is already running"
	fakeElsewhereText = "session held by another maki"
	fakeReplayText    = "replayed turn"
	fakeToolID        = "call-1"
	fakeExitStatus    = 3
)

// FakeArgvEnv names a file the fake appends the arguments it was spawned with to,
// one argument per line and a blank line after each process start. A test that must
// see the argv the engine passed sets it: the child inherits the environment, and
// the file is written before the handshake answers, so a Create that returned can
// always read it back. A respawn appends, so the file holds one block per child.
const FakeArgvEnv = "MAKI_FAKE_ARGV"

func recordArgv() {
	path := os.Getenv(FakeArgvEnv)
	if path == "" || len(os.Args) < 2 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	// One Write per process start, so two children cannot interleave their lines.
	_, _ = f.WriteString(strings.Join(os.Args[1:], "\n") + "\n\n")
}

// NewFake builds an agent that reads r and writes w. Both live for the life of the
// process, which is what a child's stdio is.
func NewFake(r io.Reader, w io.Writer) *Fake {
	f := &Fake{}
	f.conn = acpwire.New(transport{r: r, w: w}, acpwire.Options{
		OnRequest:      f.onRequest,
		OnNotification: f.onMethod,
	})
	return f
}

// ServeFakeStdio runs the fake on the process stdio. A test binary execed with the
// `acp` argument calls this instead of running tests, which is how the engine gets
// a real child process in its tests.
func ServeFakeStdio() error {
	recordArgv()
	return NewFake(os.Stdin, os.Stdout).Serve()
}

// IsFakeInvocation reports whether these argv mean "act as the fake agent".
func IsFakeInvocation(argv []string) bool {
	return len(argv) > 1 && argv[1] == argACP
}

// Serve answers frames until its input ends. A child ends when stdin closes, so
// io.EOF is a clean return.
func (f *Fake) Serve() error {
	if err := f.conn.Serve(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// onRequest answers one request the client sent. A prompt is not answered here: it
// is held until the turn ends, which is the one behaviour of the real server every
// test depends on.
func (f *Fake) onRequest(_ context.Context, r *acpwire.Request) error {
	switch r.Method {
	case methodInitialize:
		return r.Answer(map[string]any{
			"protocolVersion": acpProtocol,
			"agentCapabilities": map[string]any{
				"loadSession":         true,
				"promptCapabilities":  map[string]bool{"image": true, "embeddedContext": true},
				"mcpCapabilities":     map[string]bool{"http": true},
				"sessionCapabilities": map[string]any{"list": map[string]any{}},
			},
			"authMethods": []any{},
			"agentInfo":   map[string]string{"name": "maki", "version": "fake"},
		})
	case methodSessionNew:
		return r.Answer(map[string]any{
			"sessionId": f.sessionID(),
			"modes": map[string]any{
				"currentModeId": modeBuild,
				"availableModes": []map[string]string{
					{"id": modeBuild, "name": "Build"},
					{"id": modePlan, "name": "Plan"},
				},
			},
			"configOptions": []configOption{{ID: optionModel, CurrentValue: "fake/model"}},
		})
	case methodSessionPrompt:
		return f.handlePrompt(r)
	case methodSessionLoad:
		return f.handleLoad(r)
	case methodSessionSetMode:
		var req struct {
			SessionID string `json:"sessionId"`
			ModeID    string `json:"modeId"`
		}
		_ = json.Unmarshal(r.Params, &req)
		f.update(req.SessionID, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": req.ModeID})
		return r.Answer(map[string]any{})
	case methodSessionSetConfig:
		var req struct {
			SessionID string `json:"sessionId"`
			Value     string `json:"value"`
		}
		_ = json.Unmarshal(r.Params, &req)
		options := []configOption{{ID: optionModel, CurrentValue: req.Value}}
		f.update(req.SessionID, map[string]any{"sessionUpdate": updateConfigOption, "configOptions": options})
		return r.Answer(map[string]any{"configOptions": options})
	}
	return r.Fail(acpwire.CodeMethodNotFound, "method not found")
}

// onMethod takes the notifications the client sends. session/cancel is the only
// one that changes what a turn does.
func (f *Fake) onMethod(method string, _ json.RawMessage) {
	if method == methodSessionCancel {
		f.cancelPrompt()
	}
}

// sessionID ties the fake session to its process, so two fake children in one
// test are still two distinct sessions.
func (f *Fake) sessionID() string {
	return FakeSessionStem + strconv.Itoa(os.Getpid())
}

// handleLoad answers session/load the way the server does: the whole stored
// history arrives as updates before the response, and a session whose previous
// child has not released its claim is refused with -32000.
//
// The refusal is asked for by FakeBusyLoad, a file in the session cwd. It is
// removed as it is read, so the next load of that session succeeds, unless its
// content is FakeBusyLoadAlways, which keeps refusing.
func (f *Fake) handleLoad(r *acpwire.Request) error {
	var req struct {
		SessionID  string `json:"sessionId"`
		Cwd        string `json:"cwd"`
		MCPServers *[]any `json:"mcpServers"`
	}
	// sessionId, cwd and mcpServers are all required by the schema
	// (agent.rs:1401), so a request missing one is a client bug, not a load miss.
	if err := json.Unmarshal(r.Params, &req); err != nil || req.SessionID == "" || req.MCPServers == nil {
		return r.Fail(codeInvalidRequest, "invalid session/load params")
	}
	if held(req.Cwd) {
		return r.Fail(codeAuthOrBusy, fakeElsewhereText)
	}
	// The replay, in the session the client named rather than the one this process
	// would give itself. A reducer that is not muted for the call appends these to
	// a transcript that already holds them.
	f.update(req.SessionID, chunk(updateMessageChunk, fakeReplayText))
	f.update(req.SessionID, chunk(updateThoughtChunk, fakeReplayText))
	return r.Answer(map[string]any{
		"modes": map[string]any{
			"currentModeId": modeBuild,
			"availableModes": []map[string]string{
				{"id": modeBuild, "name": "Build"},
				{"id": modePlan, "name": "Plan"},
			},
		},
		"configOptions": []configOption{{ID: optionModel, CurrentValue: "fake/model"}},
	})
}

// held reports whether this cwd asked to refuse one load, and takes the request
// away so the next one is answered.
func held(cwd string) bool {
	path := filepath.Join(cwd, FakeBusyLoad)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	keep, err := os.ReadFile(path)
	if err == nil && string(keep) == FakeBusyLoadAlways {
		return true
	}
	_ = os.Remove(path)
	return true
}

// handlePrompt mirrors the server: one open request per session, taken under the
// lock, and the answer only at end of turn.
func (f *Fake) handlePrompt(r *acpwire.Request) error {
	var req struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(r.Params, &req); err != nil {
		return r.Fail(codeInvalidRequest, "invalid params")
	}
	f.mu.Lock()
	busy := f.openPrompt != nil
	if !busy {
		f.openPrompt = r
	}
	f.mu.Unlock()
	if busy {
		return r.Fail(codeInternal, fakeRejectDetail)
	}

	text := ""
	for _, b := range req.Prompt {
		if b.Type == blockText {
			text += b.Text
		}
	}
	switch {
	case strings.Contains(text, FakeExitAsk):
		// Checked before @exit: its marker string contains it. The ask goes out
		// before the death, because acpwire writes a frame before it returns.
		if _, err := f.conn.Send(methodRequestPermission, f.permissionBody(req.SessionID, true)); err != nil {
			return nil
		}
		os.Exit(fakeExitStatus)
	case strings.Contains(text, FakeExit):
		f.update(req.SessionID, chunk(updateMessageChunk, "leaving"))
		os.Exit(fakeExitStatus)
	case strings.Contains(text, FakeReject):
		f.release(r)
		return r.Fail(codeInternal, fakeRejectDetail)
	case strings.Contains(text, FakeElsewhere):
		f.release(r)
		return r.Fail(codeAuthOrBusy, fakeElsewhereText)
	case strings.Contains(text, FakeHold):
		go f.update(req.SessionID, chunk(updateMessageChunk, "held"))
	case strings.Contains(text, FakePermission):
		go f.turnWithPermission(req.SessionID, r,
			!strings.Contains(text, FakeDontAsk), !strings.Contains(text, FakeNoAlways))
	case strings.Contains(text, FakeAsk):
		go f.turnWithAsk(req.SessionID, r)
	case strings.Contains(text, FakeGiant):
		go func() {
			f.update(req.SessionID, chunk(updateMessageChunk, strings.Repeat("x", 200_000)))
			f.endTurn(r, "end_turn")
		}()
	default:
		go f.turnPlain(req.SessionID, r)
	}
	return nil
}

func (f *Fake) turnPlain(session string, prompt *acpwire.Request) {
	f.update(session, chunk(updateThoughtChunk, "pondering"))
	f.update(session, chunk(updateMessageChunk, "hello "))
	f.update(session, chunk(updateMessageChunk, "world"))
	f.update(session, map[string]any{
		"sessionUpdate": updateToolCall,
		"toolCallId":    fakeToolID,
		"title":         "read file",
		"kind":          "read",
		"status":        statusPending,
		"rawInput":      map[string]string{"path": "a.txt"},
	})
	f.update(session, map[string]any{
		"sessionUpdate": updateToolUpdate,
		"toolCallId":    fakeToolID,
		"status":        statusInProgress,
		"title":         "reading a.txt",
	})
	f.update(session, map[string]any{
		"sessionUpdate": updateToolUpdate,
		"toolCallId":    fakeToolID,
		"status":        statusCompleted,
		"rawOutput":     "one line",
	})
	f.update(session, chunk(updateMessageChunk, " done"))
	f.update(session, map[string]any{
		"sessionUpdate": "usage_update",
		"used":          120,
		"size":          4096,
		"cost":          map[string]any{"amount": 0.01, "currency": "USD"},
	})
	f.endTurn(prompt, "end_turn")
}

// turnWithPermission asks, waits for the answer, then streams what was picked so
// a test can read the option id back out of the transcript. An ask the client
// never answers leaves the turn open, which is what the die path is for.
func (f *Fake) turnWithPermission(session string, prompt *acpwire.Request, ask, always bool) {
	if !ask {
		f.update(session, chunk(updateMessageChunk, "never asked"))
		f.endTurn(prompt, "end_turn")
		return
	}
	chosen := f.askPermission(session, always)
	f.update(session, chunk(updateMessageChunk, "option:"+chosen))
	f.endTurn(prompt, "end_turn")
}

// askPermission parks a session/request_permission and reports the option id the
// client picked. A cancelled outcome comes back as its own name, which is what the
// transcript of a withdrawn dialog shows.
func (f *Fake) askPermission(session string, always bool) string {
	call, err := f.conn.Send(methodRequestPermission, f.permissionBody(session, always))
	if err != nil {
		return outcomeCancelled
	}
	answer := <-call.Resp
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(answer.Result, &res) == nil && res.Outcome.Outcome == outcomeSelected {
		return res.Outcome.OptionID
	}
	return outcomeCancelled
}

func (f *Fake) turnWithAsk(session string, prompt *acpwire.Request) {
	action := f.askForm(session)
	f.update(session, chunk(updateMessageChunk, "action:"+action))
	f.endTurn(prompt, "end_turn")
}

// askForm parks an elicitation/create and reports the action that answered it.
func (f *Fake) askForm(session string) string {
	call, err := f.conn.Send(methodElicitationCreate, map[string]any{
		"mode":      "form",
		"sessionId": session,
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"q1": map[string]any{"type": "string", "title": "Pick one"}},
		},
		"message": "Pick one",
	})
	if err != nil {
		return "none"
	}
	answer := <-call.Resp
	var res struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(answer.Result, &res) == nil && res.Action != "" {
		return res.Action
	}
	return "none"
}

// endTurn answers the prompt the way the real server does: at end of turn.
func (f *Fake) endTurn(prompt *acpwire.Request, reason string) {
	f.release(prompt)
	_ = prompt.Answer(map[string]any{"stopReason": reason})
}

// cancelPrompt answers an open prompt with the cancelled stop reason, like a turn
// that saw session/cancel.
func (f *Fake) cancelPrompt() {
	f.mu.Lock()
	prompt := f.openPrompt
	f.mu.Unlock()
	if prompt != nil {
		f.endTurn(prompt, outcomeCancelled)
	}
}

func (f *Fake) release(prompt *acpwire.Request) {
	f.mu.Lock()
	if f.openPrompt == prompt {
		f.openPrompt = nil
	}
	f.mu.Unlock()
}

// permissionBody is the ask the real server writes (permissions.rs:23): the four
// fixed option ids, with the tool call riding along as a ToolCallUpdate. always
// off drops the allow_always row, which is how a test asks what an agent that
// offers three options gets.
func (f *Fake) permissionBody(session string, always bool) map[string]any {
	options := []permissionOption{
		{OptionID: optionAllowOnce, Name: "Allow once", Kind: kindAllowOnce},
		{OptionID: optionAllowAlways, Name: "Allow always", Kind: kindAllowAlways},
		{OptionID: optionRejectOnce, Name: "Reject once", Kind: kindRejectOnce},
		{OptionID: "reject_always", Name: "Reject always", Kind: kindRejectAlways},
	}
	if !always {
		options = append(options[:1], options[2:]...)
	}
	return map[string]any{
		"sessionId": session,
		"toolCall": map[string]any{
			"toolCallId": "call-ask",
			"title":      "bash: ls",
			"kind":       "execute",
			"status":     statusPending,
			"rawInput":   map[string]string{"command": "ls"},
			"locations":  []map[string]any{{"path": "/tmp/a.txt"}},
		},
		"options": options,
	}
}

func chunk(update, text string) map[string]any {
	return map[string]any{
		"sessionUpdate": update,
		"content":       map[string]any{"type": blockText, "text": text},
	}
}

// update pushes one session/update notification.
func (f *Fake) update(session string, update map[string]any) {
	_ = f.conn.Notify(methodSessionUpdate, map[string]any{"sessionId": session, "update": update})
}
