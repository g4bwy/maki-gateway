package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"maki-gateway/internal/model"
)

// Optional engine capabilities probed locally (DESIGN_CORE ambiguity 2).
type planner interface {
	Plan(id string) (json.RawMessage, error)
}

type searcher interface {
	Search(q string, limit int) ([]SearchHit, error)
}

type analyst interface {
	Analytics(days int) (json.RawMessage, error)
}

// SearchHit is one /api/search result (CONTRACTS 2).
type SearchHit = model.SearchHit

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/sessions", s.handleSessions)
	m.HandleFunc("POST /api/sessions", s.handleCreate)
	m.HandleFunc("DELETE /api/sessions/{id}", s.handleDelete)
	m.HandleFunc("GET /api/sessions/{id}/messages", s.handleMessages)
	m.HandleFunc("POST /api/sessions/{id}/prompt", s.handlePrompt)
	m.HandleFunc("POST /api/sessions/{id}/cancel", s.handleCancel)
	m.HandleFunc("POST /api/sessions/{id}/mode", s.handleMode)
	m.HandleFunc("POST /api/sessions/{id}/title", s.handleTitle)
	m.HandleFunc("POST /api/permissions/{engine}/{request_id}", s.handlePermission)
	m.HandleFunc("GET /api/plan/{id}", s.handlePlan)
	m.HandleFunc("GET /api/search", s.handleSearch)
	m.HandleFunc("GET /api/analytics", s.handleAnalytics)
	m.HandleFunc("GET /api/events", s.handleEvents)
	if s.cfg.Static != nil {
		m.Handle("GET /", s.cfg.Static)
	}
	s.mux = m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

// writeError maps domain errors to statuses (DESIGN_CORE).
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrBusy):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, model.ErrOpenElsewhere):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, model.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, model.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Version  string `json:"version"`
		Maki     string `json:"maki"`
		Sessions int    `json:"sessions"`
	}{s.cfg.Version, s.MakiVersion(), len(s.cfg.Registry.Sessions())})
}

func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	list := s.cfg.Registry.Sessions()
	if list == nil {
		list = []model.Session{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd    string `json:"cwd"`
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// An empty cwd would spawn the process in the gateway's own directory, so
	// the caller has to say where the session belongs (CONTRACTS 2).
	cwd := strings.TrimSpace(body.Cwd)
	if cwd == "" {
		writeErr(w, http.StatusBadRequest, "cwd is required")
		return
	}
	eng := s.cfg.Registry.EngineForKind(model.KindWeb)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no acp engine")
		return
	}
	id, err := eng.Create(cwd, body.Model, body.Prompt)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"session": id})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if row.Kind != model.KindWeb {
		writeErr(w, http.StatusBadRequest, "only web sessions can be killed")
		return
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no acp engine")
		return
	}
	if err := eng.Kill(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"killed": true})
}

