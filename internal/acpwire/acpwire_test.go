package acpwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests talk to a Conn through a duplex pipe of os.Pipes, not net.Pipe: an
// in-memory socket is unbuffered, so every write would wait for the test to read,
// and a framing test has to leave half a line in flight while it does something
// else. Kernel pipes carry 64 KiB, which is more than any frame here.

// end is one side of a duplex pipe. It is both io.ReadWriter and io.Closer, which
// is all a Conn asks of a transport.
type end struct {
	r, w *os.File
}

func (e *end) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *end) Write(p []byte) (int, error) { return e.w.Write(p) }

func (e *end) Close() error {
	rerr, werr := e.r.Close(), e.w.Close()
	if rerr != nil {
		return rerr
	}
	return werr
}

func duplex(t *testing.T) (*end, *end) {
	t.Helper()
	ar, bw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	br, aw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	a, b := &end{r: ar, w: aw}, &end{r: br, w: bw}
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b
}

// peer is the other end of a connection. It writes bytes, so a test can frame
// badly on purpose, and reads whole lines back.
type peer struct {
	e  *end
	br *bufio.Reader
}

const peerWait = 3 * time.Second

func dial(t *testing.T, opts Options) (*Conn, *peer) {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	mine, theirs := duplex(t)
	c := New(mine, opts)
	c.Start()
	return c, &peer{e: theirs, br: bufio.NewReader(theirs)}
}

func (p *peer) send(t *testing.T, s string) {
	t.Helper()
	if err := p.e.w.SetWriteDeadline(time.Now().Add(peerWait)); err != nil {
		t.Fatalf("write deadline: %v", err)
	}
	if _, err := io.WriteString(p.e.w, s); err != nil {
		t.Fatalf("peer write %q: %v", s, err)
	}
}

func (p *peer) line(t *testing.T) string {
	t.Helper()
	if err := p.e.r.SetReadDeadline(time.Now().Add(peerWait)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	s, err := p.br.ReadString('\n')
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	return s
}

func (p *peer) frame(t *testing.T) Msg {
	t.Helper()
	raw := p.line(t)
	var m Msg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("peer read %q: %v", raw, err)
	}
	return m
}

// request is one outgoing request frame the peer wrote, as text.
func requestFrame(id, method string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `"}` + "\n"
}

// answerCh runs a pump that answers every request with result, so a caller that
// waits on a reply is not waiting forever.
func answerAll(result string) func(context.Context, *Request) error {
	return func(_ context.Context, r *Request) error { return r.Answer(json.RawMessage(result)) }
}

// TestSplitFramesAreReassembled covers the way a stream really arrives: one frame
// cut into pieces, then pieces of a second frame. A codec that delivered on a read
// instead of on a newline would mis-frame the second one, and the request in the
// batch is the probe that proves where the boundary fell.
func TestSplitFramesAreReassembled(t *testing.T) {
	notes := make(chan Msg, 8)
	_, p := dial(t, Options{
		OnRequest:      answerAll(`{"pong":true}`),
		OnNotification: func(method string, params json.RawMessage) { notes <- Msg{Method: method, Params: params} },
	})

	for _, part := range []string{
		`{"jsonrpc":"2.0","method":"sh.maki/note","params":{"n":`,
		`1}}` + "\n",
		`{"jsonrpc":"2.0","id":`,
		`7,"method":"ping"}` + "\n",
	} {
		p.send(t, part)
	}

	var got Msg
	select {
	case got = <-notes:
	case <-time.After(peerWait):
		t.Fatal("the fragmented notification never arrived")
	}
	if got.Method != "sh.maki/note" || string(got.Params) != `{"n":1}` {
		t.Errorf("notification = %s %s, want sh.maki/note with {\"n\":1}", got.Method, got.Params)
	}
	res := p.frame(t)
	if res.Method != "" || res.Error != nil {
		t.Fatalf("reply = %+v, want a plain response", res)
	}
	if string(res.ID) != "7" || string(res.Result) != `{"pong":true}` {
		t.Errorf("reply id %s result %s, want id 7 with {\"pong\":true}", res.ID, res.Result)
	}
}

