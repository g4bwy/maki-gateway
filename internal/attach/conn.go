package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"maki-gateway/internal/acpwire"
)

// errGone answers every request still open when a plugin connection ends, and is
// the cause of a local hang-up. WIRE 13.2 makes the end of a connection a fault
// of the conversation rather than of one request, so one value covers every
// death. No substring of it is a word of the WIRE 13.1 table, so MapError leaves
// it alone and the caller reads a transport fault.
var errGone = &acpwire.Error{Code: acpwire.CodeInternal, Message: "attach: plugin connection closed"}

// errNoBody is a result that is absent or JSON null where the method promises an
// object, so a caller never reads an empty success it has nothing to decode. A
// frame with neither reply field at all is the codec's ErrProtocol, and it ends
// the connection (WIRE 1.3).
var errNoBody = errors.New("attach: protocol violation: reply carries no result body")

// tooOld is the one fix for a refused plugin. It is the words in the error log
// line and in the notice the browser shows (WIRE 2.2), because a silent refusal
// is what made v1 confusing when an old plugin met a new gateway.
const tooOld = "plugin too old, run /packupdate maki-gateway"

// refusalNotice is the SSE notice of a refused plugin. It names the pid, as
// WIRE 2.2 asks, so a reader of the browser can tie it to the log line.
const refusalNotice = "attach refused (pid %d): plugin too old for this gateway, run /packupdate maki-gateway"

// noArgs is the params of a request that takes none. WIRE 1.3 fixes the
// spelling: an empty object, not an absent field.
var noArgs = map[string]any{}

// requestIDPrefix is the shape of every request id this gateway puts on a plugin
// socket: the JSON string "g-<n>". A plugin numbers its own requests from 1000
// upward, and a bare number this side could mint reaches that range, so the
// answer a plugin sends to its own prompt withdrawal used to land on an
// unrelated open call (WIRE 1.4). Strings cannot collide with numbers.
const requestIDPrefix = "g-"

// conn is one plugin socket: the acpwire connection, what the handshake
// reported, the liveness clock, and the tool cards this connection has seen open.
type conn struct {
	e    *Engine
	path string
	wire *acpwire.Conn

	mu     sync.Mutex
	misses int
	info   agentInfo
	tools  map[string]bool
}

func newConn(e *Engine, path string, nc net.Conn) *conn {
	c := &conn{e: e, path: path, tools: map[string]bool{}}
	c.wire = acpwire.New(nc, acpwire.Options{
		MaxLine:         e.t.maxLine,
		Death:           errGone,
		IDPrefix:        requestIDPrefix,
		OnRequest:       c.onAsk,
		OnNotification:  c.onNote,
		OnStrayResponse: c.withdrawn,
		OnClose:         c.died,
	})
	return c
}

// serve starts the read pump. Nothing is routed before initialize is answered
// (WIRE 2.1), so the handshake of dial runs after this and blocks on its own call.
func (c *conn) serve() { c.wire.Start() }

// initialize registers this connection (WIRE 2.1) and caches what its _meta
// reports: the pid the list rows and the notices name, the cwd, the attach build,
// and the maki version. The gate of WIRE 2.2 belongs to the caller, because it
// owes a log line and a notice rather than an error.
func (c *conn) initialize(ctx context.Context) (agentInfo, error) {
	raw, err := c.wire.Call(ctx, mInitialize, map[string]any{
		argProtocolVersion:    acpProtocol,
		argClientCapabilities: map[string]any{},
	})
	if err != nil {
		return c.agent(), err
	}
	var res initResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return c.agent(), fmt.Errorf("attach: %s result: %w", mInitialize, err)
	}
	info := agentInfo{cwd: res.Meta.str(metaCwd), makiVer: res.Meta.str(metaMakiVersion)}
	if pid, ok := res.Meta.num(metaPID); ok {
		info.pid = int(pid)
	}
	info.proto, info.protoSeen = res.Meta.num(metaAttachProto)
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	return info, nil
}

func (c *conn) agent() agentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

func (c *conn) pluginPID() int { return c.agent().pid }

// request is one call on the wire. A connection that has ended answers at once,
// so a caller never waits on a socket that cannot answer.
func (c *conn) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.wire.Closed() {
		return nil, errGone
	}
	return c.wire.Call(ctx, method, params)
}

// call sends one request and decodes the result into out (skip with nil).
func (c *conn) call(ctx context.Context, method string, params any, out any) error {
	raw, err := c.request(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("attach: %s: %w", method, errNoBody)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("attach: %s result: %w", method, err)
	}
	return nil
}

// countMiss records one failed list poll and reports whether the plugin has now
// missed the limit. The list poll is the liveness clock of WIRE 1.5: there is no
// ping, and a plugin that stops answering it is gone or stuck.
func (c *conn) countMiss() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.misses++
	return c.misses
}

func (c *conn) resetMisses() {
	c.mu.Lock()
	c.misses = 0
	c.mu.Unlock()
}

// openTool marks one tool card open and reports whether it was open already.
func (c *conn) openTool(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tools) >= maxToolCards {
		c.tools = map[string]bool{}
	}
	was := c.tools[id]
	c.tools[id] = true
	return was
}

// closeTool forgets one card and reports whether it was open.
func (c *conn) closeTool(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.tools[id]
	delete(c.tools, id)
	return was
}

// drop ends the connection. Every open call fails with errGone, and died reports
// the fact once.
func (c *conn) drop() {
	if err := c.wire.Close(); err != nil {
		slog.Debug("attach: close socket", "socket", c.path, "error", err)
	}
}

// died runs once per connection, after the codec ended it and failed every open
// call. It owns the disconnect report, because the cause is only known here.
func (c *conn) died(cause error) {
	switch {
	case errors.Is(cause, acpwire.ErrFrameTooLarge):
		slog.Warn("attach: oversized frame, dropping plugin", "socket", c.path, "limit", c.e.t.maxLine)
	case errors.Is(cause, acpwire.ErrProtocol):
		slog.Warn("attach: plugin reply with neither result nor error, dropping plugin", "socket", c.path)
	case errors.Is(cause, acpwire.ErrParse):
		slog.Warn("attach: unparseable frame, dropping plugin", "socket", c.path, "error", cause)
	default: // a clean end of stream, or a hang-up this side asked for
		slog.Debug("attach: plugin connection closed", "socket", c.path, "reason", cause)
	}
	if tracked := c.e.forgetConn(c); tracked && !c.e.isClosed() {
		c.e.disconnected(c.pluginPID())
	}
}

// canonID is the correlation key of a JSON-RPC id, the way the codec keys its
// own calls: a number on its canonical decimal form, anything else on its text.
func canonID(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if len(t) > 0 && t[0] == '"' {
		if s, err := strconv.Unquote(t); err == nil {
			return s
		}
		return t
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return t
}
