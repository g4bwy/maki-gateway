package acpwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
)

// ErrClosed ends a call or a write on a connection that has ended, and is the
// cause Serve reports when Close ended it.
var ErrClosed = errors.New("acpwire: connection closed")

// ErrFrameTooLarge reports a frame over MaxLine. It always ends the connection:
// a peer that cannot fit one answer in a line has nothing left to say.
var ErrFrameTooLarge = errors.New("acpwire: frame over the size limit")

// ErrAnswered reports a second answer to one request. A request is answered
// once. The call that got there first owns the frame.
var ErrAnswered = errors.New("acpwire: request already answered")

// ErrParse ends a connection whose peer wrote a frame that is not a JSON object.
// A peer that cannot frame one value has no useful answer left to give.
var ErrParse = errors.New("acpwire: frame is not a JSON object")

// ErrProtocol ends a connection whose peer sent a response with neither a result
// nor an error. That frame answers nothing, and reading it as an empty success
// would tell a caller its request went through (docs/CONTRACTS.md 1 fixed this the
// hard way).
var ErrProtocol = errors.New("acpwire: response carries neither result nor error")

// Options configures a Conn. The zero value is a working connection with no
// handler, which reads the stream and drops everything that is not a reply.
type Options struct {
	// MaxLine is the frame limit in bytes. Zero means MaxLine.
	MaxLine int

	// Death is the error every call still open receives when the connection
	// ends. A caller with a name for that event (a process that exited, a plugin
	// that went away) sets it, so a waiter reads a cause rather than a close.
	// nil means an internal error that says "connection closed".
	Death *Error

	// Logger takes the debug lines: a reply nobody waits for, a frame that could
	// not be answered. nil means slog.Default.
	Logger *slog.Logger

	// OnRequest answers a request the peer sends. It runs on the read pump, so it
	// must not block: park the ask and answer it later through the Request.
	//
	// Returning a nil error means the handler took ownership and will answer it
	// itself. Returning an error answers the request with that error at once, as
	// an *Error when it is one and as an internal error otherwise, unless the
	// handler already answered it.
	OnRequest func(ctx context.Context, r *Request) error

	// OnNotification takes every notification the peer sends, including the
	// methods this side has no use for. Unknown names are ignored by the handler:
	// they are not a fault, and a protocol grows by adding notifications.
	OnNotification func(method string, params json.RawMessage)

	// OnClose runs once per connection, after it ended and after every open call
	// failed. It reports the cause: io.EOF for a clean end of stream, a read
	// error, ErrFrameTooLarge, or ErrClosed for a local Close.
	OnClose func(cause error)
}

// Conn is one JSON-RPC 2.0 conversation over a bidirectional byte stream: a
// read pump that routes frames, a serialised writer, and the calls waiting for
// an answer. It is safe for concurrent use, and one goroutine must run Serve.
type Conn struct {
	rw      io.ReadWriter
	log     *slog.Logger
	maxLine int
	death   *Error
	framer  framer

	onReq   func(context.Context, *Request) error
	onNote  func(method string, params json.RawMessage)
	onClose func(cause error)

	ctx    context.Context
	cancel context.CancelFunc

	wmu  sync.Mutex
	wbuf bytes.Buffer
	enc  *json.Encoder

	mu      sync.Mutex
	next    int64
	pending map[string]chan Msg

	closed  atomic.Bool
	endOnce sync.Once
}

// New builds a connection over a transport. Nothing is read until Serve or Start
// runs, so a caller can finish its own setup first.
func New(rw io.ReadWriter, opts Options) *Conn {
	max := opts.MaxLine
	if max <= 0 {
		max = MaxLine
	}
	death := opts.Death
	if death == nil {
		death = &Error{Code: CodeInternal, Message: "connection closed"}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		rw:      rw,
		log:     log.With("comp", "acpwire"),
		maxLine: max,
		death:   death,
		framer:  framer{max: max, buf: make([]byte, 0, firstFrameCap)},
		onReq:   opts.OnRequest,
		onNote:  opts.OnNotification,
		onClose: opts.OnClose,
		ctx:     ctx,
		cancel:  cancel,
		pending: map[string]chan Msg{},
	}
	c.enc = json.NewEncoder(&c.wbuf)
	return c
}

// Start runs Serve on its own goroutine.
func (c *Conn) Start() { go c.Serve() }

// Serve reads and routes frames until the stream ends, then ends the connection
// and reports the cause: io.EOF for a clean end of stream, a read error,
// ErrFrameTooLarge, ErrParse, or ErrProtocol. A stream that stops is never a nil
// return, because a caller with work outstanding has something to fix or report.
func (c *Conn) Serve() error {
	br := bufio.NewReaderSize(c.rw, readBufferSize)
	var cause error
	for {
		line, err := c.framer.next(br)
		if err != nil {
			cause = err
			break
		}
		if len(line) == 0 {
			continue
		}
		var m Msg
		if err := json.Unmarshal(line, &m); err != nil {
			cause = fmt.Errorf("%w: %v", ErrParse, err)
			c.log.Warn("acpwire: unparseable frame, ending connection", "error", err)
			break
		}
		if err := c.dispatch(&m); err != nil {
			cause = err
			break
		}
	}
	c.end(cause)
	return cause
}

// Closed reports whether the connection has ended.
func (c *Conn) Closed() bool { return c.closed.Load() }

// Context is the connection's lifetime: it is done once the stream ended, so a
// handler parked on it stops with the peer.
func (c *Conn) Context() context.Context { return c.ctx }

