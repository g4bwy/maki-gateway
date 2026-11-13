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

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

// setup starts one engine over a fresh socket dir with one plugin on pid 111.
func setup(t *testing.T) (*fakeTUI, *Engine, chan model.Event) {
	t.Helper()
	return setupWith(t, fastTimings())
}

// setupWith is setup over one set of timings, for a test that has to keep one call
// open on the socket while it does something else.
func setupWith(t *testing.T, tt timings) (*fakeTUI, *Engine, chan model.Event) {
	t.Helper()
	dir := t.TempDir()
	e, ch := startEngine(t, dir, tt)
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

// routeSession teaches the engine which connection owns one session the way a list
// pass does, so a test of a call that needs the route does not race that poll.
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

// connCount is how many plugin sockets the engine holds. Test-side only: the
// merged session list is the production answer to the same question.
func (e *Engine) connCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.conns)
}

// v2ClientMethods is the gateway-to-plugin half of the method index of WIRE 0,
// plus session/new, which a plugin answers with -32601 (Appendix B.1). No name
// outside it may ever leave this engine: the v1 ops are the reason two builds had
// to be upgraded at the same instant.
var v2ClientMethods = []string{
	mInitialize, mSessionList, mSessionNew, mSessionCancel, mSessionSetMode,
	mSessionSetTitle, mMessages, mQueue, mNotify, mPlan, mTasks, mTaskFocus,
	mRunCommand, mPopQueue, mSessionLoad, "session/resume", "session/prompt",
}

func TestKindIsLive(t *testing.T) {
	e, _ := startEngine(t, t.TempDir(), fastTimings())
	if e.Kind() != model.KindLive {
		t.Fatalf("kind: %s", e.Kind())
	}
}

// TestInitializeThenListMapsToLiveSessions is the handshake and the row mapping of
// WIRE 2 and 3: initialize opens the connection, and one session/list row becomes
// one live session with the maki facts read out of _meta and the clock read out of
// the one timestamp format v2 writes.
func TestInitializeThenListMapsToLiveSessions(t *testing.T) {
	f, e, _ := setup(t)
	f.setTabs(
		liveTab("s1", model.StatusWorking),
		map[string]any{argSessionID: "s2", argCwd: "/other", argTitle: "",
			keyMeta: map[string]any{metaStatus: model.StatusNeedsInput, metaModel: "glm",
				metaCwd: "/other", metaMode: "plan", metaFocused: false}},
	)
	got, err := e.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tabs: %+v", got)
	}
	want := model.Session{ID: "s1", Kind: model.KindLive, Title: "tab s1",
		Status: model.StatusWorking, Cwd: "/work", Model: "m-s1",
		UpdatedAt: tabClockEpoch, PluginPID: 111}
	if got[0] != want {
		t.Fatalf("list mapping:\n got %+v\nwant %+v", got[0], want)
	}
	if got[1].ID != "s2" || got[1].Status != model.StatusNeedsInput || got[1].Title != "" ||
		got[1].Cwd != "/other" || got[1].Model != "glm" || got[1].PluginPID != 111 {
		t.Errorf("second tab: %+v", got[1])
	}
	// WIRE 3: a plugin that does not know the last activity of a tab omits the
	// clock, and the gateway reads an absent value as 0.
	if got[1].UpdatedAt != 0 {
		t.Errorf("a row with no updatedAt must read 0, got %d", got[1].UpdatedAt)
	}
	// initialize is the first request on a fresh connection, and it declares the
	// ACP version with no client capabilities and no _meta of its own (WIRE 2.1).
	methods := f.methodsSeen()
	if len(methods) == 0 || methods[0] != mInitialize {
		t.Fatalf("initialize must open the conversation: %v", methods)
	}
	fr := f.frameWithMethod(mInitialize)
	if fr[argProtocolVersion] != float64(acpProtocol) {
		t.Errorf("initialize frame: %v", fr)
	}
	caps, present := fr[argClientCapabilities].(map[string]any)
	if !present || len(caps) != 0 {
		t.Errorf("clientCapabilities must travel as an empty object: %#v", fr[argClientCapabilities])
	}
	if _, has := fr[keyMeta]; has {
		t.Errorf("the gateway declares no maki datum of its own: %v", fr)
	}
	// A method that takes no argument is sent with an empty params object
	// (WIRE 1.3), which is also how the gateway spells session/list.
	lists := f.framesOf(mSessionList)
	if len(lists) == 0 || string(lists[0].rawParams) != `{}` {
		t.Errorf("session/list params = %s, want {}", firstRaw(lists))
	}
}

func firstRaw(rs []request) json.RawMessage {
	if len(rs) == 0 {
		return nil
	}
	return rs[0].rawParams
}

// TestListReadsTheACPFieldsAndBothMetaSpellings pins the reader rules of WIRE 2.3
// and 3.1: sh.maki/cwd first and cwd as the fallback, a boolean that arrives as
// the text "true", and no failure over a missing maki key.
func TestListReadsTheACPFieldsAndBothMetaSpellings(t *testing.T) {
	f, e, _ := setup(t)
	f.setTabs(map[string]any{
		argSessionID: "s1", argCwd: "/acp-only", argTitle: "plain",
		keyClock: tabClock,
		keyMeta:  map[string]any{metaStatus: model.StatusIdle, metaFocused: "true"},
	})
	got, err := e.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: %+v", got)
	}
	if got[0].Cwd != "/acp-only" {
		t.Errorf("cwd must fall back to the ACP field: %+v", got[0])
	}
	if got[0].Model != "" || got[0].Status != model.StatusIdle {
		t.Errorf("a missing maki key is not a fault: %+v", got[0])
	}
	if got[0].UpdatedAt != tabClockEpoch {
		t.Errorf("clock: %+v", got[0])
	}
}

