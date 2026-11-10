package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maki-gateway/internal/model"
	"maki-gateway/internal/registry"
)

const testToken = "s3cret-token"

func envWith(token string) func(string) string {
	return func(key string) string {
		if key == tokenEnv {
			return token
		}
		return ""
	}
}

func bearerReq(method, path, auth string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAuthLoopbackNeedsNoToken(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765"} {
		s, _ := newTestServer(t, func(c *Config) { c.Addr = addr; c.LookupEnv = envWith("") })
		wantStatus(t, do(s, "GET", "/api/health", ""), 200)
	}
}

func TestAuthPublicWithoutTokenRefusesBind(t *testing.T) {
	for _, addr := range []string{":8765", "0.0.0.0:8765", "10.0.0.5:8765"} {
		hub := model.NewHub()
		reg := registry.New(hub)
		if _, err := New(Config{Hub: hub, Registry: reg, Addr: addr, LookupEnv: envWith("")}); err == nil {
			t.Fatalf("New(%s) without token must refuse the bind", addr)
		}
	}
}

func TestAuthPublicEnforcesToken(t *testing.T) {
	for _, addr := range []string{":8765", "0.0.0.0:8765", "10.0.0.5:8765"} {
		s, _ := newTestServer(t, func(c *Config) { c.Addr = addr; c.LookupEnv = envWith(testToken) })
		wantStatus(t, do(s, "GET", "/api/health", ""), 401)
		wantStatus(t, serve(s, bearerReq("GET", "/api/health", "Bearer "+testToken)), 200)
		wantStatus(t, serve(s, bearerReq("GET", "/api/health", "bearer "+testToken)), 200)
		wantStatus(t, serve(s, bearerReq("GET", "/api/health?token="+testToken, "")), 200)
		wantStatus(t, serve(s, bearerReq("GET", "/api/health", "Bearer wrong")), 401)
		wantStatus(t, serve(s, bearerReq("GET", "/api/health?token=wrong", "")), 401)
		wantStatus(t, serve(s, bearerReq("GET", "/api/sessions?token="+testToken, "")), 200)
		wantStatus(t, serve(s, bearerReq("POST", "/api/sessions?token=wrong", "")), 401)
	}
}

func TestAuthPublicCoversStatic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestServer(t, func(c *Config) {
		c.Addr = "0.0.0.0:8765"
		c.LookupEnv = envWith(testToken)
		c.Static = StaticHandler(dir)
	})
	wantStatus(t, serve(s, bearerReq("GET", "/", "")), 401)
	rec := serve(s, bearerReq("GET", "/?token="+testToken, ""))
	wantStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("static body = %q", rec.Body.String())
	}
}

func TestAddrHost(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8765":      "127.0.0.1",
		"localhost:8765":      "localhost",
		"[::1]:8765":          "::1",
		"[fe80::1%eth0]:8080": "fe80::1",
		":8765":               "",
		"0.0.0.0:8765":        "0.0.0.0",
		"10.0.0.5":            "10.0.0.5",
	}
	for addr, want := range cases {
		if got := addrHost(addr); got != want {
			t.Errorf("addrHost(%q) = %q, want %q", addr, got, want)
		}
	}
}
