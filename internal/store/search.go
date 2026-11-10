package store

import (
	"bytes"
	"errors"
	"fmt"
	"maki-gateway/internal/model"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Hit is one search result: the session, its title, and a window of raw log
// text around the match (CONTRACTS 2, GET /api/search).
type Hit = model.SearchHit

const (
	// searchWorkers bounds the parallel log scan: history runs to gigabytes,
	// so a serial walk is a minute of latency.
	searchWorkers = 8

	// upperASCII is the fold alphabet: bytes.ToLower only changes these bytes,
	// so a line without them needs no second pass.
	upperASCII = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// errStopWalk ends a walk early once the answer is known.
var errStopWalk = errors.New("store: stop walk")

// imageTag marks a line holding image content: base64 blobs are noise, so
// search never matches inside them.
var imageTag = []byte(`"type":"image"`)

// Search scans every log for a substring of the raw lines, case-insensitive in
// the ASCII sense (as SQL LIKE is), and keeps one snippet per session, at most
// limit sessions (limit <= 0 means the contract cap). Files are scanned in
// parallel; the hit set is the first matches in log-name order once enough
// exist. The cap never rises above maxSearchHits.
func (e *Engine) Search(q string, limit int) ([]Hit, error) {
	needle := asciiLower(strings.TrimSpace(q))
	if needle == "" {
		return nil, nil
	}
	if limit <= 0 || limit > maxSearchHits {
		limit = maxSearchHits
	}
	files, err := scanFiles(e.dir)
	if err != nil {
		return nil, err
	}
	var (
		want  = int64(limit)
		found atomic.Int64
		wg    sync.WaitGroup
		work  = make(chan int)
		slots = make([]Hit, len(files))
		hit   = make([]bool, len(files))
	)
	for range min(searchWorkers, len(files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if found.Load() >= want {
					continue
				}
				if h, ok := searchFile(files[i], needle, func() bool {
					return found.Load() >= want
				}); ok {
					slots[i], hit[i] = h, true
					found.Add(1)
				}
			}
		}()
	}
	for i := range files {
		work <- i
	}
	close(work)
	wg.Wait()

	var hits []Hit
	for i := range files {
		if hit[i] {
			hits = append(hits, slots[i])
			if len(hits) == limit {
				break
			}
		}
	}
	return hits, nil
}

// searchFile returns the first snippet of one log matching the lower-cased
// needle, with the session title folded in. stop lets a scan give up once the
// caller has enough hits, so a 1 GB log cannot hold the pool hostage.
func searchFile(path, needle string, stop func() bool) (Hit, bool) {
	seg := []byte(needle)
	h, _ := readHeader(path)
	id := h.ID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	var snippet string
	err := walk(path, maxSearchLine, func(line []byte) error {
		if stop() {
			return errStopWalk
		}
		if bytes.Contains(line, imageTag) {
			return nil
		}
		i := bytes.Index(line, seg)
		if i < 0 && bytes.ContainsAny(line, upperASCII) {
			i = bytes.Index(bytes.ToLower(line), seg)
		}
		if i < 0 {
			return nil
		}
		snippet = aroundMatch(line, i, len(seg))
		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return Hit{}, false
	}
	if snippet == "" {
		return Hit{}, false
	}
	out := Hit{Session: id, Snippet: snippet}
	if m, ok := readMeta(path); ok {
		out.Title = m.Title
	}
	return out, true
}

func isUpperASCII(c byte) bool { return 'A' <= c && c <= 'Z' }

// asciiLower folds an ASCII query down; the log side uses bytes.ToLower, so
// both halves of the comparison follow the same rule.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if isUpperASCII(c) {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// aroundMatch cuts a snippetPad-wide window around a match, snapped to rune
// boundaries.
func aroundMatch(line []byte, at, width int) string {
	start := runeStart(line, max(at-snippetPad, 0))
	end := runeStart(line, min(at+width+snippetPad, len(line)))
	if start >= end {
		return ""
	}
	edge := func(cut bool) string {
		if cut {
			return "…"
		}
		return ""
	}
	return fmt.Sprintf("%s%s%s", edge(start > 0), strings.TrimSpace(string(line[start:end])), edge(end < len(line)))
}

// runeStart walks back to the first byte of the rune at i.
func runeStart(b []byte, i int) int {
	for i > 0 && i < len(b) && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}