// TestVersionGateRefusesAnOldPlugin is the ruling that kills the silent-empty
// failure class: an agent that does not declare attach_proto 2 is refused in one
// error log line and one notice, and its tabs never reach the list (WIRE 2.2).
func TestVersionGateRefusesAnOldPlugin(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	dir := t.TempDir()
	f := newFakeTUI(t, dir, "pid-111.sock", 111)
	f.setProto(float64(1))
	e, ch := startEngine(t, dir, fastTimings())

	ev := wantEvent(t, ch, evNotice)
	if !strings.Contains(ev.Text, "pid 111") || !strings.Contains(ev.Text, "too old") ||
		!strings.Contains(ev.Text, "/packupdate maki-gateway") {
		t.Errorf("the notice must name the pid and the fix: %+v", ev)
	}
	line := logs.String()
	if !strings.Contains(line, "level=ERROR") {
		t.Errorf("the refusal is an error, got %q", line)
	}
	if !strings.Contains(line, tooOld) {
		t.Errorf("want the log to name %q, got %q", tooOld, line)
	}
	for _, want := range []string{"pid=111", "attach_proto=1", filepath.Join(dir, "pid-111.sock")} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal must report %q, got %q", want, line)
		}
	}
	waitFor(t, "the refused plugin to be gone", func() bool { return e.connCount() == 0 })
	if got, err := e.List(); err != nil || len(got) != 0 {
		t.Errorf("a refused plugin has no rows in the list: %+v %v", got, err)
	}
	// The refusal sends nothing else on that socket.
	if methods := f.methodsSeen(); !slices.Equal(methods, []string{mInitialize}) {
		t.Errorf("a refused connection gets no further request, saw %v", methods)
	}
}

// TestVersionGateRefusesAMissingProto: a missing key means v1 or older, exactly as
// a number below 2 does (WIRE 2.2).
func TestVersionGateRefusesAMissingProto(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	dir := t.TempDir()
	f := newFakeTUI(t, dir, "pid-5.sock", 5)
	f.setProto(nil)
	e, ch := startEngine(t, dir, fastTimings())
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "pid 5") {
		t.Errorf("notice: %+v", ev)
	}
	waitFor(t, "attach_proto=none in the log", func() bool {
		return strings.Contains(logs.String(), "attach_proto=none")
	})
	if n := e.connCount(); n != 0 {
		t.Errorf("a plugin that declares no attach_proto is not tracked: %d", n)
	}
}

// TestVersionGateAcceptsASecondOrLaterBuild: the gate is "2 or more", and a reader
// takes the number from a Lua encoder that spelled it as text (WIRE 2.3).
func TestVersionGateAcceptsASecondOrLaterBuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto any
	}{
		{"proto 2", float64(2)},
		{"proto 3", float64(3)},
		{"proto as text", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f := newFakeTUI(t, dir, "pid-9.sock", 9)
			f.setProto(tc.proto)
			e, _ := startEngine(t, dir, fastTimings())
			waitFor(t, "the plugin to be tracked", func() bool { return e.connCount() == 1 })
			got, err := e.List()
			if err != nil || len(got) != 1 {
				t.Fatalf("an accepted plugin lists its tabs: %+v %v", got, err)
			}
		})
	}
}

func TestMessagesPassthrough(t *testing.T) {
	f, e, _ := setup(t)
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
	// WIRE 6: the method names the tab and how many records to read from the end.
	fr := f.frameWithMethod(mMessages)
	if fr[argSessionID] != "s1" || fr[argLast] != float64(200) {
		t.Errorf("messages frame: %v", fr)
	}
	if _, err := e.Messages("nope", 10); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown session: want ErrNotFound, got %v", err)
	}
}

// TestPromptReturnsStateAndMapsErrors: the HTTP contract is frozen, so a prompt
// answers with the queue state of WIRE 7.2 at once, and the error words of WIRE
// 13.1 keep mapping to the same sentinels they did in v1.
func TestPromptReturnsStateAndMapsErrors(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	state, err := e.Prompt("s1", "do the thing")
	if err != nil || state != "started" {
		t.Fatalf("prompt: %q %v", state, err)
	}
	if fr := f.frameWithMethod(mQueue); fr[argText] != "do the thing" || fr[argSessionID] != "s1" {
		t.Errorf("queue frame: %v", fr)
	}
	f.answerQueueWith("queued")
	if state, err = e.Prompt("s1", "more"); err != nil || state != "queued" {
		t.Fatalf("queued prompt: %q %v", state, err)
	}
	if _, err := e.Prompt("gone", "x"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("session not live must map to ErrNotFound, got %v", err)
	}
	f.failWith(mQueue, "open in another window")
	if _, err := e.Prompt("s1", "x"); !errors.Is(err, model.ErrOpenElsewhere) {
		t.Errorf("open elsewhere: got %v", err)
	}
	f.failCode(mQueue, acpwire.CodeInternal, "session is busy: a prompt is already running")
	if _, err := e.Prompt("s1", "x"); !errors.Is(err, model.ErrBusy) {
		t.Errorf("a busy tab: got %v", err)
	}
}

func TestSetModeAndSetTitle(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	if err := e.SetMode("s1", "plan"); err != nil {
		t.Fatal(err)
	}
	fr := f.frameWithMethod(mSessionSetMode)
	if fr[argSessionID] != "s1" || fr[argModeID] != "plan" {
		t.Errorf("set_mode frame: %v, want sessionId and modeId", fr)
	}
	if _, has := fr["mode"]; has {
		t.Errorf("v1 called the argument mode, v2 calls it modeId: %v", fr)
	}
	if err := e.SetTitle("s1", "new name"); err != nil {
		t.Fatal(err)
	}
	if fr = f.frameWithMethod(mSessionSetTitle); fr[argTitle] != "new name" {
		t.Errorf("set_title frame: %v", fr)
	}
	// WIRE 8.2: one spelling for a clear. v1's null leaves with the JSON value
	// that had no empty form to echo.
	if err := e.SetTitle("s1", ""); err != nil {
		t.Fatal(err)
	}
	fr = f.frameWithMethod(mSessionSetTitle)
	title, present := fr[argTitle]
	if !present || title != "" {
		t.Errorf("clearing a title must send the empty string, got %#v in %v", title, fr)
	}
	// A mode the host does not know keeps the plugin's own words (WIRE 8.1), and
	// those words are not in the mapping table, so they reach the caller whole.
	err := e.SetMode("s1", "turbo")
	if err == nil || !strings.Contains(err.Error(), `mode must be "build" or "plan"`) {
		t.Errorf("bad mode: %v", err)
	}
	if errors.Is(err, model.ErrNotFound) || errors.Is(err, model.ErrBusy) {
		t.Errorf("a bad mode is not a missing tab: %v", err)
	}
}

