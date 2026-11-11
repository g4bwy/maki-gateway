package acpengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	return createIn(t, eng, t.TempDir())
}

// createIn opens a web session in a directory the test already owns, so a marker
// placed there is one the fake child can act on.
func createIn(t *testing.T, eng *Engine, dir string) string {
	t.Helper()
	id, err := eng.Create(model.CreateRequest{Cwd: dir, Model: "fake/model"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })
	return id
}

// eventDrain is how long a test waits to prove an event never came.
const eventDrain = 300 * time.Millisecond

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

// TestPermissionAllowAlwaysPicksTheAlwaysOption is the wire check for the third
// decision: the fake streams back the option id it was answered with.
func TestPermissionAllowAlwaysPicksTheAlwaysOption(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if err := eng.RespondPermission(asked.RequestID, decisionAllowAlways, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	chunk := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if want := "option:" + optionAllowAlways; !jsonContains(t, chunk.ACP, want) {
		t.Fatalf("agent saw %s, want %q", chunk.ACP, want)
	}
}

// TestPermissionAllowAlwaysFallsBackToOnce covers the degradation: an agent that
// offers no "always" option still gets an answer this side can send, and it is
// the once form.
func TestPermissionAllowAlwaysFallsBackToOnce(t *testing.T) {
	eng, c := newTestEngine(t)
	id := create(t, eng)

	if _, err := eng.Prompt(id, FakePermission+" "+FakeNoAlways); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	asked := c.next(t, 10*time.Second, ofType("permission"))
	if err := eng.RespondPermission(asked.RequestID, decisionAllowAlways, ""); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	chunk := c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))
	if want := "option:" + optionAllowOnce; !jsonContains(t, chunk.ACP, want) {
		t.Fatalf("agent saw %s, want the allow_once fallback %q", chunk.ACP, want)
	}
}

