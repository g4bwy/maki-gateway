package attach

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

// setup starts one engine over a fresh socket dir with one plugin on pid 111.
func setup(t *testing.T) (*fakeTUI, *Engine, chan model.Event) {
	t.Helper()
	dir := t.TempDir()
	e, ch := startEngine(t, dir, fastTimings())
	f := newFakeTUI(t, dir, "pid-111.sock", 111)
	waitFor(t, "the plugin socket to be tracked", func() bool { return e.connCount() == 1 })
	// Consume the connect announcement, so a later drain() only sees what the
	// test itself caused.
	wantEvent(t, ch, evSessions)
	return f, e, ch
}

// drain reads whatever is queued, so a test can assert nothing else was sent.
func drain(ch chan model.Event) []model.Event {
	var out []model.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestKindIsLive(t *testing.T) {
	e, _ := startEngine(t, t.TempDir(), fastTimings())
	if e.Kind() != model.KindLive {
		t.Fatalf("kind: %s", e.Kind())
	}
}

func TestHelloThenListMapsToLiveSessions(t *testing.T) {
	f, e, _ := setup(t)
	f.setTabs(
		liveTab("s1", model.StatusWorking),
		map[string]any{"id": "s2", "title": "", "status": model.StatusNeedsInput, "cwd": "/other",
			"model": "glm", "focused": false, "updated_at": 1781000900},
	)
	got, err := e.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tabs: %+v", got)
	}
	want := model.Session{ID: "s1", Kind: model.KindLive, Title: "tab s1",
		Status: model.StatusWorking, Cwd: "/work", Model: "m-s1", PluginPID: 111}
	if got[0] != want {
		t.Fatalf("list mapping:\n got %+v\nwant %+v", got[0], want)
	}
	if got[1].ID != "s2" || got[1].Status != model.StatusNeedsInput || got[1].Title != "" {
		t.Errorf("second tab: %+v", got[1])
	}
	// CONTRACTS 1 has no clock on the list op; one is read if the plugin sends it.
	if got[1].UpdatedAt != 1781000900 || got[0].UpdatedAt != 0 {
		t.Errorf("updated_at: %+v", got)
	}
	// hello is the first frame on a fresh connection, per CONTRACTS 1, and
	// connecting announced the list change (consumed by setup).
	ops := f.opsSeen()
	if len(ops) == 0 || ops[0] != opHello {
		t.Errorf("hello must start the conversation: %v", ops)
	}
}