// TestCreateCannotOpenATab is WIRE Appendix B.1: no session/new exists on this
// socket, a plugin answers it with -32601, and a caller that must create a session
// spawns maki acp. The request still goes to the lowest socket alone, and v2 has
// no argument for a permission answer, so no yolo field travels.
func TestCreateCannotOpenATab(t *testing.T) {
	dir := t.TempDir()
	e, _ := startEngine(t, dir, fastTimings())
	if _, err := e.Create(model.CreateRequest{Cwd: "/work"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("no tui must map to ErrNotFound, got %v", err)
	}
	first := newFakeTUI(t, dir, "pid-1.sock", 1)
	second := newFakeTUI(t, dir, "pid-2.sock", 2)
	waitFor(t, "both plugins to be tracked", func() bool { return e.connCount() == 2 })

	_, err := e.Create(model.CreateRequest{Cwd: "/work", Model: "model", Prompt: "start here", Yolo: true})
	if !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("a live tab cannot be opened over this socket, got %v", err)
	}
	if len(first.framesOf(mSessionNew)) != 1 {
		t.Errorf("the lowest socket path should be the one asked, saw %v", first.methodsSeen())
	}
	if len(second.framesOf(mSessionNew)) != 0 {
		t.Error("only one connection may be asked to open a tab")
	}
	fr := first.frameWithMethod(mSessionNew)
	if fr[argCwd] != "/work" {
		t.Errorf("session/new frame: %v", fr)
	}
	for _, gone := range []string{"yolo", keyPrompt, "model"} {
		if _, has := fr[gone]; has {
			t.Errorf("v2 sends no %q argument: %v", gone, fr)
		}
	}

	// The other refusal of WIRE 13 that means no tab: -32000, the session this TUI
	// cannot take. It is a thing no client can fix over this socket, so it reads
	// the same way as the method that is not there.
	first.failCode(mSessionNew, codeHeld, "this TUI opens tabs at the terminal")
	if _, err := e.Create(model.CreateRequest{Cwd: "/work"}); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("a refused claim: err = %v, want ErrUnsupported", err)
	}
}

