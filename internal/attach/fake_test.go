package attach

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

// fakeTUI speaks CONTRACTS 1 from the plugin side: it serves one unix socket in
// a temp dir, answers ops, pushes events, and parks permission requests until
// the gateway answers them. It stands in for the Lua plugin, so everything it
// asserts is a contract assertion.
type fakeTUI struct {
	t    *testing.T
	path string
	pid  int

	mu       sync.Mutex
	ln       *net.UnixListener
	conns    []net.Conn
	frames   []map[string]any  // every request frame received
	list     []map[string]any  // what the list op answers
	parked   map[int64]string  // permission request id -> decision wanted
	answered []int64           // ids the gateway answered
	frozen   bool              // stop answering ping
	pending  map[string]string // op -> canned error
	started  string            // what prompt answers
	closed   bool
}

func fastTimings() timings {
	return timings{
		scanEvery: 25 * time.Millisecond,
		pingEvery: 25 * time.Millisecond,
		timeout:   250 * time.Millisecond,
		tryAll:    250 * time.Millisecond,
		missLimit: 2,
		maxLine:   maxFrame,
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
		t: t, path: filepath.Join(dir, name), pid: pid,
		parked: map[int64]string{}, pending: map[string]string{}, started: "started",
	}
	if len(tabs) > 0 {
		f.list = tabs
	} else {
		f.list = []map[string]any{liveTab("s1", "idle")}
	}
	f.listen()
	t.Cleanup(f.stop)
	return f
}

func liveTab(id, status string) map[string]any {
	return map[string]any{"id": id, "title": "tab " + id, "status": status,
		"cwd": "/work", "model": "m-" + id, "focused": id == "s1"}
}

func (f *fakeTUI) listen() {
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
	f.conns, f.closed = nil, false
	go f.accept(f.ln)
}

// crash is a killed TUI: the connections die and the socket file stays behind.
func (f *fakeTUI) crash() {
	f.mu.Lock()
	ln, conns := f.ln, f.conns
	f.ln, f.conns, f.closed = nil, nil, true
	f.mu.Unlock()
	if ln != nil {
		ln.SetUnlinkOnClose(false)
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// restart is the same path coming back to life.
func (f *fakeTUI) restart() {
	f.mu.Lock()
	f.answered, f.frozen = nil, false
	f.parked, f.pending = map[int64]string{}, map[string]string{}
	f.mu.Unlock()
	f.listen()
}

func (f *fakeTUI) stop() {
	f.mu.Lock()
	ln, conns := f.ln, f.conns
	f.ln, f.conns = nil, nil
	f.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeTUI) accept(ln *net.UnixListener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		staleListener := f.closed || f.ln != ln
		if !staleListener {
			f.conns = append(f.conns, c)
		}
		f.mu.Unlock()
		if staleListener {
			_ = c.Close()
			return
		}
		go f.serve(c)
	}
}

func (f *fakeTUI) serve(c net.Conn) {
	defer f.hangUp(c)
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			f.t.Errorf("fake: engine sent an unparseable frame: %v", err)
			return
		}
		f.record(frame)
		id, _ := frame["id"].(float64)
		ok, fail, hang := f.answer(frame)
		if hang {
			continue
		}
		out := map[string]any{"id": int64(id)}
		if fail != "" {
			out["error"] = fail
		} else {
			out["ok"] = ok
		}
		b, err := json.Marshal(out)
		if err != nil {
			f.t.Errorf("fake: marshal reply: %v", err)
			return
		}
		if _, err := c.Write(append(b, '\n')); err != nil {
			return
		}
	}
}

// hangUp drops a connection the gateway closed.
func (f *fakeTUI) hangUp(c net.Conn) {
	f.mu.Lock()
	kept := f.conns[:0]
	for _, x := range f.conns {
		if x != c {
			kept = append(kept, x)
		}
	}
	f.conns = kept
	f.mu.Unlock()
}

func (f *fakeTUI) record(frame map[string]any) {
	f.mu.Lock()
	f.frames = append(f.frames, frame)
	f.mu.Unlock()
}