// TestOptionForDecisionTable pins the whole decision map against the four options
// maki-acp offers, plus the three-option agent.
func TestOptionForDecisionTable(t *testing.T) {
	full := []permissionOption{
		{OptionID: optionAllowOnce, Kind: kindAllowOnce},
		{OptionID: optionAllowAlways, Kind: kindAllowAlways},
		{OptionID: optionRejectOnce, Kind: kindRejectOnce},
		{OptionID: "reject_always", Kind: kindRejectAlways},
	}
	noAlways := []permissionOption{
		{OptionID: optionAllowOnce, Kind: kindAllowOnce},
		{OptionID: optionRejectOnce, Kind: kindRejectOnce},
	}
	// An agent that spells its ids differently is matched by kind.
	byKind := []permissionOption{
		{OptionID: "opt-1", Kind: kindAllowAlways},
		{OptionID: "opt-2", Kind: kindRejectAlways},
	}

	cases := []struct {
		name     string
		options  []permissionOption
		decision string
		want     string
		wantErr  bool
	}{
		{"allow full", full, decisionAllow, optionAllowOnce, false},
		{"allow always full", full, decisionAllowAlways, optionAllowAlways, false},
		{"deny full", full, decisionDeny, optionRejectOnce, false},
		{"allow once only", noAlways, decisionAllow, optionAllowOnce, false},
		{"allow always degrades", noAlways, decisionAllowAlways, optionAllowOnce, false},
		{"deny once only", noAlways, decisionDeny, optionRejectOnce, false},
		{"allow by kind", byKind, decisionAllow, "opt-1", false},
		{"allow always by kind", byKind, decisionAllowAlways, "opt-1", false},
		{"deny by kind", byKind, decisionDeny, "opt-2", false},
		{"unknown decision", full, "maybe", "", true},
		{"empty offer", nil, decisionAllow, "", true},
		{"no always offer", []permissionOption{{OptionID: optionRejectOnce, Kind: kindRejectOnce}}, decisionAllowAlways, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optionFor(tc.options, tc.decision)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("optionFor(%q) = %q, want an error", tc.decision, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("optionFor(%q) = %q, %v, want %q", tc.decision, got, err, tc.want)
			}
		})
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

// spawnArgvs reads back the arguments the fake recorded, one entry per child it
// was started as, in the order they started.
func spawnArgvs(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	var out [][]string
	for _, block := range strings.Split(string(raw), "\n\n") {
		if block == "" {
			continue
		}
		out = append(out, strings.Split(strings.TrimSuffix(block, "\n"), "\n"))
	}
	return out
}

// onlySpawn is the one-child case, which is also the claim that nothing respawned.
func onlySpawn(t *testing.T, path string) []string {
	t.Helper()
	argvs := spawnArgvs(t, path)
	if len(argvs) != 1 {
		t.Fatalf("%d children were spawned, want 1: %v", len(argvs), argvs)
	}
	return argvs[0]
}

// spawnDir is a temp dir plus the file the fake records its argv in.
func spawnDir(t *testing.T) (dir, argvPath string) {
	t.Helper()
	dir = t.TempDir()
	argvPath = filepath.Join(dir, "argv")
	t.Setenv(FakeArgvEnv, argvPath)
	return
}

// TestCreateYoloPassesTheFlag is the argv check for yolo web sessions. The flag
// belongs to the `acp` subcommand, so it must follow it: maki reads the global
// --yolo only on the tui and print paths (src/cmd/acp.rs:108 takes the
// subcommand field), which would make `maki --yolo acp` a silent no-op.
func TestCreateYoloPassesTheFlag(t *testing.T) {
	dir, argvPath := spawnDir(t)

	eng, _ := newTestEngine(t)
	id, err := eng.Create(model.CreateRequest{Cwd: dir, Model: "fake/model", Yolo: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })

	args := onlySpawn(t, argvPath)
	if len(args) < 2 || args[0] != argACP || args[1] != argYolo {
		t.Fatalf("spawned %v, want %q first and %q next", args, argACP, argYolo)
	}
	if i := slices.Index(args, argModel); i < 0 || i+1 >= len(args) || args[i+1] != "fake/model" {
		t.Fatalf("spawned %v, want the model spec after %q", args, argModel)
	}
}

// TestCreateWithoutYoloPassesNoFlag: yolo is opt-in per request, and a plain
// create must still park every prompt.
func TestCreateWithoutYoloPassesNoFlag(t *testing.T) {
	dir, argvPath := spawnDir(t)

	eng, _ := newTestEngine(t)
	id, err := eng.Create(model.CreateRequest{Cwd: dir, Model: "fake/model"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })

	if args := onlySpawn(t, argvPath); slices.Contains(args, argYolo) {
		t.Fatalf("spawned %v, want no %q", args, argYolo)
	}
}

// all collects every event the hub sends within d, so a test can also assert that
// something never arrived.
func (c *collector) all(d time.Duration) []model.Event {
	deadline := time.After(d)
	for {
		select {
		case e := <-c.sub:
			c.events = append(c.events, e)
		case <-deadline:
			return c.events
		}
	}
}

// TestCreateRecordsYoloForTheList: the row a browser sees has to carry the answer
// the child was started with, or its toggle chip starts out wrong.
func TestCreateRecordsYoloForTheList(t *testing.T) {
	eng, _ := newTestEngine(t)
	on, err := eng.Create(model.CreateRequest{Cwd: t.TempDir(), Yolo: true})
	if err != nil {
		t.Fatalf("Create yolo: %v", err)
	}
	off, err := eng.Create(model.CreateRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(on); _ = eng.Kill(off) })

	list, err := eng.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, s := range list {
		got[s.ID] = s.Yolo
	}
	if !got[on] || got[off] {
		t.Fatalf("yolo rows = %v, want %s on and %s off", got, on, off)
	}
}

// transcriptDump renders the cached transcript, for a before and after compare.
func transcriptDump(t *testing.T, eng *Engine, id string) string {
	t.Helper()
	raw, err := json.Marshal(one(t, eng, id))
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	return string(raw)
}

// carriesReplay reports whether any collected event holds the turn the fake
// replays on a load. Neither the transcript reducer nor the browser may see it:
// both already hold that history.
func carriesReplay(evts []model.Event) bool {
	for _, e := range evts {
		if e.Type == "update" && strings.Contains(string(e.ACP), fakeReplayText) {
			return true
		}
	}
	return false
}

// rowFor reads one session out of the engine's own list.
func rowFor(t *testing.T, eng *Engine, id string) (model.Session, bool) {
	t.Helper()
	list, err := eng.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return model.Session{}, false
}

// heldSession writes the load marker a fake child needs to answer -32000 once.
func heldSession(t *testing.T, dir, keep string) {
	t.Helper()
	path := filepath.Join(dir, FakeBusyLoad)
	if err := os.WriteFile(path, []byte(keep), 0o600); err != nil {
		t.Fatalf("write load marker: %v", err)
	}
}