// TestFramesBatchedInOneWrite covers the other half of framing: two whole lines
// landing in one read.
func TestFramesBatchedInOneWrite(t *testing.T) {
	notes := make(chan Msg, 8)
	_, p := dial(t, Options{
		OnNotification: func(method string, params json.RawMessage) { notes <- Msg{Method: method} },
	})
	p.send(t, "{\"method\":\"a\"}\n{\"method\":\"b\"}\n")

	for _, want := range []string{"a", "b"} {
		select {
		case got := <-notes:
			if got.Method != want {
				t.Fatalf("notification = %s, want %s", got.Method, want)
			}
		case <-time.After(peerWait):
			t.Fatalf("notification %s never arrived", want)
		}
	}
}

// TestOversizeFrameEndsTheConnection checks the byte limit is enforced on the way
// in, before anything is parsed, and that a call in flight learns the truth.
func TestOversizeFrameEndsTheConnection(t *testing.T) {
	const deathText = "plugin went away"
	closed := make(chan error, 2)
	c, p := dial(t, Options{
		MaxLine: 64,
		Death:   &Error{Code: -32001, Message: deathText},
		OnClose: func(cause error) { closed <- cause },
	})

	if err := c.Notify("ping", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	p.line(t)

	call, err := c.Send("session/load", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.line(t)
	p.send(t, `{"jsonrpc":"2.0","method":"big","params":{"blob":"`+strings.Repeat("x", 200)+`"}}`+"\n")

	if cause := <-closed; !errors.Is(cause, ErrFrameTooLarge) {
		t.Errorf("close cause = %v, want %v", cause, ErrFrameTooLarge)
	}
	if !c.Closed() {
		t.Error("conn still reports itself open after an oversize frame")
	}
	_, err = call.Await(context.Background())
	if code, msg := wireErrorOf(t, err); code != -32001 || msg != deathText {
		t.Errorf("pending call = %d %q, want the death error -32001 %q", code, msg, deathText)
	}
	if _, err := c.Send("anything", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after death = %v, want %v", err, ErrClosed)
	}
}

// TestOversizeWriteRefused checks a frame too big to send is refused whole: half a
// line on the wire would corrupt every frame after it.
func TestOversizeWriteRefused(t *testing.T) {
	c, p := dial(t, Options{
		MaxLine:   96,
		OnRequest: answerAll(`{"ok":true}`),
	})
	err := c.Notify("sh.maki/notify", map[string]string{"text": strings.Repeat("x", 400)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize Notify = %v, want %v", err, ErrFrameTooLarge)
	}
	p.send(t, requestFrame("1", "initialize"))
	if res := p.frame(t); string(res.ID) != "1" || res.Error != nil {
		t.Errorf("reply = %+v, want the connection to be usable still", res)
	}
}

// TestIDTypes covers the ids a peer may use. A number that comes back in another
// spelling still belongs to its caller, and a string id is echoed as it arrived
// because this side never invents one.
func TestIDTypes(t *testing.T) {
	c, p := dial(t, Options{OnRequest: answerAll(`{"ok":true}`)})

	for _, tc := range []struct{ name, sent, echoed string }{
		{"number echoed as written", "1", "1"},
		{"number re-spelled by the peer", "2", "2.0"},
	} {
		call, err := c.Send("method", nil)
		if err != nil {
			t.Fatalf("%s: Send: %v", tc.name, err)
		}
		if string(call.ID) != tc.sent {
			t.Fatalf("%s: outgoing id = %s, want %s", tc.name, call.ID, tc.sent)
		}
		p.line(t)
		p.send(t, `{"jsonrpc":"2.0","id":`+tc.echoed+`,"result":{"n":1}}`+"\n")
		if _, err := call.Await(context.Background()); err != nil {
			t.Errorf("%s: Await: %v", tc.name, err)
		}
	}

	p.send(t, `{"jsonrpc":"2.0","id":"tab-42/x","method":"session/request_permission","params":{}}`+"\n")
	if res := p.frame(t); string(res.ID) != `"tab-42/x"` {
		t.Errorf("echoed id = %s, want the peer's own string id", res.ID)
	}

	// A reply for no caller is dropped, and the conversation goes on.
	p.send(t, `{"jsonrpc":"2.0","id":9999,"result":{}}`+"\n")
	call, err := c.Send("session/list", nil)
	if err != nil {
		t.Fatalf("Send after a stray reply: %v", err)
	}
	p.line(t)
	p.send(t, `{"jsonrpc":"2.0","id":3,"result":{}}`+"\n")
	if _, err := call.Await(context.Background()); err != nil {
		t.Errorf("call after a stray reply: %v", err)
	}
}

// TestIDKeySpelling pins the folding rule a numeric id needs, so a peer that
// re-serialises a number still reaches its caller.
func TestIDKeySpelling(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`1`, "1"},
		{` 1 `, "1"},
		{`1.0`, "1"},
		{`007`, "7"},
		{`18446744073709551615`, "18446744073709551615"},
		{`-3`, "-3"},
		{`"1"`, `"1"`},
		{`"tab/1"`, `"tab/1"`},
		{`null`, "null"},
		{``, ""},
	}
	for _, tc := range cases {
		if got := idKey(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("idKey(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestPendingCallsFailWhenThePeerDied is the attach case: the plugin process ends
// mid-turn, and nobody waiting on it may keep waiting.
func TestPendingCallsFailWhenThePeerDied(t *testing.T) {
	const deathText = "process exited"
	closed := make(chan error, 2)
	c, p := dial(t, Options{
		Death:   &Error{Code: CodeInternal, Message: deathText},
		OnClose: func(cause error) { closed <- cause },
	})

	first, err := c.Send("session/prompt", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.line(t)
	second, err := c.Send("session/set_mode", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.line(t)

	p.send(t, `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"cancelled"}}`+"\n")
	if m := <-first.Resp; !strings.Contains(string(m.Result), "cancelled") {
		t.Errorf("answered call = %s, want its own answer", m.Result)
	}
	if err := p.e.Close(); err != nil {
		t.Fatalf("peer close: %v", err)
	}

	_, err = second.Await(context.Background())
	if code, msg := wireErrorOf(t, err); code != CodeInternal || msg != deathText {
		t.Errorf("pending call = %d %q, want the death frame %d %q", code, msg, CodeInternal, deathText)
	}
	if cause := <-closed; cause == nil {
		t.Error("OnClose reported no cause for a dead peer")
	}
	if !c.Closed() {
		t.Error("conn still reports itself open")
	}
}

// TestUnknownNotificationIgnored is a protocol rule that holds on both ends: a
// method this build does not know is skipped, and the conversation goes on.
func TestUnknownNotificationIgnored(t *testing.T) {
	notes := make(chan string, 8)
	c, p := dial(t, Options{
		OnRequest:      answerAll(`{"ready":true}`),
		OnNotification: func(method string, _ json.RawMessage) { notes <- method },
	})
	p.send(t, `{"jsonrpc":"2.0","method":"sh.maki/from_the_future","params":{"whatever":1}}`+"\n")
	select {
	case got := <-notes:
		if got != "sh.maki/from_the_future" {
			t.Fatalf("handler saw %s, want the unknown notification", got)
		}
	case <-time.After(peerWait):
		t.Fatal("the unknown notification never reached the handler")
	}

	if _, err := c.Send("initialize", nil); err != nil {
		t.Fatalf("Send after an unknown notification: %v", err)
	}
	p.line(t)
	p.send(t, requestFrame("5", "session/request_permission"))
	res := p.frame(t)
	if string(res.ID) != "5" || res.Method != "" || res.Error != nil {
		t.Errorf("reply to the parked ask = %+v, want a response to id 5", res)
	}
	if !strings.Contains(string(res.Result), "ready") {
		t.Errorf("result = %s, want the handler's answer", res.Result)
	}

	// With no notification handler at all the frame is simply gone.
	quiet, qp := dial(t, Options{})
	qp.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{}}`+"\n")
	if err := quiet.Notify("ping", nil); err != nil {
		t.Fatalf("Notify on a conn with no handlers: %v", err)
	}
	if !strings.Contains(qp.line(t), "ping") {
		t.Error("the notification this side sent never went out")
	}
}

// TestIncomingRequestAnsweredLate is the permission bridge: the ask is parked, and
// the answer goes out whenever a person replies.
func TestIncomingRequestAnsweredLate(t *testing.T) {
	parked := make(chan *Request, 2)
	c, p := dial(t, Options{OnRequest: func(_ context.Context, r *Request) error {
		parked <- r
		return nil
	}})

	p.send(t, `{"jsonrpc":"2.0","id":1000,"method":"session/request_permission","params":{"sessionId":"s"}}`+"\n")
	r := <-parked
	if r.Method != "session/request_permission" || !strings.Contains(string(r.Params), "sessionId") {
		t.Fatalf("parked ask = %+v, want the permission request", r)
	}
	if r.Answered() {
		t.Error("a fresh ask reports itself answered")
	}
	if err := r.Answer(map[string]any{"outcome": map[string]string{"outcome": "selected"}}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	res := p.frame(t)
	if string(res.ID) != "1000" || !strings.Contains(string(res.Result), "selected") {
		t.Errorf("reply id %s result %s, want id 1000 with the chosen outcome", res.ID, res.Result)
	}
	if err := r.Fail(CodeInternal, "too late"); !errors.Is(err, ErrAnswered) {
		t.Errorf("second answer = %v, want %v", err, ErrAnswered)
	}
	// Nothing was written for the refused second answer: the next frame on the
	// wire is the reply to the next ask, and not a stale echo of the first.
	p.send(t, `{"jsonrpc":"2.0","id":1001,"method":"session/request_permission","params":{}}`+"\n")
	next := <-parked
	if err := next.Answer(map[string]string{"outcome": "cancelled"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res := p.frame(t); string(res.ID) != "1001" {
		t.Errorf("next frame id = %s, want 1001", res.ID)
	}
	if _, err := c.Send("ping", nil); err != nil {
		t.Errorf("Send: %v", err)
	}
	p.line(t)
}

// TestIncomingRequestErrorFromHandler covers a handler that refuses at once: the
// peer gets an error frame rather than silence.
func TestIncomingRequestErrorFromHandler(t *testing.T) {
	_, p := dial(t, Options{OnRequest: func(_ context.Context, r *Request) error {
		if r.Method == "session/nope" {
			return &Error{Code: CodeMethodNotFound, Message: "method not found"}
		}
		return errors.New("boom")
	}})

	p.send(t, requestFrame(`"a1"`, "session/nope"))
	res := p.frame(t)
	if string(res.ID) != `"a1"` || res.Error == nil || res.Error.Code != CodeMethodNotFound {
		t.Errorf("reply = %+v, want a method-not-found error with the string id echoed", res)
	}

	p.send(t, requestFrame(`"a2"`, "session/other"))
	res = p.frame(t)
	if res.Error == nil || res.Error.Code != CodeInternal || res.Error.Message != "boom" {
		t.Errorf("reply = %+v, want an internal error quoting the handler", res)
	}
}

// TestCallTimeoutDropsThePendingCall: a caller that gives up must not hold the
// connection, and the answer that arrives afterwards is ignored.
func TestCallTimeoutDropsThePendingCall(t *testing.T) {
	c, p := dial(t, Options{OnRequest: answerAll(`{"ok":true}`)})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "session/load", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call = %v, want the deadline to end it", err)
	}
	if c.Closed() {
		t.Fatal("a timed-out call ended the connection")
	}
	p.send(t, `{"jsonrpc":"2.0","id":1,"result":{"late":true}}`+"\n")

	call, err := c.Send("session/list", nil)
	if err != nil {
		t.Fatalf("Send after a timeout: %v", err)
	}
	p.line(t)
	p.send(t, `{"jsonrpc":"2.0","id":2,"result":{"sessions":[]}}`+"\n")
	raw, err := call.Await(context.Background())
	if err != nil {
		t.Fatalf("Await after a timed-out call: %v", err)
	}
	if !strings.Contains(string(raw), "sessions") {
		t.Errorf("result = %s, want the second call's answer", raw)
	}
}

// TestFrameShape checks what goes on the wire: every frame carries jsonrpc, a
// notification carries no id, and nothing else is invented.
func TestFrameShape(t *testing.T) {
	c, p := dial(t, Options{})
	if err := c.Notify("session/cancel", map[string]string{"sessionId": "s1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got, want := p.line(t), `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`+"\n"; got != want {
		t.Errorf("notification = %s, want %s", got, want)
	}
	if _, err := c.Send("session/load", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, want := p.line(t), `{"jsonrpc":"2.0","id":1,"method":"session/load"}`+"\n"; got != want {
		t.Errorf("request = %s, want %s", got, want)
	}
	if err := c.Notify("answer/null", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got, want := p.line(t), `{"jsonrpc":"2.0","method":"answer/null"}`+"\n"; got != want {
		t.Errorf("frame without params = %s, want %s", got, want)
	}
}

// TestServeReportsCauseAndRunsOnce covers the pump's own end: a clean EOF is
// reported to the caller and to OnClose, and a Close afterwards changes nothing.
func TestServeReportsCauseAndRunsOnce(t *testing.T) {
	mine, theirs := duplex(t)
	closes := make(chan error, 4)
	c := New(mine, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnClose: func(cause error) { closes <- cause }})

	done := make(chan error, 1)
	go func() { done <- c.Serve() }()
	if err := theirs.Close(); err != nil {
		t.Fatalf("peer close: %v", err)
	}

	if cause := <-done; !errors.Is(cause, io.EOF) {
		t.Errorf("Serve = %v, want EOF", cause)
	}
	if cause := <-closes; !errors.Is(cause, io.EOF) {
		t.Errorf("OnClose = %v, want EOF", cause)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close after the pump ended: %v", err)
	}
	if len(closes) != 0 {
		t.Errorf("OnClose ran %d more times, want once", len(closes))
	}
}

// wireErrorOf reads the code and message of a failed call.
// TestResponseWithNoAnswerCloses is the rule a bare `{"id":N}` frame breaks. It
// answers nothing, so the connection ends rather than read it as an empty success.
func TestResponseWithNoAnswerCloses(t *testing.T) {
	closed := make(chan error, 2)
	c, p := dial(t, Options{OnClose: func(cause error) { closed <- cause }})

	call, err := c.Send("session/prompt", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.line(t)
	p.send(t, `{"jsonrpc":"2.0","id":1}`+"\n")

	if cause := <-closed; !errors.Is(cause, ErrProtocol) {
		t.Errorf("close cause = %v, want %v", cause, ErrProtocol)
	}
	if _, err := call.Await(context.Background()); err == nil {
		t.Error("the call that was waiting reported a success")
	}
	// A response whose result is JSON null is still an answer.
	quiet, qp := dial(t, Options{OnRequest: answerAll(`{"ok":true}`)})
	if err := quiet.Notify("nullish", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	qp.send(t, `{"jsonrpc":"2.0","id":1,"result":null}`+"\n")
	if !strings.Contains(qp.line(t), "nullish") {
		t.Error("a null result ended the connection")
	}
}

// TestStrayResponseReachesTheHandler is the report a caller gets when a response
// matches no open call: the answer of a live call is routed and never reaches the
// hook, a second answer for the same id is a stray, and an error answer arrives as
// a stray with no result. None of them is a fault of the connection.
func TestStrayResponseReachesTheHandler(t *testing.T) {
	stray := make(chan Msg, 4)
	c, p := dial(t, Options{OnStrayResponse: func(id, result json.RawMessage) {
		stray <- Msg{ID: id, Result: result}
	}})

	call, err := c.Send("session/list", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.line(t)
	p.send(t, `{"jsonrpc":"2.0","id":1,"result":{"sessions":[]}}`+"\n")
	if raw, err := call.Await(context.Background()); err != nil || string(raw) != `{"sessions":[]}` {
		t.Fatalf("Await = %s, %v", raw, err)
	}
	if len(stray) != 0 {
		t.Fatalf("the answer of an open call reached the hook: %v", <-stray)
	}

	p.send(t, `{"jsonrpc":"2.0","id":1,"result":{"late":true}}`+"\n")
	p.send(t, `{"jsonrpc":"2.0","id":"tab-9","error":{"code":-32603,"message":"boom"}}`+"\n")
	for _, want := range []struct{ id, result string }{
		{"1", `{"late":true}`},
		{`"tab-9"`, ""},
	} {
		var got Msg
		select {
		case got = <-stray:
		case <-time.After(peerWait):
			t.Fatalf("no stray report for id %s", want.id)
		}
		if string(got.ID) != want.id || string(got.Result) != want.result {
			t.Errorf("stray = (%s, %s), want (%s, %s)", got.ID, got.Result, want.id, want.result)
		}
	}
	if c.Closed() {
		t.Error("a stray reply must not end the connection")
	}
	// A connection with no hook keeps the old behavior: the frame is dropped, and
	// the conversation goes on (TestIDTypes covers the drop itself).
	quiet, qp := dial(t, Options{OnRequest: answerAll(`{"ok":true}`)})
	qp.send(t, `{"jsonrpc":"2.0","id":42,"result":{}}`+"\n")
	if err := quiet.Notify("after", nil); err != nil {
		t.Fatalf("Notify after a stray reply: %v", err)
	}
	if !strings.Contains(qp.line(t), "after") {
		t.Error("the connection did not survive a stray reply")
	}
}

// TestIDPrefixKeysStringRequests is the ruling of WIRE 1.4 for a side that shares
// its socket with a peer that also sends requests: its ids are JSON strings, so a
// peer's answer to one of its own requests can never be read as the answer to a
// call here. The number and the string spelling of the same count both miss.
func TestIDPrefixKeysStringRequests(t *testing.T) {
	stray := make(chan string, 4)
	c, p := dial(t, Options{
		IDPrefix:        "g-",
		OnRequest:       answerAll(`{"ok":true}`),
		OnStrayResponse: func(id, _ json.RawMessage) { stray <- string(id) },
	})

	call, err := c.Send("session/prompt", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(call.ID) != `"g-1"` {
		t.Fatalf("outgoing id = %s, want the string \"g-1\"", call.ID)
	}
	sent := p.frame(t)
	if string(sent.ID) != `"g-1"` || sent.Method != "session/prompt" {
		t.Fatalf("frame on the wire = %+v, want a string id and the method", sent)
	}

	p.send(t, `{"jsonrpc":"2.0","id":1,"result":{"outcome":"cancelled"}}`+"\n")
	p.send(t, `{"jsonrpc":"2.0","id":"1","result":{"outcome":"cancelled"}}`+"\n")
	for _, foreign := range []string{"1", `"1"`} {
		select {
		case got := <-stray:
			if got != foreign {
				t.Errorf("stray id = %s, want %s", got, foreign)
			}
		case <-time.After(peerWait):
			t.Fatalf("the reply to id %s never reached the stray hook", foreign)
		}
	}
	select {
	case got := <-stray:
		t.Fatalf("a foreign id was reported twice: %s", got)
	default:
	}

	p.send(t, `{"jsonrpc":"2.0","id":"g-1","result":{"stopReason":"end_turn"}}`+"\n")
	raw, err := call.Await(context.Background())
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if string(raw) != `{"stopReason":"end_turn"}` {
		t.Errorf("result = %s, want the body of the call", raw)
	}
	if len(stray) != 0 {
		t.Errorf("the answer of an open call reached the hook: %s", <-stray)
	}
}

func wireErrorOf(t *testing.T, err error) (int, string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v (%T), want a wire error", err, err)
	}
	return e.Code, e.Message
}
