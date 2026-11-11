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
	methodSessionLoad       = "session/load"
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

	optionAllowOnce   = "allow_once"
	optionAllowAlways = "allow_always"
	optionRejectOnce  = "reject_once"
	kindAllowOnce     = "allow_once"
	kindAllowAlways   = "allow_always"
	kindRejectOnce    = "reject_once"
	kindRejectAlways  = "reject_always"

	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
	actionCancel     = "cancel"

	// The three decisions of CONTRACTS 2, spelled once in internal/model.
	decisionAllow       = model.DecisionAllow
	decisionAllowAlways = model.DecisionAllowAlways
	decisionDeny        = model.DecisionDeny

	modeBuild = "build"
	modePlan  = "plan"

	optionModel = "model"

	noticeQuestion = "question asked; answer in terminal"

	// JSON-RPC codes the server answers with (docs/ACP_MIN.md).
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternal       = -32603
	codeAuthOrBusy     = -32000

	// codeNotFound is the schema's resource_not_found: the agent has no session
	// under the id a method named (`server.rs:331,742`). It is a plain answer to a
	// request for a chat that is gone, so it is not a gateway fault.
	codeNotFound = -32002

	// codeGone is not a JSON-RPC code. It labels only a reply this package
	// synthesizes when the child died before answering.
	codeGone = 1

	// busyPromptText is what maki-acp answers a duplicate prompt with.
	busyPromptText = "already running"

	argACP   = "acp"
	argModel = "-m"
	// argYolo is the acp subcommand's own flag, not the global one (see spawn).
	argYolo = "--yolo"
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
//
// -32002 is the agent saying it has no such session. Before that case existed a
// prompt or a load of an id the child never opened reached the browser as a 500,
// which reads as a broken gateway rather than as a chat that has ended.
func mapWireError(e *wireError) error {
	switch {
	case e.Code == codeInternal && strings.Contains(e.Message, busyPromptText):
		return model.ErrBusy
	case e.Code == codeAuthOrBusy:
		return model.ErrOpenElsewhere
	case e.Code == codeInvalidRequest:
		return fmt.Errorf("%w: no active session", model.ErrNotFound)
	case e.Code == codeNotFound:
		return fmt.Errorf("%w: %s", model.ErrNotFound, e.Message)
	case e.Code == codeGone:
		return errGone
	default:
		return e
	}
}

var errNoOption = errors.New("permission offers no matching option")

// optionFor picks the option id a decision names. The four ids maki-acp offers
// are fixed (permissions.rs:6-9), but the answer is still read out of the options
// of this one request, and anything the agent did not offer is refused rather
// than guessed at.
//
//	allow        -> allow_once, then any allow-shaped option
//	allow_always -> allow_always, then allow_once
//	deny         -> reject_once, then any reject-shaped option
//
// The allow_always fallback drops the "always" part. An agent that offers no
// "always" option cannot be told to remember the answer, so the turn is allowed
// once and the next call asks again. maki always offers all four options, so this
// only bites a different ACP agent.
func optionFor(options []permissionOption, decision string) (string, error) {
	switch decision {
	case decisionAllow:
		return pickOption(options, optionAllowOnce, kindAllowOnce, kindAllowAlways)
	case decisionAllowAlways:
		if id, ok := findOption(options, optionAllowAlways, kindAllowAlways); ok {
			return id, nil
		}
		return pickOption(options, optionAllowOnce, kindAllowOnce)
	case decisionDeny:
		return pickOption(options, optionRejectOnce, kindRejectOnce, kindRejectAlways)
	}
	return "", fmt.Errorf("%w: unknown decision %q", errNoOption, decision)
}

// pickOption answers the first option an id or kind names, or the error.
func pickOption(options []permissionOption, want string, kinds ...string) (string, error) {
	id, ok := findOption(options, want, kinds...)
	if !ok {
		return "", fmt.Errorf("%w: %s", errNoOption, want)
	}
	return id, nil
}

// findOption looks for an exact optionId first, then for any option of these
// kinds, because a client names what it wants and the agent spells it.
func findOption(options []permissionOption, want string, kinds ...string) (string, bool) {
	for _, o := range options {
		if o.OptionID == want {
			return o.OptionID, true
		}
	}
	for _, o := range options {
		for _, kind := range kinds {
			if o.Kind == kind {
				return o.OptionID, true
			}
		}
	}
	return "", false
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