// messagesSource maps a merged row kind to the contract's source field.
func messagesSource(kind string) string {
	switch kind {
	case model.KindWeb:
		return "acp"
	case model.KindLive:
		return "attach"
	default:
		return "file"
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	last := parseQueryInt(r, "last", defaultLast)
	kind := model.KindStored
	if row, ok := s.row(id); ok {
		kind = row.Kind
	}
	eng := s.cfg.Registry.EngineForKind(kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for source "+messagesSource(kind))
		return
	}
	msgs, err := eng.Messages(id, last)
	if err != nil {
		writeError(w, err)
		return
	}
	if msgs == nil {
		msgs = []model.Message{}
	}
	writeJSON(w, http.StatusOK, struct {
		Source   string          `json:"source"`
		Messages []model.Message `json:"messages"`
	}{messagesSource(kind), msgs})
}

// mutationEngine resolves the engine for a mutation by merged row kind
// (DESIGN_CORE ambiguity 1). It answers the request when returning false.
func (s *Server) mutationEngine(w http.ResponseWriter, id, notFoundMsg string) (model.Engine, bool) {
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return nil, false
	}
	if row.Kind == model.KindStored {
		writeErr(w, http.StatusNotFound, notFoundMsg)
		return nil, false
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for kind "+row.Kind)
		return nil, false
	}
	return eng, true
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	eng, ok := s.mutationEngine(w, id, "cannot prompt a stored session")
	if !ok {
		return
	}
	state, err := eng.Prompt(id, body.Text)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	switch row.Kind {
	case model.KindLive:
		// No plugin API exists yet (DESIGN_CORE ambiguity 3).
		writeErr(w, http.StatusNotImplemented, "cannot cancel a live session")
		return
	case model.KindStored:
		writeErr(w, http.StatusNotFound, "cannot cancel a stored session")
		return
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for kind "+row.Kind)
		return
	}
	if err := eng.Cancel(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if row.Kind == model.KindStored {
		writeErr(w, http.StatusMethodNotAllowed, "cannot set mode on a stored session")
		return
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for kind "+row.Kind)
		return
	}
	if err := eng.SetMode(id, body.Mode); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTitle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title *string `json:"title"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if row.Kind == model.KindStored {
		writeErr(w, http.StatusMethodNotAllowed, "cannot set title on a stored session")
		return
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for kind "+row.Kind)
		return
	}
	title := ""
	if body.Title != nil {
		title = *body.Title
	}
	if err := eng.SetTitle(id, title); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	rid, err := strconv.ParseInt(r.PathValue("request_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "request_id must be an integer")
		return
	}
	eng := s.cfg.Registry.EngineByName(r.PathValue("engine"))
	if eng == nil {
		writeErr(w, http.StatusNotFound, "unknown engine")
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Guidance string `json:"guidance"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := eng.RespondPermission(rid, body.Decision, body.Guidance); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok := s.row(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if row.Kind == model.KindWeb {
		writeErr(w, http.StatusNotImplemented, "no plan API for web sessions")
		return
	}
	eng := s.cfg.Registry.EngineForKind(row.Kind)
	if eng == nil {
		writeErr(w, http.StatusNotImplemented, "no engine for kind "+row.Kind)
		return
	}
	p, ok := eng.(planner)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "engine has no plan API")
		return
	}
	raw, err := p.Plan(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	writeRaw(w, raw)
}

func writeRaw(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

const (
	snippetBefore = 40
	snippetAfter  = 120
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	hits := []SearchHit{}
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
		return
	}
	probed := false
	for _, eng := range s.cfg.Engines {
		sh, ok := eng.(searcher)
		if !ok {
			continue
		}
		probed = true
		found, err := sh.Search(q, searchCap-len(hits))
		if err != nil {
			writeError(w, err)
			return
		}
		hits = append(hits, found...)
		if len(hits) >= searchCap {
			break
		}
	}
	if !probed {
		hits = s.scanSessions(q)
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": slices.Clip(hits[:min(len(hits), searchCap)])})
}

// scanSessions is the searcher-absent fallback: case-insensitive substring
// over text blocks of the last 200 messages per merged session, cap 50.
func (s *Server) scanSessions(q string) []SearchHit {
	hits := []SearchHit{}
	lower := strings.ToLower(q)
	for _, sess := range s.cfg.Registry.Sessions() {
		if len(hits) >= searchCap {
			break
		}
		eng := s.cfg.Registry.EngineForKind(sess.Kind)
		if eng == nil {
			continue
		}
		msgs, err := eng.Messages(sess.ID, defaultLast)
		if err != nil {
			continue
		}
		if hit, ok := firstHit(sess, msgs, lower); ok {
			hits = append(hits, hit)
		}
	}
	return hits
}

func firstHit(sess model.Session, msgs []model.Message, lowerQ string) (SearchHit, bool) {
	for _, m := range msgs {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "text" {
				continue
			}
			if idx := strings.Index(strings.ToLower(b.Text), lowerQ); idx >= 0 {
				return SearchHit{Session: sess.ID, Title: sess.Title, Snippet: snippet(b.Text, idx, len(lowerQ))}, true
			}
		}
	}
	return SearchHit{}, false
}

func snippet(text string, idx, qlen int) string {
	start := max(0, idx-snippetBefore)
	end := min(len(text), idx+qlen+snippetAfter)
	return strings.ToValidUTF8(text[start:end], "")
}

type analyticsRow struct {
	Session   string `json:"session"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	Cost      int    `json:"cost"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	days := parseQueryInt(r, "days", defaultDays)
	for _, eng := range s.cfg.Engines {
		an, ok := eng.(analyst)
		if !ok {
			continue
		}
		raw, err := an.Analytics(days)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(raw) == 0 {
			raw = json.RawMessage(`{"daily":[],"per_session":[]}`)
		}
		writeRaw(w, raw)
		return
	}
	rows := []analyticsRow{}
	for _, sess := range s.cfg.Registry.Sessions() {
		rows = append(rows, analyticsRow{sess.ID, sess.Title, sess.Model, 0, sess.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, struct {
		Daily      []any          `json:"daily"`
		PerSession []analyticsRow `json:"per_session"`
	}{[]any{}, rows})
}