func TestMessagesPassthrough(t *testing.T) {
	_, e, _ := setup(t)
	got, err := e.Messages("s1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 records: %d", len(got))
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Errorf("roles: %+v", got)
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(got[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if string(blocks[0]["type"]) != `"tool_use"` || string(blocks[0]["name"]) != `"bash"` {
		t.Errorf("content must pass through untouched: %s", got[1].Content)
	}
	if _, err := e.Messages("nope", 10); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown session: want ErrNotFound, got %v", err)
	}
}

func TestPromptReturnsStateAndMapsErrors(t *testing.T) {
	f, e, _ := setup(t)
	state, err := e.Prompt("s1", "do the thing")
	if err != nil || state != "started" {
		t.Fatalf("prompt: %q %v", state, err)
	}
	if fr := f.frameWithOp(opPrompt); fr[argText] != "do the thing" {
		t.Errorf("prompt frame: %v", fr)
	}
	f.answerPromptWith("queued")
	if state, err = e.Prompt("s1", "more"); err != nil || state != "queued" {
		t.Fatalf("queued prompt: %q %v", state, err)
	}
	if _, err := e.Prompt("gone", "x"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("session not live must map to ErrNotFound, got %v", err)
	}
	f.failWith(opPrompt, "open in another window")
	if _, err := e.Prompt("s1", "x"); !errors.Is(err, model.ErrOpenElsewhere) {
		t.Errorf("open elsewhere: got %v", err)
	}
}

func TestSetModeAndSetTitle(t *testing.T) {
	f, e, _ := setup(t)
	if err := e.SetMode("s1", "plan"); err != nil {
		t.Fatal(err)
	}
	fr := f.frameWithOp(opSetMode)
	if fr[argSession] != "s1" || fr[argMode] != "plan" {
		t.Errorf("set_mode frame: %v", fr)
	}
	if err := e.SetTitle("s1", "new name"); err != nil {
		t.Fatal(err)
	}
	if fr = f.frameWithOp(opSetTitle); fr[argTitle] != "new name" {
		t.Errorf("set_title frame: %v", fr)
	}
	// An empty title clears the tab, which CONTRACTS 1 spells null.
	if err := e.SetTitle("s1", ""); err != nil {
		t.Fatal(err)
	}
	fr = f.frameWithOp(opSetTitle)
	title, present := fr[argTitle]
	if !present || title != nil {
		t.Errorf("clearing a title must send null, got %#v in %v", title, fr)
	}
}

func TestCreateUsesTheFirstLiveConn(t *testing.T) {
	dir := t.TempDir()
	e, _ := startEngine(t, dir, fastTimings())
	if _, err := e.Create(model.CreateRequest{Cwd: "/work", Model: "model", Prompt: "hi"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("no tui must map to ErrNotFound, got %v", err)
	}
	first := newFakeTUI(t, dir, "pid-1.sock", 1)
	second := newFakeTUI(t, dir, "pid-2.sock", 2)
	waitFor(t, "both plugins to be tracked", func() bool { return e.connCount() == 2 })

	id, err := e.Create(model.CreateRequest{Cwd: "/work", Model: "model", Prompt: "start here"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "created-1" {
		t.Fatalf("new reply: %q", id)
	}
	if first.frameWithOp(opNew) == nil {
		t.Error("the lowest socket path should own the new tab")
	}
	if second.frameWithOp(opNew) != nil {
		t.Error("only one connection may take the new tab")
	}
	if fr := first.frameWithOp(opNew); fr[argCwd] != "/work" || fr[argPrompt] != "start here" {
		t.Errorf("new frame: %v", fr)
	}
	if _, routed := e.owner(id); !routed {
		t.Error("a created session must be routable right away")
	}
}

func TestPermissionBridgeIsArmedOnConnect(t *testing.T) {
	f, _, _ := setup(t)
	fr := f.frameWithOp(opPerms)
	if fr == nil {
		t.Fatalf("the gateway must turn the bridge on, saw ops %v", f.opsSeen())
	}
	if fr[argOn] != true {
		t.Errorf("permissions frame: %v", fr)
	}
	if ops := f.opsSeen(); slices.Index(ops, opPerms) < slices.Index(ops, opHello) {
		t.Errorf("hello must come first: %v", ops)
	}
}

func TestPlanForwardsTheSocketAnswer(t *testing.T) {
	_, e, _ := setup(t)
	raw, err := e.Plan("s1")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["mode"] != "plan" || got["path"] != "/tmp/plan.md" || got["ready"] != true ||
		got["content"] != "# plan" {
		t.Fatalf("plan passthrough: %s", raw)
	}
	if _, err := e.Plan("nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
}

// TestTasksForwardsTheSocketAnswer: the rows reach the caller as maki wrote them,
// and the frame names no session, because the host only lists the focused tab.
func TestTasksForwardsTheSocketAnswer(t *testing.T) {
	f, e, _ := setup(t)
	raw, err := e.Tasks("s1")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("tasks passthrough: %s", raw)
	}
	if len(rows) != 2 || rows[0]["id"] != "main" || rows[0]["status"] != nil ||
		rows[1]["status"] != "working" || rows[1]["tool"] != "task" {
		t.Fatalf("tasks = %s", raw)
	}
	fr := f.frameWithOp(opTasks)
	if fr == nil {
		t.Fatalf("the tasks op never reached the plugin, saw %v", f.opsSeen())
	}
	if _, has := fr[argSession]; has {
		t.Errorf("the tasks op takes no session argument: %v", fr)
	}
	// No rows is still a list, not null.
	f.setTasks()
	if raw, err = e.Tasks("s1"); err != nil || string(raw) != "[]" {
		t.Fatalf("empty tasks = %s, %v", raw, err)
	}
	f.failWith(opTasks, "unknown session: s9")
	if _, err := e.Tasks("s1"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("plugin error mapping: %v", err)
	}
}

// routeSession teaches the engine which connection owns one session the way a list
// pass does, so a test of an op that needs the route does not race that poll.
func routeSession(t *testing.T, e *Engine, id string) {
	t.Helper()
	if _, err := e.List(); err != nil {
		t.Fatalf("List: %v", err)
	}
	waitFor(t, "the route of "+id, func() bool {
		_, ok := e.owner(id)
		return ok
	})
}

// TestFocusTaskCarriesTheTaskID pins the frame of one follow: the task rides in
// `task_id`, so the frame keeps a request number of its own and the plugin answers
// it like every other op. A follow is one call to the TUI that owns the session, and
// it never carries a session argument, because the host moves a chat of the tab the
// terminal already has focused.
func TestFocusTaskCarriesTheTaskID(t *testing.T) {
	f, e, ch := setup(t)
	routeSession(t, e, "s1")
	drain(ch)
	if err := e.FocusTask("s1", "tu1"); err != nil {
		t.Fatalf("FocusTask: %v", err)
	}
	fr := f.frameWithOp(opTaskFocus)
	if fr == nil {
		t.Fatalf("task_focus never reached the plugin, saw %v", f.opsSeen())
	}
	if fr[argTaskID] != "tu1" {
		t.Errorf("frame = %v, want the task in task_id", fr)
	}
	if n, ok := fr["id"].(float64); !ok || n == 0 {
		t.Errorf("frame = %v, want a request number in id", fr)
	}
	if _, has := fr[argSession]; has {
		t.Errorf("the host takes no session, so none travels: %v", fr)
	}
	if f.followedTask() != "tu1" {
		t.Errorf("the plugin was left following %q", f.followedTask())
	}
	// Following a live tab is one change on the screen, so the browser hears about
	// it from the plugin's own event rather than from the answer of its call.
	ev := wantEvent(t, ch, evTaskFocus)
	if ev.Session != "s1" || ev.TaskID != "tu1" {
		t.Errorf("follow published %+v, want a task_focus for tu1", ev)
	}
	// "main" is the main chat, and the gateway passes it through unchanged.
	if err := e.FocusTask("s1", "main"); err != nil {
		t.Errorf("follow the main chat: %v", err)
	}
	if ev := wantEvent(t, ch, evTaskFocus); ev.TaskID != "main" {
		t.Errorf("follow main published %+v", ev)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a follow publishes task_focus only: %+v", extra)
	}
	// No frame on this socket may spend its request number on a task name: such a
	// plugin answer falls back to id 0, where no waiting request can find it.
	if bad := f.framesWithoutNumber(); len(bad) != 0 {
		t.Errorf("%d request frames arrived with no number in id: %v", len(bad), bad)
	}
}

// TestFocusTaskRefusalsKeepTheWords is the body of POST .../tasks/{tid}/focus. The
// terminal's own sentence is what a reader can act on, so a reworded refusal or a
// 500 for a chat that simply is not there both count as broken.
func TestFocusTaskRefusalsKeepTheWords(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	err := e.FocusTask("s1", "tu9")
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown task: err = %v, want ErrNotFound", err)
	}
	if err.Error() != "no task with id tu9" {
		t.Errorf("refusal = %q, want the host's words unchanged", err.Error())
	}

	f.refuseUI()
	err = e.FocusTask("s1", "tu1")
	if !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("no terminal: err = %v, want ErrUnsupported", err)
	}
	if err.Error() != noUI+" attached" {
		t.Errorf("refusal = %q, want the plugin's words unchanged", err.Error())
	}

	// A session no connection claims has no screen to move, and the call is never
	// fanned out: another TUI's tab is not the one the browser is looking at.
	if err := e.FocusTask("gone", "tu1"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unrouted session: %v", err)
	}
	if n := len(f.framesOf(opTaskFocus)); n != 2 {
		t.Errorf("the plugin was asked to follow %d times, want the two live calls only", n)
	}
}

// TestFocusCallsAnswerInOrder drives the same connection from several browsers at
// once. Each follow now carries its own request number, so the answers cannot be
// mixed up, and one slow or refused call cannot make another caller wait: every
// call gets its own reply, in any order.
func TestFocusCallsAnswerInOrder(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	f.setTasks(
		map[string]any{"id": "main", "session": "s1", "name": "main", "focused": true},
		map[string]any{"id": "t-a", "session": "s1", "name": "a", "status": "done"},
		map[string]any{"id": "t-b", "session": "s1", "name": "b", "status": "done"},
	)
	var wg sync.WaitGroup
	for _, task := range []string{"t-a", "t-b", "t-a", "t-b", "main"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.FocusTask("s1", task); err != nil {
				t.Errorf("FocusTask(%s): %v", task, err)
			}
		}()
	}
	wg.Wait()
	frames := f.framesOf(opTaskFocus)
	if len(frames) != 5 {
		t.Fatalf("saw %d task_focus frames, want one per call", len(frames))
	}
	numbers := map[float64]string{}
	for _, fr := range frames {
		n, _ := fr["id"].(float64)
		task := str(fr[argTaskID])
		if n == 0 {
			t.Errorf("follow of %s carried no request number: %v", task, fr)
			continue
		}
		if twice, dup := numbers[n]; dup {
			t.Errorf("request number %v was used by the follow of %s and of %s", n, twice, task)
		}
		numbers[n] = task
	}
	if len(numbers) != 5 {
		t.Errorf("%d distinct request numbers for 5 follows, want one each", len(numbers))
	}
}

// TestTaskFocusEventCarriesTheTask is the other direction: the person at the
// terminal moved the followed chat with the cycling keys. That event names the task
// in the `id` field, where CONTRACTS 1 otherwise puts a request number, so a reader
// that only takes a number there drops the frame and the browser never hears.
func TestTaskFocusEventCarriesTheTask(t *testing.T) {
	f, _, ch := setup(t)
	f.push(map[string]any{"event": evTaskFocus, "session": "s1", "id": "tu1"})
	ev := wantEvent(t, ch, evTaskFocus)
	if ev.Session != "s1" || ev.TaskID != "tu1" {
		t.Errorf("task_focus event = %+v, want session s1 and task tu1", ev)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a follow change publishes task_focus only: %+v", extra)
	}
}

// TestTasksChangedEventMapsToTasks is the live half of task visibility: one hub
// event per plugin event, with the session the task belongs to.
func TestTasksChangedEventMapsToTasks(t *testing.T) {
	f, _, ch := setup(t)
	f.push(map[string]any{"event": "tasks_changed", "session": "s1"})
	ev := wantEvent(t, ch, evTasks)
	if ev.Type != evTasks || ev.Session != "s1" {
		t.Errorf("tasks event: %+v", ev)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a task change publishes tasks only: %+v", extra)
	}
}

// TestRespondAllowAlwaysSendsAllow pins the degradation: the bridge verdict is
// binary, so "always" is dropped here rather than reaching Lua.
func TestRespondAllowAlwaysSendsAllow(t *testing.T) {
	f, e, ch := setup(t)
	f.pushPermission(81, "s1", "bash", map[string]any{"command": "ls"})
	if ev := wantEvent(t, ch, evPermission); ev.RequestID != 81 {
		t.Fatalf("permission event: %+v", ev)
	}
	if err := e.RespondPermission(81, model.DecisionAllowAlways, "remember it"); err != nil {
		t.Fatal(err)
	}
	f.waitForAnswer(t, 81)
	fr := f.frameWithOp(opRespond)
	if fr == nil || fr[argDecision] != model.DecisionAllow {
		t.Fatalf("respond frame: %v, want decision %q", fr, model.DecisionAllow)
	}
	if fr[argGuidance] != "remember it" {
		t.Errorf("guidance must still travel: %v", fr)
	}
}

// TestCreateYoloTravelsToTheNewOp: the field is only sent when asked for, and the
// plugin that does not know it keeps working.
func TestCreateYoloTravelsToTheNewOp(t *testing.T) {
	f, e, _ := setup(t)
	if _, err := e.Create(model.CreateRequest{Cwd: "/work", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	if fr := f.frameWithOp(opNew); fr != nil {
		if _, has := fr[argYolo]; has {
			t.Errorf("a plain create sends no yolo: %v", fr)
		}
	}
	if _, err := e.Create(model.CreateRequest{Cwd: "/work", Prompt: "go", Yolo: true}); err != nil {
		t.Fatal(err)
	}
	if fr := f.frameWithOp(opNew); fr[argYolo] != true {
		t.Fatalf("yolo create frame: %v", fr)
	}
}

func TestCancelAndKillAreUnsupported(t *testing.T) {
	_, e, _ := setup(t)
	if err := e.Cancel("s1"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("cancel: %v", err)
	}
	if err := e.Kill("s1"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("kill: %v", err)
	}
}

func TestEventMapping(t *testing.T) {
	f, _, ch := setup(t)
	for _, ev := range []map[string]any{
		{"event": "turn_start", "session": "s1", "text": "go"},
		{"event": "tool_start", "session": "s1", "tool_id": "tu1", "tool": "bash"},
		{"event": "tool_done", "session": "s1", "tool_id": "tu1", "tool": "bash",
			"is_error": false, "bytes": 12, "duration_ms": 30},
		{"event": "turn_end", "session": "s1", "reason": "complete", "usage": map[string]any{},
			"cost": 0.1, "context_size": 900, "num_turns": 3},
	} {
		f.push(ev)
	}
	for _, reason := range []string{reasonStatus, reasonToolStart, reasonToolDone, reasonTurnEnd} {
		ev := wantEvent(t, ch, evDirty)
		if ev.Session != "s1" || ev.Reason != reason || ev.Type != evDirty {
			t.Errorf("dirty event: got %+v, want session s1 reason %s", ev, reason)
		}
	}
	// turn_end carries usage, cost and context_size; none of it is forwarded,
	// because analytics reads the meta record the TUI writes itself.
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("content changes must publish dirty only: %+v", extra)
	}

	f.push(map[string]any{"event": evStatus, "session": "s1", "status": model.StatusNeedsInput,
		"title": "tab s1", "focused": true})
	ev := wantEvent(t, ch, evStatus)
	if ev.Session != "s1" || ev.Status != model.StatusNeedsInput || ev.Title != "tab s1" ||
		ev.Focused == nil || !*ev.Focused {
		t.Errorf("status event: %+v", ev)
	}

	f.push(map[string]any{"event": "title", "session": "s1", "title": "renamed"})
	if ev = wantEvent(t, ch, evStatus); ev.Title != "renamed" || ev.Status != "" {
		t.Errorf("a title change is a status frame with the title: %+v", ev)
	}

	f.push(map[string]any{"event": "model", "session": "s1", "model": "glm-4.6"})
	if ev = wantEvent(t, ch, evStatus); ev.Session != "s1" || ev.Title != "" {
		t.Errorf("model event: %+v", ev)
	}

	f.push(map[string]any{"event": "question", "session": "s1"})
	if ev = wantEvent(t, ch, evNotice); ev.Text != questionNotice || ev.Session != "" {
		t.Errorf("question notice: %+v", ev)
	}
}

func TestPermissionBridgeRoundTrip(t *testing.T) {
	f, e, ch := setup(t)
	input := map[string]any{"command": "rm -rf /tmp/x"}
	f.pushPermission(77, "s1", "bash", input)

	ev := wantEvent(t, ch, evPermission)
	if ev.RequestID != 77 || ev.Session != "s1" || ev.Tool != "bash" ||
		ev.Engine != model.EngineAttach || ev.Type != evPermission {
		t.Fatalf("permission event: %+v", ev)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Input, &got); err != nil || got["command"] != "rm -rf /tmp/x" {
		t.Fatalf("input must pass through raw: %s", ev.Input)
	}
	if err := e.RespondPermission(77, "deny", "use a scratch dir"); err != nil {
		t.Fatal(err)
	}
	f.waitForAnswer(t, 77)
	fr := f.frameWithOp(opRespond)
	if fr == nil || fr[argRequest] != float64(77) || fr[argDecision] != "deny" ||
		fr[argGuidance] != "use a scratch dir" {
		t.Fatalf("respond frame: %v", fr)
	}
	// The bridge falling through withdraws the dialog (CONTRACTS 1).
	f.push(map[string]any{"event": evPermissionCancel, "request_id": 78})
	if ev = wantEvent(t, ch, evPermissionCancel); ev.RequestID != 78 {
		t.Errorf("permission_cancel: %+v", ev)
	}
	// An expired id is a 404 for the browser.
	if err := e.RespondPermission(9999, "allow", ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("expired respond: want ErrNotFound, got %v", err)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("respond publishes nothing: %+v", extra)
	}
}

func TestUnknownOpErrorMapsToNotFound(t *testing.T) {
	f, e, _ := setup(t)
	f.failWith(opSetMode, "unknown op: set_mode")
	if err := e.SetMode("s1", "plan"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("an unknown id or op must map to ErrNotFound, got %v", err)
	}
}

func TestNoticeOnDisconnectAndReconnect(t *testing.T) {
	f, e, ch := setup(t)
	f.crash()
	ev := wantEvent(t, ch, evNotice)
	if ev.Text != "tui disconnected (pid 111)" {
		t.Fatalf("notice text: %q", ev.Text)
	}
	if extra := drain(ch); len(extra) != 1 || extra[0].Type != evSessions {
		t.Errorf("a disconnect also means the list changed: %+v", extra)
	}
	waitFor(t, "the engine to drop the dead connection", func() bool { return e.connCount() == 0 })
	if got, err := e.List(); err != nil || len(got) != 0 {
		t.Fatalf("a dead tui lists nothing: %+v %v", got, err)
	}
	// The stale socket file stays, and the scan loop retries it, so a TUI that
	// comes back on the same path is re-attached without a gateway restart.
	f.restart()
	waitFor(t, "the plugin to be re-attached", func() bool { return e.connCount() == 1 })
	if got, err := e.List(); err != nil || len(got) != 1 {
		t.Fatalf("after reconnect: %+v %v", got, err)
	}
	if n := strings.Count(strings.Join(f.opsSeen(), ","), opHello); n < 2 {
		t.Errorf("expected a second hello after reconnect, saw %d", n)
	}
}

func TestOversizedFrameDropsConnection(t *testing.T) {
	dir := t.TempDir()
	tt := fastTimings()
	tt.maxLine = 128
	e, ch := startEngine(t, dir, tt)
	f := newFakeTUI(t, dir, "pid-7.sock", 7)
	waitFor(t, "the plugin socket to be tracked", func() bool { return e.connCount() == 1 })
	wantEvent(t, ch, evSessions)

	f.writeRaw(`{"event":"tool_done","session":"s1","tool":"bash","junk":"` +
		strings.Repeat("x", 4096) + `"}}` + "\n")
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected (pid 7)") {
		t.Fatalf("an oversize frame must end the connection with a notice: %+v", ev)
	}
	// The next scan reconnects, so one bad frame is a hiccup, not a loss.
	waitFor(t, "a reconnect after the oversized frame", func() bool {
		return strings.Count(strings.Join(f.opsSeen(), ","), opHello) >= 2
	})
	if got, err := e.List(); err != nil || len(got) != 1 {
		t.Fatalf("after reconnect: %+v %v", got, err)
	}
}

func TestPingTimeoutDropsConnection(t *testing.T) {
	dir := t.TempDir()
	tt := fastTimings()
	tt.timeout = 20 * time.Millisecond // a frozen ping must be judged quickly
	e, ch := startEngine(t, dir, tt)
	f := newFakeTUI(t, dir, "pid-9.sock", 9)
	waitFor(t, "the plugin socket to be tracked", func() bool { return e.connCount() == 1 })
	wantEvent(t, ch, evSessions)

	f.setFrozen(true)
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected (pid 9)") {
		t.Fatalf("two missed pings must drop the tui: %+v", ev)
	}
	waitFor(t, "the stalled connection to be gone", func() bool { return e.connCount() == 0 })
	// The plugin is wedged, so a reconnect is refused until it answers again.
	if n := strings.Count(strings.Join(f.opsSeen(), ","), opPing); n < 2 {
		t.Errorf("two pings must be sent before the drop, saw %d", n)
	}
}

