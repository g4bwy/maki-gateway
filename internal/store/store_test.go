package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

// allFixtures is every testdata session, so the list assertions also prove the
// v9 log is skipped and the truncated one still reads.
var allFixtures = []string{"alpha", "beta", "gamma", "delta", "zeta", "future"}

// fixtureEngine copies named testdata logs into a throwaway state dir.
func fixtureEngine(t *testing.T, names ...string) *Engine {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, sessionsSub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", n+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n+".jsonl"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// writeLogFile drops one raw log in a throwaway dir and returns its path.
func writeLogFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// addLog writes one raw log into the engine's sessions dir.
func addLog(t *testing.T, e *Engine, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewToleratesMissingSessionsDir(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("missing dir must not fail wiring: %v", err)
	}
	got, err := e.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty list, got %v", got)
	}
	if _, err := New(""); err == nil {
		t.Fatal("want error for empty state dir")
	}
}

func TestListProjectsAndSortsByUpdatedAtDesc(t *testing.T) {
	got, err := fixtureEngine(t, allFixtures...).List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	want := []string{"zeta", "delta", "gamma", "beta", "alpha"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("order/ids:\n got %v\nwant %v", ids, want)
	}
	byID := map[string]model.Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if s := byID["alpha"]; s.Title != "rename the greeter module to hello" {
		t.Errorf("last meta must win: %q", s.Title)
	}
	if s := byID["alpha"]; s.UpdatedAt != 1781000900 || s.Kind != model.KindStored ||
		s.Status != model.StatusIdle || s.Cwd != "/home/dev/alpha" ||
		s.Model != "anthropic/claude-opus-4.1" || s.PluginPID != 0 {
		t.Errorf("unexpected projection: %+v", s)
	}
	// zeta has no meta: created_at and an empty title are the fallback.
	if s := byID["zeta"]; s.Title != "" || s.UpdatedAt != 1781400000 {
		t.Errorf("meta-less fallback: %+v", s)
	}
	// gamma's last line is a half-written meta: the complete one before it wins.
	if s := byID["gamma"]; s.Title != "truncated reader check" || s.UpdatedAt != 1781200100 {
		t.Errorf("truncated tail: %+v", s)
	}
	// delta is the {"t":"header","d":{..}} shape from CONTRACTS 3.
	if s := byID["delta"]; s.Title != "nested shape" || s.Cwd != "/home/dev/delta" ||
		s.Model != "xai/grok-4" || s.UpdatedAt != 1781300500 {
		t.Errorf("nested-d shape: %+v", s)
	}
}

