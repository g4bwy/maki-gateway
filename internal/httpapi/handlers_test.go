package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maki-gateway/internal/model"
	"maki-gateway/internal/registry"
)

type fakeEngine struct {
	kind     string
	rows     []model.Session
	msgsF    func(id string, last int) ([]model.Message, error)
	promptF  func(id, text string) (string, error)
	cancelF  func(id string) error
	modeF    func(id, mode string) error
	titleF   func(id, title string) error
	createF  func(req model.CreateRequest) (string, error)
	killF    func(id string) error
	respondF func(rid int64, decision, guidance string) error
}

func (f *fakeEngine) Kind() string                   { return f.kind }
func (f *fakeEngine) List() ([]model.Session, error) { return slices.Clone(f.rows), nil }
func (f *fakeEngine) Messages(id string, last int) ([]model.Message, error) {
	if f.msgsF == nil {
		return nil, model.ErrUnsupported
	}
	return f.msgsF(id, last)
}

func (f *fakeEngine) Prompt(id, text string) (string, error) {
	if f.promptF == nil {
		return "", model.ErrUnsupported
	}
	return f.promptF(id, text)
}

func (f *fakeEngine) Cancel(id string) error {
	if f.cancelF == nil {
		return model.ErrUnsupported
	}
	return f.cancelF(id)
}

func (f *fakeEngine) SetMode(id, mode string) error {
	if f.modeF == nil {
		return model.ErrUnsupported
	}
	return f.modeF(id, mode)
}

func (f *fakeEngine) SetTitle(id, title string) error {
	if f.titleF == nil {
		return model.ErrUnsupported
	}
	return f.titleF(id, title)
}

func (f *fakeEngine) Create(req model.CreateRequest) (string, error) {
	if f.createF == nil {
		return "", model.ErrUnsupported
	}
	return f.createF(req)
}

func (f *fakeEngine) Kill(id string) error {
	if f.killF == nil {
		return model.ErrUnsupported
	}
	return f.killF(id)
}

func (f *fakeEngine) RespondPermission(rid int64, decision, guidance string) error {
	if f.respondF == nil {
		return model.ErrNotFound
	}
	return f.respondF(rid, decision, guidance)
}

// capEngine adds planner/searcher/analyst to a fakeEngine.
type capEngine struct {
	*fakeEngine
	planRaw      json.RawMessage
	planErr      error
	searchHits   []SearchHit
	searchErr    error
	analyticsRaw json.RawMessage
	analyticsErr error
}

func (c *capEngine) Plan(string) (json.RawMessage, error) { return c.planRaw, c.planErr }

func (c *capEngine) Search(_ string, limit int) ([]SearchHit, error) {
	if c.searchErr != nil {
		return nil, c.searchErr
	}
	return c.searchHits[:min(limit, len(c.searchHits))], nil
}

func (c *capEngine) Analytics(int) (json.RawMessage, error) { return c.analyticsRaw, c.analyticsErr }

// taskEngine is the tasker probe: only the attach engine answers a task list, so
// it is a type of its own rather than another capEngine method.
type taskEngine struct {
	*fakeEngine
	tasksRaw json.RawMessage
	tasksErr error
}

func (t *taskEngine) Tasks(string) (json.RawMessage, error) { return t.tasksRaw, t.tasksErr }

// focusEngine is the focuser probe: only a live tab has a screen to move, so the
// follow action is a capability of its own. It records what it was asked for.
type focusEngine struct {
	*fakeEngine
	focusF func(session, taskID string) error
	calls  []string
}

func (f *focusEngine) FocusTask(session, taskID string) error {
	f.calls = append(f.calls, session+"/"+taskID)
	if f.focusF == nil {
		return nil
	}
	return f.focusF(session, taskID)
}

// readerEngine is the taskReader probe: a task's transcript comes out of the
// session file, which only the store engine reads.
type readerEngine struct {
	*fakeEngine
	msgsF func(sessionID, taskID string, last int) ([]json.RawMessage, error)
	calls []string
}

func (r *readerEngine) SubMessages(sessionID, taskID string, last int) ([]json.RawMessage, error) {
	r.calls = append(r.calls, fmt.Sprintf("%s/%s/%d", sessionID, taskID, last))
	if r.msgsF == nil {
		return nil, nil
	}
	return r.msgsF(sessionID, taskID, last)
}

// refusal is what an engine that carries another process's words looks like to
// the HTTP layer: the status comes from the sentinel, the body from Error.
type refusal struct {
	sentinel error
	words    string
}

func (r refusal) Error() string { return r.words }

func (r refusal) Unwrap() error { return r.sentinel }

// yoloEngine is the yoloSetter probe: only the web engine can start its child with
// another permission answer, so it is a type of its own rather than another
// fakeEngine method. It records what it was asked for, in order.
type yoloEngine struct {
	*fakeEngine
	setF  func(id string, on bool) error
	calls []string
}

