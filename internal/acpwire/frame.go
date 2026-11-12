// Package acpwire is the one ACP codec in this repository: JSON-RPC 2.0 over
// newline-delimited JSON on any io.ReadWriter.
//
// Two transports use it. internal/acpengine drives an `maki acp` child on its
// stdio (docs/ACP_MIN.md), and the attach bridge drives a maki Lua plugin on a
// unix socket (docs/WIRE.md). Both are ACP, so both speak this wire, and the
// codec knows nothing about either: it frames, numbers, correlates, and routes.
//
// The role is symmetric. This side sends requests and waits for answers, and it
// answers the requests the peer sends, on the same connection. ACP calls the
// two sides client and agent. A Conn is neither, and the caller decides by
// choosing which methods it handles.
package acpwire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Version is the JSON-RPC version written on every outgoing frame.
	Version = "2.0"

	// MaxLine is the frame limit in bytes, the trailing newline included. A
	// frame larger than this ends the connection: a peer that cannot fit an
	// answer in one line has nothing sensible left to say (docs/CONTRACTS.md 1).
	MaxLine = 8 << 20

	// readBufferSize bounds one Read on the transport, not one frame: a longer
	// frame is assembled from several reads.
	readBufferSize = 64 << 10

	// firstFrameCap is what the frame buffer starts at, so a normal frame costs
	// one allocation for the life of the connection.
	firstFrameCap = 4 << 10

	// Standard JSON-RPC 2.0 codes. A peer's own codes start at -32000 and are
	// the caller's business, so they are not named here.
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Frame kinds, as reported by Msg.Kind.
const (
	KindResponse     = "response"
	KindRequest      = "request"
	KindNotification = "notification"
)

// Msg is one decoded frame. The same shape covers a request, a notification,
// and a response, and Kind says which. Fields stay raw so a frame this codec does
// not know still reaches the caller intact.
type Msg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Kind reports whether the frame answers a request, asks one, or announces one.
// A frame that does none of those is a notification with nothing in it, which is
// what an unreadable id or a stray result looks like, and is ignored.
func (m *Msg) Kind() string {
	switch {
	case m.Method == "" && (len(m.Result) > 0 || m.Error != nil):
		return KindResponse
	case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
		return KindRequest
	default:
		return KindNotification
	}
}

// Payload is the answer of a response: its result, or its error.
func (m *Msg) Payload() (json.RawMessage, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

// Error is a JSON-RPC error object, and the error value of a failed call.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("acp error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("acp error %d: %s", e.Code, e.Message)
}

// outFrame is one outgoing frame. It is separate from Msg because a caller hands
// Params and Result as values to marshal rather than as bytes to re-encode.
type outFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// idKey is the correlation key of a JSON-RPC id.
//
// An id may be a number or a string: the ACP schema allows both, and a peer that
// re-serialises a number can move its spelling (1 to 1.0). Numbers are folded
// onto their canonical decimal form, and anything else keys on its raw text, so
// a string id matches only itself.
func idKey(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t[0] == '"' || t == "null" {
		return t
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	if n, err := strconv.ParseUint(t, 10, 64); err == nil {
		return strconv.FormatUint(n, 10)
	}
	if n, err := strconv.ParseFloat(t, 64); err == nil {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return t
}

// framer cuts a byte stream into frames on the newline. It owns the buffer the
// decoded bytes live in, which is valid until the next call.
type framer struct {
	max int
	buf []byte
}

// next returns the next frame without its newline. A frame over the limit is
// reported instead of grown into, so the caller can end the connection rather
// than follow an unbounded write.
func (f *framer) next(br *bufio.Reader) ([]byte, error) {
	f.buf = f.buf[:0]
	oversize := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !oversize {
			if len(f.buf)+len(chunk) > f.max {
				oversize, f.buf = true, f.buf[:0]
			} else {
				f.buf = append(f.buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if oversize {
			return nil, ErrFrameTooLarge
		}
		return trimEOL(f.buf), nil
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