func TestRouteFromEventsAndFanOutFallback(t *testing.T) {
	dir := t.TempDir()
	e, _ := startEngine(t, dir, fastTimings())
	a := newFakeTUI(t, dir, "pid-1.sock", 1, liveTab("s1", model.StatusIdle))
	b := newFakeTUI(t, dir, "pid-2.sock", 2, liveTab("s2", model.StatusIdle))
	waitFor(t, "both plugins", func() bool { return e.connCount() == 2 })

	// s3 is on neither list: the event names it, so the route follows the event.
	b.push(map[string]any{"event": evStatus, "session": "s3", "status": model.StatusWorking})
	waitFor(t, "the event route", func() bool {
		c, ok := e.owner("s3")
		return ok && c.path == b.path
	})
	// A session no route claims falls back to asking everyone: the first
	// connection refuses, the second has it.
	a.failWith(opMessages, "session not live: s9")
	b.setTabs(liveTab("s2", model.StatusIdle), liveTab("s9", model.StatusIdle))
	got, err := e.Messages("s9", 5)
	if err != nil {
		t.Fatalf("fan-out must find the session: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("messages from the fallback: %d", len(got))
	}
	if a.frameWithOp(opMessages) == nil {
		t.Error("the fallback must ask every connection")
	}
}

func TestStaleRouteIsReResolved(t *testing.T) {
	f, e, _ := setup(t)
	if _, err := e.Messages("s1", 5); err != nil {
		t.Fatal(err)
	}
	// The tab closes in the TUI while the gateway still routes it there.
	f.setTabs(liveTab("other", model.StatusIdle))
	if _, err := e.Messages("s1", 5); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("want ErrNotFound after the tab closed, got %v", err)
	}
	if _, ok := e.owner("s1"); ok {
		t.Error("a route the plugin refused must be dropped")
	}
}

func TestScanOnDemandSkipsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	ticking, _ := startEngine(t, dir, fastTimings())
	tt := fastTimings()
	tt.scanEvery = time.Hour // only ScanNow may attach on this engine
	manual, _ := startEngine(t, dir, tt)
	if got, err := manual.List(); err != nil || len(got) != 0 {
		t.Fatalf("nothing to attach yet: %+v %v", got, err)
	}
	// A regular file with a socket name must be skipped, not dialed.
	if err := os.WriteFile(filepath.Join(dir, "pid-5.sock"), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	manual.ScanNow()
	if n := manual.connCount(); n != 0 {
		t.Fatalf("a regular file named *.sock must not become a connection: %d", n)
	}
	newFakeTUI(t, dir, "pid-6.sock", 6)
	manual.ScanNow()
	if n := manual.connCount(); n != 1 {
		t.Fatalf("ScanNow must attach the new socket: %d", n)
	}
	waitFor(t, "the ticking engine to attach too", func() bool {
		return ticking.connCount() == 1
	})
}

func TestCloseIsQuietAndIdempotent(t *testing.T) {
	f, e, ch := setup(t)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a gateway shutdown publishes nothing: %+v", extra)
	}
	if got, err := e.List(); err != nil || len(got) != 0 {
		t.Errorf("a closed engine has no sessions: %+v %v", got, err)
	}
	f.crash()
	time.Sleep(50 * time.Millisecond)
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a closed engine stays silent even when a plugin dies: %+v", extra)
	}
}

