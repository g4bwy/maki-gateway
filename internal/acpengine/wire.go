package acpengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"maki-gateway/internal/model"
)

// Wire constants. Every name here is quoted from the Rust server in
// /home/gab/test/maki/maki-acp/src with the file:line in docs/ACP_MIN.md.
const (
	jsonrpcVersion = "2.0"
	acpProtocol    = 1

	methodInitialize        = "initialize"
	methodSessionNew        = "session/new"
	methodSessionPrompt     = "session/prompt"
	methodSessionCancel     = "session/cancel"
	methodSessionSetMode    = "session/set_mode"
	methodSessionSetConfig  = "session/set_config_option"
	methodSessionUpdate     = "session/update"
	methodRequestPermission = "session/request_permission"
	methodElicitationCreate = "elicitation/create"

	updateMessageChunk = "agent_message_chunk"
	updateThoughtChunk = "agent_thought_chunk"
	updateToolCall     = "tool_call"
	updateToolUpdate   = "tool_call_update"
	updateConfigOption = "config_option_update"
	updateSessionInfo  = "session_info_update"

	blockText     = "text"
	blockThinking = "thinking"
	blockToolUse  = "tool_use"
	blockToolName = "name"
	blockToolDone = "tool_result"

	statusPending    = "pending"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"

	optionAllowOnce  = "allow_once"
	optionRejectOnce = "reject_once"
	kindAllowOnce    = "allow_once"
	kindAllowAlways  = "allow_always"
	kindRejectOnce   = "reject_once"
	kindRejectAlways = "reject_always"

	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
	actionCancel     = "cancel"

	decisionAllow = "allow"
	decisionDeny  = "deny"

	modeBuild = "build"
	modePlan  = "plan"

	optionModel = "model"

	noticeQuestion = "question asked; answer in terminal"

	// JSON-RPC codes the server answers with (docs/ACP_MIN.md).
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternal       = -32603
	codeAuthOrBusy     = -32000

	// codeGone is not a JSON-RPC code. It labels only a reply this package
	// synthesizes when the child died before answering.
	codeGone = 1

	// busyPromptText is what maki-acp answers a duplicate prompt with.
	busyPromptText = "already running"

	argACP   = "acp"
	argModel = "-m"
)

// wireRequest is one outgoing frame. A nil ID makes it a notification.
type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

// wireMessage is any incoming frame: request, notification, or response.
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *wireError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("acp error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("acp error %d: %s", e.Code, e.Message)
}

// kind reports whether the frame answers a request, asks one, or announces one.
func (m *wireMessage) kind() string {
	switch {
	case m.Method == "" && (len(m.Result) > 0 || m.Error != nil):
		return "response"
	case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
		return "request"
	default:
		return "notification"
	}
}

// result returns the payload of a response, or its mapped error.
func (m *wireMessage) result() (json.RawMessage, error) {
	if m.Error != nil {
		return nil, mapWireError(m.Error)
	}
	return m.Result, nil
}

// mapWireError turns an ACP error into the gateway errors CONTRACTS 2 defines.
//
// -32000 has two meanings on this wire. The schema calls it auth_required
// (error.rs:211), and maki-acp answers a session that another maki holds with
// the same number (server.rs:55, load_error at server.rs:736). CONTRACTS 2 maps
// it to open-elsewhere, so that is what it means here. A failed turn reports a
// provider rejection with the same number (server.rs:1090), and the only place
// that reaches a caller is a turn that dies inside the admission wait. The
// notice always quotes the agent, so the user reads the real reason either way.
func mapWireError(e *wireError) error {
	switch {
	case e.Code == codeInternal && strings.Contains(e.Message, busyPromptText):
		return model.ErrBusy
	case e.Code == codeAuthOrBusy:
		return model.ErrOpenElsewhere
	case e.Code == codeInvalidRequest:
		return fmt.Errorf("%w: no active session", model.ErrNotFound)
	case e.Code == codeGone:
		return errGone
	default:
		return e
	}
}

var errNoOption = errors.New("permission offers no matching option")

// optionFor picks the option id a decision names. allow prefers the literal
// allow_once and falls back to any allow-shaped kind; deny rejects once.
// Anything the server did not offer is refused rather than guessed at.
func optionFor(options []permissionOption, decision string) (string, error) {
	want, kinds := optionAllowOnce, []string{kindAllowOnce, kindAllowAlways}
	if decision == decisionDeny {
		want, kinds = optionRejectOnce, []string{kindRejectOnce, kindRejectAlways}
	} else if decision != decisionAllow {
		return "", fmt.Errorf("%w: unknown decision %q", errNoOption, decision)
	}
	for _, o := range options {
		if o.OptionID == want {
			return o.OptionID, nil
		}
	}
	for _, o := range options {
		for _, kind := range kinds {
			if o.Kind == kind {
				return o.OptionID, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", errNoOption, decision)
}

// permissionOption is one entry of `options` in session/request_permission.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// permissionRequest is the params of session/request_permission. ToolCall is a
// ToolCallUpdate: its optional fields are flattened next to toolCallId.
type permissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
	} `json:"toolCall"`
	Options []permissionOption `json:"options"`
}
