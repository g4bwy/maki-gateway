package store

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"maki-gateway/internal/model"
)

// Analytics is the payload of GET /api/analytics (CONTRACTS 2).
type Analytics struct {
	Daily      []DailyStat   `json:"daily"`
	PerSession []SessionStat `json:"per_session"`
}

// DailyStat is one local calendar day of billed usage.
type DailyStat struct {
	Date          string  `json:"date"`
	Sessions      int     `json:"sessions"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
	Cost          float64 `json:"cost"`
}

// SessionStat is one session's billed total.
type SessionStat struct {
	Session   string  `json:"session"`
	Title     string  `json:"title"`
	Model     string  `json:"model"`
	Cost      float64 `json:"cost"`
	UpdatedAt int64   `json:"updated_at"`
}

// dayLayout is the date key the HTTP API and the browser agree on.
const dayLayout = "2006-01-02"

// Analytics is the GET /api/analytics body: {"daily":[...],"per_session":[...]}
// (CONTRACTS 2). It is raw JSON so the HTTP layer can forward it untouched.
func (e *Engine) Analytics(days int) (json.RawMessage, error) {
	a, err := e.analytics(days)
	if err != nil {
		return nil, err
	}
	return json.Marshal(a)
}

// analytics folds every log's last meta record into per-session totals and
// per-day buckets, keeping sessions whose meta landed in the last days
// calendar days (local time, today included). days <= 0 covers all history.
// Cost comes from meta usage_by_model; logs older than that field fall back to
// meta token_usage, so they still show their tokens with a zero cost.
func (e *Engine) analytics(days int) (Analytics, error) {
	files, err := scanFiles(e.dir)
	if err != nil {
		return Analytics{}, err
	}
	var cutoff int64
	if days > 0 {
		cutoff = startOfDay(time.Now()).AddDate(0, 0, -(days - 1)).Unix()
	}
	out := Analytics{Daily: []DailyStat{}, PerSession: []SessionStat{}}
	byDay := map[string]*DailyStat{}
	for _, path := range files {
		h, ok := readHeader(path)
		if !ok {
			continue
		}
		m, _ := readMeta(path)
		at := m.UpdatedAt
		if at == 0 {
			at = h.CreatedAt
		}
		if at < cutoff {
			continue
		}
		id := h.ID
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
		u := m.totals()
		var cost float64
		if u.Cost != nil {
			cost = *u.Cost
		}
		out.PerSession = append(out.PerSession, SessionStat{
			Session: id, Title: m.Title, Model: h.Model, Cost: cost, UpdatedAt: at,
		})
		day := time.Unix(at, 0).In(time.Local).Format(dayLayout)
		bucket := byDay[day]
		if bucket == nil {
			bucket = &DailyStat{Date: day}
			byDay[day] = bucket
		}
		bucket.Sessions++
		bucket.Input += u.Input
		bucket.Output += u.Output
		bucket.CacheRead += u.CacheRead
		bucket.CacheCreation += u.CacheCreation
		bucket.Cost += cost
	}
	for _, d := range byDay {
		out.Daily = append(out.Daily, *d)
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Date < out.Daily[j].Date })
	sort.Slice(out.PerSession, func(i, j int) bool {
		if out.PerSession[i].UpdatedAt != out.PerSession[j].UpdatedAt {
			return out.PerSession[i].UpdatedAt > out.PerSession[j].UpdatedAt
		}
		return out.PerSession[i].Session < out.PerSession[j].Session
	})
	return out, nil
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// compile-time proof the engine satisfies the interface other engines share.
var _ model.Engine = (*Engine)(nil)
