package attach

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"maki-gateway/internal/model"
)

// Wire constants of docs/WIRE.md, the frozen v2 attach protocol. The gateway is
// the ACP client: it sends requests, and it answers the one request the plugin
// sends (session/request_permission) on the same connection. Framing, ids,
// deadlines and the error object come from internal/acpwire.
const (
	// acpProtocol is the ACP version this gateway offers (WIRE 2.1). The attach
	// build number is a maki fact, so it travels in _meta, not in that field.
	acpProtocol = 1

	// minAttachProto is the lowest attach_proto this gateway accepts. Anything
	// under it speaks v1 and is refused (WIRE 2.2).
	minAttachProto = 2

	// pluginIDFloor is where a plugin starts numbering its own requests, so the
	// two directions do not share a number space (WIRE 1.4).
	pluginIDFloor = 1000
)

// Methods this gateway sends, from the index of WIRE 0.
const (
	mInitialize  = "initialize"
	mSessionList = "session/list"
	// mSessionLoad reads a tab's history as ACP updates (WIRE 4). This gateway
	// reads a transcript through _sh.maki/messages instead, because WIRE 6 says an
	// update stream cannot rebuild a native record, so only a plugin that offers
	// the replay has to answer it.
	mSessionLoad       = "session/load"
	mSessionNew        = "session/new"
	mSessionCancel     = "session/cancel"
	mSessionSetMode    = "session/set_mode"
	mSessionSetTitle   = "session/set_title"
	mMessages          = "_sh.maki/messages"
	mQueue             = "_sh.maki/queue"
	mNotify            = "_sh.maki/notify"
	mPlan              = "_sh.maki/plan"
	mTasks             = "_sh.maki/tasks"
	mTaskFocus         = "_sh.maki/task_focus"
	mRunCommand        = "_sh.maki/run_command"
	mPopQueue          = "_sh.maki/pop_queue"
	mRequestPermission = "session/request_permission"
)

// Methods the plugin pushes or asks (WIRE 11, 10).
const (
	mSessionUpdate = "session/update"
	mSessionEvent  = "_sh.maki/session_event"
)

// _meta keys of WIRE 2.1, 3.1 and 10.1.
const (
	metaPID         = "sh.maki/pid"
	metaCwd         = "sh.maki/cwd"
	metaAttachProto = "sh.maki/attach_proto"
	metaMakiVersion = "sh.maki/maki_version"
	metaStatus      = "sh.maki/status"
	metaModel       = "sh.maki/model"
	metaMode        = "sh.maki/mode"
	metaFocused     = "sh.maki/focused"
	metaGuidance    = "sh.maki/guidance"
	metaBytes       = "sh.maki/bytes"
	metaDuration    = "sh.maki/duration_ms"
)

// Argument names of WIRE 12, the rename table.
const (
	argSessionID          = "sessionId"
	argLast               = "last"
	argText               = "text"
	argModeID             = "modeId"
	argTitle              = "title"
	argTaskID             = "taskId"
	argCwd                = "cwd"
	argMCPServers         = "mcpServers"
	argProtocolVersion    = "protocolVersion"
	argClientCapabilities = "clientCapabilities"
)

// sessionUpdate tags of a live tab (WIRE 11.1) and the tool card statuses.
const (
	upToolCall   = "tool_call"
	upToolUpdate = "tool_call_update"
	upUsage      = "usage_update"

	statusPending    = "pending"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"
)

// session_event kinds (WIRE 11.2). A new kind is an addition, and a reader
// ignores what it does not know.
const (
	kindStatus       = "status"
	kindTitle        = "title"
	kindModel        = "model"
	kindQuestion     = "question"
	kindTasksChanged = "tasks_changed"
	kindTaskFocus    = "task_focus"
)

// Permission words of WIRE 10. The host slot answers two things, so the wire
// offers two options and the gateway picks between them.
const (
	outcomeSelected = "selected"
	optionAllowOnce = "allow_once"
	optionReject    = "reject_once"

	// codeNotLive is the -32002 of WIRE 13: a sessionId this TUI does not hold.
	codeNotLive = -32002
	// codeHeld is the -32000 of WIRE 13: a session this TUI cannot take.
	codeHeld = -32000
)