// TestReplyWithoutOkOrErrorFailsAndDropsPlugin pins the framing rule of
// CONTRACTS 1: exactly one of ok|error. A bare {"id":N} is a plugin bug, so the
// caller must see a fault rather than a success with no payload, the connection
// must end, and the refusal must be visible in the log.
func TestReplyWithoutOkOrErrorFailsAndDropsPlugin(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	f, e, ch := setup(t)
	f.replyBare(opSetTitle)
	err := e.SetTitle("s1", "renamed")
	if err == nil {
		t.Fatal("a reply with neither ok nor error must fail the request")
	}
	if !errors.Is(err, errProtocol) || !strings.Contains(err.Error(), "protocol") {
		t.Errorf("want a protocol fault, got %v", err)
	}
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected") {
		t.Errorf("the framing fault must end the connection: %+v", ev)
	}
	if !strings.Contains(logs.String(), "reply without ok or error") {
		t.Errorf("want a warning naming the framing fault, got %q", logs.String())
	}
}

func TestHelloRefusalIsWarned(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	dir := t.TempDir()
	f := newFakeTUI(t, dir, "pid-3.sock", 3)
	f.failWith(opHello, "plugin is not ready")
	e, _ := startEngine(t, dir, fastTimings())
	// The plugin records a request before it answers, so waiting on the frame lets
	// this read the log before the refusal could land in it. The warning is the
	// fact under test, so the wait goes on that.
	waitFor(t, "a warning about the refused hello", func() bool {
		return strings.Contains(logs.String(), "hello refused")
	})
	if n := e.connCount(); n != 0 {
		t.Errorf("a plugin that refuses hello is not tracked: %d conns", n)
	}
	if !slices.Contains(f.opsSeen(), opHello) {
		t.Error("hello never reached the plugin")
	}
}

// connCount is how many plugin sockets the engine holds. Test-side only: the
// merged session list is the production answer to the same question.
func (e *Engine) connCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.conns)
}