// answer is the plugin side of CONTRACTS 1. hang means the request is never
// answered, which is how the ping-timeout path is exercised.
func (f *fakeTUI) answer(frame map[string]any) (ok any, fail string, hang bool) {
	op, _ := frame["op"].(string)
	f.mu.Lock()
	canned, hasCanned := f.pending[op]
	frozen, started := f.frozen, f.started
	f.mu.Unlock()
	if hasCanned {
		return nil, canned, false
	}
	switch op {
	case opHello:
		return map[string]any{"pid": f.pid, "plugin": pluginVersion, "cwd": "/work",
			"sessions": len(f.snapshot())}, "", false
	case opPing:
		if frozen {
			return nil, "", true
		}
		return "pong", "", false
	case opList:
		return f.snapshot(), "", false
	case opMessages:
		if !f.has(str(frame[argSession])) {
			return nil, "session not live: " + str(frame[argSession]), false
		}
		return map[string]any{"messages": f.messagesFor(str(frame[argSession]))}, "", false
	case opPrompt:
		if !f.has(str(frame[argSession])) {
			return nil, "session not live: " + str(frame[argSession]), false
		}
		return started, "", false
	case opNew:
		return map[string]any{"session": "created-1"}, "", false
	case "plan":
		if !f.has(str(frame[argSession])) {
			return nil, "session not live: " + str(frame[argSession]), false
		}
		return map[string]any{"mode": "plan", "path": "/tmp/plan.md", "ready": true,
			"content": "# plan"}, "", false
	case opSetMode, opSetTitle, "notify", opPerms, "pop_queue", "tasks":
		return true, "", false
	case opRespond:
		return f.respond(frame)
	}
	return nil, "unknown op: " + op, false
}

// respond drains a parked permission, or says expired (CONTRACTS 1).
func (f *fakeTUI) respond(frame map[string]any) (any, string, bool) {
	id, _ := frame[argRequest].(float64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, parked := f.parked[int64(id)]; !parked {
		return nil, "expired", false
	}
	delete(f.parked, int64(id))
	f.answered = append(f.answered, int64(id))
	return true, "", false
}

func (f *fakeTUI) waitForAnswer(t *testing.T, id int64) {
	t.Helper()
	waitFor(t, "the permission answer to reach the plugin", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.answered, id)
	})
}

func (f *fakeTUI) snapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.list...)
}

func (f *fakeTUI) has(id string) bool {
	for _, s := range f.snapshot() {
		if s["id"] == id {
			return true
		}
	}
	return false
}

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

// push sends one event frame to every live connection, as the plugin does.
func (f *fakeTUI) push(event map[string]any) {
	f.writeRaw(jsonOf(event) + "\n")
}

// pushPermission parks a request the way the permission slot wrapper does.
func (f *fakeTUI) pushPermission(id int64, session, tool string, input map[string]any) {
	f.mu.Lock()
	f.parked[id] = "deny"
	f.mu.Unlock()
	f.push(map[string]any{"event": evPermission, "request_id": id, "session": session,
		"tool": tool, "input": input})
}

// writeRaw sends arbitrary bytes at the gateway: oversized and malformed frames.
func (f *fakeTUI) writeRaw(s string) {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		if _, err := io.WriteString(c, s); err != nil {
			f.t.Logf("fake: write failed: %v", err)
		}
	}
}

func (f *fakeTUI) setFrozen(v bool) {
	f.mu.Lock()
	f.frozen = v
	f.mu.Unlock()
}

// failWith makes one op answer an error string, to exercise the error mapping.
func (f *fakeTUI) failWith(op, msg string) {
	f.mu.Lock()
	f.pending[op] = msg
	f.mu.Unlock()
}

func (f *fakeTUI) answerPromptWith(state string) {
	f.mu.Lock()
	f.started = state
	f.mu.Unlock()
}

// setTabs replaces what the list op answers.
func (f *fakeTUI) setTabs(tabs ...map[string]any) {
	f.mu.Lock()
	f.list = tabs
	f.mu.Unlock()
}

func (f *fakeTUI) opsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.frames))
	for _, fr := range f.frames {
		out = append(out, str(fr["op"]))
	}
	return out
}

func (f *fakeTUI) frameWithOp(op string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.frames) - 1; i >= 0; i-- {
		if str(f.frames[i]["op"]) == op {
			return f.frames[i]
		}
	}
	return nil
}

func (f *fakeTUI) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func str(v any) string {
	s, _ := v.(string)
	return s
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

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
