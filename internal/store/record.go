package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// tailScratch reuses the tail read buffer: a list pass over a deep history
// would otherwise allocate a window per log, several times a second.
var tailScratch = sync.Pool{New: func() any {
	buf := make([]byte, 0, tailWindow)
	return &buf
}}

// On-disk shapes. CONTRACTS 3 writes header and meta as {"t":..,"d":{..}} while
// maki serialises both with the fields flat on the line (serde internally
// tagged enums). Both are accepted so either side can change first.
type (
	// headerBody is line 1 of a session log: it never moves after creation.
	headerBody struct {
		Version   int    `json:"v"`
		ID        string `json:"id"`
		Model     string `json:"model"`
		Cwd       string `json:"cwd"`
		CreatedAt int64  `json:"created_at"`
	}
	headerLine struct {
		T string `json:"t"`
		headerBody
		D *headerBody `json:"d"`
	}
	// metaBody is the latest-wins analytics and title record.
	metaBody struct {
		Title        string           `json:"title"`
		UpdatedAt    int64            `json:"updated_at"`
		UsageByModel map[string]usage `json:"usage_by_model"`
		TokenUsage   tokenUsage       `json:"token_usage"`
	}
	metaLine struct {
		T string `json:"t"`
		metaBody
		D *metaBody `json:"d"`
	}
	// usage is one model's share of a session (StoredTokenUsage in maki).
	usage struct {
		Input         int64    `json:"input"`
		Output        int64    `json:"output"`
		CacheCreation int64    `json:"cache_creation"`
		CacheRead     int64    `json:"cache_read"`
		Cost          *float64 `json:"cost"`
	}
	// tokenUsage is the older aggregate the log carries next to usage_by_model.
	// Logs written before per-model accounting have only this.
	tokenUsage struct {
		Input          int64 `json:"input_tokens"`
		Output         int64 `json:"output_tokens"`
		CacheCreation  int64 `json:"cache_creation_input_tokens"`
		CacheReadInput int64 `json:"cache_read_input_tokens"`
	}
	msgLine struct {
		T string          `json:"t"`
		D json.RawMessage `json:"d"`
	}
)

func (l headerLine) body() headerBody {
	if l.D != nil && (l.ID == "" || l.Version == 0) {
		return *l.D
	}
	return l.headerBody
}

func (l metaLine) body() metaBody {
	if l.D != nil && l.Title == "" && l.UpdatedAt == 0 {
		return *l.D
	}
	return l.metaBody
}

// totals folds usage_by_model into one row, falling back to the session-wide
// token_usage when the log predates per-model accounting.
func (m metaBody) totals() usage {
	if len(m.UsageByModel) == 0 {
		if m.TokenUsage.Input == 0 && m.TokenUsage.Output == 0 {
			return usage{}
		}
		return usage{
			Input:         m.TokenUsage.Input,
			Output:        m.TokenUsage.Output,
			CacheCreation: m.TokenUsage.CacheCreation,
			CacheRead:     m.TokenUsage.CacheReadInput,
		}
	}
	var out usage
	for _, u := range m.UsageByModel {
		out.Input += u.Input
		out.Output += u.Output
		out.CacheCreation += u.CacheCreation
		out.CacheRead += u.CacheRead
		if u.Cost != nil {
			c := out.Cost
			if c == nil {
				c = new(float64)
			}
			*c += *u.Cost
			out.Cost = c
		}
	}
	return out
}

// Record prefixes. maki writes the tag as the first key of a compact JSON
// object, so a prefix check is enough to skip a line without parsing it.
var (
	headerTag = []byte(`{"t":"header"`)
	metaTag   = []byte(`{"t":"meta"`)
	msgTag    = []byte(`{"t":"msg"`)
)

func hasTag(line, tag []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(line, " \t"), tag)
}

// readHeader returns line 1 of path. ok is false when the file is unreadable,
// headerless, or written by a newer maki than we can parse.
func readHeader(path string) (h headerBody, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return headerBody{}, false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, bufioReaderSize)
	line, err := readLine(br, make([]byte, 0, firstLineStep), maxFirstLine)
	if err != nil || !hasTag(line, headerTag) {
		return headerBody{}, false
	}
	var hl headerLine
	if err := json.Unmarshal(line, &hl); err != nil || hl.body().Version > maxKnownLogVersion {
		return headerBody{}, false
	}
	return hl.body(), true
}

// readMeta returns the last complete meta record of path. The window grows
// past tailWindow while no meta line is found, because a meta with many
// subagents can be wider than one window. A rewrite may shrink the file
// between the stat and the read, so a short read only narrows the window.
func readMeta(path string) (m metaBody, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return metaBody{}, false
	}
	defer f.Close()
	size, err := f.Stat()
	if err != nil {
		return metaBody{}, false
	}
	total := size.Size()
	scratch := tailScratch.Get().(*[]byte)
	defer tailScratch.Put(scratch)
	for window := int64(tailWindow); window < maxTailWindow; window *= 2 {
		if window >= total {
			window = total
		}
		if int64(cap(*scratch)) < window {
			*scratch = make([]byte, window)
		}
		buf := (*scratch)[:window]
		n, err := f.ReadAt(buf, total-window)
		if err != nil && !errors.Is(err, io.EOF) {
			return metaBody{}, false
		}
		if m, ok := lastMeta(buf[:n], window < total); ok {
			return m, true
		}
		if window == total {
			break
		}
	}
	return metaBody{}, false
}

// lastMeta scans the complete lines of a tail slice, newest wins. cut says the
// slice starts mid-line, so its first line is dropped.
func lastMeta(buf []byte, cut bool) (metaBody, bool) {
	if cut {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return metaBody{}, false
		}
		buf = buf[i+1:]
	}
	var last metaBody
	found := false
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return last, found
		}
		line := buf[:i]
		buf = buf[i+1:]
		if !hasTag(line, metaTag) {
			continue
		}
		var ml metaLine
		if err := json.Unmarshal(line, &ml); err != nil {
			continue
		}
		last, found = ml.body(), true
	}
}

// scanFiles lists the session logs in dir, sorted by name.
func scanFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".jsonl") {
			continue
		}
		// os.ReadDir is sorted by name, so the join keeps log-name order.
		out = append(out, filepath.Join(dir, en.Name()))
	}
	return out, nil
}
