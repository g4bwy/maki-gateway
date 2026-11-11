// Package model holds the types shared across engines, the hub, and the HTTP layer.
// Contract source: docs/CONTRACTS.md. Change signatures only via the integrator.
package model

import (
	"encoding/json"
	"errors"
	"sync"
)

// Engine kinds and statuses per CONTRACTS section 2.
const (
	KindWeb    = "web"
	KindLive   = "live"
	KindStored = "stored"

	StatusWorking    = "working"
	StatusNeedsInput = "needs_input"
	StatusIdle       = "idle"
	StatusExited     = "exited"

	EngineACP    = "acp"
	EngineAttach = "attach"
)

var (
	ErrUnsupported   = errors.New("unsupported")
	ErrBusy          = errors.New("busy")
	ErrNotFound      = errors.New("not found")
	ErrOpenElsewhere = errors.New("open elsewhere")
)

// Session is the merged session entry (CONTRACTS 2).
type Session struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	UpdatedAt int64  `json:"updated_at"`
	PluginPID int    `json:"plugin_pid,omitempty"`
	// Yolo is the permission answer a web child was started with. Only the acp
	// engine reports it, so the field stays absent for every other row rather
	// than arriving as a false "the terminal asks". The struct must stay
	// comparable: the registry diffs rows with ==.
	Yolo bool `json:"yolo,omitempty"`
}

// Message is the native maki transcript record (CONTRACTS 3, the "d" of a "t":"msg" line).
type Message struct {
	Role        string          `json:"role"`
	Content     json.RawMessage `json:"content"`
	DisplayText *string         `json:"display_text,omitempty"`
	CreatedAt   *int64          `json:"created_at,omitempty"`
	Kind        *string         `json:"kind,omitempty"`
}

// SearchHit is one /api/search result (CONTRACTS 2). It lives here so the
// httpapi capability probe and store's searcher agree by construction.
type SearchHit struct {
	Session string `json:"session"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

// Event is the SSE envelope (CONTRACTS 2). Fields are per-type; empty ones are omitted.
type Event struct {
	Type      string          `json:"type"`
	Session   string          `json:"session,omitempty"`
	Engine    string          `json:"engine,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Status    string          `json:"status,omitempty"`
	Title     string          `json:"title,omitempty"`
	Focused   *bool           `json:"focused,omitempty"`
	RequestID int64           `json:"request_id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ACP       json.RawMessage `json:"event,omitempty"`
	Text      string          `json:"text,omitempty"`
	Code      *int            `json:"code,omitempty"`
}

// CreateRequest is one POST /api/sessions body (CONTRACTS 2). Yolo skips every
// permission prompt in the new session. Only the web engine can honour it, and
// the attach bridge reports the request to a plugin that ignores it.
type CreateRequest struct {
	Cwd    string
	Model  string
	Prompt string
	Yolo   bool
}

// Permission decisions the HTTP API accepts (CONTRACTS 2). Each engine maps
// them onto what its own protocol offers, and degrades where it has no match.
const (
	DecisionAllow       = "allow"
	DecisionAllowAlways = "allow_always"
	DecisionDeny        = "deny"
)

// IsDecision reports whether s names one of the three decisions the API accepts.
func IsDecision(s string) bool {
	return s == DecisionAllow || s == DecisionAllowAlways || s == DecisionDeny
}

// Engine is one source of sessions. acpengine, attach, and store implement it;
// registry merges them. RespondPermission is routed by the caller to the engine
// whose name matches the Event.Engine field of the permission event.
type Engine interface {
	Kind() string
	List() ([]Session, error)
	Messages(id string, last int) ([]Message, error)
	// Prompt returns "started" or "queued". ErrBusy when the engine rejects concurrency.
	Prompt(id, text string) (string, error)
	Cancel(id string) error
	SetMode(id, mode string) error
	SetTitle(id, title string) error
	// Create spawns/opens a session. ErrUnsupported for read-only engines.
	Create(req CreateRequest) (string, error)
	// Kill terminates an engine-owned process. ErrUnsupported otherwise.
	Kill(id string) error
	// RespondPermission answers a parked prompt. decision is one of
	// DecisionAllow, DecisionAllowAlways or DecisionDeny, and guidance is free
	// text only the attach bridge can deliver. An engine whose protocol has no
	// "always" option answers the nearest one it has.
	RespondPermission(requestID int64, decision, guidance string) error
}

// Hub fans events out to SSE subscribers. Slow subscribers are dropped, never blocked.
type Hub struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	dropped int64
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
			h.dropped++
		}
	}
}

func (h *Hub) Dropped() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}