// TestSetYoloTurnsItOn respawns an idle session's child with the flag, on the same
// session: the transcript cache keeps what it held, the replayed history reaches
// neither the cache nor the browser, the row reports the new answer with its cwd
// and model intact, and the next prompt runs on the new child.
func TestSetYoloTurnsItOn(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, c := newTestEngine(t)
	id := createIn(t, eng, dir)

	if _, err := eng.Prompt(id, "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	before := transcriptDump(t, eng, id)

	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on: %v", err)
	}

	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 2 {
		t.Fatalf("%d children, want 2 (the respawn): %v", len(argvs), argvs)
	}
	if slices.Contains(argvs[0], argYolo) {
		t.Errorf("first spawn = %v, want no %q", argvs[0], argYolo)
	}
	if len(argvs[1]) < 2 || argvs[1][0] != argACP || argvs[1][1] != argYolo {
		t.Errorf("respawn = %v, want %q right after %q", argvs[1], argYolo, argACP)
	}
	if i := slices.Index(argvs[1], argModel); i < 0 || i+1 >= len(argvs[1]) || argvs[1][i+1] != "fake/model" {
		t.Errorf("respawn = %v, want the model the session was created with", argvs[1])
	}
	if carriesReplay(c.all(eventDrain)) {
		t.Error("a replayed turn reached the browser during the load")
	}
	if got := transcriptDump(t, eng, id); got != before {
		t.Errorf("transcript changed across the load:\n before %s\n after  %s", before, got)
	}

	row, ok := rowFor(t, eng, id)
	if !ok {
		t.Fatal("the session left the list during the toggle")
	}
	if !row.Yolo || row.Kind != model.KindWeb || row.Cwd != dir || row.Status != model.StatusIdle {
		t.Fatalf("row = %+v, want web/idle in %s with yolo on", row, dir)
	}

	if _, err := eng.Prompt(id, "again on the new child"); err != nil {
		t.Fatalf("Prompt after the respawn: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	if got := transcriptDump(t, eng, id); got == before {
		t.Error("the turn after the respawn left no trace in the transcript")
	}
	if !strings.Contains(transcriptDump(t, eng, id), "again on the new child") {
		t.Error("the turn after the respawn is not in the transcript")
	}

	// A second load of the same session has nothing left to do.
	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on again: %v", err)
	}
	if argvs := spawnArgvs(t, argvPath); len(argvs) != 2 {
		t.Fatalf("toggling to the answer it already has respawned: %v", argvs)
	}
}

// TestSetYoloTurnsItOff is the other direction: the new child must not carry the
// flag, which is the only way it starts asking again.
func TestSetYoloTurnsItOff(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, _ := newTestEngine(t)
	id, err := eng.Create(model.CreateRequest{Cwd: dir, Model: "fake/model", Yolo: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })

	if err := eng.SetYolo(id, false); err != nil {
		t.Fatalf("SetYolo off: %v", err)
	}
	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 2 {
		t.Fatalf("%d children, want 2: %v", len(argvs), argvs)
	}
	if !slices.Contains(argvs[0], argYolo) {
		t.Errorf("first spawn = %v, want %q", argvs[0], argYolo)
	}
	if slices.Contains(argvs[1], argYolo) {
		t.Errorf("respawn = %v, want no %q", argvs[1], argYolo)
	}
	row, ok := rowFor(t, eng, id)
	if !ok || row.Yolo {
		t.Fatalf("row = %+v (%v), want yolo off", row, ok)
	}
}