func (y *yoloEngine) SetYolo(id string, on bool) error {
	y.calls = append(y.calls, fmt.Sprintf("%s=%v", id, on))
	if y.setF == nil {
		return nil
	}
	return y.setF(id, on)
}

func textMessage(text string) model.Message {
	return model.Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":` + jsonEncode(text) + `}]`)}
}

func jsonEncode(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newTestServer(t *testing.T, tune func(*Config), engs ...model.Engine) (*Server, *model.Hub) {
	t.Helper()
	hub := model.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.New(hub, engs...)
	reg.Start(ctx)
	cfg := Config{
		Hub:       hub,
		Registry:  reg,
		Addr:      "127.0.0.1:8765",
		MakiPath:  "no-such-maki-binary-xyz",
		LookupEnv: func(string) string { return "" },
		Engines:   engs,
		Ctx:       ctx,
	}
	if tune != nil {
		tune(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, hub
}

func do(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, code, rec.Body.String())
	}
}

func wantBody(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// trio: one web, one live, one stored session across three engines.
func trio() (acp, att, st *fakeEngine) {
	acp = &fakeEngine{kind: model.KindWeb, rows: []model.Session{
		{ID: "w1", Kind: model.KindWeb, Title: "Web", Status: model.StatusWorking, Cwd: "/w", Model: "opus", UpdatedAt: 300},
	}}
	att = &fakeEngine{kind: model.KindLive, rows: []model.Session{
		{ID: "l1", Kind: model.KindLive, Title: "Live", Status: model.StatusIdle, UpdatedAt: 200, PluginPID: 11},
	}}
	st = &fakeEngine{kind: model.KindStored, rows: []model.Session{
		{ID: "s1", Kind: model.KindStored, Title: "Stored", Status: model.StatusExited, UpdatedAt: 100},
	}}
	return
}

func TestAllRoutesResolve(t *testing.T) {
	acp, att, st := trio()
	we := &yoloEngine{fakeEngine: acp}
	acp.promptF = func(string, string) (string, error) { return "started", nil }
	acp.cancelF = func(string) error { return nil }
	acp.modeF = func(string, string) error { return nil }
	acp.titleF = func(string, string) error { return nil }
	acp.createF = func(model.CreateRequest) (string, error) { return "w2", nil }
	acp.killF = func(string) error { return nil }
	acp.msgsF = func(string, int) ([]model.Message, error) { return []model.Message{textMessage("m")}, nil }
	att.msgsF = func(string, int) ([]model.Message, error) { return nil, nil }
	st.msgsF = func(string, int) ([]model.Message, error) { return nil, model.ErrNotFound }
	live := &focusEngine{fakeEngine: att}
	files := &readerEngine{fakeEngine: st}
	s, _ := newTestServer(t, nil, we, live, files)

	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/health", "", 200},
		{"GET", "/api/sessions", "", 200},
		{"POST", "/api/sessions", `{"cwd":"/x"}`, 201},
		{"DELETE", "/api/sessions/w1", "", 200},
		{"GET", "/api/sessions/w1/messages", "", 200},
		{"GET", "/api/sessions/l1/tasks/tu1/messages", "", 200},
		{"POST", "/api/sessions/l1/tasks/tu1/focus", "", 200},
		{"POST", "/api/sessions/w1/prompt", `{"text":"hi"}`, 200},
		{"POST", "/api/sessions/w1/cancel", "", 200},
		{"POST", "/api/sessions/w1/mode", `{"mode":"plan"}`, 200},
		{"POST", "/api/sessions/w1/yolo", `{"on":true}`, 200},
		{"POST", "/api/sessions/w1/title", `{"title":"t"}`, 200},
		{"GET", "/api/search", "", 200},
		{"GET", "/api/analytics", "", 200},
	}
	for _, c := range cases {
		rec := do(s, c.method, c.path, c.body)
		if rec.Code != c.want {
			t.Errorf("%s %s: status %d, want %d (%s)", c.method, c.path, rec.Code, c.want, rec.Body.String())
		}
	}
	// Wrong method = 405 free via mux.
	if rec := do(s, "DELETE", "/api/sessions", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/sessions: %d, want 405", rec.Code)
	}
	if rec := do(s, "POST", "/api/health", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/health: %d, want 405", rec.Code)
	}
	if rec := do(s, "GET", "/api/nothing", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/nothing: %d, want 404", rec.Code)
	}
}

func TestHealthShape(t *testing.T) {
	acp, att, st := trio()
	s, _ := newTestServer(t, func(c *Config) { c.Version = "1.2.3" }, acp, att, st)
	rec := do(s, "GET", "/api/health", "")
	wantStatus(t, rec, 200)
	var body struct {
		Version  string `json:"version"`
		Maki     string `json:"maki"`
		Sessions int    `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Version != "1.2.3" || body.Maki != "" || body.Sessions != 3 {
		t.Fatalf("health = %+v", body)
	}
	if got := s.MakiVersion(); got != "" {
		t.Fatalf("missing maki binary must cache empty, got %q", got)
	}
}

func TestListSessionsOrder(t *testing.T) {
	acp, att, st := trio()
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "GET", "/api/sessions", "")
	wantStatus(t, rec, 200)
	var list []model.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	if !slices.Equal(ids, []string{"w1", "l1", "s1"}) {
		t.Fatalf("order = %v", ids)
	}
}

func TestEmptyListNeverNull(t *testing.T) {
	s, _ := newTestServer(t, nil)
	wantStatus(t, do(s, "GET", "/api/sessions", ""), 200)
	if rec := do(s, "GET", "/api/sessions", ""); !strings.HasPrefix(rec.Body.String(), "[") {
		t.Fatalf("want JSON array, got %s", rec.Body.String())
	}
	acp, att, st := trio()
	st.msgsF = func(string, int) ([]model.Message, error) { return nil, nil }
	att.msgsF = func(string, int) ([]model.Message, error) { return nil, nil }
	s2, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s2, "GET", "/api/sessions/l1/messages", "")
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"messages":[]`) {
		t.Fatalf("nil messages must serialize as [], got %s", rec.Body.String())
	}
}

func TestCreateSession(t *testing.T) {
	acp, att, st := trio()
	var got model.CreateRequest
	acp.createF = func(req model.CreateRequest) (string, error) {
		got = req
		return "w2", nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "POST", "/api/sessions", `{"cwd":"/tmp/demo","model":"opus","prompt":"start"}`)
	wantStatus(t, rec, 201)
	wantBody(t, rec, `{"session":"w2"}`)
	if got.Cwd != "/tmp/demo" || got.Model != "opus" || got.Prompt != "start" || got.Yolo {
		t.Fatalf("create args = %+v", got)
	}
	if rec := do(s, "POST", "/api/sessions", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d, want 400", rec.Code)
	}
}

// TestCreateYolo is the POST body half of the yolo flag: the field is optional,
// and only true reaches the engine.
func TestCreateYolo(t *testing.T) {
	acp, att, st := trio()
	var got []model.CreateRequest
	acp.createF = func(req model.CreateRequest) (string, error) {
		got = append(got, req)
		return "w2", nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(s, "POST", "/api/sessions", `{"cwd":"/tmp/demo","yolo":true}`), 201)
	wantStatus(t, do(s, "POST", "/api/sessions", `{"cwd":"/tmp/demo"}`), 201)
	if len(got) != 2 || !got[0].Yolo || got[0].Cwd != "/tmp/demo" || got[1].Yolo {
		t.Fatalf("create requests = %+v", got)
	}
}

// TestCreateRequiresCwd: an empty cwd would spawn `maki acp` in whatever
// directory the gateway itself runs in, so the request is refused instead.
func TestCreateRequiresCwd(t *testing.T) {
	acp, att, st := trio()
	creates := 0
	acp.createF = func(model.CreateRequest) (string, error) {
		creates++
		return "w2", nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)
	for _, body := range []string{`{}`, `{"cwd":""}`, `{"cwd":"   "}`, `{"prompt":"hi"}`} {
		rec := do(s, "POST", "/api/sessions", body)
		wantStatus(t, rec, 400)
		wantBody(t, rec, `{"error":"cwd is required"}`)
	}
	if creates != 0 {
		t.Fatalf("a refused create must not reach the engine, got %d calls", creates)
	}
}

func TestDeleteRouting(t *testing.T) {
	acp, att, st := trio()
	acp.killF = func(string) error { return nil }
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "DELETE", "/api/sessions/w1", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"killed":true}`)
	wantStatus(t, do(s, "DELETE", "/api/sessions/l1", ""), 400)
	wantStatus(t, do(s, "DELETE", "/api/sessions/s1", ""), 400)
	wantStatus(t, do(s, "DELETE", "/api/sessions/nope", ""), 404)
}

func TestMessagesSourcePerKind(t *testing.T) {
	acp, att, st := trio()
	var webLast, liveLast int
	acp.msgsF = func(_ string, last int) ([]model.Message, error) {
		webLast = last
		return []model.Message{textMessage("hello")}, nil
	}
	att.msgsF = func(_ string, last int) ([]model.Message, error) { liveLast = last; return []model.Message{}, nil }
	st.msgsF = func(id string, _ int) ([]model.Message, error) {
		if id == "gone" {
			return nil, model.ErrNotFound
		}
		return []model.Message{}, nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)

	rec := do(s, "GET", "/api/sessions/w1/messages", "")
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"source":"acp"`) {
		t.Fatalf("web source: %s", rec.Body.String())
	}
	if webLast != defaultLast {
		t.Fatalf("default last = %d, want %d", webLast, defaultLast)
	}
	rec = do(s, "GET", "/api/sessions/l1/messages?last=5", "")
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"source":"attach"`) {
		t.Fatalf("live source: %s", rec.Body.String())
	}
	if liveLast != 5 {
		t.Fatalf("last override = %d, want 5", liveLast)
	}
	rec = do(s, "GET", "/api/sessions/s1/messages", "")
	if !strings.Contains(rec.Body.String(), `"source":"file"`) {
		t.Fatalf("stored source: %s", rec.Body.String())
	}
	rec = do(s, "GET", "/api/sessions/unknown-id/messages", "")
	wantStatus(t, rec, 200) // unknown ids fall through to the file source
	if !strings.Contains(rec.Body.String(), `"source":"file"`) {
		t.Fatalf("unknown source: %s", rec.Body.String())
	}
	wantStatus(t, do(s, "GET", "/api/sessions/gone/messages", ""), 404) // deleted file
}

func TestPromptRoutingAndErrors(t *testing.T) {
	acp, att, st := trio()
	s, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(s, "POST", "/api/sessions/w1/prompt", `{"text":"hi"}`), 501) // engine rejects
	wantStatus(t, do(s, "POST", "/api/sessions/w1/prompt", `not json`), 400)

	busy := &fakeEngine{kind: model.KindWeb, rows: acp.rows, promptF: func(string, string) (string, error) { return "", model.ErrBusy }}
	s2, _ := newTestServer(t, nil, busy)
	rec := do(s2, "POST", "/api/sessions/w1/prompt", `{"text":"hi"}`)
	wantStatus(t, rec, 409)
	wantBody(t, rec, `{"error":"busy"}`)

	elsewhere := &fakeEngine{kind: model.KindWeb, rows: acp.rows, promptF: func(string, string) (string, error) { return "", model.ErrOpenElsewhere }}
	s3, _ := newTestServer(t, nil, elsewhere)
	rec = do(s3, "POST", "/api/sessions/w1/prompt", `{"text":"hi"}`)
	wantStatus(t, rec, 409)
	wantBody(t, rec, `{"error":"open elsewhere"}`)

	queued := &fakeEngine{kind: model.KindWeb, rows: acp.rows, promptF: func(string, string) (string, error) { return "queued", nil }}
	livePrompted := false
	att.promptF = func(string, string) (string, error) { livePrompted = true; return "started", nil }
	s4, _ := newTestServer(t, nil, queued, att)
	rec = do(s4, "POST", "/api/sessions/l1/prompt", `{"text":"hi"}`)
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"state":"started"}`)
	if !livePrompted {
		t.Fatal("live prompt must reach the attach engine")
	}
	rec = do(s4, "POST", "/api/sessions/w1/prompt", `{"text":"hi"}`)
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"state":"queued"}`)
	wantStatus(t, do(s4, "POST", "/api/sessions/s1/prompt", `{"text":"hi"}`), 404) // stored
	wantStatus(t, do(s4, "POST", "/api/sessions/nope/prompt", `{"text":"hi"}`), 404)
}

func TestCancelRouting(t *testing.T) {
	acp, att, st := trio()
	liveTouched := false
	att.cancelF = func(string) error { liveTouched = true; return nil }
	acp.cancelF = func(string) error { return nil }
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "POST", "/api/sessions/w1/cancel", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"cancelled":true}`)
	wantStatus(t, do(s, "POST", "/api/sessions/l1/cancel", ""), 501)
	if liveTouched {
		t.Fatal("live cancel must not reach the engine (ambiguity 3)")
	}
	wantStatus(t, do(s, "POST", "/api/sessions/s1/cancel", ""), 404)
	wantStatus(t, do(s, "POST", "/api/sessions/nope/cancel", ""), 404)
}