// meta is the _meta object of a frame. Values stay raw: ACP reads them as JSON,
// and a Lua encoder can write true and "true" for the same fact (WIRE 2.3), so
// every reader here takes both spellings.
type meta map[string]json.RawMessage

func (m meta) str(key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	t := strings.TrimSpace(string(raw))
	if t == "null" {
		return ""
	}
	if s, err := strconv.Unquote(t); err == nil {
		return s
	}
	return t
}

func (m meta) flag(key string) bool { return m.str(key) == "true" }

func (m meta) num(key string) (float64, bool) {
	t := m.str(key)
	if t == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(t, 64)
	return n, err == nil
}

// initResult is the answer to initialize (WIRE 2.1). Only its _meta is read: a
// live tab offers no capability this gateway can use.
type initResult struct {
	Meta meta `json:"_meta"`
}

// agentInfo is what initialize reports about the TUI behind one socket. The pid is
// the one the notice text and model.Session.PluginPID report.
type agentInfo struct {
	pid       int
	cwd       string
	proto     float64
	protoSeen bool
	makiVer   string
}

// sessionInfo is one row of session/list (WIRE 3): ACP's SessionInfo, with the
// maki fields in _meta. mode and focused are read but not forwarded:
// model.Session has no such field (CONTRACTS 2), so the flag only travels on the
// SSE status event and in the row.
type sessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
	Meta      meta   `json:"_meta"`
}

// listResult is the answer to session/list.
type listResult struct {
	Sessions []sessionInfo `json:"sessions"`
}

// messagesResult is the answer to _sh.maki/messages (WIRE 6): native maki
// records, with no field renamed, dropped, or added.
type messagesResult struct {
	Messages []model.Message `json:"messages"`
}

// queueResult is the answer to _sh.maki/queue (WIRE 7.2): the two words the HTTP
// route returns, and the only thing a live tab can report about a turn it has
// just taken.
type queueResult struct {
	State string `json:"state"`
}

// updateFrame is one session/update notification (WIRE 11.1). The inner object
// stays raw: only the tool cards are read, and the transcript is refetched.
type updateFrame struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// toolUpdate is the part of a tool card this gateway reads.
type toolUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Status        string `json:"status"`
}

// sessionEvent is one _sh.maki/session_event notification (WIRE 11.2). The
// params are flat, and every field but kind and sessionId belongs to one kind.
type sessionEvent struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	TaskID    string `json:"taskId"`
	Focused   *bool  `json:"focused"`
}

// permissionRequest is the ask of WIRE 10: the tool card it belongs to, and the
// two options the host slot can honor.
type permissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Name       string          `json:"name"`
		RawInput   json.RawMessage `json:"rawInput"`
	} `json:"toolCall"`
	Options []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

// optionID picks the option id a decision answers, out of the list the plugin
// offered. The well-known id is the fallback, which is what WIRE 10 fixes.
func (p *permissionRequest) optionID(want string) string {
	for _, o := range p.Options {
		if o.Kind == want || o.OptionID == want {
			return o.OptionID
		}
	}
	return want
}

// permissionResult is the answer of WIRE 10.1. Guidance has no ACP field, so it
// travels in the _meta of the result.
type permissionResult struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId,omitempty"`
	} `json:"outcome"`
	Meta meta `json:"_meta,omitempty"`
}

// newPermissionResult builds the one answer this bridge gives: a picked option.
func newPermissionResult(optionID, guidance string) permissionResult {
	var r permissionResult
	r.Outcome.Outcome = outcomeSelected
	r.Outcome.OptionID = optionID
	if guidance != "" {
		r.Meta = meta{metaGuidance: json.RawMessage(strconv.Quote(guidance))}
	}
	return r
}

// epochSeconds turns the one timestamp format of WIRE 3 into the seconds the
// registry and the analytics route work in. An absent or unreadable clock reads
// 0, which is what an unknown last activity means today.
func epochSeconds(iso string) int64 {
	if iso == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return 0
	}
	return t.Unix()
}
