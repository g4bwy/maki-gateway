package acpengine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Fake is a stand-in `maki acp` agent. It speaks the same ACP v1 subset written
// in docs/ACP_MIN.md, so the engine can be tested without a model, an API key,
// or a network.
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
type Fake struct {
	mu         sync.Mutex
	enc        *json.Encoder
	nextID     int64
	openPrompt int64
	answers    chan wireMessage
}

// Prompt markers the fake reads.
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

	fakeRejectDetail  = "a prompt is already running"
	fakeElsewhereText = "session held by another maki"
	fakeToolID        = "call-1"
	fakeExitStatus    = 3
)

// FakeArgvEnv names a file the fake writes the arguments it was spawned with to,
// one per line. A test that must see the argv the engine passed sets it: the
// child inherits the environment, and the file is written before the handshake
// answers, so a Create that returned can always read it back.
const FakeArgvEnv = "MAKI_FAKE_ARGV"

func recordArgv() {
	path := os.Getenv(FakeArgvEnv)
	if path == "" || len(os.Args) < 2 {
		return
	}
	_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
}

// NewFake writes frames to w. r is read by Serve, so it is not kept here.
func NewFake(r io.Reader, w io.Writer) *Fake {
	_ = r
	return &Fake{
		enc:     json.NewEncoder(w),
		nextID:  1000, // server.rs:47 FIRST_OUTGOING_REQUEST_ID
		answers: make(chan wireMessage, 8),
	}
}

// ServeFakeStdio runs the fake on the process stdio. A test binary execed with the
// `acp` argument calls this instead of running tests, which is how the engine gets
// a real child process in its tests.
func ServeFakeStdio() error {
	recordArgv()
	f := NewFake(nil, os.Stdout)
	return f.Serve(os.Stdin)
}

// IsFakeInvocation reports whether these argv mean "act as the fake agent".
func IsFakeInvocation(argv []string) bool {
	return len(argv) > 1 && argv[1] == argACP
}

// Serve reads frames until its input ends. A child ends when stdin closes, so
// io.EOF is a clean return.
func (f *Fake) Serve(r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<16))
	for {
		var m wireMessage
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		f.handle(&m)
	}
}