func TestModeAndTitle(t *testing.T) {
	acp, att, st := trio()
	var gotMode, gotTitle string
	acp.modeF = func(_, mode string) error { gotMode = mode; return nil }
	acp.titleF = func(_, title string) error { gotTitle = title; return nil }
	s, _ := newTestServer(t, nil, acp, att, st)

	rec := do(s, "POST", "/api/sessions/w1/mode", `{"mode":"plan"}`)
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"ok":true}`)
	if gotMode != "plan" {
		t.Fatalf("mode = %q", gotMode)
	}
	rec = do(s, "POST", "/api/sessions/w1/title", `{"title":null}`)
	wantStatus(t, rec, 200)
	if gotTitle != "" {
		t.Fatalf("null title must clear to empty, got %q", gotTitle)
	}
	wantStatus(t, do(s, "POST", "/api/sessions/s1/mode", `{"mode":"plan"}`), 405)
	wantStatus(t, do(s, "POST", "/api/sessions/s1/title", `{"title":"x"}`), 405)
	wantStatus(t, do(s, "POST", "/api/sessions/nope/mode", `{"mode":"plan"}`), 404)
	wantStatus(t, do(s, "POST", "/api/sessions/nope/title", `{"title":"x"}`), 404)
}

func TestPermissionRouting(t *testing.T) {
	acp, att, st := trio()
	var acpArg, attachArg string
	acp.respondF = func(rid int64, decision, guidance string) error {
		acpArg = fmt.Sprintf("%s %d %s", decision, rid, guidance)
		return nil
	}
	att.respondF = func(rid int64, _, _ string) error { attachArg = strconv.FormatInt(rid, 10); return nil }
	s, _ := newTestServer(t, nil, acp, att, st)

	rec := do(s, "POST", "/api/permissions/acp/7", `{"decision":"allow"}`)
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"ok":true}`)
	if acpArg != "allow 7 " {
		t.Fatalf("acp respond args = %q", acpArg)
	}
	if rec := do(s, "POST", "/api/permissions/attach/9", `{"decision":"deny","guidance":"no"}`); rec.Code != 200 {
		t.Fatalf("attach permission: %d", rec.Code)
	}
	if attachArg != "9" {
		t.Fatalf("attach rid = %q", attachArg)
	}
	wantStatus(t, do(s, "POST", "/api/permissions/web/1", `{"decision":"allow"}`), 200)
	wantStatus(t, do(s, "POST", "/api/permissions/acp/abc", `{"decision":"allow"}`), 400)
	wantStatus(t, do(s, "POST", "/api/permissions/nope/1", `{"decision":"allow"}`), 404)
	wantStatus(t, do(s, "POST", "/api/permissions/live/1", `{"decision":"allow"}`), 200) // kind spelling
	expired := &fakeEngine{kind: model.KindLive, rows: att.rows}                         // respondF nil -> ErrNotFound
	s2, _ := newTestServer(t, nil, expired)
	wantStatus(t, do(s2, "POST", "/api/permissions/attach/1", `{"decision":"allow"}`), 404)
}

