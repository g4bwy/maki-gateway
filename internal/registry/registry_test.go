package registry

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

type fakeEngine struct {
	kind string
	mu   sync.Mutex
	rows []model.Session
	err  error
}

func newFake(kind string, rows ...model.Session) *fakeEngine {
	return &fakeEngine{kind: kind, rows: rows}
}

func (f *fakeEngine) Kind() string { return f.kind }

func (f *fakeEngine) List() ([]model.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.rows), nil
}

func (f *fakeEngine) set(rows ...model.Session) {
	f.mu.Lock()
	f.rows = rows
	f.mu.Unlock()
}

func (f *fakeEngine) Messages(string, int) ([]model.Message, error) { return nil, model.ErrUnsupported }
func (f *fakeEngine) Prompt(string, string) (string, error)         { return "", model.ErrUnsupported }
func (f *fakeEngine) Cancel(string) error                           { return model.ErrUnsupported }
func (f *fakeEngine) SetMode(string, string) error                  { return model.ErrUnsupported }
func (f *fakeEngine) SetTitle(string, string) error                 { return model.ErrUnsupported }
func (f *fakeEngine) Create(model.CreateRequest) (string, error)    { return "", model.ErrUnsupported }
func (f *fakeEngine) Kill(string) error                             { return model.ErrUnsupported }
func (f *fakeEngine) RespondPermission(int64, string, string) error { return model.ErrNotFound }

