package attach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// errGone reports a connection that died while a request was in flight.
var errGone = errors.New("attach: plugin connection closed")

// errOversizeLine reports a frame past 8 MiB, which ends the connection
// (CONTRACTS 1).
var errOversizeLine = errors.New("attach: oversized frame")

// reply is one answer, matched to its request by id.
type reply struct {
	result json.RawMessage
	err    error
}

// pluginError carries the error string the plugin replied with, so the op
// mappers can recognise it.
type pluginError string

func (p pluginError) Error() string { return string(p) }

// conn is one plugin socket: a reader goroutine, a serialised writer, and the
// requests waiting for their answer.
type conn struct {
	e    *Engine
	path string
	nc   net.Conn

	writeMu sync.Mutex
	enc     *json.Encoder

	mu      sync.Mutex
	pending map[int64]chan reply
	misses  int
	pid     int

	done      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
}

func newConn(e *Engine, path string, nc net.Conn) *conn {
	c := &conn{
		e:       e,
		path:    path,
		nc:      nc,
		pending: map[int64]chan reply{},
		done:    make(chan struct{}),
	}
	c.enc = json.NewEncoder(nc)
	return c
}

// hello asks the plugin who it is and remembers the pid the notice text and
// model.Session.PluginPID report.
func (c *conn) hello(ctx context.Context) (helloBody, error) {
	var h helloBody
	if err := c.call(ctx, opHello, nil, &h); err != nil {
		return h, err
	}
	c.mu.Lock()
	c.pid = h.PID
	c.mu.Unlock()
	return h, nil
}

func (c *conn) pluginPID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid
}

// request is one op on the wire. args are merged into the frame next to id and
// op, so a nil map means an argument-less op. The id is registered before the
// check of closed: that way a drop racing this call either finds the id in the
// map and answers it, or the caller sees errGone. Never a hung request.
func (c *conn) request(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	id := c.e.nextID.Add(1)
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	dead := c.closed.Load()
	c.mu.Unlock()
	defer c.forget(id)
	if dead {
		return nil, errGone
	}

	frame := make(map[string]any, len(args)+2)
	frame["id"] = id
	frame["op"] = op
	for k, v := range args {
		frame[k] = v
	}
	if err := c.write(frame); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("attach: %s: %w", op, ctx.Err())
	case <-c.done:
		return nil, errGone
	}
}

// call sends one op and decodes the ok value into out (skip with nil).
func (c *conn) call(ctx context.Context, op string, args map[string]any, out any) error {
	raw, err := c.request(ctx, op, args)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("attach: %s reply: %w", op, err)
	}
	return nil
}

func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// write serialises one frame. json.Encoder appends the newline for us.
func (c *conn) write(frame any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errGone
	}
	if err := c.enc.Encode(frame); err != nil {
		return fmt.Errorf("attach: write %s: %w", c.path, err)
	}
	return nil
}

// read splits the stream into frames and routes them: an id goes to the waiting
// request, an event name goes to the hub. Frames are cut on the newline by hand
// rather than with a json.Decoder, because CONTRACTS 1 makes an 8 MiB frame
// fatal: the cap has to be enforced on the bytes, before anything is parsed.
// It owns the connection's death: whatever the exit, everything pending is
// failed and the socket closed.
func (c *conn) read() {
	br := bufio.NewReaderSize(c.nc, readBufferSize)
	buf := make([]byte, 0, firstLineStep)
	var reason error
	for {
		line, err := readLine(br, buf, c.e.t.maxLine)
		switch {
		case errors.Is(err, errOversizeLine):
			slog.Warn("attach: oversized frame, dropping plugin", "socket", c.path, "limit", c.e.t.maxLine)
			reason = errOversizeLine
		case errors.Is(err, io.EOF):
			reason = io.EOF
		case err != nil:
			reason = err
		}
		if reason != nil {
			break
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Debug("attach: unparseable frame", "socket", c.path, "error", err)
			continue
		}
		if m.ID != nil {
			c.deliver(&m)
			continue
		}
		if m.Event != "" {
			c.handleEvent(&m)
		}
	}
	c.drop(reason)
}

// deliver hands an answer to its waiter, if anyone still waits.
func (c *conn) deliver(m *message) {
	c.mu.Lock()
	ch := c.pending[*m.ID]
	delete(c.pending, *m.ID)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	r := reply{result: m.OK}
	if m.Error != nil {
		r.err = pluginError(*m.Error)
	}
	ch <- r
}

// keepalive pings on its own clock: two misses in a row mean the TUI is gone or
// stuck, and a stuck plugin would otherwise hold its sessions open forever.
func (c *conn) keepalive() {
	t := time.NewTicker(c.e.t.pingEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			var pong string
			ctx, cancel := context.WithTimeout(context.Background(), c.e.t.timeout)
			err := c.call(ctx, opPing, nil, &pong)
			cancel()
			if err == nil {
				c.resetMisses()
				continue
			}
			if c.countMiss() < c.e.t.missLimit {
				slog.Debug("attach: ping missed", "socket", c.path, "error", err)
				continue
			}
			c.drop(fmt.Errorf("ping: %w", err))
			return
		}
	}
}

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

// drop ends the connection once, whatever the reason.
func (c *conn) drop(reason error) {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		if err := c.nc.Close(); err != nil {
			slog.Debug("attach: close socket", "error", err)
		}
		c.mu.Lock()
		for _, ch := range c.pending {
			ch <- reply{err: errGone}
		}
		c.pending = map[int64]chan reply{}
		c.mu.Unlock()
		slog.Debug("attach: plugin connection closed", "socket", c.path, "reason", reason)
		if tracked := c.e.forgetConn(c); tracked && !c.e.isClosed() {
			c.e.disconnected(c.pluginPID())
		}
	})
}

// readLine returns the next '\n'-terminated frame without its newline. The
// result aliases buf and is only valid until the next call. A frame longer than
// max is drained and reported as errOversizeLine, so the caller can close the
// connection instead of growing without bound.
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
			return nil, err
		}
		if oversize {
			return nil, errOversizeLine
		}
		return line, nil
	}
}