// TestPermissionDecisionValues: the API takes three decisions and refuses every
// other spelling before an engine sees it, so a typo cannot leave a dialog parked.
func TestPermissionDecisionValues(t *testing.T) {
	acp, att, st := trio()
	var got string
	acp.respondF = func(_ int64, decision, _ string) error {
		got = decision
		return nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)
	for _, d := range []string{"allow", "allow_always", "deny"} {
		rec := do(s, "POST", "/api/permissions/acp/3", `{"decision":"`+d+`"}`)
		wantStatus(t, rec, 200)
		if got != d {
			t.Fatalf("decision = %q, want %q", got, d)
		}
	}
	for _, body := range []string{`{"decision":"always"}`, `{"decision":"reject_always"}`, `{}`, `{"decision":"ALLOW"}`} {
		if rec := do(s, "POST", "/api/permissions/acp/3", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}

// TestTasksRouting: only a live session has a task list, and the rows pass
// through with the field names maki gave them.
func TestTasksRouting(t *testing.T) {
	acp, att, st := trio()
	live := &taskEngine{fakeEngine: att, tasksRaw: json.RawMessage(
		`[{"id":"main","session":"s1","name":"main","focused":true},` +
			`{"id":"tu1","session":"s1","name":"explore","status":"working","tool":"task","focused":false,"cost":0.02}]`)}
	s, _ := newTestServer(t, nil, acp, live, st)

	rec := do(s, "GET", "/api/sessions/l1/tasks", "")
	wantStatus(t, rec, 200)
	var body struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 2 || body.Tasks[0]["id"] != "main" ||
		body.Tasks[1]["status"] != "working" || body.Tasks[1]["cost"] != 0.02 {
		t.Fatalf("tasks passthrough = %s", rec.Body.String())
	}
	// web and stored kinds have no task API, and an unknown id has no row.
	wantStatus(t, do(s, "GET", "/api/sessions/w1/tasks", ""), http.StatusNotImplemented)
	wantStatus(t, do(s, "GET", "/api/sessions/s1/tasks", ""), http.StatusNotImplemented)
	wantStatus(t, do(s, "GET", "/api/sessions/nope/tasks", ""), http.StatusNotFound)
	// A live engine without the capability answers 501 too.
	plain, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(plain, "GET", "/api/sessions/l1/tasks", ""), http.StatusNotImplemented)
	// An empty payload is still a list.
	empty := &taskEngine{fakeEngine: att}
	noRows, _ := newTestServer(t, nil, acp, empty, st)
	rec = do(noRows, "GET", "/api/sessions/l1/tasks", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"tasks":[]}`)
	// A plugin error keeps its status mapping.
	bad := &taskEngine{fakeEngine: att, tasksErr: model.ErrNotFound}
	errSrv, _ := newTestServer(t, nil, acp, bad, st)
	wantStatus(t, do(errSrv, "GET", "/api/sessions/l1/tasks", ""), http.StatusNotFound)
}

// TestTaskMessagesRoute: one task's transcript comes out of the session file for
// every kind of row, an empty transcript is a list, and an id with no log behind
// it is the 404.
func TestTaskMessagesRoute(t *testing.T) {
	acp, att, st := trio()
	var reader readerEngine
	reader.fakeEngine = st
	reader.msgsF = func(sessionID, taskID string, last int) ([]json.RawMessage, error) {
		switch sessionID + "/" + taskID {
		case "gone/tu1": // no log behind the id
			return nil, model.ErrNotFound
		case "s1/running": // maki writes the burst when the run ends
			return nil, nil
		}
		if last < 2 {
			return []json.RawMessage{json.RawMessage(`{"role":"assistant","content":[]}`)}, nil
		}
		return []json.RawMessage{
			json.RawMessage(`{"role":"user","content":[{"type":"text","text":"do it"}]}`),
			json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
		}, nil
	}
	s, _ := newTestServer(t, nil, acp, att, &reader)

	rec := do(s, "GET", "/api/sessions/l1/tasks/tu1/messages", "")
	wantStatus(t, rec, 200)
	var body struct {
		Messages []model.Message `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 || body.Messages[1].Role != "assistant" {
		t.Fatalf("transcript = %s", rec.Body.String())
	}
	if want := "l1/tu1/" + strconv.Itoa(defaultLast); !slices.Contains(reader.calls, want) {
		t.Fatalf("the route reads the log named in the path with the default page, saw %v", reader.calls)
	}
	// The page size travels, and a running task answers an empty list rather than
	// null, so the browser has one shape to render.
	rec = do(s, "GET", "/api/sessions/s1/tasks/tu1/messages?last=1", "")
	wantStatus(t, rec, 200)
	if !slices.Contains(reader.calls, "s1/tu1/1") {
		t.Fatalf("last=1 never reached the reader: %v", reader.calls)
	}
	rec = do(s, "GET", "/api/sessions/s1/tasks/running/messages", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"messages":[]}`)
	rec = do(s, "GET", "/api/sessions/gone/tasks/tu1/messages", "")
	wantStatus(t, rec, 404)
	// A task id arrives decoded, as the browser encoded it.
	do(s, "GET", "/api/sessions/s1/tasks/tu%201/messages", "")
	if !slices.Contains(reader.calls, "s1/tu 1/"+strconv.Itoa(defaultLast)) {
		t.Fatalf("encoded task id never reached the reader: %v", reader.calls)
	}
	// With no engine that reads files, the route says so.
	plain, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(plain, "GET", "/api/sessions/s1/tasks/tu1/messages", ""), http.StatusNotImplemented)
}

// TestTaskFocusRoute: following is a live-tab action, and the plugin's refusal
// words reach the browser as the terminal wrote them.
func TestTaskFocusRoute(t *testing.T) {
	acp, att, st := trio()
	live := &focusEngine{fakeEngine: att}
	s, _ := newTestServer(t, nil, acp, live, st)

	rec := do(s, "POST", "/api/sessions/l1/tasks/tu1/focus", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, `{"ok":true}`)
	if !slices.Equal(live.calls, []string{"l1/tu1"}) {
		t.Fatalf("focus calls = %v", live.calls)
	}
	rec = do(s, "POST", "/api/sessions/w1/tasks/tu1/focus", "")
	wantStatus(t, rec, 501)
	wantBody(t, rec, `{"error":"`+followLiveText+`"}`)
	rec = do(s, "POST", "/api/sessions/s1/tasks/tu1/focus", "")
	wantStatus(t, rec, 501)
	wantBody(t, rec, `{"error":"`+followLiveText+`"}`)
	rec = do(s, "POST", "/api/sessions/nope/tasks/tu1/focus", "")
	wantStatus(t, rec, 404)
	wantBody(t, rec, `{"error":"unknown session"}`)
	// A live engine with no follow answer is refused the same way the plan route
	// refuses an engine with no plan.
	plain, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(plain, "POST", "/api/sessions/l1/tasks/tu1/focus", ""), http.StatusNotImplemented)

	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"unknown task", refusal{model.ErrNotFound, "no task with id tu9"}, 404,
			`{"error":"no task with id tu9"}`},
		{"no terminal", refusal{model.ErrUnsupported, "no interactive UI attached"}, 501,
			`{"error":"no interactive UI attached"}`},
		{"tab gone", refusal{model.ErrNotFound, "not found: session not live: l1"}, 404,
			`{"error":"not found: session not live: l1"}`},
		{"engine fault", errors.New("attach: task_focus: write error"), 500,
			`{"error":"attach: task_focus: write error"}`},
	}
	for _, c := range cases {
		broken := &focusEngine{fakeEngine: att, focusF: func(string, string) error { return c.err }}
		srv, _ := newTestServer(t, nil, acp, broken, st)
		rec := do(srv, "POST", "/api/sessions/l1/tasks/tu1/focus", "")
		if rec.Code != c.wantCode {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.wantCode, rec.Body.String())
		}
		wantBody(t, rec, c.wantBody)
	}
}

func TestPlanRouting(t *testing.T) {
	_, att, st := trio()
	w, _, _ := trio()
	livePlan := &capEngine{fakeEngine: att, planRaw: json.RawMessage(`{"mode":"plan","ready":true}`)}
	s, _ := newTestServer(t, nil, w, livePlan, st)
	wantStatus(t, do(s, "GET", "/api/plan/w1", ""), 501) // web kind per contract
	rec := do(s, "GET", "/api/plan/l1", "")
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"ready":true`) {
		t.Fatalf("plan body = %s", rec.Body.String())
	}
	wantStatus(t, do(s, "GET", "/api/plan/s1", ""), 501) // stored fake has no planner
	wantStatus(t, do(s, "GET", "/api/plan/nope", ""), 404)
}

