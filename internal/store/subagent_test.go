package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"maki-gateway/internal/model"
)

// subLog builds one session log: a header, the given record lines, and a
// trailing meta line so the whole file counts as committed.
func subLog(id string, records ...string) string {
	lines := []string{
		`{"t":"header","v":3,"id":"` + id + `","model":"m","cwd":"/c","created_at":1781000000}`,
	}
	lines = append(lines, records...)
	lines = append(lines, `{"t":"meta","title":"`+id+`","updated_at":1781000900}`)
	return strings.Join(lines, "\n") + "\n"
}

func subMsg(task, text string) string {
	return `{"t":"sub_msg","sub":"` + task + `","d":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
}

// jsonString is one value as the log would hold it, quotes and all.
func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func payloadTexts(t *testing.T, msgs []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, raw := range msgs {
		var m struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("payload is not a Message: %v (%s)", err, raw)
		}
		text := ""
		if len(m.Content) > 0 {
			text = m.Content[0].Text
		}
		out = append(out, text)
	}
	return out
}

func TestSubMessagesFiltersByTaskAndKeepsFileOrder(t *testing.T) {
	e := fixtureEngine(t)
	addLog(t, e, "subs.jsonl", subLog("subs",
		subMsg("t-1", "one"),
		`{"t":"msg","d":{"role":"user","content":[{"type":"text","text":"main chat"}]}}`,
		subMsg("t-2", "other task"),
		subMsg("t-1", "two"),
		`{"t":"out","id":"t-1","d":"tool output"}`,
		subMsg("t-1", "three"),
	))

	got, err := e.SubMessages("subs", "t-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two", "three"}; len(got) != 3 ||
		payloadTexts(t, got)[0] != "one" || payloadTexts(t, got)[2] != "three" {
		t.Fatalf("t-1 records = %v, want %v", payloadTexts(t, got), want)
	}
	if got, err = e.SubMessages("subs", "t-2", 0); err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); len(texts) != 1 || texts[0] != "other task" {
		t.Fatalf("t-2 records = %v", texts)
	}
	// The main chat is not a sub-agent transcript, and an unknown task has none.
	for _, task := range []string{"main", "t-9"} {
		if got, err = e.SubMessages("subs", task, 0); err != nil || len(got) != 0 {
			t.Fatalf("%s transcript = %v, %v, want none", task, got, err)
		}
	}
}

// TestSubMessagesStopsAtTheLastMetaLine is the commit rule: maki ends every
// append batch with one meta line inside one write_all, so records after the last
// complete one are still arriving and must not be served.
func TestSubMessagesStopsAtTheLastMetaLine(t *testing.T) {
	e := fixtureEngine(t)
	head := `{"t":"header","v":3,"id":"mid","model":"m","cwd":"/c","created_at":1781000000}`
	body := strings.Join([]string{
		head,
		subMsg("t-1", "committed"),
		`{"t":"meta","title":"first batch","updated_at":1781000100}`,
		subMsg("t-1", "in flight"),
		`{"t":"frame","d":{"a":1}}`,
	}, "\n") + "\n" + subMsg("t-1", "torn tail")
	addLog(t, e, "mid.jsonl", body)

	got, err := e.SubMessages("mid", "t-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); len(texts) != 1 || texts[0] != "committed" {
		t.Fatalf("committed records = %v, want the one before the last meta line", texts)
	}
}

// TestSubMessagesGrowsTheWindow covers the two reasons a window has to grow: it
// holds no meta line at all, and it holds one but fewer records than asked for.
func TestSubMessagesGrowsTheWindow(t *testing.T) {
	e := fixtureEngine(t)

	// A batch wider than the first window: the meta line that commits it sits
	// past 4 KiB of records.
	var wide []string
	for range 200 {
		wide = append(wide, subMsg("t-1", strings.Repeat("x", 60)))
	}
	addLog(t, e, "wide.jsonl", subLog("wide", wide...))
	got, err := e.SubMessages("wide", "t-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("window growth lost records: %d", len(got))
	}

	// Fewer records exist than were asked for, so the reader walks the whole file
	// and answers what it found.
	records := append(wide, subMsg("t-2", "y"))
	addLog(t, e, "few.jsonl", subLog("few", records...))
	if got, err = e.SubMessages("few", "t-2", 400); err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); len(texts) != 1 || texts[0] != "y" {
		t.Fatalf("exhausted scan = %v", texts)
	}

	// A log whose only meta line is half-written commits nothing.
	addLog(t, e, "nometa.jsonl", `{"t":"header","v":3,"id":"nometa","model":"m","cwd":"/c","created_at":1}`+
		"\n"+subMsg("t-1", "uncommitted")+"\n"+"{\"t\":\"meta\",\"title\":\"cut\"")
	if got, err = e.SubMessages("nometa", "t-1", 0); err != nil || len(got) != 0 {
		t.Fatalf("meta-less log = %v, %v, want none", got, err)
	}
}

func TestSubMessagesLastTakesTheNewestPage(t *testing.T) {
	e := fixtureEngine(t)
	var records []string
	for i := range 30 {
		records = append(records, subMsg("t-1", fmt.Sprintf("m%02d", i)))
	}
	addLog(t, e, "many.jsonl", subLog("many", records...))

	got, err := e.SubMessages("many", "t-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); strings.Join(texts, ",") != "m27,m28,m29" {
		t.Fatalf("last 3 = %v, want the newest three in file order", texts)
	}
	if got, err = e.SubMessages("many", "t-1", 999); err != nil {
		t.Fatal(err)
	} else if len(got) != 30 {
		t.Fatalf("an over-large last is the cap, not a refusal: %d", len(got))
	}
}

// TestSubMessagesCapsAtMaxRecords is the ceiling one request may cost: a log with
// more records than the cap answers the newest maxSubMessages of them.
func TestSubMessagesCapsAtMaxRecords(t *testing.T) {
	e := fixtureEngine(t)
	var records []string
	for i := range maxSubMessages + 40 {
		records = append(records, subMsg("t-1", fmt.Sprintf("m%04d", i)))
	}
	addLog(t, e, "cap.jsonl", subLog("cap", records...))

	for _, last := range []int{0, -1, maxSubMessages, maxSubMessages + 100} {
		got, err := e.SubMessages("cap", "t-1", last)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != maxSubMessages {
			t.Fatalf("last=%d returned %d records, want the cap %d", last, len(got), maxSubMessages)
		}
	}
	got, err := e.SubMessages("cap", "t-1", maxSubMessages)
	if err != nil {
		t.Fatal(err)
	}
	texts := payloadTexts(t, got)
	if texts[0] != "m0040" || texts[len(texts)-1] != fmt.Sprintf("m%04d", maxSubMessages+39) {
		t.Fatalf("capped page = %v … %v, want the newest %d", texts[0], texts[len(texts)-1], maxSubMessages)
	}
}

// TestSubMessagesSkipsUnparseableLines keeps one bad record from failing a page.
// The prefix match runs on the raw line, so a body that is not JSON at all costs
// nothing but the skip.
func TestSubMessagesSkipsUnparseableLines(t *testing.T) {
	e := fixtureEngine(t)
	addLog(t, e, "junk.jsonl", subLog("junk",
		subMsg("t-1", "good"),
		`{"t":"sub_msg","sub":"t-1","d":`,
		`{"t":"sub_msg","sub":"t-1"}`,
		`{"t":"sub_msg","sub":"t-1x","d":{"role":"user","content":[]}}`,
		subMsg("t-1", "also good"),
	))
	got, err := e.SubMessages("junk", "t-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); len(texts) != 2 || texts[1] != "also good" {
		t.Fatalf("records after a broken line = %v", texts)
	}
}

// TestSubMessagesMatchesAtTheLineStartOnly is the cheap filter doing its job: a
// record that only quotes another task's prefix inside its body is not a record of
// that task, so it is skipped on the raw bytes and never parsed.
func TestSubMessagesMatchesAtTheLineStartOnly(t *testing.T) {
	e := fixtureEngine(t)
	quoted := `{"t":"msg","d":{"role":"assistant","content":[{"type":"text","text":` +
		jsonString(subMsg("t-1", "quoted inside a main-chat record")) + `}]}}`
	addLog(t, e, "quoted.jsonl", subLog("quoted", quoted, subMsg("t-1", "real")))

	got, err := e.SubMessages("quoted", "t-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if texts := payloadTexts(t, got); len(texts) != 1 || texts[0] != "real" {
		t.Fatalf("records = %v, want only the line whose own start matched", texts)
	}
}

// TestSubMessagesRejectsUnknownSessionAndTaskID is the 404 half of the route.
func TestSubMessagesRejectsUnknownSessionAndTaskID(t *testing.T) {
	e := fixtureEngine(t, "alpha")
	if _, err := e.SubMessages("alpha", "t-1", 0); err != nil {
		t.Fatalf("a session with no records of this task is an empty answer: %v", err)
	}
	for _, id := range []string{"gone", "../etc/passwd", "alpha "} {
		if _, err := e.SubMessages(id, "t-1", 0); err != model.ErrNotFound {
			t.Errorf("session %q: want ErrNotFound, got %v", id, err)
		}
	}
	for _, task := range []string{"", `t-1"`, "t-1,\"d\":{}", "t 1"} {
		if _, err := e.SubMessages("alpha", task, 0); err != model.ErrNotFound {
			t.Errorf("task %q: want ErrNotFound, got %v", task, err)
		}
	}
}
