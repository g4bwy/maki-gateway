package attach

import (
	"context"
	"encoding/json"
	"log/slog"

	"maki-gateway/internal/acpwire"
	"maki-gateway/internal/model"
)

// maxToolCards bounds the open tool cards one connection remembers. A card this
// map forgets reads as an out-of-order start, which costs one dirty event and
// nothing worse (WIRE 11.1 rule 1).
const maxToolCards = 256

// onNote routes one notification the plugin pushes. A method with no meaning here
// is ignored rather than treated as a fault: a protocol grows by adding
// notifications, and WIRE 12.1 keeps the connection for it.
func (c *conn) onNote(method string, params json.RawMessage) {
	switch method {
	case mSessionUpdate:
		c.sessionUpdate(params)
	case mSessionEvent:
		c.sessionEvent(params)
	default:
		slog.Debug("attach: unmapped plugin notification", "method", method, "socket", c.path)
	}
}

// onAsk answers the one request a plugin sends. Anything else is refused on the
// spot, so the plugin never waits on an ask nobody read (WIRE 12.2).
func (c *conn) onAsk(_ context.Context, r *acpwire.Request) error {
	if r.Method != mRequestPermission {
		slog.Debug("attach: unmapped plugin request", "method", r.Method, "socket", c.path)
		return &acpwire.Error{Code: acpwire.CodeMethodNotFound, Message: "method not found"}
	}
	return c.parkAsk(r)
}

// sessionUpdate maps the ACP stream onto the hub. Changes to a tab are reported
// as dirty rather than as content: the SSE surface carries no tool result
// bodies, so the browser refetches the transcript (WIRE 11.1 rule 2).
func (c *conn) sessionUpdate(params json.RawMessage) {
	var f updateFrame
	if json.Unmarshal(params, &f) != nil || len(f.Update) == 0 {
		slog.Debug("attach: unreadable session/update", "socket", c.path)
		return
	}
	c.e.routeTo(f.SessionID, c)
	var u toolUpdate
	if json.Unmarshal(f.Update, &u) != nil {
		return
	}
	switch u.SessionUpdate {
	case upToolCall:
		c.openTool(u.ToolCallID)
		c.dirty(f.SessionID, reasonToolStart)
	case upToolUpdate:
		// A terminal status ends the card. A non-terminal update for a card this
		// connection never saw open is a valid frame and reads as an out-of-order
		// start (rule 1). One for a card that is still running changes nothing the
		// browser has to refetch.
		if terminalStatus(u.Status) {
			c.closeTool(u.ToolCallID)
			c.dirty(f.SessionID, reasonToolDone)
		} else if !c.openTool(u.ToolCallID) {
			c.dirty(f.SessionID, reasonToolStart)
		}
	case upUsage:
		// WIRE 11.1 rule 3 and Appendix A put the end of a streamed turn at
		// usage_update, so that frame carries the refetch nudge v1 called turn_end.
		// The numbers themselves have no home on the frozen SSE surface, and are
		// dropped here as they were in v1.
		c.dirty(f.SessionID, reasonTurnEnd)
	default:
		// Streamed text and a mode move have no place on this SSE surface: the tab's
		// status event is what tells the browser to refetch.
		slog.Debug("attach: unmapped session/update", "update", u.SessionUpdate, "socket", c.path)
	}
}

func terminalStatus(status string) bool {
	return status == statusCompleted || status == statusFailed
}