func TestListJSONMatchesContractShape(t *testing.T) {
	got, err := fixtureEngine(t, "alpha").List()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"alpha","kind":"stored","title":"rename the greeter module to hello",` +
		`"status":"idle","cwd":"/home/dev/alpha","model":"anthropic/claude-opus-4.1",` +
		`"updated_at":1781000900}`
	if string(b) != want {
		t.Fatalf("json:\n got %s\nwant %s", b, want)
	}
}

func TestMessagesPassesBlocksThroughAndKeepsLastN(t *testing.T) {
	e := fixtureEngine(t, "alpha")
	all, err := e.Messages("alpha", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("out/frame/meta lines must not be messages: %d", len(all))
	}
	if all[0].Role != "user" || all[1].Role != "assistant" {
		t.Errorf("roles: %+v", all[:2])
	}
	if all[0].CreatedAt != nil {
		t.Errorf("absent created_at must stay nil: %+v", all[0])
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(all[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || string(blocks[0]["type"]) != `"thinking"` {
		t.Fatalf("content passthrough: %s", all[1].Content)
	}
	last, err := e.Messages("alpha", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || last[1].Role != "assistant" {
		t.Fatalf("want the 2 newest, got %+v", last)
	}
	if !strings.Contains(string(last[0].Content), "tool_result") {
		t.Errorf("want the tool_result record, got %s", last[0].Content)
	}
}

func TestMessagesRejectsUnknownAndUnsafeIDs(t *testing.T) {
	e := fixtureEngine(t, allFixtures...)
	for _, id := range []string{"nope", "../alpha", "a/b", "", ".."} {
		if _, err := e.Messages(id, 10); err != model.ErrNotFound {
			t.Errorf("id %q: got %v, want ErrNotFound", id, err)
		}
	}
}

func TestReadMetaGrowsPastTheTailWindow(t *testing.T) {
	pad := strings.Repeat("x", tailWindow)
	body := fmt.Sprintf(
		"{\"t\":\"header\",\"v\":3,\"id\":\"big\",\"model\":\"m\",\"cwd\":\"/c\",\"created_at\":1}\n"+
			"{\"t\":\"meta\",\"title\":\"wide meta\",\"updated_at\":2,\"subagents\":[{\"name\":\"%s\"}]}\n", pad)
	got, ok := readMeta(writeLogFile(t, "big.jsonl", body))
	if !ok || got.Title != "wide meta" {
		t.Fatalf("want the meta wider than %d bytes, got %+v ok=%v", tailWindow, got, ok)
	}
}

func TestReadMetaIgnoresPartialTrailingLine(t *testing.T) {
	path := writeLogFile(t, "half.jsonl",
		"{\"t\":\"header\",\"v\":3,\"id\":\"half\",\"model\":\"m\",\"cwd\":\"/c\",\"created_at\":1}\n"+
			"{\"t\":\"meta\",\"title\":\"good\",\"updated_at\":2}\n"+
			"{\"t\":\"meta\",\"title\":\"unfinish")
	got, ok := readMeta(path)
	if !ok || got.Title != "good" {
		t.Fatalf("partial tail must be ignored: %+v ok=%v", got, ok)
	}
}

func TestHasTagAndBodyFallbacks(t *testing.T) {
	if hasTag([]byte(`{"t":"msg","d":{}}`), metaTag) || !hasTag([]byte(`{"t":"meta","title":"x"}`), metaTag) {
		t.Fatal("tag detection")
	}
	nested := metaLine{}
	if err := json.Unmarshal([]byte(`{"t":"meta","d":{"title":"n","updated_at":7}}`), &nested); err != nil {
		t.Fatal(err)
	}
	if b := nested.body(); b.Title != "n" || b.UpdatedAt != 7 {
		t.Fatalf("nested body: %+v", b)
	}
	flat := metaLine{}
	if err := json.Unmarshal([]byte(`{"t":"meta","title":"f","updated_at":8}`), &flat); err != nil {
		t.Fatal(err)
	}
	if b := flat.body(); b.Title != "f" || b.UpdatedAt != 8 {
		t.Fatalf("flat body: %+v", b)
	}
}

func TestSearchReturnsOneHitPerSessionWithTitle(t *testing.T) {
	e := fixtureEngine(t, allFixtures...)
	got, err := e.Search("greeter", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Session != "alpha" {
		t.Fatalf("hits: %+v", got)
	}
	if got[0].Title != "rename the greeter module to hello" {
		t.Errorf("title: %+v", got[0])
	}
	if !strings.Contains(got[0].Snippet, "greeter") {
		t.Errorf("snippet: %q", got[0].Snippet)
	}
	if hits, err := e.Search("TRUNCATED READER", 0); err != nil || len(hits) != 1 ||
		hits[0].Session != "gamma" {
		t.Fatalf("case-insensitive: %+v %v", hits, err)
	}
	if hits, err := e.Search("", 0); err != nil || hits != nil {
		t.Fatalf("empty query: %+v %v", hits, err)
	}
	if hits, err := e.Search("nothing matches this", 0); err != nil || len(hits) != 0 {
		t.Fatalf("no match: %+v %v", hits, err)
	}
}

func TestSearchSnippetsAreBoundedAndSkipImages(t *testing.T) {
	long := strings.Repeat("a", 500) + "needle" + strings.Repeat("b", 500)
	e := fixtureEngine(t)
	addLog(t, e, "wide.jsonl",
		"{\"t\":\"header\",\"v\":3,\"id\":\"wide\",\"model\":\"m\",\"cwd\":\"/c\",\"created_at\":1}\n"+
			fmt.Sprintf("{\"t\":\"msg\",\"d\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}\n", long)+
			"{\"t\":\"msg\",\"d\":{\"role\":\"user\",\"content\":[{\"type\":\"image\",\"source\":{\"data\":\"needle\"}}]}}\n")
	got, err := e.Search("needle", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("image line must not match: %+v", got)
	}
	snip := got[0].Snippet
	if !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Errorf("want ellipses on both cut ends: %q", snip)
	}
	if n := len([]rune(snip)); n > 2*snippetPad+len("needle")+2*len("…") {
		t.Errorf("snippet too wide: %d runes", n)
	}
	if strings.Contains(snip, "\n") {
		t.Errorf("snippet must stay on one line: %q", snip)
	}
}

func TestSearchCapsHits(t *testing.T) {
	e := fixtureEngine(t)
	for i := range maxSearchHits + 10 {
		addLog(t, e, fmt.Sprintf("s%02d.jsonl", i), fmt.Sprintf(
			"{\"t\":\"header\",\"v\":3,\"id\":\"s%02d\",\"model\":\"m\",\"cwd\":\"/c\",\"created_at\":1}\n"+
				"{\"t\":\"msg\",\"d\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"findme %d\"}]}}\n", i, i))
	}
	hits, err := e.Search("findme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != maxSearchHits {
		t.Fatalf("want %d hits, got %d", maxSearchHits, len(hits))
	}
}

func TestAnalyticsSumsUsageByModelAndBucketsByDay(t *testing.T) {
	got, err := fixtureEngine(t, allFixtures...).analytics(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PerSession) != 5 {
		t.Fatalf("per_session: %+v", got.PerSession)
	}
	if got.PerSession[0].Session != "zeta" || got.PerSession[len(got.PerSession)-1].Session != "alpha" {
		t.Fatalf("per_session order: %+v", got.PerSession)
	}
	alpha := got.PerSession[len(got.PerSession)-1]
	if alpha.Model != "anthropic/claude-opus-4.1" || alpha.Cost != 0.25 || alpha.UpdatedAt != 1781000900 {
		t.Fatalf("alpha row: %+v", alpha)
	}
	// zeta has no meta at all, so it reports its header clock and zero usage.
	if z := got.PerSession[0]; z.UpdatedAt != 1781400000 || z.Model != "llama-cpp/qwen" || z.Cost != 0 {
		t.Fatalf("zeta row: %+v", z)
	}
	// beta has no cost in usage_by_model, so its day carries only tokens.
	dates := map[string]DailyStat{}
	for _, d := range got.Daily {
		dates[d.Date] = d
	}
	if len(got.Daily) != 5 {
		t.Fatalf("one bucket per day: %+v", got.Daily)
	}
	if got.Daily[0].Date > got.Daily[4].Date {
		t.Fatalf("daily must ascend: %+v", got.Daily)
	}
	wantDate := time.Unix(1781000900, 0).In(time.Local).Format(dayLayout)
	ad, ok := dates[wantDate]
	if !ok {
		t.Fatalf("missing %s in %+v", wantDate, got.Daily)
	}
	if ad.Sessions != 1 || ad.Input != 2400 || ad.Output != 310 ||
		ad.CacheRead != 1800 || ad.CacheCreation != 500 || ad.Cost != 0.25 {
		t.Fatalf("alpha day: %+v", ad)
	}
	// gamma/beta fall back to token_usage when usage_by_model is absent.
	bd := dates[time.Unix(1781100600, 0).In(time.Local).Format(dayLayout)]
	if bd.Input != 40 || bd.Output != 12 {
		t.Fatalf("beta day (token_usage fallback): %+v", bd)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"daily"`, `"per_session"`, `"cache_read"`, `"cache_creation"`, `"updated_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("analytics json must carry %s: %s", key, b)
		}
	}
}

func TestAnalyticsDaysFilterIsLocalCalendarDays(t *testing.T) {
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1).Unix()
	lastWeek := now.AddDate(0, 0, -7).Unix()
	e := fixtureEngine(t)
	log := func(id string, at int64) {
		addLog(t, e, id+".jsonl", fmt.Sprintf(
			"{\"t\":\"header\",\"v\":3,\"id\":\"%s\",\"model\":\"m\",\"cwd\":\"/c\",\"created_at\":%d}\n"+
				"{\"t\":\"meta\",\"title\":\"%s\",\"updated_at\":%d,\"usage_by_model\":{\"m\":{\"input\":1,\"output\":1,\"cost\":0.1}}}\n",
			id, at, id, at))
	}
	log("today", now.Unix())
	log("yesterday", yesterday)
	log("lastweek", lastWeek)

	got, err := e.analytics(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PerSession) != 1 || got.PerSession[0].Session != "today" {
		t.Fatalf("days=1 is today only: %+v", got.PerSession)
	}
	if len(got.Daily) != 1 || got.Daily[0].Sessions != 1 || got.Daily[0].Cost != 0.1 {
		t.Fatalf("days=1 daily: %+v", got.Daily)
	}
	if got, err = e.analytics(7); err != nil {
		t.Fatal(err)
	} else if len(got.PerSession) != 2 {
		t.Fatalf("days=7 is today plus 6 more, so lastweek is out: %+v", got.PerSession)
	}
	if got, err = e.analytics(30); err != nil {
		t.Fatal(err)
	} else if len(got.PerSession) != 3 {
		t.Fatalf("days=30 covers the week: %+v", got.PerSession)
	}
	if got, err = e.analytics(-1); err != nil {
		t.Fatal(err)
	} else if len(got.PerSession) != 3 {
		t.Fatalf("days<=0 must not filter: %+v", got.PerSession)
	}
}

func TestReadOnlyEngineMethods(t *testing.T) {
	e := fixtureEngine(t, "alpha")
	if e.Kind() != model.KindStored {
		t.Fatalf("kind: %s", e.Kind())
	}
	if _, err := e.Prompt("alpha", "hi"); err != model.ErrUnsupported {
		t.Errorf("prompt: %v", err)
	}
	if _, err := e.Create(model.CreateRequest{Cwd: "/c", Model: "m", Prompt: "p"}); err != model.ErrUnsupported {
		t.Errorf("create: %v", err)
	}
	for _, err := range []error{
		e.Cancel("alpha"), e.SetMode("alpha", "plan"), e.SetTitle("alpha", "t"), e.Kill("alpha"),
	} {
		if err != model.ErrUnsupported {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	}
	if err := e.RespondPermission(1, "allow", ""); err != model.ErrNotFound {
		t.Errorf("respond: want ErrNotFound, got %v", err)
	}
}

func TestListSurvivesAFileShrinkingUnderIt(t *testing.T) {
	e := fixtureEngine(t, "alpha")
	path := filepath.Join(e.dir, "alpha.jsonl")
	head, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// maki rewrites logs in place and can truncate mid-read: keep line 1 only.
	first := strings.IndexByte(string(head), '\n')
	if err := os.WriteFile(path, head[:first+1], 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := e.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "" || got[0].UpdatedAt != 1781000000 {
		t.Fatalf("header-only fallback: %+v", got)
	}
}