func TestSearchProbeCap50(t *testing.T) {
	_, att, st := trio()
	w, _, _ := trio()
	hits := make([]SearchHit, 60)
	for i := range hits {
		hits[i] = SearchHit{Session: strconv.Itoa(i)}
	}
	capW := &capEngine{fakeEngine: w, searchHits: hits}
	s, _ := newTestServer(t, nil, capW, att, st)
	rec := do(s, "GET", "/api/search?q=x", "")
	wantStatus(t, rec, 200)
	var body struct {
		Hits []SearchHit `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Hits) != searchCap {
		t.Fatalf("hits = %d, want %d", len(body.Hits), searchCap)
	}
	if rec := do(s, "GET", "/api/search", ""); !strings.Contains(rec.Body.String(), `"hits":[]`) {
		t.Fatalf("empty q: %s", rec.Body.String())
	}
}

func TestSearchFallbackScan(t *testing.T) {
	acp, att, st := trio()
	acp.msgsF = func(string, int) ([]model.Message, error) {
		return []model.Message{
			textMessage(strings.Repeat("x", 50) + "a Needle here" + strings.Repeat("y", 50)),
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"needle"}]`)},
		}, nil
	}
	att.msgsF = func(string, int) ([]model.Message, error) { return []model.Message{textMessage("nothing")}, nil }
	st.msgsF = func(string, int) ([]model.Message, error) { return nil, model.ErrNotFound }
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "GET", "/api/search?q=NEEDLE", "")
	wantStatus(t, rec, 200)
	var body struct {
		Hits []SearchHit `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Hits) != 1 || body.Hits[0].Session != "w1" || body.Hits[0].Title != "Web" {
		t.Fatalf("hits = %+v", body.Hits)
	}
	if !strings.Contains(strings.ToLower(body.Hits[0].Snippet), "needle") {
		t.Fatalf("snippet = %q", body.Hits[0].Snippet)
	}

	// Cap 50 over many matching stored sessions.
	many := make([]model.Session, 60)
	for i := range many {
		many[i] = model.Session{ID: "id" + strconv.Itoa(i), Kind: model.KindStored, UpdatedAt: int64(i)}
	}
	st2 := &fakeEngine{kind: model.KindStored, rows: many, msgsF: func(string, int) ([]model.Message, error) {
		return []model.Message{textMessage("has needle")}, nil
	}}
	s2, _ := newTestServer(t, nil, st2)
	rec = do(s2, "GET", "/api/search?q=needle", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Hits) != searchCap {
		t.Fatalf("fallback hits = %d, want %d", len(body.Hits), searchCap)
	}
}

func TestAnalytics(t *testing.T) {
	acp, att, st := trio()
	raw := json.RawMessage(`{"daily":[{"date":"2026-01-01","sessions":1}],"per_session":[]}`)
	capSt := &capEngine{fakeEngine: st, analyticsRaw: raw}
	s, _ := newTestServer(t, nil, acp, att, capSt)
	rec := do(s, "GET", "/api/analytics?days=7", "")
	wantStatus(t, rec, 200)
	wantBody(t, rec, string(raw))

	// No analyst: fallback per_session from registry rows, zero cost, empty daily.
	s2, _ := newTestServer(t, nil, acp, att, st)
	rec = do(s2, "GET", "/api/analytics", "")
	wantStatus(t, rec, 200)
	var body struct {
		Daily      []any          `json:"daily"`
		PerSession []analyticsRow `json:"per_session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Daily == nil || len(body.Daily) != 0 || len(body.PerSession) != 3 {
		t.Fatalf("fallback = %s", rec.Body.String())
	}
	if body.PerSession[0].Session != "w1" || body.PerSession[0].Cost != 0 || body.PerSession[0].UpdatedAt != 300 {
		t.Fatalf("fallback rows = %+v", body.PerSession)
	}
}