// sessionEvent maps one _sh.maki/session_event onto the hub events of CONTRACTS
// 2, kind by kind, with the shapes the bridge has always published. A kind this
// build does not know is ignored (WIRE 12.3): the list grows by addition.
func (c *conn) sessionEvent(params json.RawMessage) {
	var ev sessionEvent
	if json.Unmarshal(params, &ev) != nil {
		slog.Debug("attach: unreadable session_event", "socket", c.path)
		return
	}
	// Any event naming a tab tells us who owns it, which is how routing is
	// learned between two list calls.
	c.e.routeTo(ev.SessionID, c)
	switch ev.Kind {
	case kindStatus:
		c.e.publish(model.Event{
			Type:    evStatus,
			Session: ev.SessionID,
			Status:  ev.Status,
			Title:   ev.Title,
			Focused: ev.Focused,
		})
		// WIRE 11.2 pairs the status change with a dirty of reason status, because
		// v1 pushed turn_start beside it: a tab that starts or stops working changed
		// its transcript, and the browser refetches it.
		c.dirty(ev.SessionID, reasonStatus)
	case kindTitle:
		// The status frame carries a title, so a rename is one status event.
		c.e.publish(model.Event{Type: evStatus, Session: ev.SessionID, Title: ev.Title})
	case kindModel:
		// No model field exists in the SSE envelope, so a model change is a plain
		// status event: the client refetches and sees the new model.
		c.e.publish(model.Event{Type: evStatus, Session: ev.SessionID})
	case kindQuestion:
		c.e.publish(model.Event{Type: evNotice, Text: questionNotice})
	case kindTasksChanged:
		// A task started, moved, or stopped. The event names no task, so the
		// browser refetches GET /api/sessions/{id}/tasks.
		c.e.publish(model.Event{Type: evTasks, Session: ev.SessionID})
	case kindTaskFocus:
		// The terminal moved the followed chat. The rows did not change, so this
		// names the new one rather than asking for a refetch (CONTRACTS 2).
		c.e.publish(model.Event{Type: evTaskFocus, Session: ev.SessionID, TaskID: ev.TaskID})
	default:
		slog.Debug("attach: unmapped session_event kind", "kind", ev.Kind, "socket", c.path)
	}
}

func (c *conn) dirty(session, reason string) {
	c.e.publish(model.Event{Type: evDirty, Session: session, Reason: reason})
}

// parkAsk holds a permission ask open until the browser answers it. The request
// id the plugin used stays on the wire side: what the hub and the HTTP route
// carry is this gateway's own counter, as it always was (CONTRACTS 2 keeps
// request ids per engine).
func (c *conn) parkAsk(r *acpwire.Request) error {
	var req permissionRequest
	if err := json.Unmarshal(r.Params, &req); err != nil {
		slog.Debug("attach: unreadable request_permission", "socket", c.path, "error", err)
		return &acpwire.Error{Code: acpwire.CodeInvalidRequest, Message: "invalid params"}
	}
	c.e.routeTo(req.SessionID, c)
	tool := req.ToolCall.Title
	if tool == "" {
		tool = req.ToolCall.Name
	}
	a := &ask{c: c, wireID: canonID(r.ID), req: r, requestID: c.e.nextAskID(), params: req}
	c.e.armAsk(a)
	c.e.publish(model.Event{
		Type:      evPermission,
		Engine:    model.EngineAttach,
		Session:   req.SessionID,
		RequestID: a.requestID,
		Tool:      tool,
		Input:     req.ToolCall.RawInput,
	})
	return nil
}

// withdrawn closes a dialog the plugin ended on its own, which is the 55 second
// fallthrough of WIRE 10.2 and the answer elsewhere of WIRE 10.3. The frame that
// says so answers a request this side never sent, so it reaches here through the
// codec's report of a reply with no open call rather than through any waiter. An
// id that names no parked ask belongs to a call this side gave up on, and is
// nothing's business. A withdrawal that answers with an error carries no result,
// and closes the dialog the same way: the plugin holds nothing any more.
func (c *conn) withdrawn(id json.RawMessage, _ json.RawMessage) {
	a := c.e.removeAskByWire(c, canonID(id))
	if a == nil {
		return
	}
	slog.Debug("attach: plugin withdrew its permission ask", "socket", c.path, "request_id", a.requestID)
	c.e.publish(model.Event{Type: evPermissionCancel, RequestID: a.requestID})
}
