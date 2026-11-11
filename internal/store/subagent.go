package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"maki-gateway/internal/model"
)

const (
	// subTailWindow is the first tail read of a transcript. One append batch of
	// sub-agent records plus its meta line usually fits, and a session log runs to
	// a gigabyte, so reading one whole to answer a page is not an option.
	subTailWindow = 4 << 10

	// maxSubMessages caps one answer (CONTRACTS 2). maki writes a task's whole
	// transcript in one burst when the run ends, so a bounded page is all a
	// browser can show and all one request should cost.
	maxSubMessages = 500

	// subMsgHead is the first two keys of a sub-agent record as maki serialises
	// them, up to the quote that opens the task id. The id closes the prefix, so
	// one task's filter never lands on a longer id of another task.
	subMsgHead = `{"t":"sub_msg","sub":"`
)

// SubMessages returns the `d` payloads of one task's records, oldest first
// (CONTRACTS 2, GET /api/sessions/{id}/tasks/{tid}/messages). It reads the
// session log, so a live tab, a web child and a stored session all answer the
// same way and no engine has to be alive to answer.
//
// A task that is still running has no records to give: maki keeps a sub-agent's
// messages in memory and appends them together when the run ends. That is a real
// empty answer, so it is reported as one rather than dressed up as a wait.
//
// Only what maki has committed counts. Every append batch ends with exactly one
// meta line inside one write_all, so a line at or before the file's last complete
// meta line is finished and a line after it belongs to a batch still in flight.
// Bytes past the final newline cost nothing either, since an unterminated line is
// no record.
//
// last is capped at maxSubMessages, and a last of 0 or less asks for that many.
// An unknown session answers model.ErrNotFound, and so does a task id outside the
// alphabet maki writes, since no record of one can ever match.
func (e *Engine) SubMessages(sessionID, taskID string, last int) ([]json.RawMessage, error) {
	path, err := e.path(sessionID)
	if err != nil {
		return nil, err
	}
	if !safeTaskID(taskID) {
		return nil, model.ErrNotFound
	}
	want := last
	if want <= 0 || want > maxSubMessages {
		want = maxSubMessages
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrNotFound
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	total := info.Size()
	tag := []byte(subMsgHead + taskID + `"`)
	// The window runs to the end of the file, so the last complete meta line
	// inside it is the last one the file has. A window holding none of them has
	// not reached the committed edge, and the next one covers twice the ground.
	for window := min(subTailWindow, total); ; window = min(2*window, total) {
		msgs, enough, err := readSubTail(f, window, total, tag, want)
		if err != nil || enough || window >= total {
			return msgs, err
		}
	}
}

// readSubTail reads the last window bytes of an open log and returns the
// committed payloads of the lines carrying tag, oldest first. enough says this
// window already holds `want` of them, so reading further back cannot change the
// answer.
func readSubTail(f *os.File, window, total int64, tag []byte, want int) (msgs []json.RawMessage, enough bool, err error) {
	if window <= 0 {
		return nil, true, nil // an empty log has committed nothing
	}
	buf := make([]byte, window)
	n, err := f.ReadAt(buf, total-window)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	lines := buf[:n]
	if window < total {
		// The window starts mid-line, so its first line is a record fragment.
		i := bytes.IndexByte(lines, '\n')
		if i < 0 {
			return nil, false, nil
		}
		lines = lines[i+1:]
	}
	end := bytes.LastIndexByte(lines, '\n')
	if end < 0 {
		return nil, false, nil
	}
	edge, ok := committedEdge(lines[:end+1])
	if !ok {
		return nil, false, nil
	}
	// Records past the committed edge belong to a batch that has not ended.
	whole := lines[:edge]
	spans := subSpans(whole, tag, want)
	out := make([]json.RawMessage, 0, len(spans))
	for _, start := range spans {
		i := bytes.IndexByte(whole[start:], '\n')
		if i < 0 {
			break
		}
		var rec msgLine
		if err := json.Unmarshal(whole[start:start+i], &rec); err != nil || len(rec.D) == 0 {
			continue
		}
		// Copied out, so the returned page does not pin the whole window.
		out = append(out, append(json.RawMessage(nil), rec.D...))
	}
	return out, len(spans) >= want, nil
}

// committedEdge returns the offset just past the last complete meta line of buf,
// which is where maki finished writing. ok is false when buf holds no meta line,
// so nothing in it is committed yet. The walk starts at the end, because one
// append batch normally puts its meta line within the last few bytes.
func committedEdge(buf []byte) (edge int, ok bool) {
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end-1], '\n') + 1
		if hasTag(buf[start:end-1], metaTag) {
			return end, true
		}
		end = start
	}
	return 0, false
}

// subSpans returns the start offsets of the lines of buf carrying tag, oldest
// first, keeping the newest want of them. Only the prefix is checked, because a
// log holds far more lines than it holds matches and parsing them all is what a
// big file must not cost. A line past the record cap is skipped rather than
// parsed, as elsewhere in this reader.
func subSpans(buf, tag []byte, want int) []int {
	var spans []int
	for start := 0; start < len(buf); {
		i := bytes.IndexByte(buf[start:], '\n')
		if i < 0 {
			break
		}
		if line := buf[start : start+i]; hasTag(line, tag) && len(line) <= maxRecordLine {
			spans = append(spans, start)
			if over := len(spans) - want; over > 0 {
				clear(spans[:over])
				spans = spans[over:]
			}
		}
		start += i + 1
	}
	return spans
}

// safeTaskID accepts the alphabet maki writes for a task id, which is a tool call
// id or the literal "main". The filter this builds is a byte prefix, so an id
// carrying a quote or a brace would reach into another task's records.
func safeTaskID(id string) bool {
	if id == "" {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' ||
			('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'))
	}) < 0
}