func (f *Fake) handle(m *wireMessage) {
	switch m.Method {
	case methodInitialize:
		f.reply(m.ID, map[string]any{
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
		f.reply(m.ID, map[string]any{
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
		f.handlePrompt(m)
	case methodSessionSetMode:
		var req struct {
			SessionID string `json:"sessionId"`
			ModeID    string `json:"modeId"`
		}
		_ = json.Unmarshal(m.Params, &req)
		f.update(req.SessionID, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": req.ModeID})
		f.reply(m.ID, map[string]any{})
	case methodSessionSetConfig:
		var req struct {
			SessionID string `json:"sessionId"`
			Value     string `json:"value"`
		}
		_ = json.Unmarshal(m.Params, &req)
		options := []configOption{{ID: optionModel, CurrentValue: req.Value}}
		f.update(req.SessionID, map[string]any{"sessionUpdate": updateConfigOption, "configOptions": options})
		f.reply(m.ID, map[string]any{"configOptions": options})
	case methodSessionCancel:
		f.cancelPrompt()
	case "":
		f.collect(*m)
	default:
		f.replyError(m.ID, codeMethodNotFound, "method not found")
	}
}

// collect takes the answer to an ask the fake parked.
func (f *Fake) collect(m wireMessage) {
	select {
	case f.answers <- m:
	default:
	}
}

// sessionID ties the fake session to its process, so two fake children in one
// test are still two distinct sessions.
func (f *Fake) sessionID() string {
	return FakeSessionStem + strconv.Itoa(os.Getpid())
}

// handlePrompt mirrors the server: one open id per session, taken under the lock,
// and the answer only at end of turn.
func (f *Fake) handlePrompt(m *wireMessage) {
	var req struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(m.Params, &req); err != nil {
		f.replyError(m.ID, codeInvalidRequest, "invalid params")
		return
	}
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	f.mu.Lock()
	busy := f.openPrompt != 0
	if !busy {
		f.openPrompt = id
	}
	f.mu.Unlock()
	if busy {
		f.replyError(m.ID, codeInternal, fakeRejectDetail)
		return
	}

	text := ""
	for _, b := range req.Prompt {
		if b.Type == blockText {
			text += b.Text
		}
	}
	switch {
	case strings.Contains(text, FakeExitAsk):
		// Checked before @exit: its marker string contains it.
		go func() {
			f.request(f.nextRequestID(), methodRequestPermission, f.permissionBody(req.SessionID, true))
			f.wireFlush()
			os.Exit(fakeExitStatus)
		}()
	case strings.Contains(text, FakeExit):
		f.update(req.SessionID, chunk(updateMessageChunk, "leaving"))
		os.Exit(fakeExitStatus)
	case strings.Contains(text, FakeReject):
		f.release(id)
		f.replyError(m.ID, codeInternal, fakeRejectDetail)
	case strings.Contains(text, FakeElsewhere):
		f.release(id)
		f.replyError(m.ID, codeAuthOrBusy, fakeElsewhereText)
	case strings.Contains(text, FakeHold):
		go func() {
			f.update(req.SessionID, chunk(updateMessageChunk, "held"))
		}()
	case strings.Contains(text, FakePermission):
		go f.turnWithPermission(req.SessionID, id,
			!strings.Contains(text, FakeDontAsk), !strings.Contains(text, FakeNoAlways))
	case strings.Contains(text, FakeAsk):
		go f.turnWithAsk(req.SessionID, id)
	case strings.Contains(text, FakeGiant):
		go func() {
			f.update(req.SessionID, chunk(updateMessageChunk, strings.Repeat("x", 200_000)))
			f.endTurn(id, "end_turn")
		}()
	default:
		go f.turnPlain(req.SessionID, id)
	}
}

func (f *Fake) turnPlain(session string, prompt int64) {
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
func (f *Fake) turnWithPermission(session string, prompt int64, ask, always bool) {
	if !ask {
		f.update(session, chunk(updateMessageChunk, "never asked"))
		f.endTurn(prompt, "end_turn")
		return
	}
	f.request(f.nextRequestID(), methodRequestPermission, f.permissionBody(session, always))
	answer := <-f.answers
	chosen := outcomeCancelled
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(answer.Result, &res) == nil && res.Outcome.Outcome == outcomeSelected {
		chosen = res.Outcome.OptionID
	}
	f.update(session, chunk(updateMessageChunk, "option:"+chosen))
	f.endTurn(prompt, "end_turn")
}

func (f *Fake) turnWithAsk(session string, prompt int64) {
	f.request(f.nextRequestID(), methodElicitationCreate, map[string]any{
		"mode":      "form",
		"sessionId": session,
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"q1": map[string]any{"type": "string", "title": "Pick one"}},
		},
		"message": "Pick one",
	})
	answer := <-f.answers
	action := "none"
	var res struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(answer.Result, &res) == nil && res.Action != "" {
		action = res.Action
	}
	f.update(session, chunk(updateMessageChunk, "action:"+action))
	f.endTurn(prompt, "end_turn")
}

// endTurn answers the prompt the way the real server does: at end of turn.
func (f *Fake) endTurn(prompt int64, reason string) {
	f.release(prompt)
	f.reply(json.RawMessage(fmt.Sprintf("%d", prompt)), map[string]any{"stopReason": reason})
}

// cancelPrompt answers an open prompt with the cancelled stop reason, like a turn
// that saw session/cancel.
func (f *Fake) cancelPrompt() {
	f.mu.Lock()
	prompt := f.openPrompt
	f.mu.Unlock()
	if prompt != 0 {
		f.endTurn(prompt, outcomeCancelled)
	}
}

func (f *Fake) release(prompt int64) {
	f.mu.Lock()
	if f.openPrompt == prompt {
		f.openPrompt = 0
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

// wireFlush is a no-op barrier: every frame is written under the same lock, so
// taking it means earlier frames reached the pipe already.
func (f *Fake) wireFlush() {
	f.mu.Lock()
	defer f.mu.Unlock()
}

func chunk(update, text string) map[string]any {
	return map[string]any{
		"sessionUpdate": update,
		"content":       map[string]any{"type": blockText, "text": text},
	}
}

func (f *Fake) nextRequestID() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	return json.RawMessage(strconv.FormatInt(id, 10))
}

func (f *Fake) reply(rawID json.RawMessage, result any) {
	f.wire(map[string]any{"jsonrpc": jsonrpcVersion, "id": rawID, "result": result})
}

func (f *Fake) replyError(rawID json.RawMessage, code int, message string) {
	f.wire(map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      rawID,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (f *Fake) request(rawID json.RawMessage, method string, params any) {
	f.wire(map[string]any{"jsonrpc": jsonrpcVersion, "id": rawID, "method": method, "params": params})
}

func (f *Fake) update(session string, update map[string]any) {
	f.wire(wireRequest{
		JSONRPC: jsonrpcVersion,
		Method:  methodSessionUpdate,
		Params:  map[string]any{"sessionId": session, "update": update},
	})
}

func (f *Fake) wire(frame any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.enc.Encode(frame)
}