// TestNoV1FrameEverLeavesTheGateway pins the rewire: none of the bespoke ops of
// CONTRACTS 1 may still go out, and every method that does is in the index of
// WIRE 0. The permission bridge needs no arming any more (WIRE 10.4), and the list
// poll is the only clock (WIRE 1.5).
func TestNoV1FrameEverLeavesTheGateway(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	if _, err := e.Messages("s1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Prompt("s1", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Tasks("s1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Notify("s1", "gateway: session saved"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Plan("s1"); err != nil {
		t.Fatal(err)
	}
	for _, method := range f.methodsSeen() {
		if !slices.Contains(v2ClientMethods, method) {
			t.Errorf("method %q is not in the index of WIRE 0", method)
		}
	}
	for _, v1 := range []string{"hello", "ping", "respond", "permissions", "new", "list", "messages"} {
		if slices.Contains(f.methodsSeen(), v1) {
			t.Errorf("the v1 op %q still leaves the gateway", v1)
		}
	}
	// The notice went to the tab it named (WIRE 9.1).
	fr := f.frameWithMethod(mNotify)
	if fr[argText] != "gateway: session saved" || fr[argSessionID] != "s1" {
		t.Errorf("notify frame: %v", fr)
	}
}

func TestPlanForwardsTheSocketAnswer(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	raw, err := e.Plan("s1")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got[keyMode] != "plan" || got[keyPath] != "/tmp/plan.md" || got[keyReady] != true ||
		got[keyContent] != "# plan" {
		t.Fatalf("plan passthrough: %s", raw)
	}
	if f.frameWithMethod(mPlan)[argSessionID] != "s1" {
		t.Errorf("plan frame: %v", f.frameWithMethod(mPlan))
	}
	if _, err := e.Plan("nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
}

// TestTasksForwardsTheBareArray: the rows reach the caller as maki wrote them, and
// the method names no session, because the host only lists the focused tab
// (WIRE 9.3). The {"tasks":...} wrapper the HTTP route answers is built upstream.
func TestTasksForwardsTheBareArray(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	raw, err := e.Tasks("s1")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("tasks passthrough: %s", raw)
	}
	if len(rows) != 2 || rows[0][keyRowID] != "main" || rows[0][keyStatus] != nil ||
		rows[1][keyStatus] != "working" || rows[1]["tool"] != "task" {
		t.Fatalf("tasks = %s", raw)
	}
	frames := f.framesOf(mTasks)
	if len(frames) == 0 {
		t.Fatalf("the tasks method never reached the plugin, saw %v", f.methodsSeen())
	}
	if string(frames[0].rawParams) != `{}` {
		t.Errorf("the tasks method takes no session argument: %s", frames[0].rawParams)
	}
	// No rows is still a list, not null.
	f.setTasks()
	if raw, err = e.Tasks("s1"); err != nil || string(raw) != "[]" {
		t.Fatalf("empty tasks = %s, %v", raw, err)
	}
	f.failWith(mTasks, "unknown session: s9")
	if _, err := e.Tasks("s1"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("plugin error mapping: %v", err)
	}
}

// TestFocusTaskCarriesTheTaskID pins the frame of one follow: the task rides in
// `taskId` and nowhere else, so the request keeps a number of its own and the
// plugin answers it like every other call. A follow is one call to the TUI that
// owns the session, and it never carries a session argument, because the host
// moves a chat of the tab the terminal already has focused (WIRE 9.4).
func TestFocusTaskCarriesTheTaskID(t *testing.T) {
	f, e, ch := setup(t)
	routeSession(t, e, "s1")
	drain(ch)
	if err := e.FocusTask("s1", "tu1"); err != nil {
		t.Fatalf("FocusTask: %v", err)
	}
	fr := f.frameWithMethod(mTaskFocus)
	if fr == nil {
		t.Fatalf("task_focus never reached the plugin, saw %v", f.methodsSeen())
	}
	if fr[argTaskID] != "tu1" {
		t.Errorf("frame = %v, want the task in taskId", fr)
	}
	if _, has := fr["task_id"]; has {
		t.Errorf("the v1 snake_case name is gone: %v", fr)
	}
	if _, has := fr[argSessionID]; has {
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
	// No frame on this socket may spend its request id on a number: the plugin is
	// free to mint the same number for its own ask, and the answer to that ask
	// would then be read as the answer to this call (WIRE 1.4).
	if bad := f.framesWithForeignID(); len(bad) != 0 {
		t.Errorf("%d request frames arrived with an id this gateway does not mint: %v", len(bad), bad)
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

	f.refuseFollow()
	err = e.FocusTask("s1", "tu1")
	if !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("no terminal: err = %v, want ErrUnsupported", err)
	}
	if err.Error() != refuseUI {
		t.Errorf("refusal = %q, want the plugin's words unchanged", err.Error())
	}

	// A session no connection claims has no screen to move, and the call is never
	// fanned out: another TUI's tab is not the one the browser is looking at.
	if err := e.FocusTask("gone", "tu1"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unrouted session: %v", err)
	}
	if n := len(f.framesOf(mTaskFocus)); n != 2 {
		t.Errorf("the plugin was asked to follow %d times, want the two live calls only", n)
	}
}

// TestFocusCallsAnswerInOrder drives the same connection from several browsers at
// once. Each follow carries its own request number, so the answers cannot be mixed
// up, and one slow or refused call cannot make another caller wait.
func TestFocusCallsAnswerInOrder(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	f.setTasks(
		map[string]any{keyRowID: "main", keyTaskSession: "s1", keyName: "main", keyFocused: true},
		map[string]any{keyRowID: "t-a", keyTaskSession: "s1", keyName: "a", keyStatus: "done"},
		map[string]any{keyRowID: "t-b", keyTaskSession: "s1", keyName: "b", keyStatus: "done"},
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
	frames := f.framesOf(mTaskFocus)
	if len(frames) != 5 {
		t.Fatalf("saw %d task_focus frames, want one per call", len(frames))
	}
	ids := map[string]string{}
	for _, fr := range frames {
		if !isGatewayID(fr.id) {
			t.Errorf("follow of %s carried an id this gateway does not mint: %s",
				strParam(fr.params, argTaskID), fr.id)
			continue
		}
		task := strParam(fr.params, argTaskID)
		if twice, dup := ids[string(fr.id)]; dup {
			t.Errorf("request id %s was used by the follow of %s and of %s", fr.id, twice, task)
		}
		ids[string(fr.id)] = task
	}
	if len(ids) != 5 {
		t.Errorf("%d distinct request ids for 5 follows, want one each", len(ids))
	}
}

// TestTaskFocusEventCarriesTheTask is the other direction: the person at the
// terminal moved the followed chat. v1 carried that name in the frame's `id`
// field; v2 names it taskId inside params, and the hub event keeps the shape the
// browser already reads (WIRE 11.2).
func TestTaskFocusEventCarriesTheTask(t *testing.T) {
	f, _, ch := setup(t)
	f.pushEvent(map[string]any{keyKind: kindTaskFocus, argSessionID: "s1", argTaskID: "tu1"})
	ev := wantEvent(t, ch, evTaskFocus)
	if ev.Session != "s1" || ev.TaskID != "tu1" {
		t.Errorf("task_focus event = %+v, want session s1 and task tu1", ev)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a follow change publishes task_focus only: %+v", extra)
	}
}

func TestTasksChangedEventMapsToTasks(t *testing.T) {
	f, _, ch := setup(t)
	f.pushEvent(map[string]any{keyKind: kindTasksChanged, argSessionID: "s1"})
	ev := wantEvent(t, ch, evTasks)
	if ev.Type != evTasks || ev.Session != "s1" {
		t.Errorf("tasks event: %+v", ev)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a task change publishes tasks only: %+v", extra)
	}
}

// TestStatusEventCarriesItsFields is the row of WIRE 11.2 that patches a browser
// row in place: the fields the event carries, and no others.
func TestStatusEventCarriesItsFields(t *testing.T) {
	f, _, ch := setup(t)
	f.pushEvent(map[string]any{keyKind: kindStatus, argSessionID: "s1", keyStatus: model.StatusNeedsInput,
		argTitle: "tab s1", keyFocused: true})
	ev := wantEvent(t, ch, evStatus)
	if ev.Session != "s1" || ev.Status != model.StatusNeedsInput || ev.Title != "tab s1" ||
		ev.Focused == nil || !*ev.Focused {
		t.Errorf("status event: %+v", ev)
	}
	// WIRE 11.2 pairs the status change with a dirty of reason status, because v1
	// pushed turn_start beside it: a tab that starts or stops working changed its
	// transcript, and the browser refetches it.
	dirty := wantEvent(t, ch, evDirty)
	if dirty.Session != "s1" || dirty.Reason != reasonStatus {
		t.Errorf("a status change also means refetch the tab: %+v", dirty)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("one status change publishes two events: %+v", extra)
	}

	f.pushEvent(map[string]any{keyKind: kindStatus, argSessionID: "s1", keyStatus: model.StatusIdle})
	if ev = wantEvent(t, ch, evStatus); ev.Title != "" || ev.Focused != nil {
		t.Errorf("a status event with no title or focus must not invent one: %+v", ev)
	}
}

func TestTitleAndModelEventsAreStatusEvents(t *testing.T) {
	f, _, ch := setup(t)
	f.pushEvent(map[string]any{keyKind: kindTitle, argSessionID: "s1", argTitle: "renamed"})
	if ev := wantEvent(t, ch, evStatus); ev.Title != "renamed" || ev.Status != "" {
		t.Errorf("a title change is a status frame with the title: %+v", ev)
	}
	f.pushEvent(map[string]any{keyKind: kindModel, argSessionID: "s1", metaModel: "glm-4.6"})
	if ev := wantEvent(t, ch, evStatus); ev.Session != "s1" || ev.Title != "" {
		t.Errorf("a model change is a plain status frame: %+v", ev)
	}
	f.pushEvent(map[string]any{keyKind: kindQuestion, argSessionID: "s1"})
	if ev := wantEvent(t, ch, evNotice); ev.Text != questionNotice || ev.Session != "" {
		t.Errorf("question notice: %+v", ev)
	}
}

// TestUnknownEventKindIsIgnored is WIRE 12.3: a new value in a field the reader
// knows is not a protocol error, and the connection stays.
func TestUnknownEventKindIsIgnored(t *testing.T) {
	f, e, ch := setup(t)
	f.pushEvent(map[string]any{"kind": "summoned_a_dragon", argSessionID: "s1"})
	f.push("sh.maki/dream_of_the_past", map[string]any{"x": 1})
	f.pushUpdate("s1", map[string]any{keySessionUpd: "a_update_nobody_has_named"})
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("an unknown kind, method, or update publishes nothing: %+v", extra)
	}
	if _, err := e.List(); err != nil || e.connCount() != 1 {
		t.Errorf("the connection must survive them: %v %d", err, e.connCount())
	}
}

// TestToolUpdatesMapToDirty is WIRE 11.1: the tool stream of a live tab derives
// the two dirty reasons the bridge has always published. No tool payload joins the
// SSE surface: the browser refetches the transcript, which is what rule 2 says.
func TestToolUpdatesMapToDirty(t *testing.T) {
	f, _, ch := setup(t)
	f.pushUpdate("s1", map[string]any{keySessionUpd: upToolCall, keyToolCallID: "tc1",
		keyName: "bash", argTitle: "bash: ls -1", keyStatus: statusPending})
	f.pushUpdate("s1", map[string]any{keySessionUpd: upToolUpdate, keyToolCallID: "tc1",
		keyStatus: statusInProgress, keyMeta: map[string]any{metaBytes: 212}})
	f.pushUpdate("s1", map[string]any{keySessionUpd: upToolUpdate, keyToolCallID: "tc1",
		keyStatus: statusCompleted, keyMeta: map[string]any{metaDuration: 40}})
	// A terminal update for a card this connection never saw open is still a valid
	// frame (rule 1), and the transcript changed all the same.
	f.pushUpdate("s1", map[string]any{keySessionUpd: upToolUpdate, keyToolCallID: "tc2",
		keyStatus: statusFailed})
	// A non-terminal update for a card never opened reads as an out-of-order start.
	f.pushUpdate("s2", map[string]any{keySessionUpd: upToolUpdate, keyToolCallID: "tc7",
		keyStatus: statusInProgress})
	for _, want := range [][2]string{{"s1", reasonToolStart}, {"s1", reasonToolDone},
		{"s1", reasonToolDone}, {"s2", reasonToolStart}} {
		ev := wantEvent(t, ch, evDirty)
		if ev.Session != want[0] || ev.Reason != want[1] {
			t.Errorf("dirty event: got %+v, want session %s reason %s", ev, want[0], want[1])
		}
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a tool change publishes dirty only: %+v", extra)
	}
	// WIRE 11.1 rule 3: a streamed turn closes with usage_update, and that frame
	// carries the refetch nudge v1 called turn_end. The numbers have no home here.
	f.pushUpdate("s1", map[string]any{keySessionUpd: upUsage, keyUsed: 53000, keySize: 200000})
	if ev := wantEvent(t, ch, evDirty); ev.Session != "s1" || ev.Reason != reasonTurnEnd {
		t.Errorf("the end of a turn: got %+v, want reason %s", ev, reasonTurnEnd)
	}
	// Streamed text has no home on the frozen SSE surface.
	f.pushUpdate("s1", map[string]any{keySessionUpd: upMessageChunk,
		keyContentBlock: map[string]any{keyType: "text", argText: "streamed text never joins the SSE surface"}})
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("streamed text publishes nothing yet: %+v", extra)
	}
}

// TestReplayFramesDeriveDirtyOnly is the history stream of WIRE 4 seen by the same
// reducer: the tool cards change the transcript, the chunks do not reach the
// frozen SSE surface.
func TestReplayFramesDeriveDirtyOnly(t *testing.T) {
	f, _, ch := setup(t)
	f.pushReplay("s1")
	for _, reason := range []string{reasonToolStart, reasonToolDone} {
		ev := wantEvent(t, ch, evDirty)
		if ev.Session != "s1" || ev.Reason != reason {
			t.Errorf("replay dirty: got %+v, want reason %s", ev, reason)
		}
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a replay publishes dirty only: %+v", extra)
	}
}

// TestRespondPermissionAnswersTheParkedAsk is the whole bridge of WIRE 10: the
// plugin asks, the gateway publishes a dialog under its own counter, and the
// browser's answer goes back as the ACP outcome, with the guidance in the _meta of
// the result (ruling 6).
func TestRespondPermissionAnswersTheParkedAsk(t *testing.T) {
	f, e, ch := setup(t)
	routeSession(t, e, "s1")
	drain(ch)
	wireID := f.askPermission("s1", "bash: rm -rf /tmp/x", map[string]any{"command": "rm -rf /tmp/x"})

	ev := wantEvent(t, ch, evPermission)
	if ev.RequestID == 0 || ev.Session != "s1" || ev.Engine != model.EngineAttach || ev.Type != evPermission {
		t.Fatalf("permission event: %+v", ev)
	}
	if ev.Tool != "bash: rm -rf /tmp/x" {
		t.Errorf("the dialog shows the line the terminal shows: %+v", ev)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Input, &got); err != nil || got["command"] != "rm -rf /tmp/x" {
		t.Fatalf("input must pass through raw: %s", ev.Input)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("an ask publishes permission only: %+v", extra)
	}

	if err := e.RespondPermission(ev.RequestID, model.DecisionDeny, "use a scratch dir"); err != nil {
		t.Fatal(err)
	}
	answered := f.waitForAnswer(t, wireID)
	if answered.Outcome.Outcome != outcomeSelected || answered.Outcome.OptionID != optionReject {
		t.Fatalf("ask answered with %+v, want reject_once", answered)
	}
	if answered.Meta[metaGuidance] != "use a scratch dir" {
		t.Errorf("guidance must land in the _meta of the result: %#v", answered.Meta)
	}
	// The dialog is gone, so a second answer reads as the withdrawal of a prompt
	// this side no longer holds (WIRE 13.1).
	err := e.RespondPermission(ev.RequestID, model.DecisionAllow, "")
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("answered twice: want ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), expired) {
		t.Errorf("want the word the contract writes, got %v", err)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("respond publishes nothing: %+v", extra)
	}
}

// TestRespondAllowAlwaysAnswersAllowOnce pins the degradation of WIRE 10.1: the
// host slot has nothing else to give, so "always" arrives as allow_once and the
// live tab asks again on the next call.
func TestRespondAllowAlwaysAnswersAllowOnce(t *testing.T) {
	f, e, ch := setup(t)
	wireID := f.askPermission("s1", "bash: ls", map[string]any{"command": "ls"})
	ev := wantEvent(t, ch, evPermission)
	if err := e.RespondPermission(ev.RequestID, model.DecisionAllowAlways, "remember it"); err != nil {
		t.Fatal(err)
	}
	answered := f.waitForAnswer(t, wireID)
	if answered.Outcome.OptionID != optionAllowOnce {
		t.Fatalf("answered with %+v, want %q", answered.Outcome, optionAllowOnce)
	}
	if answered.Meta[metaGuidance] != "remember it" {
		t.Errorf("guidance travels with an allow too: %#v", answered.Meta)
	}
}

// TestAskWithNoGuidanceCarriesNoMeta: a deny without words is all the terminal
// prompt gives, so the key stays away rather than arrive empty.
func TestAskWithNoGuidanceCarriesNoMeta(t *testing.T) {
	f, e, ch := setup(t)
	wireID := f.askPermission("s1", "bash: ls", nil)
	ev := wantEvent(t, ch, evPermission)
	if err := e.RespondPermission(ev.RequestID, model.DecisionAllow, ""); err != nil {
		t.Fatal(err)
	}
	if answered := f.waitForAnswer(t, wireID); answered.Meta != nil {
		t.Errorf("a bare allow must not write _meta: %#v", answered.Meta)
	}
}

// TestWithdrawnAskClosesTheDialog is the 55 second fallthrough of WIRE 10.2. The
// plugin ends its own ask in a frame that answers the request it sent, which the
// codec drops by rule (WIRE 1.4), so this is the one inbound frame the bridge
// reads off the stream itself. The browser must see permission_cancel with the id
// the gateway published, and the ask must be gone.
func TestWithdrawnAskClosesTheDialog(t *testing.T) {
	f, e, ch := setup(t)
	wireID := f.askPermission("s1", "bash: ls", map[string]any{"command": "ls"})
	ev := wantEvent(t, ch, evPermission)

	f.withdraw(wireID)
	cancelled := wantEvent(t, ch, evPermissionCancel)
	if cancelled.RequestID != ev.RequestID {
		t.Fatalf("permission_cancel = %+v, want the id of the ask (%d)", cancelled, ev.RequestID)
	}
	if err := e.RespondPermission(ev.RequestID, model.DecisionAllow, ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("answering a withdrawn ask: %v", err)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("a withdrawal publishes permission_cancel only: %+v", extra)
	}
	// The ask was never answered on the wire, so the plugin's own answer stands and
	// the connection is healthy for everything else.
	if _, err := e.List(); err != nil || e.connCount() != 1 {
		t.Errorf("a withdrawn ask must not cost the connection: %v %d", err, e.connCount())
	}
}

// TestWithdrawalCannotBeMisroutedIntoAnOpenCall is the 83 minute bug on the wire.
// The plugin numbered its own ask with a number this side had also spent, so its
// withdrawal answered an unrelated open call. WIRE 1.4 mints gateway ids as
// strings, so the two number spaces cannot meet: the withdrawal closes the dialog
// and the call keeps waiting for its own answer.
func TestWithdrawalCannotBeMisroutedIntoAnOpenCall(t *testing.T) {
	long := fastTimings()
	long.timeout = 3 * time.Second
	f, e, ch := setupWith(t, long)
	routeSession(t, e, "s1")
	f.holdOn(mPlan)

	type answer struct {
		raw json.RawMessage
		err error
	}
	held := make(chan answer, 1)
	go func() {
		raw, err := e.raw("s1", mPlan, noArgs)
		held <- answer{raw, err}
	}()
	waitFor(t, "the gateway plan call to reach the plugin", func() bool {
		return len(f.framesOf(mPlan)) > 0
	})
	calls := f.framesOf(mPlan)
	if len(calls) != 1 {
		t.Fatalf("saw %d plan frames, want the one open call", len(calls))
	}
	// The number this side spent on that call. A plugin that mints its asks from
	// 1000 can hand the same number back as its own request id.
	clash := idNumber(t, calls[0].id)

	f.askWithID(clash, "s1", "bash: ls")
	ev := wantEvent(t, ch, evPermission)
	f.answerOwnAsk(clash)
	cancelled := wantEvent(t, ch, evPermissionCancel)
	if cancelled.RequestID != ev.RequestID {
		t.Fatalf("permission_cancel = %+v, want the dialog of the ask (%d)", cancelled, ev.RequestID)
	}
	select {
	case got := <-held:
		t.Fatalf("the withdrawal answered the open call: %+v %v", got.raw, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	const planBody = `"# plan"`
	f.answerHeld(t, mPlan, map[string]any{keyMode: "plan", keyPath: "/tmp/plan.md",
		keyReady: true, keyContent: "# plan"})
	var got answer
	select {
	case got = <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("the open call never got its own answer")
	}
	if got.err != nil {
		t.Fatalf("plan: %v", got.err)
	}
	if !strings.Contains(string(got.raw), planBody) {
		t.Errorf("plan result = %s, want the plugin's own body", got.raw)
	}
	if err := e.RespondPermission(ev.RequestID, model.DecisionAllow, ""); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("the withdrawn dialog must be gone: %v", err)
	}
	if e.connCount() != 1 {
		t.Errorf("a stray answer must not cost the connection: %d conns", e.connCount())
	}
}

// TestAskAnsweredElsewhereIsClosedOnEveryConnection is WIRE 10.3: with two
// gateways, or two connections on one plugin, every connection gets its own dialog
// and each answer goes back on the socket that asked for it.
func TestAskPerConnectionHasItsOwnDialog(t *testing.T) {
	dir := t.TempDir()
	e, ch := startEngine(t, dir, fastTimings())
	a := newFakeTUI(t, dir, "pid-1.sock", 1)
	b := newFakeTUI(t, dir, "pid-2.sock", 2)
	waitFor(t, "both plugins", func() bool { return e.connCount() == 2 })
	routeSession(t, e, "s1")
	drain(ch)

	idA := a.askPermission("s1", "bash: a", nil)
	idB := b.askPermission("s1", "bash: b", nil)
	first := wantEvent(t, ch, evPermission)
	second := wantEvent(t, ch, evPermission)
	if first.RequestID == second.RequestID {
		t.Fatalf("two asks share one request id: %+v %+v", first, second)
	}
	byB, byA := second, first
	if second.Tool != "bash: b" {
		byB, byA = first, second
	}
	if err := e.RespondPermission(byB.RequestID, model.DecisionDeny, "not here"); err != nil {
		t.Fatal(err)
	}
	answered := b.waitForAnswer(t, idB)
	if answered.Outcome.OptionID != optionReject {
		t.Errorf("socket B saw %+v, want %q", answered.Outcome, optionReject)
	}
	if a.isAnswered(idA) {
		t.Error("an answer went out on the socket that was not asked")
	}
	// The dialog on the other socket is still parked, and answers on its own.
	if err := e.RespondPermission(byA.RequestID, model.DecisionAllow, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.waitForAnswer(t, idA); got.Outcome.OptionID != optionAllowOnce {
		t.Errorf("socket A saw %+v, want %q", got, optionAllowOnce)
	}
	if extra := drain(ch); len(extra) != 0 {
		t.Errorf("two dialogs publish two permission events: %+v", extra)
	}
}

// TestCancelIsUnsupported is ruling 4 and WIRE 7.3: session/cancel goes out as a
// request, and both answers a live tab can give, -32601 and silence, mean the same
// thing to the caller.
func TestCancelIsUnsupported(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	if err := e.Cancel("s1"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("a -32601 answer: %v", err)
	}
	if fr := f.frameWithMethod(mSessionCancel); fr[argSessionID] != "s1" {
		t.Errorf("cancel must go out as a request naming the tab: %v", fr)
	}
	f.hangOn(mSessionCancel)
	if err := e.Cancel("s1"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("no answer at all: %v", err)
	}
	// A tab no connection claims cannot be cancelled either, and nothing is asked.
	if err := e.Cancel("gone"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("unrouted cancel: %v", err)
	}
}

// TestKillHangsUpTheSocket: no HTTP route reaches Kill for a live row, and v2 has
// no method that stops a TUI, so what this side can do is close the socket, which
// reports the tab list as it now reads.
func TestKillHangsUpTheSocket(t *testing.T) {
	f, e, ch := setup(t)
	routeSession(t, e, "s1")
	if err := e.Kill("gone"); !errors.Is(err, model.ErrUnsupported) {
		t.Errorf("nothing to hang up: %v", err)
	}
	if err := e.Kill("s1"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected (pid 111)") {
		t.Errorf("a hang-up reports the disconnect: %+v", ev)
	}
	waitFor(t, "the socket to be gone", func() bool { return e.connCount() == 0 })
	waitFor(t, "the plugin to see its client hang up", func() bool { return f.connCount() == 0 })
}

func TestUnknownRequestIDMapsToExpired(t *testing.T) {
	_, e, _ := setup(t)
	err := e.RespondPermission(9999, model.DecisionAllow, "")
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("an id this side never parked: want ErrNotFound, got %v", err)
	}
	if err.Error() != "not found: permission 9999 expired" {
		t.Errorf("want the words of WIRE 13.1, got %q", err.Error())
	}
}

func TestEventRouteAndFanOutFallback(t *testing.T) {
	dir := t.TempDir()
	e, _ := startEngine(t, dir, fastTimings())
	a := newFakeTUI(t, dir, "pid-1.sock", 1, liveTab("s1", model.StatusIdle))
	b := newFakeTUI(t, dir, "pid-2.sock", 2, liveTab("s2", model.StatusIdle))
	waitFor(t, "both plugins", func() bool { return e.connCount() == 2 })

	// s3 is on neither list: the event names it, so the route follows the event.
	b.pushEvent(map[string]any{keyKind: kindStatus, argSessionID: "s3", keyStatus: model.StatusWorking})
	waitFor(t, "the event route", func() bool {
		c, ok := e.owner("s3")
		return ok && c.path == b.path
	})
	// A session no route claims falls back to asking everyone: the first refuses
	// with the -32002 of WIRE 13, the second has it.
	a.failCode(mMessages, codeNotLive, "session not live: s9")
	b.setTabs(liveTab("s2", model.StatusIdle), liveTab("s9", model.StatusIdle))
	got, err := e.Messages("s9", 5)
	if err != nil {
		t.Fatalf("fan-out must find the session: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("messages from the fallback: %d", len(got))
	}
	if len(a.framesOf(mMessages)) == 0 {
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
	handshakes := len(f.framesOf(mInitialize))
	if handshakes < 2 {
		t.Errorf("expected a second handshake after reconnect, saw %d", handshakes)
	}
	// The pid survives from the new initialize, because PluginPID is read from
	// _meta and not from a socket name.
	if got, _ := e.List(); len(got) == 1 && got[0].PluginPID != 111 {
		t.Errorf("PluginPID after reconnect: %+v", got[0])
	}
}

func TestOversizedFrameDropsConnection(t *testing.T) {
	dir := t.TempDir()
	tt := fastTimings()
	tt.maxLine = 2048 // big enough for a handshake, too small for this frame
	e, ch := startEngine(t, dir, tt)
	f := newFakeTUI(t, dir, "pid-7.sock", 7)
	waitFor(t, "the plugin socket to be tracked", func() bool { return e.connCount() == 1 })
	wantEvent(t, ch, evSessions)

	f.writeRaw(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` +
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` +
		strings.Repeat("x", 4096) + `"}}}}` + "\n")
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected (pid 7)") {
		t.Fatalf("an oversize frame must end the connection with a notice: %+v", ev)
	}
	// The next scan reconnects, so one bad frame is a hiccup, not a loss. Wait on
	// the engine's own bookkeeping: the plugin records a request before the
	// handshake answer lands, so counting frames would race the tracking.
	waitFor(t, "the engine to drop the plugin", func() bool { return e.connCount() == 0 })
	waitFor(t, "a reconnect after the oversized frame", func() bool { return e.connCount() == 1 })
	if got, err := e.List(); err != nil || len(got) != 1 {
		t.Fatalf("after reconnect: %+v %v", got, err)
	}
	if n := len(f.framesOf(mInitialize)); n < 2 {
		t.Errorf("a reconnect runs the handshake again, saw %d", n)
	}
}

// TestTwoMissedListPollsDropConnection is ruling 9 and WIRE 1.5: the list poll is
// the liveness clock, there is no ping, and two polls in a row that fail or time
// out end the connection.
func TestTwoMissedListPollsDropConnection(t *testing.T) {
	dir := t.TempDir()
	tt := fastTimings()
	tt.timeout = 40 * time.Millisecond // a stalled plugin must be judged quickly
	e, ch := startEngine(t, dir, tt)
	f := newFakeTUI(t, dir, "pid-9.sock", 9)
	waitFor(t, "the plugin socket to be tracked", func() bool { return e.connCount() == 1 })
	wantEvent(t, ch, evSessions)
	drain(ch)

	f.freeze()
	if got, err := e.List(); err != nil || len(got) != 0 {
		t.Fatalf("one missed poll leaves the rows out and the connection in: %+v %v", got, err)
	}
	if n := e.connCount(); n != 1 {
		t.Fatalf("one miss must not drop the tui: %d conns", n)
	}
	if _, err := e.List(); err != nil {
		t.Fatal(err)
	}
	ev := wantEvent(t, ch, evNotice)
	if !strings.Contains(ev.Text, "tui disconnected (pid 9)") {
		t.Fatalf("two missed list polls must drop the tui: %+v", ev)
	}
	waitFor(t, "the stalled connection to be gone", func() bool { return e.connCount() == 0 })
	// Two polls went out, and no ping is involved anywhere on this wire.
	if n := len(f.framesOf(mSessionList)); n < 2 {
		t.Errorf("two list polls must be sent before the drop, saw %d", n)
	}
	for _, method := range f.methodsSeen() {
		if method == "ping" {
			t.Error("v2 has no ping")
		}
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

// TestReplyWithoutResultFailsAndDropsPlugin pins the framing rule of WIRE 1.3: a
// response carries exactly one of result and error. A bare {"id":N} is a plugin
// bug, so the caller must see a fault rather than a success whose payload happens
// to be empty, the connection must end, and the refusal must be visible.
func TestReplyWithoutResultFailsAndDropsPlugin(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	f, e, ch := setup(t)
	routeSession(t, e, "s1")
	f.replyBare(mSessionSetTitle)
	if err := e.SetTitle("s1", "renamed"); err == nil {
		t.Fatal("a reply with neither result nor error must fail the request")
	}
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected") {
		t.Errorf("the framing fault must end the connection: %+v", ev)
	}
	waitFor(t, "a warning naming the framing fault", func() bool {
		return strings.Contains(logs.String(), "neither result nor error")
	})
}

// TestUnparseableFrameDropsPlugin is the other framing rule of WIRE 1.2: a line
// that is not one JSON object ends the connection, because nothing after it can be
// trusted.
func TestUnparseableFrameDropsPlugin(t *testing.T) {
	f, e, ch := setup(t)
	f.writeRaw("not a frame\n")
	if ev := wantEvent(t, ch, evNotice); !strings.Contains(ev.Text, "tui disconnected") {
		t.Errorf("a frame that is not an object must end the connection: %+v", ev)
	}
	waitFor(t, "the engine to drop the plugin", func() bool { return e.connCount() == 0 })
}

func TestInitializeRefusalIsWarned(t *testing.T) {
	var logs safeBuffer
	restore := warnLog(t, &logs)
	defer restore()

	dir := t.TempDir()
	f := newFakeTUI(t, dir, "pid-3.sock", 3)
	f.failWith(mInitialize, "plugin is not ready")
	e, _ := startEngine(t, dir, fastTimings())
	// The plugin records a request before it answers, so waiting on the frame lets
	// this read the log before the refusal could land in it. The warning is the
	// fact under test, so the wait goes on that.
	waitFor(t, "a warning about the refused handshake", func() bool {
		return strings.Contains(logs.String(), "initialize refused")
	})
	if n := e.connCount(); n != 0 {
		t.Errorf("a plugin that refuses initialize is not tracked: %d conns", n)
	}
	if !slices.Contains(f.methodsSeen(), mInitialize) {
		t.Error("initialize never reached the plugin")
	}
}

// TestUnknownMethodFaultNamesTheMethod is WIRE 12.2: a -32601 for any method but
// session/cancel reaches the caller as an error that names the method, and is not
// one of the mapped sentinels, so the HTTP layer answers a 500. The connection
// keeps working.
func TestUnknownMethodFaultNamesTheMethod(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	f.failCode(mSessionSetMode, acpwire.CodeMethodNotFound, "method not found")
	err := e.SetMode("s1", "plan")
	if err == nil {
		t.Fatal("a refused method must fail the call")
	}
	if !strings.Contains(err.Error(), mSessionSetMode) {
		t.Errorf("the fault must name the method, got %v", err)
	}
	if errors.Is(err, model.ErrNotFound) || errors.Is(err, model.ErrBusy) ||
		errors.Is(err, model.ErrUnsupported) {
		t.Errorf("a -32601 is not one of the mapped sentinels: %v", err)
	}
	if _, err := e.List(); err != nil {
		t.Errorf("an unanswered method is not a transport fault: %v", err)
	}
}

// TestSessionNotLiveFaultIsStale covers the route repair of WIRE 13: a -32002 for
// a tab the route still claims drops the route, and the answer maps to a 404.
func TestSessionNotLiveFaultIsStale(t *testing.T) {
	f, e, _ := setup(t)
	routeSession(t, e, "s1")
	f.failCode(mPlan, codeNotLive, "session not live: s1")
	_, err := e.Plan("s1")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, ok := e.owner("s1"); ok {
		t.Error("a -32002 must drop the route that claimed the tab")
	}
}