// Close ends the connection, fails every open call, and closes the transport
// when it can be closed. It is idempotent, and unblocks Serve.
func (c *Conn) Close() error {
	c.closed.Store(true)
	var err error
	if cl, ok := c.rw.(io.Closer); ok {
		err = cl.Close()
	}
	c.end(ErrClosed)
	return err
}

// Send writes a request and returns the call that waits for its answer. The
// caller owns the wait: a method answered at the end of a turn is answered by
// whoever reads Call.Resp, whenever that is.
func (c *Conn) Send(method string, params any) (*Call, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	c.mu.Lock()
	c.next++
	key := strconv.FormatInt(c.next, 10)
	ch := make(chan Msg, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.write(outFrame{ID: json.RawMessage(key), Method: method, Params: params}); err != nil {
		c.forget(key)
		return nil, err
	}
	return &Call{ID: json.RawMessage(key), Resp: ch, conn: c, key: key}, nil
}

// Call sends a request and waits for the answer, or for ctx to end. A ctx that
// ends drops the pending call: its answer, whenever it arrives, is ignored.
// Nothing on this wire can undo a request that already left.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cl, err := c.Send(method, params)
	if err != nil {
		return nil, err
	}
	return cl.Await(ctx)
}

// Notify writes a frame that expects no answer.
func (c *Conn) Notify(method string, params any) error {
	return c.write(outFrame{Method: method, Params: params})
}

// dispatch routes one frame. A non-nil error is a fault of the peer's framing, and
// ends the connection.
func (c *Conn) dispatch(m *Msg) error {
	switch m.Kind() {
	case KindResponse:
		c.answer(m)
	case KindRequest:
		c.ask(m)
	default:
		if len(m.ID) > 0 && m.Method == "" {
			c.log.Warn("acpwire: response with neither result nor error, ending connection",
				"id", string(m.ID))
			return ErrProtocol
		}
		if c.onNote != nil {
			c.onNote(m.Method, m.Params)
		}
	}
	return nil
}

// ask hands an incoming request to the handler. A handler that returns an error
// without answering gets that error written back, so a parked ask is always the
// handler's own choice rather than a forgotten reply.
func (c *Conn) ask(m *Msg) {
	r := &Request{ID: m.ID, Method: m.Method, Params: m.Params, conn: c}
	if c.onReq == nil {
		_ = r.Fail(CodeMethodNotFound, "method not found")
		return
	}
	err := c.onReq(c.ctx, r)
	if err == nil {
		return
	}
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Message: err.Error()}
	}
	_ = r.failWith(e)
}

func (c *Conn) write(frame outFrame) error {
	frame.JSONRPC = Version
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	c.wbuf.Reset()
	if err := c.enc.Encode(frame); err != nil {
		return err
	}
	if c.wbuf.Len() > c.maxLine {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, c.wbuf.Len())
	}
	if _, err := c.rw.Write(c.wbuf.Bytes()); err != nil {
		return err
	}
	return nil
}

// answer hands a response to the call that sent it. An id with no waiter is
// logged and dropped: the call timed out, or the peer answered twice.
func (c *Conn) answer(m *Msg) {
	key := idKey(m.ID)
	c.mu.Lock()
	ch, ok := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if !ok {
		c.log.Debug("acpwire: reply for a call that is gone", "id", string(m.ID))
		return
	}
	ch <- *m
}

func (c *Conn) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

// end runs the teardown once: no more writes, no more waiters, then the caller's
// notice that the conversation is over.
func (c *Conn) end(cause error) {
	if cause == nil {
		cause = io.EOF
	}
	c.closed.Store(true)
	c.mu.Lock()
	waiting := c.pending
	c.pending = map[string]chan Msg{}
	c.mu.Unlock()
	gone := Msg{Error: c.death}
	for _, ch := range waiting {
		ch <- gone
	}
	c.endOnce.Do(func() {
		c.cancel()
		if c.onClose != nil {
			c.onClose(cause)
		}
	})
}

// Request is one request the peer sent and this side still owes an answer to.
// Keep it to answer late. A request never answered parks whatever waits for it on
// the other end.
type Request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage

	conn     *Conn
	answered atomic.Bool
}

// Answer replies with result. A nil result answers with JSON null, which is a
// value: a response with no result field at all is not an answer.
func (r *Request) Answer(result any) error {
	if !r.answered.CompareAndSwap(false, true) {
		return ErrAnswered
	}
	if result == nil {
		result = json.RawMessage("null")
	}
	return r.conn.write(outFrame{ID: r.ID, Result: result})
}

// Fail replies with a JSON-RPC error.
func (r *Request) Fail(code int, message string) error {
	return r.failWith(&Error{Code: code, Message: message})
}

// FailWith replies with a full error object, data included.
func (r *Request) FailWith(e *Error) error {
	return r.failWith(e)
}

func (r *Request) failWith(e *Error) error {
	if !r.answered.CompareAndSwap(false, true) {
		return ErrAnswered
	}
	return r.conn.write(outFrame{ID: r.ID, Error: e})
}

// Answered reports whether the request has an answer written or claimed already.
func (r *Request) Answered() bool { return r.answered.Load() }

// Call is one request this side sent. Resp carries exactly one frame: the
// answer, or the death of the connection.
type Call struct {
	ID   json.RawMessage
	Resp <-chan Msg

	conn *Conn
	key  string
}

// Await waits for the answer on Resp, or for ctx to end.
func (cl *Call) Await(ctx context.Context) (json.RawMessage, error) {
	select {
	case m := <-cl.Resp:
		return m.Payload()
	case <-ctx.Done():
		cl.conn.forget(cl.key)
		return nil, ctx.Err()
	}
}
