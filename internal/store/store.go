// Package store implements the read-only "stored" engine over maki session
// logs (CONTRACTS 3). It reads <stateDir>/sessions/*.jsonl, takes no lock,
// writes nothing, and never trusts an offset saved across reads: a rewrite can
// shrink a log at any moment.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"maki-gateway/internal/model"
)

const (
	sessionsSub = "sessions"

	// maxKnownLogVersion is the newest header version this reader understands.
	// Newer logs are skipped rather than mis-read (CONTRACTS 3).
	maxKnownLogVersion = 3

	tailWindow      = 32 << 10 // first tail read, per CONTRACTS 3
	maxTailWindow   = 1 << 20  // ceiling when a meta line overflows the window
	bufioReaderSize = 64 << 10
	firstLineStep   = 4 << 10
	maxFirstLine    = 1 << 20 // header lines are small; this only bounds damage
	maxRecordLine   = 8 << 20 // same cap as the socket protocol (CONTRACTS 1)
	maxSearchLine   = 1 << 20 // search skips lines above this (tool output noise)
	snippetPad      = 80      // context chars kept around a search match
	maxSearchHits   = 50      // per CONTRACTS 2
)

// errOversizeLine reports a line past the caller's cap: the file stays
// readable, the line is skipped.
var errOversizeLine = errors.New("store: oversized line")

// Engine answers model.Engine for kind "stored".
type Engine struct{ dir string }

// New points the engine at <stateDir>/sessions. A missing directory is not an
// error: the engine lists nothing, so a machine with no maki history still
// starts. dir must exist as a directory when it is present.
func New(stateDir string) (*Engine, error) {
	if stateDir == "" {
		return nil, errors.New("store: empty state dir")
	}
	dir := filepath.Join(stateDir, sessionsSub)
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return nil, fmt.Errorf("store: %s is not a directory", dir)
	}
	return &Engine{dir: dir}, nil
}

func (e *Engine) Kind() string { return model.KindStored }

// List reads every log's header and last meta record, newest change first.
// A log that cannot be parsed is left out rather than failing the call: maki
// owns these files and may be writing one while we read it.
func (e *Engine) List() ([]model.Session, error) {
	files, err := scanFiles(e.dir)
	if err != nil {
		return nil, err
	}
	out := make([]model.Session, 0, len(files))
	for _, path := range files {
		s, ok := e.session(path)
		if ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// session projects one log onto model.Session. Stored sessions have no live
// state, so they report idle: the UI shows a finished conversation rather
// than nothing.
func (e *Engine) session(path string) (model.Session, bool) {
	h, ok := readHeader(path)
	if !ok {
		return model.Session{}, false
	}
	id := h.ID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	s := model.Session{
		ID:        id,
		Kind:      model.KindStored,
		Status:    model.StatusIdle,
		Cwd:       h.Cwd,
		Model:     h.Model,
		UpdatedAt: h.CreatedAt,
	}
	if m, ok := readMeta(path); ok {
		s.Title = m.Title
		if m.UpdatedAt != 0 {
			s.UpdatedAt = m.UpdatedAt
		}
	}
	return s, true
}

// Messages returns the last N transcript records of one session, oldest
// first. last <= 0 returns all of them. Records are passed through as native
// maki JSON (CONTRACTS 3).
func (e *Engine) Messages(id string, last int) ([]model.Message, error) {
	path, err := e.path(id)
	if err != nil {
		return nil, err
	}
	var out []model.Message
	err = walk(path, maxRecordLine, func(line []byte) error {
		if !hasTag(line, msgTag) {
			return nil
		}
		var rec msgLine
		if err := json.Unmarshal(line, &rec); err != nil || len(rec.D) == 0 {
			return nil
		}
		var m model.Message
		if err := json.Unmarshal(rec.D, &m); err != nil {
			return nil
		}
		out = append(out, m)
		if last > 0 && len(out) > last {
			// Zero the dropped prefix: the backing array would otherwise keep
			// every transcript block of a long session alive for this request.
			drop := len(out) - last
			clear(out[:drop])
			out = out[drop:]
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, model.ErrNotFound
	}
	return out, err
}

// path resolves a session id to its log, refusing anything that is not a bare
// log name: ids arrive from HTTP.
func (e *Engine) path(id string) (string, error) {
	if id == "" || id != filepath.Base(id) || !strings.HasSuffix(id, ".jsonl") {
		id += ".jsonl"
	}
	if !safeName(id) {
		return "", model.ErrNotFound
	}
	path := filepath.Join(e.dir, id)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.ErrNotFound
		}
		return "", err
	}
	return path, nil
}

// safeName accepts the id alphabet maki writes (uuids and short base62-ish
// ids) and nothing that could escape the sessions directory.
func safeName(id string) bool {
	if len(id) <= len(".jsonl") || !strings.HasSuffix(id, ".jsonl") {
		return false
	}
	return strings.IndexFunc(id[:len(id)-len(".jsonl")], func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' ||
			('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'))
	}) < 0
}

// walk hands every complete line of path to fn, stopping on its error. Lines
// longer than maxLine are skipped. The line buffer is only valid during the
// call.
func walk(path string, maxLine int, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, bufioReaderSize)
	buf := make([]byte, 0, firstLineStep)
	for {
		line, err := readLine(br, buf, maxLine)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errOversizeLine) {
			continue
		}
		if err != nil {
			return err
		}
		if err := fn(line); err != nil {
			return err
		}
	}
}

// readLine returns the next complete '\n'-terminated line without its newline.
// The result aliases buf and is only valid until the next call.
func readLine(br *bufio.Reader, buf []byte, max int) ([]byte, error) {
	line := buf[:0]
	oversize := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !oversize {
			if len(line)+len(chunk) > max {
				oversize, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			// A trailing line without '\n' may be a half-written record.
			return nil, err
		}
		if oversize {
			return nil, errOversizeLine
		}
		return bytes.TrimRight(line, "\r\n"), nil
	}
}

// Read-only engine: writes belong to the live and web engines. Title edits on
// a stored session go through them once the session is open again, per the
// registry routing (CONTRACTS 4).
func (e *Engine) Prompt(string, string) (string, error) { return "", model.ErrUnsupported }
func (e *Engine) Cancel(string) error                   { return model.ErrUnsupported }
func (e *Engine) SetMode(string, string) error          { return model.ErrUnsupported }
func (e *Engine) SetTitle(string, string) error         { return model.ErrUnsupported }
func (e *Engine) Create(string, string, string) (string, error) {
	return "", model.ErrUnsupported
}
func (e *Engine) Kill(string) error { return model.ErrUnsupported }

// RespondPermission reports not found: a parked permission prompt only exists
// while its TUI lives, so the request id never belongs to this engine.
func (e *Engine) RespondPermission(int64, string, string) error { return model.ErrNotFound }
