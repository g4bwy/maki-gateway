package acpengine

import (
	"bytes"
	"encoding/json"

	"maki-gateway/internal/model"
)

// block is one native message block (CONTRACTS 3). Text and thinking payloads
// grow in place so a streamed turn stays linear.
type block struct {
	typ     string
	text    []byte
	id      string
	name    string
	input   json.RawMessage
	content []byte
	isErr   bool
}

func (b *block) json() map[string]any {
	switch b.typ {
	case blockThinking:
		return map[string]any{"type": blockThinking, "thinking": string(b.text)}
	case blockToolUse:
		input := b.input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]any{"type": blockToolUse, "id": b.id, blockToolName: b.name, "input": input}
	case blockToolDone:
		return map[string]any{
			"type":        blockToolDone,
			"tool_use_id": b.id,
			"content":     string(b.content),
			"is_error":    b.isErr,
		}
	default:
		return map[string]any{"type": blockText, "text": string(b.text)}
	}
}

type message struct {
	role   string
	blocks []*block
}

// transcript turns the ACP update stream into the native message shape. It is
// best-effort UI fuel, not a second history: maki still owns the real record.
// The limits are listed in docs/ACP_MIN.md.
type transcript struct {
	msgs []*message
	// done counts the calls whose terminal result was already recorded, so a
	// repeated completed update cannot double the tool_result.
	done map[string]bool
}

func newTranscript() *transcript {
	return &transcript{done: map[string]bool{}}
}

// user opens a turn with what the gateway sent.
func (t *transcript) user(text string) {
	t.msgs = append(t.msgs, &message{role: "user", blocks: []*block{{typ: blockText, text: []byte(text)}}})
}

type updateHead struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// feed applies one raw `params.update` object. Only the four update kinds that
// carry transcript content are read; the rest are forwarded to the browser as
// passthrough and ignored here.
func (t *transcript) feed(raw json.RawMessage) {
	var head updateHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return
	}
	switch head.SessionUpdate {
	case updateMessageChunk, updateThoughtChunk:
		t.chunk(head.SessionUpdate == updateThoughtChunk, raw)
	case updateToolCall, updateToolUpdate:
		t.toolCall(raw)
	}
}

func (t *transcript) chunk(thinking bool, raw json.RawMessage) {
	var c struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Content.Type != blockText || c.Content.Text == "" {
		return
	}
	typ := blockText
	if thinking {
		typ = blockThinking
	}
	msg := t.assistant()
	if last := lastBlock(msg); last != nil && last.typ == typ {
		last.text = append(last.text, c.Content.Text...)
		return
	}
	msg.blocks = append(msg.blocks, &block{typ: typ, text: []byte(c.Content.Text)})
}

func lastBlock(msg *message) *block {
	if len(msg.blocks) == 0 {
		return nil
	}
	return msg.blocks[len(msg.blocks)-1]
}

// toolCall records a card and, when it arrives terminal, its result too. Both
// update kinds land here: `tool_call` opens the card, `tool_call_update` fills it.
func (t *transcript) toolCall(raw json.RawMessage) {
	var u struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Name       string          `json:"name"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
		RawOutput  json.RawMessage `json:"rawOutput"`
		Content    []struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || u.ToolCallID == "" {
		return
	}
	msg := t.assistant()
	b := t.findTool(msg, u.ToolCallID)
	if b == nil {
		b = &block{typ: blockToolUse, id: u.ToolCallID}
		msg.blocks = append(msg.blocks, b)
	}
	// Same precedence as the browser reducer (web/acp.js nameOf): a tool name
	// wins over a title, and a later title replaces the pending card's bare
	// tool name with the summary maki reports at tool start.
	if name := u.Name; name != "" {
		b.name = name
	} else if u.Title != "" {
		b.name = u.Title
	}
	if len(u.RawInput) > 0 {
		b.input = u.RawInput
	}
	if terminal(u.Status) && !t.done[u.ToolCallID] {
		t.done[u.ToolCallID] = true
		msg.blocks = append(msg.blocks, &block{
			typ:     blockToolDone,
			id:      u.ToolCallID,
			content: resultText(u.RawOutput, contentText(u.Content)),
			isErr:   u.Status == statusFailed,
		})
	}
}

// findTool returns the recorded card for a call, so a repeated `tool_call` and
// the `tool_call_update` stream that follows it both land on one block.
func (t *transcript) findTool(msg *message, id string) *block {
	for _, b := range msg.blocks {
		if b.typ == blockToolUse && b.id == id {
			return b
		}
	}
	return nil
}

// assistant is the message streamed text joins, opening one when the turn has
// not produced anything yet or a user message came last.
func (t *transcript) assistant() *message {
	if n := len(t.msgs); n > 0 {
		if last := t.msgs[n-1]; last.role == "assistant" {
			return last
		}
	}
	msg := &message{role: "assistant"}
	t.msgs = append(t.msgs, msg)
	return msg
}

func terminal(status string) bool {
	return status == statusCompleted || status == statusFailed
}

// resultText prefers the tool's raw output and falls back to the content blocks
// a card carried, which is what the update stream holds for a notice card.
func resultText(raw json.RawMessage, content string) []byte {
	if len(raw) == 0 {
		return []byte(content)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s)
	}
	return append([]byte(nil), raw...)
}

func contentText(items []struct {
	Type    string `json:"type"`
	Content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}) string {
	var out bytes.Buffer
	for _, item := range items {
		if item.Type == "content" && item.Content.Type == blockText {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(item.Content.Text)
		}
	}
	return out.String()
}

// tail returns the last n messages in native shape, oldest first. n<=0 is all.
func (t *transcript) tail(n int) []model.Message {
	start := 0
	if n > 0 && n < len(t.msgs) {
		start = len(t.msgs) - n
	}
	out := make([]model.Message, 0, len(t.msgs)-start)
	for _, m := range t.msgs[start:] {
		blocks := make([]map[string]any, 0, len(m.blocks))
		for _, b := range m.blocks {
			blocks = append(blocks, b.json())
		}
		content, err := json.Marshal(blocks)
		if err != nil {
			continue
		}
		out = append(out, model.Message{Role: m.role, Content: content})
	}
	return out
}
