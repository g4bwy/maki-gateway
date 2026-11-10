package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maki-gateway/internal/model"
)

func TestSSEStream(t *testing.T) {
	acp, att, st := trio()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, hub := newTestServer(t, func(c *Config) { c.Ctx = ctx }, acp, att, st)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache control = %q", got)
	}

	lines := make(chan string, 128)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	nextLine := func() (string, bool) {
		select {
		case l, ok := <-lines:
			return l, ok
		case <-time.After(2 * time.Second):
			return "", false
		}
	}

	if l, ok := nextLine(); !ok || l != "retry: 3000" {
		t.Fatalf("stream must open with literal retry hint, got %q (ok=%v)", l, ok)
	}
	if l, ok := nextLine(); !ok || l != "" {
		t.Fatalf("retry field must end with a blank line, got %q", l)
	}

	hub.Publish(model.Event{Type: "notice", Text: "hello sse"})
	for {
		l, ok := nextLine()
		if !ok {
			t.Fatal("stream ended before the maki event arrived")
		}
		if l == "event: maki" {
			break
		}
	}
	data, ok := nextLine()
	if !ok || !strings.HasPrefix(data, "data: ") {
		t.Fatalf("want data line after event header, got %q", data)
	}
	if !strings.Contains(data, `"type":"notice"`) || !strings.Contains(data, `"text":"hello sse"`) {
		t.Fatalf("event JSON = %q", data)
	}

	// Config.Ctx cancellation must end the stream (server shutdown).
	cancel()
	drained := make(chan struct{})
	go func() {
		for range lines {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("stream not closed after Ctx cancel")
	}
}
