package attach

import (
	"log/slog"

	"maki-gateway/internal/model"
)

// handleEvent maps one plugin event onto the hub, per the events table of
// CONTRACTS 1. Every id in these events is the plugin's own request id, so the
// browser can answer a permission prompt with POST /api/permissions/attach/{id}
// and land on the same parked slot (CONTRACTS 2).
//
// Changes to a tab are reported as dirty rather than as content: maki has no
// delta events, so the client refetches the transcript. turn_end usage, cost and
// context_size are dropped here; the analytics source is the stored engine's
// meta record, which the TUI writes itself.
func (c *conn) handleEvent(m *message) {
	// Any event naming a session tells us who owns it, which is how routing is
	// learned between two list calls.
	if m.Session != "" {
		c.e.routeTo(m.Session, c)
	}
	switch m.Event {
	case evStatus:
		c.e.publish(model.Event{
			Type:    evStatus,
			Session: m.Session,
			Status:  m.Status,
			Title:   m.Title,
			Focused: m.Focused,
		})
	case "turn_start":
		// No turn_start reason exists in CONTRACTS 2; "status" carries the same
		// instruction: this tab changed, refetch it.
		c.dirty(m.Session, reasonStatus)
	case "tool_start":
		c.dirty(m.Session, reasonToolStart)
	case "tool_done":
		c.dirty(m.Session, reasonToolDone)
	case "turn_end":
		c.dirty(m.Session, reasonTurnEnd)
	case "title":
		// The status frame carries a title, so a rename is one status event.
		c.e.publish(model.Event{Type: evStatus, Session: m.Session, Title: m.Title})
	case "model":
		// No model field exists in the SSE envelope, so a model change is a
		// plain status event: the client refetches and sees the new model.
		c.e.publish(model.Event{Type: evStatus, Session: m.Session})
	case "tasks_changed":
		// A task started, moved, or stopped. maki sends no task rows on the
		// socket, so the browser refetches GET /api/sessions/{id}/tasks.
		c.e.publish(model.Event{Type: evTasks, Session: m.Session})
	case evTaskFocus:
		// The terminal moved the followed chat, by key or by the FocusTask op.
		// The rows themselves did not change, so this names the new one rather
		// than asking for a refetch (CONTRACTS 2).
		c.e.publish(model.Event{Type: evTaskFocus, Session: m.Session, TaskID: m.taskName()})
	case "question":
		c.e.publish(model.Event{Type: evNotice, Text: questionNotice})
	case evPermission:
		c.notePermission(m)
	case evPermissionCancel:
		c.e.dropTicket(m.RequestID)
		c.e.publish(model.Event{Type: evPermissionCancel, RequestID: m.RequestID})
	default:
		slog.Debug("attach: unmapped plugin event", "event", m.Event)
	}
}

func (c *conn) dirty(session, reason string) {
	c.e.publish(model.Event{Type: evDirty, Session: session, Reason: reason})
}

// notePermission parks a bridge request: the hub needs the engine name and the
// plugin id, and RespondPermission needs to find the connection again.
func (c *conn) notePermission(m *message) {
	c.e.noteTicket(m.RequestID, c)
	c.e.publish(model.Event{
		Type:      evPermission,
		Engine:    model.EngineAttach,
		Session:   m.Session,
		RequestID: m.RequestID,
		Tool:      m.Tool,
		Input:     m.Input,
	})
}
