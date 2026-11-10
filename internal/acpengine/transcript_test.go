package acpengine

import (
	"encoding/json"
	"testing"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// feedAll reduces a whole turn and hands back the single assistant message it
// left behind.
func feedAll(t *testing.T, updates ...any) *transcript {
	t.Helper()
	tr := newTranscript()
	tr.user("go")
	for _, u := range updates {
		tr.feed(raw(t, u))
	}
	return tr
}

func blocksOf(t *testing.T, tr *transcript, msg int) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	if err := json.Unmarshal(tr.tail(0)[msg].Content, &out); err != nil {
		t.Fatalf("message %d: %v", msg, err)
	}
	return out
}

func toolCard(blocks []map[string]any) map[string]any {
	for _, b := range blocks {
		if b["type"] == blockToolUse {
			return b
		}
	}
	return nil
}

func TestToolResultIsRecordedOncePerCall(t *testing.T) {
	tr := feedAll(t,
		map[string]any{"sessionUpdate": updateToolCall, "toolCallId": "c1", "title": "bash", "status": statusPending},
		map[string]any{"sessionUpdate": updateToolUpdate, "toolCallId": "c1", "status": statusCompleted, "rawOutput": "ok"},
		map[string]any{"sessionUpdate": updateToolUpdate, "toolCallId": "c1", "status": statusCompleted},
	)
	blocks := blocksOf(t, tr, 1)
	var results int
	for _, b := range blocks {
		if b["type"] == blockToolDone {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("tool_result blocks = %d, want 1: %+v", results, blocks)
	}
}

// TestToolNameFollowsTheCard pins the choice the browser makes too: the pending
// card names the tool, and the summary that arrives with the update wins.
func TestToolNameFollowsTheCard(t *testing.T) {
	tr := feedAll(t,
		map[string]any{"sessionUpdate": updateToolCall, "toolCallId": "c1", "title": "bash", "status": statusPending},
		map[string]any{"sessionUpdate": updateToolUpdate, "toolCallId": "c1", "title": "bash: ls -1"},
	)
	if got := toolCard(blocksOf(t, tr, 1))[blockToolName]; got != "bash: ls -1" {
		t.Fatalf("name = %v", got)
	}

	tr = feedAll(t,
		map[string]any{"sessionUpdate": updateToolCall, "toolCallId": "c1", "title": "bash"},
		map[string]any{"sessionUpdate": updateToolUpdate, "toolCallId": "c1", "name": "grep"},
	)
	if got := toolCard(blocksOf(t, tr, 1))[blockToolName]; got != "grep" {
		t.Fatalf("an explicit name must win, got %v", got)
	}
}

func TestUnknownAndUserUpdatesDoNotOpenBlocks(t *testing.T) {
	tr := feedAll(t,
		map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": blockText, "text": "echo"}},
		map[string]any{"sessionUpdate": "usage_update", "used": 12, "size": 4096},
		map[string]any{"sessionUpdate": "available_commands_update"},
		map[string]any{"sessionUpdate": updateMessageChunk, "content": map[string]any{"type": blockText, "text": "real"}},
	)
	msgs := tr.tail(0)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the prompt and one reply", len(msgs))
	}
	blocks := blocksOf(t, tr, 1)
	if len(blocks) != 1 || blocks[0]["text"] != "real" {
		t.Fatalf("blocks = %+v, want only the agent text", blocks)
	}
}

// TestResultTextPrefersTheRawOutput covers the three sources the reducer reads,
// in the order it reads them.
func TestResultText(t *testing.T) {
	if got := string(resultText(raw(t, "plain"), "fallback")); got != "plain" {
		t.Errorf("string rawOutput = %q", got)
	}
	if got := string(resultText(raw(t, map[string]int{"n": 1}), "")); got != `{"n":1}` {
		t.Errorf("object rawOutput = %q", got)
	}
	if got := string(resultText(nil, "from content")); got != "from content" {
		t.Errorf("absent rawOutput = %q", got)
	}
}

func TestTailKeepsTheLastN(t *testing.T) {
	tr := newTranscript()
	for range 5 {
		tr.user("x")
	}
	if got := len(tr.tail(2)); got != 2 {
		t.Errorf("tail(2) = %d", got)
	}
	if got := len(tr.tail(0)); got != 5 {
		t.Errorf("tail(0) = %d, want all", got)
	}
	if got := len(tr.tail(50)); got != 5 {
		t.Errorf("tail(50) = %d", got)
	}
}

func TestOptionForFallsBackToKind(t *testing.T) {
	// An agent that renamed its options still gets a sane answer by kind.
	renamed := []permissionOption{
		{OptionID: "yes-please", Kind: kindAllowAlways},
		{OptionID: "no-thanks", Kind: kindRejectOnce},
	}
	if got, err := optionFor(renamed, decisionAllow); err != nil || got != "yes-please" {
		t.Errorf("allow = %q, %v", got, err)
	}
	if got, err := optionFor(renamed, decisionDeny); err != nil || got != "no-thanks" {
		t.Errorf("deny = %q, %v", got, err)
	}
	if _, err := optionFor(renamed, "maybe"); err == nil {
		t.Error("an unknown decision must not pick an option")
	}
	if _, err := optionFor(nil, decisionAllow); err == nil {
		t.Error("no options must not pick an option")
	}
}
