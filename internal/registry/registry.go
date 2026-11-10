// Package registry merges the session lists of all engines into one deduplicated
// view and publishes "sessions" events on the hub when that view changes.
// Spec: docs/DESIGN_CORE.md.
package registry

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"maki-gateway/internal/model"
)

const (
	PollInterval      = 5 * time.Second
	Debounce          = 250 * time.Millisecond
	EventTypeSessions = "sessions"
)

// SessionKind normalizes engine names ("acp", "attach", "store") and session
// kinds ("web", "live", "stored") to the canonical session kind.
func SessionKind(s string) string {
	switch s {
	case "acp", model.KindWeb:
		return model.KindWeb
	case "attach", model.KindLive:
		return model.KindLive
	case "store", model.KindStored:
		return model.KindStored
	}
	return s
}

func kindRank(kind string) int {
	switch SessionKind(kind) {
	case model.KindLive:
		return 3
	case model.KindWeb:
		return 2
	case model.KindStored:
		return 1
	}
	return 0
}

type mergeRow struct {
	session model.Session
	engine  model.Engine
	rank    int
	seq     int
}

// mergeEngines flattens all engine lists, keeps one row per id (highest rank,
// first row wins ties), fills the winner's blank fields from the runner-up
// rows (rank order), and returns the sorted list plus the owner per id.
func mergeEngines(engines []model.Engine) ([]model.Session, map[string]model.Engine) {
	var ids []string
	byID := map[string][]mergeRow{}
	var seq int
	for _, eng := range engines {
		rows, err := eng.List()
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.ID == "" {
				continue
			}
			kind := SessionKind(row.Kind)
			if kindRank(kind) == 0 {
				kind = SessionKind(eng.Kind())
			}
			row.Kind = kind
			if _, ok := byID[row.ID]; !ok {
				ids = append(ids, row.ID)
			}
			byID[row.ID] = append(byID[row.ID], mergeRow{session: row, engine: eng, rank: kindRank(kind), seq: seq})
			seq++
		}
	}
	sessions := make([]model.Session, 0, len(ids))
	owners := make(map[string]model.Engine, len(ids))
	for _, id := range ids {
		rows := byID[id]
		slices.SortStableFunc(rows, func(a, b mergeRow) int {
			return cmp.Or(-cmp.Compare(a.rank, b.rank), cmp.Compare(a.seq, b.seq))
		})
		best := rows[0].session
		for _, other := range rows[1:] {
			if best.Title == "" {
				best.Title = other.session.Title
			}
			if best.Status == "" {
				best.Status = other.session.Status
			}
			if best.Cwd == "" {
				best.Cwd = other.session.Cwd
			}
			if best.Model == "" {
				best.Model = other.session.Model
			}
			if best.UpdatedAt == 0 {
				best.UpdatedAt = other.session.UpdatedAt
			}
			if best.PluginPID == 0 {
				best.PluginPID = other.session.PluginPID
			}
		}
		sessions = append(sessions, best)
		owners[id] = rows[0].engine
	}
	slices.SortStableFunc(sessions, func(a, b model.Session) int {
		return cmp.Or(-cmp.Compare(a.UpdatedAt, b.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	return sessions, owners
}

// Merge flattens and deduplicates the engines' session lists (DESIGN_CORE).
func Merge(engines ...model.Engine) []model.Session {
	sessions, _ := mergeEngines(engines)
	return sessions
}

// Registry keeps the merged session list fresh and announces changes.
type Registry struct {
	hub     *model.Hub
	engines []model.Engine
	byKind  map[string]model.Engine
	byName  map[string]model.Engine

	signal  chan struct{}
	started sync.Once

	mu       sync.RWMutex
	last     []byte
	sessions []model.Session
	owners   map[string]model.Engine
}

func New(hub *model.Hub, engines ...model.Engine) *Registry {
	r := &Registry{
		hub:      hub,
		engines:  slices.Clone(engines),
		byKind:   map[string]model.Engine{},
		byName:   map[string]model.Engine{},
		signal:   make(chan struct{}, 1),
		sessions: []model.Session{},
	}
	for _, e := range engines {
		kind := SessionKind(e.Kind())
		r.byKind[kind] = e
		r.byName[e.Kind()] = e
		r.byName[kind] = e
	}
	return r
}

// Start seeds the list synchronously without publishing, then polls in the
// background until ctx ends. Repeated calls are no-ops.
func (r *Registry) Start(ctx context.Context) {
	r.started.Do(func() {
		r.refresh(false)
		events := r.hub.Subscribe() // before spawn, so no event races with Start
		go r.poll(ctx, events)
	})
}

// Refresh requests a debounced re-merge. It never blocks.
func (r *Registry) Refresh() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

func (r *Registry) poll(ctx context.Context, events chan model.Event) {
	defer r.hub.Unsubscribe(events)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	fired := make(chan struct{}, 1)
	var debounce *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.signal:
			if debounce == nil {
				debounce = time.AfterFunc(Debounce, func() {
					select {
					case fired <- struct{}{}:
					default:
					}
				})
			}
		case <-fired:
			debounce = nil
			r.refresh(true)
		case <-ticker.C:
			if debounce != nil {
				debounce.Stop()
				debounce = nil
			}
			r.refresh(true)
		case e := <-events:
			if refreshOn(e) {
				r.Refresh()
			}
		}
	}
}

// Sessions returns a copy of the merged list, ordered UpdatedAt desc, ID asc.
func (r *Registry) Sessions() []model.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.sessions)
}

// Find looks one merged session row up by id.
func (r *Registry) Find(id string) (model.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return model.Session{}, false
}

// EngineFor returns the engine that owns the winning row for id, falling back
// to acp, attach, then store when the id is unknown.
func (r *Registry) EngineFor(id string) model.Engine {
	r.mu.RLock()
	owner := r.owners[id]
	r.mu.RUnlock()
	if owner != nil {
		return owner
	}
	for _, name := range []string{"acp", "attach", "store"} {
		if e := r.EngineByName(name); e != nil {
			return e
		}
	}
	return nil
}

// EngineForKind returns the engine registered for a session kind (either
// spelling accepted), or nil.
func (r *Registry) EngineForKind(kind string) model.Engine {
	return r.byKind[SessionKind(kind)]
}

// EngineByName returns the engine registered under a name or kind spelling, or nil.
func (r *Registry) EngineByName(name string) model.Engine {
	if e := r.byName[name]; e != nil {
		return e
	}
	return r.byName[SessionKind(name)]
}

// refresh re-merges and publishes a sessions event only when the JSON view
// changed (and publish is set).
func (r *Registry) refresh(publish bool) {
	sessions, owners := mergeEngines(r.engines)
	data, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	r.mu.Lock()
	changed := !bytes.Equal(r.last, data)
	if changed {
		r.last, r.sessions, r.owners = data, sessions, owners
	}
	r.mu.Unlock()
	if changed && publish {
		r.hub.Publish(model.Event{Type: EventTypeSessions})
	}
}

// refreshOn reports whether a hub event should trigger a re-merge.
func refreshOn(e model.Event) bool {
	switch e.Type {
	case "status", "dirty", "exit":
		return true
	case "update", "notice":
		return strings.Contains(e.Reason, "turn_end") || bytes.Contains(e.ACP, []byte("turn_end"))
	}
	return false
}