// notHeld stands for the web engine meeting an id it never spawned: a live tab, or
// a stored session that a terminal owns.
func notHeld(string, bool) error { return model.ErrNotFound }

// TestYoloToggleStatuses is the route table of CONTRACTS 2: the web row's answer,
// the errors that mean try again, the kinds this side cannot reach, and a body that
// is not a flag.
func TestYoloToggleStatuses(t *testing.T) {
	const terminalBody = `{"error":"yolo is a terminal-side toggle for attached sessions"}`
	cases := []struct {
		name, id, body string
		setF           func(string, bool) error
		want           int
		wantBody       string
	}{
		{"web on", "w1", `{"on":true}`, nil, 200, `{"yolo":true}`},
		{"web off", "w1", `{"on":false}`, nil, 200, `{"yolo":false}`},
		{"busy turn", "w1", `{"on":true}`, func(string, bool) error { return model.ErrBusy }, 409, ""},
		{"still claimed", "w1", `{"on":true}`,
			func(string, bool) error { return model.ErrOpenElsewhere }, 409, ""},
		{"web row whose child is gone", "w1", `{"on":true}`, notHeld, 404, ""},
		{"unknown session", "nope", `{"on":true}`, nil, 404, ""},
		{"live tab", "l1", `{"on":true}`, notHeld, 501, terminalBody},
		{"stored session", "s1", `{"on":true}`, notHeld, 501, terminalBody},
		// The row is stored because the child died, and the engine still holds the
		// session, so the toggle loads it back rather than refusing.
		{"stored row this gateway can revive", "s1", `{"on":true}`, nil, 200, `{"yolo":true}`},
		{"no flag", "w1", `{}`, nil, 400, ""},
		{"flag that is not a bool", "w1", `{"on":"yes"}`, nil, 400, ""},
		{"body that is not json", "w1", `yolo`, nil, 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			acp, att, st := trio()
			s, _ := newTestServer(t, nil, &yoloEngine{fakeEngine: acp, setF: c.setF}, att, st)
			rec := do(s, "POST", "/api/sessions/"+c.id+"/yolo", c.body)
			wantStatus(t, rec, c.want)
			if c.wantBody != "" {
				wantBody(t, rec, c.wantBody)
			}
		})
	}
}

