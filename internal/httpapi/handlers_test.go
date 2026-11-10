package httpapi

import (
	"context"
	"encoding/json"
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
	createF  func(cwd, m, prompt string) (string, error)
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

func (f *fakeEngine) Create(cwd, m, prompt string) (string, error) {
	if f.createF == nil {
		return "", model.ErrUnsupported
	}
	return f.createF(cwd, m, prompt)
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
	acp.promptF = func(string, string) (string, error) { return "started", nil }
	acp.cancelF = func(string) error { return nil }
	acp.modeF = func(string, string) error { return nil }
	acp.titleF = func(string, string) error { return nil }
	acp.createF = func(string, string, string) (string, error) { return "w2", nil }
	acp.killF = func(string) error { return nil }
	acp.msgsF = func(string, int) ([]model.Message, error) { return []model.Message{textMessage("m")}, nil }
	att.msgsF = func(string, int) ([]model.Message, error) { return nil, nil }
	st.msgsF = func(string, int) ([]model.Message, error) { return nil, model.ErrNotFound }
	s, _ := newTestServer(t, nil, acp, att, st)

	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/health", "", 200},
		{"GET", "/api/sessions", "", 200},
		{"POST", "/api/sessions", `{"cwd":"/x"}`, 201},
		{"DELETE", "/api/sessions/w1", "", 200},
		{"GET", "/api/sessions/w1/messages", "", 200},
		{"POST", "/api/sessions/w1/prompt", `{"text":"hi"}`, 200},
		{"POST", "/api/sessions/w1/cancel", "", 200},
		{"POST", "/api/sessions/w1/mode", `{"mode":"plan"}`, 200},
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
	var gotCwd, gotModel, gotPrompt string
	acp.createF = func(cwd, m, prompt string) (string, error) {
		gotCwd, gotModel, gotPrompt = cwd, m, prompt
		return "w2", nil
	}
	s, _ := newTestServer(t, nil, acp, att, st)
	rec := do(s, "POST", "/api/sessions", `{"cwd":"/tmp/demo","model":"opus","prompt":"start"}`)
	wantStatus(t, rec, 201)
	wantBody(t, rec, `{"session":"w2"}`)
	if gotCwd != "/tmp/demo" || gotModel != "opus" || gotPrompt != "start" {
		t.Fatalf("create args = %q %q %q", gotCwd, gotModel, gotPrompt)
	}
	if rec := do(s, "POST", "/api/sessions", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d, want 400", rec.Code)
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
