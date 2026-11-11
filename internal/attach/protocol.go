package attach

import (
	"encoding/json"

	"maki-gateway/internal/model"
)

// Wire shapes of CONTRACTS 1. The gateway is the client here: it sends
// {"id","op",...} and reads exactly one {"id","ok"|"error"} back, while the
// plugin pushes {"event",...} frames with no id at any time.

// fieldOK and fieldErr are the two reply fields of CONTRACTS 1: a response
// carries exactly one of them, and a response with neither is a protocol
// violation rather than an empty success.
const (
	fieldOK  = "ok"
	fieldErr = "error"
)

// message is one frame in either direction. Fields overlap on purpose: one
// decode covers responses and every event payload we map.
type message struct {
	// ID is the request number of a response and absent on an event (CONTRACTS 1).
	// It stays raw because the task_focus event carries a task name in this same
	// field, and a frame that failed to decode costs the gateway every change it
	// was reporting. requestID and taskName read the one each kind holds.
	ID    json.RawMessage `json:"id"`
	OK    json.RawMessage `json:"ok"`
	Error *string         `json:"error"`

	// Event payload. Whatever the mapping does not need is not decoded:
	// turn_end usage and cost, tool ids, and the model name have no place in the
	// SSE envelope.
	Event     string          `json:"event"`
	Session   string          `json:"session"`
	Status    string          `json:"status"`
	Title     string          `json:"title"`
	Focused   *bool           `json:"focused"`
	Tool      string          `json:"tool"`
	RequestID int64           `json:"request_id"`
	Input     json.RawMessage `json:"input"`
}

// requestID is the correlation number of a response frame, and false for a frame
// that carries no number there: an event, or one whose id is a task name.
func (m *message) requestID() (int64, bool) {
	if len(m.ID) == 0 {
		return 0, false
	}
	var id int64
	return id, json.Unmarshal(m.ID, &id) == nil
}

// taskName is the task id a task_focus frame carries in the `id` field, and "" for
// a response, whose number there is not a name.
func (m *message) taskName() string {
	var name string
	if json.Unmarshal(m.ID, &name) != nil {
		return ""
	}
	return name
}

// helloBody is the {"pid","plugin","cwd","sessions"} answer of the hello op.
type helloBody struct {
	PID      int    `json:"pid"`
	Plugin   string `json:"plugin"`
	Cwd      string `json:"cwd"`
	Sessions int    `json:"sessions"`
}

// liveSession is one entry of the list op: a tab of a running TUI. focused is
// read but not forwarded: model.Session has no such field (CONTRACTS 2), so the
// flag only travels on the SSE status event. updated_at is not in CONTRACTS 1
// either; it is decoded so a plugin that starts reporting one sorts live sessions
// on the same clock as stored ones, with no gateway change.
type liveSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Focused   bool   `json:"focused"`
	UpdatedAt int64  `json:"updated_at"`
}

// messagesBody is the {"messages":[Message]} answer of the messages op.
type messagesBody struct {
	Messages []model.Message `json:"messages"`
}

// newBody is the {"session":id} answer of the new op.
type newBody struct {
	Session string `json:"session"`
}