// TestYoloToggleAsksTheWebEngine pins who is called with what: the flag travels as
// posted, and only the engine registered for kind web is asked.
func TestYoloToggleAsksTheWebEngine(t *testing.T) {
	acp, att, st := trio()
	we := &yoloEngine{fakeEngine: acp}
	s, _ := newTestServer(t, nil, we, att, st)

	wantStatus(t, do(s, "POST", "/api/sessions/w1/yolo", `{"on":true}`), 200)
	wantStatus(t, do(s, "POST", "/api/sessions/w1/yolo", `{"on":false}`), 200)
	if len(we.calls) != 2 || we.calls[0] != "w1=true" || we.calls[1] != "w1=false" {
		t.Fatalf("engine saw %v, want w1=true then w1=false", we.calls)
	}

	// An engine with no toggle cannot be asked for one, and neither can a registry
	// that has no web engine at all.
	plain, _ := newTestServer(t, nil, acp, att, st)
	wantStatus(t, do(plain, "POST", "/api/sessions/w1/yolo", `{"on":true}`), 501)
	noWeb, _ := newTestServer(t, nil, att, st)
	wantStatus(t, do(noWeb, "POST", "/api/sessions/l1/yolo", `{"on":true}`), 501)
}

// TestSessionJSONCarriesYolo is the row shape the browser reads the toggle state
// from: the field rides on a yolo web row and on nothing else, so an absent field
// and a plain false both mean the session still asks.
func TestSessionJSONCarriesYolo(t *testing.T) {
	acp, att, st := trio()
	acp.rows[0].Yolo = true
	s, _ := newTestServer(t, nil, acp, att, st)

	rec := do(s, "GET", "/api/sessions", "")
	wantStatus(t, rec, 200)
	body := rec.Body.String()
	if !strings.Contains(body, `"yolo":true`) {
		t.Errorf("a yolo web row reported without the field: %s", body)
	}
	if strings.Count(body, `"yolo"`) != 1 {
		t.Errorf("yolo reached a row that has no such answer: %s", body)
	}
}
