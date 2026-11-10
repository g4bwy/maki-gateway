package attach

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	if _, err := e.Create("/work", "model", "hi"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("no tui must map to ErrNotFound, got %v", err)
	}
	first := newFakeTUI(t, dir, "pid-1.sock", 1)
	second := newFakeTUI(t, dir, "pid-2.sock", 2)
	waitFor(t, "both plugins to be tracked", func() bool { return e.connCount() == 2 })

	id, err := e.Create("/work", "model", "start here")
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

// connCount is how many plugin sockets the engine holds. Test-side only: the
// merged session list is the production answer to the same question.
func (e *Engine) connCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.conns)
}