func TestSessionKind(t *testing.T) {
	cases := map[string]string{
		"acp": "web", "web": "web",
		"attach": "live", "live": "live",
		"store": "stored", "stored": "stored",
		"": "", "bogus": "bogus",
	}
	for in, want := range cases {
		if got := SessionKind(in); got != want {
			t.Errorf("SessionKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeLiveWinsAndFillsBlanks(t *testing.T) {
	acp := newFake("acp", model.Session{ID: "x", Kind: "web", Title: "acp title", Status: "working", Cwd: "/acp", UpdatedAt: 5})
	attach := newFake("attach", model.Session{ID: "x", Kind: "live", Status: "idle", Model: "m1"})
	store := newFake("store", model.Session{ID: "x", Kind: "stored", Title: "stored title", Cwd: "/store", UpdatedAt: 9, PluginPID: 42})

	got := Merge(store, acp, attach)
	if len(got) != 1 {
		t.Fatalf("want 1 merged row, got %d", len(got))
	}
	row := got[0]
	want := model.Session{ID: "x", Kind: "live", Title: "acp title", Status: "idle", Cwd: "/acp", Model: "m1", UpdatedAt: 5, PluginPID: 42}
	if row != want {
		t.Errorf("merged row = %+v, want %+v", row, want)
	}
}

func TestMergeFirstRowWinsAtSameRank(t *testing.T) {
	a := newFake("web", model.Session{ID: "x", Kind: "web", Title: "first", UpdatedAt: 1})
	b := newFake("acp", model.Session{ID: "x", Kind: "web", Title: "second", Status: "working", UpdatedAt: 2})
	got := Merge(a, b)
	if len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("first row should win ties, got %+v", got)
	}
	if got[0].Status != "working" {
		t.Errorf("blank fields should fill from the runner-up, got %+v", got[0])
	}
	if got[0].UpdatedAt != 1 {
		t.Errorf("UpdatedAt must be the winner's, not max across rows (ambiguity 5), got %d", got[0].UpdatedAt)
	}
}

func TestMergeUpdatedAtRunnerUp(t *testing.T) {
	live := newFake("live", model.Session{ID: "x", Kind: "live"})
	web := newFake("web", model.Session{ID: "x", Kind: "web", UpdatedAt: 100})
	store := newFake("stored", model.Session{ID: "x", Kind: "stored", UpdatedAt: 200})
	got := Merge(store, web, live)
	if len(got) != 1 || got[0].UpdatedAt != 100 {
		t.Fatalf("want runner-up 100, got %+v", got)
	}
}

func TestMergeSkipsErroredAndEmptyIDs(t *testing.T) {
	bad := newFake("web")
	bad.err = context.DeadlineExceeded
	good := newFake("live", model.Session{ID: ""}, model.Session{ID: "ok", Kind: "live", UpdatedAt: 1})
	got := Merge(bad, good)
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("errored engine and empty ids must be skipped, got %+v", got)
	}
}

func TestMergeSortOrder(t *testing.T) {
	e := newFake("store",
		model.Session{ID: "b", Kind: "stored", UpdatedAt: 2},
		model.Session{ID: "a", Kind: "stored", UpdatedAt: 2},
		model.Session{ID: "c", Kind: "stored", UpdatedAt: 7},
	)
	got := Merge(e)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if want := []string{"c", "a", "b"}; !slices.Equal(ids, want) {
		t.Fatalf("sort want %v, got %v", want, ids)
	}
}

func TestEngineLookups(t *testing.T) {
	acp := newFake("acp", model.Session{ID: "w", Kind: "web", UpdatedAt: 3})
	attach := newFake("attach", model.Session{ID: "l", Kind: "live", UpdatedAt: 2})
	store := newFake("store", model.Session{ID: "s", Kind: "stored", UpdatedAt: 1})
	r := New(model.NewHub(), acp, attach, store)
	r.refresh(false)

	if got := r.EngineFor("w"); got != model.Engine(acp) {
		t.Errorf("EngineFor(w) owner wrong")
	}
	if got := r.EngineFor("l"); got != model.Engine(attach) {
		t.Errorf("EngineFor(l) owner wrong")
	}
	if got := r.EngineFor("nope"); got != model.Engine(acp) {
		t.Errorf("EngineFor fallback must be acp, got %v", got)
	}
	if got := r.EngineForKind("live"); got != model.Engine(attach) {
		t.Errorf("EngineForKind(live) wrong")
	}
	if got := r.EngineForKind("acp"); got != model.Engine(acp) {
		t.Errorf("EngineForKind must accept engine spelling")
	}
	if got := r.EngineByName("acp"); got != model.Engine(acp) {
		t.Errorf("EngineByName(acp) wrong")
	}
	if got := r.EngineByName("store"); got != model.Engine(store) {
		t.Errorf("EngineByName(store) wrong")
	}
	if got := r.EngineByName("web"); got != model.Engine(acp) {
		t.Errorf("EngineByName must accept kind spelling")
	}
	if got := r.EngineByName("nope"); got != nil {
		t.Errorf("unknown name must be nil")
	}
}

func TestChangeDetectionPublishesOnce(t *testing.T) {
	acp := newFake("acp", model.Session{ID: "w", Kind: "web", Title: "one", UpdatedAt: 1})
	hub := model.NewHub()
	r := New(hub, acp)
	r.refresh(false) // seed, no event expected ever
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	r.refresh(true)
	if _, got := recvNow(sub); got {
		t.Fatal("unchanged refresh must not publish")
	}
	acp.set(model.Session{ID: "w", Kind: "web", Title: "two", UpdatedAt: 2})
	r.refresh(true)
	e, got := recvNow(sub)
	if !got || e.Type != EventTypeSessions {
		t.Fatalf("mutation must publish one sessions event, got %+v", e)
	}
	r.refresh(true)
	if _, got := recvNow(sub); got {
		t.Fatal("second refresh over same data must not publish again")
	}
}

func recvNow(ch chan model.Event) (model.Event, bool) {
	select {
	case e := <-ch:
		return e, true
	default:
		return model.Event{}, false
	}
}

func TestStartSeedsWithoutEventThenDebouncedRefresh(t *testing.T) {
	store := newFake("store", model.Session{ID: "s", Kind: "stored", Title: "old", UpdatedAt: 1})
	hub := model.NewHub()
	r := New(hub, store)
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Start(ctx) // sync.Once: second call is a no-op

	if list := r.Sessions(); len(list) != 1 || list[0].Title != "old" {
		t.Fatalf("Start must seed synchronously, got %+v", list)
	}
	if _, got := recvNow(sub); got {
		t.Fatal("first merge publishes nothing (ambiguity 4)")
	}

	store.set(model.Session{ID: "s", Kind: "stored", Title: "new", UpdatedAt: 2})
	r.Refresh()
	select {
	case e := <-sub:
		if e.Type != EventTypeSessions {
			t.Fatalf("want sessions event, got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("debounced refresh did not publish after mutation")
	}
	if list := r.Sessions(); len(list) != 1 || list[0].Title != "new" {
		t.Fatalf("sessions not refreshed, got %+v", list)
	}
}

func TestPollReactsWithReasonEvent(t *testing.T) {
	store := newFake("store")
	hub := model.NewHub()
	r := New(hub, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	probe := hub.Subscribe()
	defer hub.Unsubscribe(probe)

	store.set(model.Session{ID: "s1", Kind: "stored", UpdatedAt: 1})
	hub.Publish(model.Event{Type: "update", Reason: "turn_end", Session: "s1"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-probe:
			if e.Type == EventTypeSessions {
				if list := r.Sessions(); len(list) != 1 || list[0].ID != "s1" {
					t.Fatalf("sessions not refreshed, got %+v", list)
				}
				return
			}
		case <-deadline:
			t.Fatal("event-triggered refresh did not publish sessions")
		}
	}
}

func TestRefreshOn(t *testing.T) {
	cases := []struct {
		e    model.Event
		want bool
	}{
		{model.Event{Type: "status"}, true},
		{model.Event{Type: "dirty"}, true},
		{model.Event{Type: "exit"}, true},
		{model.Event{Type: "update", Reason: "turn_end"}, true},
		{model.Event{Type: "update", ACP: []byte(`{"sessionUpdate":"chunk"}`)}, false},
		{model.Event{Type: "update", ACP: []byte(`{"x":"turn_end"}`)}, true},
		{model.Event{Type: "notice", ACP: []byte(`{"x":"session/turn_end"}`)}, true},
		{model.Event{Type: "notice", Text: "hello"}, false},
		{model.Event{Type: "permission"}, false},
		{model.Event{Type: "update", Reason: "chunk"}, false},
	}
	for i, c := range cases {
		if got := refreshOn(c.e); got != c.want {
			t.Errorf("case %d: refreshOn(%+v) = %v, want %v", i, c.e, got, c.want)
		}
	}
}

func TestSessionsCloneAndFind(t *testing.T) {
	e := newFake("web", model.Session{ID: "w", Kind: "web", UpdatedAt: 1})
	r := New(model.NewHub(), e)
	r.refresh(false)
	list := r.Sessions()
	list[0].ID = "mutated"
	if got, ok := r.Find("w"); !ok || got.ID != "w" {
		t.Fatal("Sessions must return a copy")
	}
	if _, ok := r.Find("missing"); ok {
		t.Fatal("missing id must not be found")
	}
}

// row is one merged web entry; the clock is the volatile part of it.
func row(title, status string, at int64) model.Session {
	return model.Session{
		ID: "w", Kind: model.KindWeb, Title: title, Status: status,
		Cwd: "/w", Model: "m", UpdatedAt: at, PluginPID: 7,
	}
}

func withClock(s model.Session, at int64) model.Session {
	s.UpdatedAt = at
	return s
}

func withTitle(s model.Session, v string) model.Session {
	s.Title = v
	return s
}

func withStatus(s model.Session, v string) model.Session {
	s.Status = v
	return s
}

func withPID(s model.Session, v int) model.Session {
	s.PluginPID = v
	return s
}

// refreshPublishes runs one publishing refresh and reports whether it announced
// a sessions event, failing on any other event type.
func refreshPublishes(t *testing.T, r *Registry, sub chan model.Event) bool {
	t.Helper()
	r.refresh(true)
	e, got := recvNow(sub)
	if got && e.Type != EventTypeSessions {
		t.Fatalf("want a sessions event, got %+v", e)
	}
	return got
}

// TestRefreshIgnoresUpdatedAtDrift is the SSE storm fix: the merged view carries
// a clock that moves on its own, and that field alone moving must leave every
// open tab alone while the stored list still shows the fresh value.
func TestRefreshIgnoresUpdatedAtDrift(t *testing.T) {
	web := newFake("acp", row("one", model.StatusWorking, 100))
	hub := model.NewHub()
	r := New(hub, web)
	r.refresh(false)
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	web.set(row("one", model.StatusWorking, 999))
	if refreshPublishes(t, r, sub) {
		t.Fatal("a row that only moved its updated_at must not publish")
	}
	if list := r.Sessions(); len(list) != 1 || list[0].UpdatedAt != 999 {
		t.Fatalf("the stored list keeps the real updated_at: %+v", list)
	}

	web.set(withTitle(row("one", model.StatusWorking, 1000), "renamed"))
	if !refreshPublishes(t, r, sub) {
		t.Fatal("a title change must publish")
	}
	web.set(withStatus(row("renamed", model.StatusIdle, 1001), model.StatusIdle))
	if !refreshPublishes(t, r, sub) {
		t.Fatal("a status change must publish")
	}
}

func TestEqualExceptClock(t *testing.T) {
	base := row("a", model.StatusIdle, 1)
	cases := []struct {
		name string
		a, b []model.Session
		want bool
	}{
		{"same row", []model.Session{base}, []model.Session{base}, true},
		{"clock only", []model.Session{base}, []model.Session{withClock(base, 9)}, true},
		{"title", []model.Session{base}, []model.Session{withTitle(base, "b")}, false},
		{"status", []model.Session{base}, []model.Session{withStatus(base, model.StatusWorking)}, false},
		{"plugin pid", []model.Session{base}, []model.Session{withPID(base, 3)}, false},
		{"row added", []model.Session{base}, []model.Session{base, withClock(base, 9)}, false},
	}
	for _, c := range cases {
		if got := equalExceptClock(c.a, c.b); got != c.want {
			t.Errorf("%s: equalExceptClock = %v, want %v", c.name, got, c.want)
		}
	}
}
