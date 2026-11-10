package acpengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

// TestMain doubles as the fake agent: the engine spawns this same test binary as
// `maki acp`, and the child answers the ACP subset instead of running tests.
func TestMain(m *testing.M) {
	if IsFakeInvocation(os.Args) {
		if err := ServeFakeStdio(); err != nil {
			fmt.Fprintln(os.Stderr, "fake acp:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// The engine logs child deaths at Info; keep them out of a green run.
	if os.Getenv("MAKI_TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

// fakePath is this compiled test binary, which answers `acp` as the fake agent.
var fakePath = func() string {
	path, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return path
}()

// collector reads the hub stream. Every event stays in the list, so a wait that
// runs after an action still finds what it missed while the test was busy.
type collector struct {
	events []model.Event
	sub    chan model.Event
}

func subscribe(hub *model.Hub) *collector {
	return &collector{sub: hub.Subscribe()}
}

func (c *collector) next(t *testing.T, timeout time.Duration, want func(model.Event) bool) model.Event {
	t.Helper()
	for {
		for _, e := range c.events {
			if want(e) {
				return e
			}
		}
		select {
		case e := <-c.sub:
			c.events = append(c.events, e)
		case <-time.After(timeout):
			t.Fatalf("no event within %s, saw %v", timeout, typesOf(c.events))
			return model.Event{}
		}
	}
}

// waitStatus reads live session state, not the event stream. A turn that ends
// frees the session asynchronously, so a test that wants to prompt again waits
// here rather than for a status event an earlier transition already published.
func waitStatus(t *testing.T, eng *Engine, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := eng.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, s := range list {
			if s.ID == id && s.Status == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q never reached for %s, saw %+v", want, id, list)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// typesOf names what a failed wait saw, so the failure says more than a count.
func typesOf(evts []model.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.Type)
	}
	return out
}

func ofType(kind string) func(model.Event) bool {
	return func(e model.Event) bool { return e.Type == kind }
}

func ofUpdate(update string) func(model.Event) bool {
	return func(e model.Event) bool {
		if e.Type != "update" {
			return false
		}
		var head updateHead
		return json.Unmarshal(e.ACP, &head) == nil && head.SessionUpdate == update
	}
}

func newTestEngine(t *testing.T) (*Engine, *collector) {
	t.Helper()
	hub := model.NewHub()
	eng := New(fakePath, hub)
	c := subscribe(hub)
	t.Cleanup(func() { hub.Unsubscribe(c.sub) })
	return eng, c
}

func create(t *testing.T, eng *Engine) string {
	t.Helper()
	id, err := eng.Create(t.TempDir(), "fake/model", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })
	return id
}

// blocks decodes one message's content array into plain maps.
func blocks(t *testing.T, msg model.Message) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(msg.Content, &out); err != nil {
		t.Fatalf("content %s: %v", msg.Content, err)
	}
	return out
}

func one(t *testing.T, eng *Engine, id string) []model.Message {
	t.Helper()
	msgs, err := eng.Messages(id, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	return msgs
}

// TestCreateHandshakePromptReduce covers the whole happy path: the two-step
// handshake, one turn of streamed updates, the passthrough events, and the
// transcript the reducer builds from them.
func TestCreateHandshakePromptReduce(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if got := eng.Kind(); got != model.KindWeb {
		t.Fatalf("Kind = %q, want %q", got, model.KindWeb)
	}
	list, err := eng.List()
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].Model != "fake/model" {
		t.Errorf("List model = %q, want the reported one", list[0].Model)
	}

	state, err := eng.Prompt(id, "hi")
	if err != nil || state != "started" {
		t.Fatalf("Prompt = %q, %v", state, err)
	}

	update := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if update.Engine != model.EngineACP || update.Session != id {
		t.Fatalf("update event = %+v", update)
	}
	c.next(t, 10*time.Second, ofUpdate("usage_update"))

	msgs := one(t, eng, id)
	if len(msgs) != 2 {
		t.Fatalf("transcript has %d messages, want user and assistant: %v", len(msgs), msgs)
	}
	user := blocks(t, msgs[0])
	if msgs[0].Role != "user" || len(user) != 1 || user[0]["text"] != "hi" {
		t.Fatalf("user message = %+v", user)
	}
	got := blocks(t, msgs[1])
	want := []string{blockThinking, blockText, blockToolUse, blockToolDone, blockText}
	if len(got) != len(want) {
		t.Fatalf("assistant blocks = %d %+v, want %d", len(got), got, len(want))
	}
	for i, kind := range want {
		if got[i]["type"] != kind {
			t.Errorf("block %d = %v, want type %s", i, got[i], kind)
		}
	}
	if got[1]["text"] != "hello world" {
		t.Errorf("chunks did not join: %v", got[1]["text"])
	}
	tool := got[2]
	if tool["id"] != fakeToolID || tool[blockToolName] != "reading a.txt" {
		t.Errorf("tool_use = %v", tool)
	}
	input, ok := tool["input"].(map[string]any)
	if !ok || input["path"] != "a.txt" {
		t.Errorf("tool_use input = %v", tool["input"])
	}
	result := got[3]
	if result["tool_use_id"] != fakeToolID || result["content"] != "one line" || result["is_error"] != false {
		t.Errorf("tool_result = %v", result)
	}
	if got[4]["text"] != " done" {
		t.Errorf("text after the tool card did not split: %v", got[4])
	}

	// The turn answered the open request, so the session is free again.
	waitStatus(t, eng, id, model.StatusIdle)
	if state, err := eng.Prompt(id, "again"); err != nil || state != "started" {
		t.Fatalf("second Prompt = %q, %v", state, err)
	}
}

// TestPromptStaysOpenUntilEndOfTurn pins the timing: a chunk arrives, and the
// pending request is still open until the agent ends its turn.
func TestPromptStaysOpenUntilEndOfTurn(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, "hold "+FakeHold); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))

	if _, err := eng.Prompt(id, "second"); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("second Prompt err = %v, want ErrBusy", err)
	}
	if err := eng.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	if _, err := eng.Prompt(id, "free again"); err != nil {
		t.Fatalf("Prompt after cancel: %v", err)
	}
}