// TestSetYoloKeepsTheModelTheUserChose is the model half of the toggle. The child
// reads -m once, at startup, so a respawn that repeats only the create request puts
// the session back on a model the user left during the chat.
func TestSetYoloKeepsTheModelTheUserChose(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, c := newTestEngine(t)
	id, err := eng.Create(model.CreateRequest{Cwd: dir, Model: "model/a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })
	if got := onlySpawn(t, argvPath); !slices.Contains(got, "model/a") {
		t.Fatalf("first spawn = %v, want the model of the create request", got)
	}

	if err := eng.SetModel(id, "model/b"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	c.next(t, 10*time.Second, ofUpdate(updateConfigOption))

	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on: %v", err)
	}
	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 2 {
		t.Fatalf("%d children, want 2 (the respawn): %v", len(argvs), argvs)
	}
	if i := slices.Index(argvs[1], argModel); i < 0 || i+1 >= len(argvs[1]) || argvs[1][i+1] != "model/b" {
		t.Fatalf("respawn = %v, want the model the user set last after %q", argvs[1], argModel)
	}
	if !slices.Contains(argvs[1], argYolo) {
		t.Errorf("respawn = %v, want %q", argvs[1], argYolo)
	}
	// The answer it already has starts nothing, so a second toggle leaves the argv
	// of the session alone.
	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on again: %v", err)
	}
	if argvs := spawnArgvs(t, argvPath); len(argvs) != 2 {
		t.Fatalf("toggling to the answer it has respawned: %v", argvs)
	}
}

// TestSetYoloIgnoresTheModelTheChildReports draws the line the carry-over stops at:
// a child names a model in its own configOptions on every handshake and every load,
// and that report is what the list row shows. It is not a choice of the user, so a
// session created without a model must not grow one from it.
func TestSetYoloIgnoresTheModelTheChildReports(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, _ := newTestEngine(t)
	id, err := eng.Create(model.CreateRequest{Cwd: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = eng.Kill(id) })
	if got := onlySpawn(t, argvPath); slices.Contains(got, argModel) {
		t.Fatalf("first spawn = %v, want no %q", got, argModel)
	}
	// The fake answers every session/new with a model of its own, which is the
	// report this test must not mistake for a request.
	if row, ok := rowFor(t, eng, id); !ok || row.Model == "" {
		t.Fatalf("row = %+v (%v), want the model the child reported", row, ok)
	}

	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on: %v", err)
	}
	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 2 {
		t.Fatalf("%d children, want 2 (the respawn): %v", len(argvs), argvs)
	}
	if slices.Contains(argvs[1], argModel) {
		t.Errorf("respawn = %v, want no %q: a reported model is not a chosen one", argvs[1], argModel)
	}
}

// TestSetYoloRefusesToCutATurnShort: a running prompt is not interrupted. The
// child keeps the flag it has, and no process is replaced.
func TestSetYoloRefusesToCutATurnShort(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, c := newTestEngine(t)
	id := createIn(t, eng, dir)

	if _, err := eng.Prompt(id, "hold "+FakeHold); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	c.next(t, 10*time.Second, ofUpdate(updateMessageChunk))

	if err := eng.SetYolo(id, true); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("SetYolo mid-turn err = %v, want ErrBusy", err)
	}
	onlySpawn(t, argvPath)

	// The turn is still the one that was running: the toggle left it alone.
	if err := eng.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo once idle: %v", err)
	}
	if argvs := spawnArgvs(t, argvPath); len(argvs) != 2 {
		t.Fatalf("%d children after the turn ended, want 2: %v", len(argvs), argvs)
	}
}

// TestSetYoloRerespawnWhileTheSessionIsClaimed covers the flock window: the child
// that is dying holds the session file until it is gone, so the first fresh child
// is answered -32000 and the whole attempt starts over.
func TestSetYoloRerespawnWhileTheSessionIsClaimed(t *testing.T) {
	dir, argvPath := spawnDir(t)
	heldSession(t, dir, "")
	eng, _ := newTestEngine(t)
	eng.yoloRetryWait = 10 * time.Millisecond
	id := createIn(t, eng, dir)

	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo after one refused load: %v", err)
	}
	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 3 {
		t.Fatalf("%d children, want 3 (create, refused load, loaded): %v", len(argvs), argvs)
	}
	for _, args := range argvs[1:] {
		if !slices.Contains(args, argYolo) {
			t.Errorf("respawn %v, want %q", args, argYolo)
		}
	}
	if row, ok := rowFor(t, eng, id); !ok || !row.Yolo {
		t.Fatalf("row = %+v (%v), want the session back with yolo on", row, ok)
	}
	if _, err := eng.Prompt(id, "still working"); err != nil {
		t.Fatalf("Prompt after the retry: %v", err)
	}
}

// TestSetYoloGivesUpOnAPermanentClaim bounds the retry loop: a session that never
// frees is reported as it is, open elsewhere, and the record is left exited with
// its transcript.
func TestSetYoloGivesUpOnAPermanentClaim(t *testing.T) {
	dir, argvPath := spawnDir(t)
	heldSession(t, dir, FakeBusyLoadAlways)
	eng, c := newTestEngine(t)
	eng.yoloRetries = 2
	eng.yoloRetryWait = 10 * time.Millisecond
	id := createIn(t, eng, dir)
	if _, err := eng.Prompt(id, "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
	before := transcriptDump(t, eng, id)

	err := eng.SetYolo(id, true)
	if !errors.Is(err, model.ErrOpenElsewhere) {
		t.Fatalf("SetYolo err = %v, want ErrOpenElsewhere", err)
	}
	// One create plus one child per attempt, and no third.
	if argvs := spawnArgvs(t, argvPath); len(argvs) != 3 {
		t.Fatalf("%d children, want 3 (create and %d attempts): %v", len(argvs), eng.yoloRetries, argvs)
	}
	if _, ok := rowFor(t, eng, id); ok {
		t.Error("the row is still listed after the respawn gave up")
	}
	c.next(t, 10*time.Second, ofType("exit"))
	if got := transcriptDump(t, eng, id); got != before {
		t.Errorf("a failed respawn rewrote the transcript:\n before %s\n after  %s", before, got)
	}
}

// TestSetYoloRevivesAnExitedSession is the reason an exited row is not an error:
// the session is stored, a fresh child loads it back, and the chat continues with
// the answer the toggle asked for.
func TestSetYoloRevivesAnExitedSession(t *testing.T) {
	dir, argvPath := spawnDir(t)
	eng, c := newTestEngine(t)
	id := createIn(t, eng, dir)

	if _, err := eng.Prompt(id, FakeExit); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	c.next(t, 10*time.Second, ofType("exit"))
	if _, ok := rowFor(t, eng, id); ok {
		t.Fatal("a dead child is still listed as live")
	}

	if err := eng.SetYolo(id, true); err != nil {
		t.Fatalf("SetYolo on an exited session: %v", err)
	}
	argvs := spawnArgvs(t, argvPath)
	if len(argvs) != 2 || !slices.Contains(argvs[1], argYolo) {
		t.Fatalf("spawned %v, want a second child with %q", argvs, argYolo)
	}
	row, ok := rowFor(t, eng, id)
	if !ok || !row.Yolo || row.Kind != model.KindWeb {
		t.Fatalf("row = %+v (%v), want the session back as web with yolo on", row, ok)
	}
	if _, err := eng.Prompt(id, "the chat continues"); err != nil {
		t.Fatalf("Prompt on the revived session: %v", err)
	}
	waitStatus(t, eng, id, model.StatusIdle)
}

// TestSetYoloUnknownSession is the 404 half of the contract.
func TestSetYoloUnknownSession(t *testing.T) {
	eng, _ := newTestEngine(t)
	if err := eng.SetYolo("no-such-session", true); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("SetYolo unknown err = %v, want ErrNotFound", err)
	}
}

// TestCreateRejectsARelativeCwd keeps the guard that would otherwise cost a
// child process per bad request: session/new demands an absolute cwd.
func TestCreateRejectsARelativeCwd(t *testing.T) {
	eng, _ := newTestEngine(t)

	if _, err := eng.Create(model.CreateRequest{Cwd: "relative/path"}); !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("Create relative err = %v, want ErrUnsupported", err)
	}
	if _, err := eng.Create(model.CreateRequest{}); err != nil {
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

// TestMapWireErrorCodes is the table the web engine answers with, and the one row
// that was missing from it: an agent that has no such session says so with -32002,
// which is a plain answer about the session rather than a gateway fault, so the
// browser must see 404 and not 500.
func TestMapWireErrorCodes(t *testing.T) {
	const missingText = "Session not found: 01J8NOSESSION"
	cases := []struct {
		name string
		in   wireError
		want error
	}{
		{"busy_prompt", wireError{Code: codeInternal, Message: busyPromptText}, model.ErrBusy},
		{"held_elsewhere", wireError{Code: codeAuthOrBusy, Message: fakeElsewhereText}, model.ErrOpenElsewhere},
		{"no_active_session", wireError{Code: codeInvalidRequest}, model.ErrNotFound},
		{"session_not_found", wireError{Code: codeNotFound, Message: missingText}, model.ErrNotFound},
		{"child_gone", wireError{Code: codeGone, Message: "the child died"}, errGone},
	}
	for _, c := range cases {
		in := c.in
		got := mapWireError(&in)
		if !errors.Is(got, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, got, c.want)
		}
	}
	// The agent's own sentence travels with the sentinel, because it is the only
	// part of the answer that names the session the user clicked.
	in := wireError{Code: codeNotFound, Message: missingText}
	if got := mapWireError(&in); !strings.Contains(got.Error(), missingText) {
		t.Errorf("not-found error = %q, want it to quote %q", got, missingText)
	}
	// A code with no gateway meaning stays the wire error rather than becoming a
	// not-found guess.
	other := wireError{Code: codeMethodNotFound, Message: "method not found"}
	if got := mapWireError(&other); !errors.Is(got, &other) {
		t.Errorf("method-not-found: err = %v (%T), want the wire error itself", got, got)
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
	id, err := eng.Create(model.CreateRequest{
		Cwd:    t.TempDir(),
		Prompt: "Reply with exactly one word: READY",
	})
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