// TestPermissionRoundtrip checks the parked ask, the published event, and the
// exact response frame written back with the agent's own id.
func TestPermissionRoundtrip(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if asked.Engine != model.EngineACP || asked.Session != id {
		t.Fatalf("permission event = %+v", asked)
	}
	if asked.Tool != "bash: ls" || asked.RequestID == 0 {
		t.Fatalf("permission event fields = %+v", asked)
	}
	var input map[string]any
	if err := json.Unmarshal(asked.Input, &input); err != nil || input["command"] != "ls" {
		t.Fatalf("permission input = %s", asked.Input)
	}
	if list, _ := eng.List(); list[0].Status != model.StatusNeedsInput {
		t.Fatalf("status while parked = %v, want needs_input", list[0].Status)
	}

	if err := eng.RespondPermission(asked.RequestID, decisionAllow, "please do"); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	c.next(t, 10*time.Second, func(e model.Event) bool {
		return e.Type == "status" && e.Status == model.StatusWorking
	})
	last := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	var update struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(last.ACP, &update); err != nil {
		t.Fatalf("answer update = %s", last.ACP)
	}
	// The fake streams back the option id it received, so this is the wire check.
	if update.Content.Text != "option:"+optionAllowOnce {
		t.Fatalf("agent saw %q, want option:%s (guidance must be dropped)", update.Content.Text, optionAllowOnce)
	}

	if err := eng.RespondPermission(asked.RequestID, decisionAllow, ""); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("answering twice err = %v, want ErrNotFound", err)
	}
}

func TestPermissionDenyPicksRejectOnce(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if err := eng.RespondPermission(asked.RequestID, decisionDeny, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	last := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if want := "option:" + optionRejectOnce; !jsonContains(t, last.ACP, want) {
		t.Fatalf("agent saw %s, want %q", last.ACP, want)
	}
}

func TestPermissionUnknownDecisionKeepsAsk(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if err := eng.RespondPermission(asked.RequestID, "maybe", ""); err == nil {
		t.Fatal("an unknown decision must not answer the ask")
	}
	// The ask is still parked, so a real answer still goes through.
	if err := eng.RespondPermission(asked.RequestID, decisionAllow, ""); err != nil {
		t.Fatalf("retry after a bad decision: %v", err)
	}
}

// TestElicitationIsDismissed covers the MVP path: no web form exists, so the ask
// is answered as a cancellation and a notice is published.
func TestElicitationIsDismissed(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeAsk); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	noticed := c.next(t, 10*time.Second, ofType("notice"))
	if noticed.Text != noticeQuestion || noticed.Session != id {
		t.Fatalf("notice event = %+v", noticed)
	}
	answered := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if !jsonContains(t, answered.ACP, "action:"+actionCancel) {
		t.Fatalf("agent saw %s, want the cancel action", answered.ACP)
	}
}

// TestBusyMapping checks both sources of a busy turn: the engine's own bookkeeping
// and the -32603 the agent answers with.
func TestBusyMapping(t *testing.T) {
	eng, _ := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeReject); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("server-side busy err = %v, want ErrBusy", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	if _, err := eng.Prompt(id, "still usable"); err != nil {
		t.Fatalf("Prompt after a rejected one: %v", err)
	}
}

func TestOpenElsewhereMapping(t *testing.T) {
	eng, _ := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeElsewhere); !errors.Is(err, model.ErrOpenElsewhere) {
		t.Fatalf("err = %v, want ErrOpenElsewhere", err)
	}
}

// TestExitCleanup checks the two things a death must do: fail the open turn and
// withdraw the ask it parked, then report the exit code.
func TestExitCleanup(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeExitAsk); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	cancelled := c.next(t, 10*time.Second, func(e model.Event) bool {
		return e.Type == "permission_cancel" && e.RequestID == asked.RequestID
	})
	if cancelled.Session != id {
		t.Fatalf("permission_cancel = %+v", cancelled)
	}
	exited := c.next(t, 10*time.Second, ofType("exit"))
	if exited.Session != id || exited.Code == nil || *exited.Code != fakeExitStatus {
		t.Fatalf("exit event = %+v", exited)
	}
	if list, _ := eng.List(); len(list) != 0 {
		t.Fatalf("List after exit = %+v, want the session dropped", list)
	}
	if err := eng.Cancel(id); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Cancel on a dead session err = %v, want ErrNotFound", err)
	}
	if _, err := eng.Prompt(id, "x"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Prompt on a dead session err = %v, want ErrNotFound", err)
	}
	// The transcript cache survives the death, so a refresh still renders it.
	if len(one(t, eng, id)) == 0 {
		t.Fatal("transcript lost on exit")
	}
}

func TestKillPublishesExit(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if err := eng.Kill(id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	exited := c.next(t, 10*time.Second, ofType("exit"))
	if exited.Session != id {
		t.Fatalf("exit event = %+v", exited)
	}
	if list, _ := eng.List(); len(list) != 0 {
		t.Fatalf("List after Kill = %+v", list)
	}
	if err := eng.Kill(id); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("second Kill err = %v, want ErrNotFound", err)
	}
	if _, err := eng.Messages(id, 10); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Messages after Kill err = %v, want ErrNotFound", err)
	}
}

func TestUnknownSessionID(t *testing.T) {
	eng, _ := newTestEngine(t)
	const unknown = "01JNEVERSEEEDIT"

	if _, err := eng.Prompt(unknown, "hi"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Prompt err = %v", err)
	}
	if err := eng.Cancel(unknown); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Cancel err = %v", err)
	}
	if err := eng.SetMode(unknown, modePlan); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("SetMode err = %v", err)
	}
	if err := eng.Kill(unknown); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Kill err = %v", err)
	}
	if _, err := eng.Messages(unknown, 5); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Messages err = %v", err)
	}
}

func TestSetModeAndTitle(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if err := eng.SetMode(id, modePlan); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	c.next(t, 10*time.Second, ofUpdate("current_mode_update"))
	if err := eng.SetMode(id, "yolo"); !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("SetMode yolo err = %v, want ErrUnsupported", err)
	}
	if err := eng.SetTitle(id, "anything"); !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("SetTitle err = %v, want ErrUnsupported", err)
	}
}

func TestSetModelUsesConfigOption(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if err := eng.SetModel(id, "other/model"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	c.next(t, 10*time.Second, ofUpdate(updateConfigOption))
	list, _ := eng.List()
	if len(list) != 1 || list[0].Model != "other/model" {
		t.Fatalf("List = %+v, want the new model", list)
	}
}

// TestGiantChunk keeps the read loop honest: one update well past the 64 KiB a
// line scanner would refuse.
func TestGiantChunk(t *testing.T) {
	eng, _ := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeGiant); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	msgs := one(t, eng, id)
	if len(msgs) != 2 {
		t.Fatalf("transcript = %d messages", len(msgs))
	}
	text := blocks(t, msgs[1])[0]["text"]
	if s, _ := text.(string); len(s) != 200_000 {
		t.Fatalf("chunk len = %d, want 200000", len(s))
	}
}

// TestTwoSessionsStayApart pins the per-session process model and the engine-side
// permission id counter, which is what keeps two children that both start at 1000
// from answering each other.
func TestTwoSessionsStayApart(t *testing.T) {
	eng, c := newTestEngine(t)
	first := create(t, eng)
	second := create(t, eng)
	if first == second {
		t.Fatalf("two children share session id %q", first)
	}

	if _, err := eng.Prompt(first, FakePermission); err != nil {
		t.Fatalf("Prompt first: %v", err)
	}
	asked := c.next(t, 10*time.Second, func(e model.Event) bool {
		return e.Type == "permission" && e.Session == first
	})
	if asked.RequestID == 0 {
		t.Fatal("permission event carries no request id")
	}
	if err := eng.RespondPermission(asked.RequestID, decisionAllow, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	if _, err := eng.Prompt(second, "unbothered"); err != nil {
		t.Fatalf("Prompt second: %v", err)
	}
	if list, _ := eng.List(); len(list) != 2 {
		t.Fatalf("List = %+v, want both sessions", list)
	}
}

// TestCancelAnswersTheParkedAsk pins the part of cancellation the agent sees:
// a session/cancel alone leaves a parked tool parked forever, so the ask must be
// answered with the cancelled outcome as well (client.rs:2440).
func TestCancelAnswersTheParkedAsk(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if err := eng.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	withdrawn := c.next(t, 10*time.Second, func(e model.Event) bool {
		return e.Type == "permission_cancel" && e.RequestID == asked.RequestID
	})
	if withdrawn.Session != id {
		t.Fatalf("permission_cancel = %+v", withdrawn)
	}
	// The fake streams what it was answered with, so this reads the wire back.
	answered := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if !jsonContains(t, answered.ACP, "option:"+outcomeCancelled) {
		t.Fatalf("agent saw %s, want the cancelled outcome", answered.ACP)
	}
	if err := eng.RespondPermission(asked.RequestID, decisionAllow, ""); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("answering a withdrawn ask err = %v, want ErrNotFound", err)
	}
}

// TestCreateRejectsARelativeCwd keeps the guard that would otherwise cost a
// child process per bad request: session/new demands an absolute cwd.
func TestCreateRejectsARelativeCwd(t *testing.T) {
	eng, _ := newTestEngine(t)

	if _, err := eng.Create("relative/path", "", ""); !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("Create relative err = %v, want ErrUnsupported", err)
	}
	if _, err := eng.Create("", "", ""); err != nil {
		t.Fatalf("Create with no cwd: %v", err)
	}
}

// TestFailedTurnNotice covers the one report a turn failure gets after the
// handler already answered: a notice carrying the agent's own words.
func TestFailedTurnNotice(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakeElsewhere); !errors.Is(err, model.ErrOpenElsewhere) {
		t.Fatalf("err = %v, want ErrOpenElsewhere", err)
	}
	noticed := c.next(t, 10*time.Second, func(e model.Event) bool {
		return e.Type == "notice" && strings.Contains(e.Text, fakeElsewhereText)
	})
	if noticed.Session != id || noticed.Engine != model.EngineACP {
		t.Fatalf("notice = %+v", noticed)
	}
}

// jsonContains reports whether raw JSON holds want as a substring.
func jsonContains(t *testing.T, raw json.RawMessage, want string) bool {
	t.Helper()
	return strings.Contains(string(raw), want)
}

// TestRealMakiSession drives the binary the gateway will really run. It needs a
// configured provider and a network, so it stays behind an env flag.
func TestRealMakiSession(t *testing.T) {
	if os.Getenv("MAKI_E2E") != "1" {
		t.Skip("set MAKI_E2E=1 to run against the real maki")
	}
	path := os.Getenv("MAKI_PATH")
	if path == "" {
		path = "maki"
	}

	eng, c := newTestEngine(t)
	eng.makiPath = path
	id, err := eng.Create(t.TempDir(), "", "Reply with exactly one word: READY")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.next(t, 2*time.Minute, ofUpdate(updateMessageChunk))
	waitStatus(t, eng, id, model.StatusIdle)

	msgs := one(t, eng, id)
	if len(msgs) < 2 {
		t.Fatalf("transcript = %v, want the prompt and a reply", msgs)
	}
	t.Logf("real session %s replied %v", id, blocks(t, msgs[len(msgs)-1]))
}
