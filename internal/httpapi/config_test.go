package httpapi

import (
	"net/http"
	"net/http/cookiejar"
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

func staticDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html": "<html>ok</html>",
		"app.js":     "console.log('ui');",
		"style.css":  "body{}",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return jar
}

func cookieReq(path, cookie string) *http.Request {
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: authCookie, Value: cookie})
	}
	return req
}

// A token bind must serve the SPA shell: the document request seeds a cookie,
// and the subresource requests the browser cannot give a token carry it back.
func TestAuthCookieSeededByQueryAndHeader(t *testing.T) {
	dir := staticDir(t)
	s, _ := newTestServer(t, func(c *Config) {
		c.Addr = "0.0.0.0:8765"
		c.LookupEnv = envWith(testToken)
		c.Static = StaticHandler(dir)
	})
	rec := serve(s, bearerReq("GET", "/?token="+testToken, ""))
	wantStatus(t, rec, 200)
	got := rec.Header().Get("Set-Cookie")
	for _, want := range []string{authCookie + "=" + testToken, "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(got, want) {
			t.Errorf("Set-Cookie %q missing %q", got, want)
		}
	}
	hdr := serve(s, bearerReq("GET", "/api/health", "Bearer "+testToken))
	wantStatus(t, hdr, 200)
	if hdr.Header().Get("Set-Cookie") == "" {
		t.Error("header auth must seed the cookie too")
	}
}

func TestAuthCookieAcceptedAndRejected(t *testing.T) {
	dir := staticDir(t)
	s, _ := newTestServer(t, func(c *Config) {
		c.Addr = "0.0.0.0:8765"
		c.LookupEnv = envWith(testToken)
		c.Static = StaticHandler(dir)
	})
	asset := serve(s, cookieReq("/app.js", testToken))
	wantStatus(t, asset, 200)
	if !strings.Contains(asset.Body.String(), "ui") {
		t.Fatalf("asset body = %q", asset.Body.String())
	}
	if asset.Header().Get("Set-Cookie") != "" {
		t.Errorf("a cookie-authenticated request must not re-seed: %q", asset.Header().Get("Set-Cookie"))
	}
	wantStatus(t, serve(s, cookieReq("/style.css", testToken)), 200)
	wantStatus(t, serve(s, cookieReq("/api/health", testToken)), 200)
	wantStatus(t, serve(s, cookieReq("/app.js", "wrong-"+testToken)), 401)
	wantStatus(t, serve(s, cookieReq("/app.js", "")), 401)
	// A forged query token is not rescued by a good cookie, and a good query
	// token still works next to a bad cookie.
	wantStatus(t, serve(s, bearerReq("GET", "/app.js?token=wrong", "")), 401)
}

func TestAuthLoopbackNeverTouchesCookie(t *testing.T) {
	dir := staticDir(t)
	s, _ := newTestServer(t, func(c *Config) {
		c.Addr = "127.0.0.1:8765"
		c.LookupEnv = envWith(testToken)
		c.Static = StaticHandler(dir)
	})
	rec := serve(s, bearerReq("GET", "/?token="+testToken, ""))
	wantStatus(t, rec, 200)
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("loopback must not set a cookie, got %q", rec.Header().Get("Set-Cookie"))
	}
	wantStatus(t, serve(s, cookieReq("/app.js", "garbage")), 200)
	wantStatus(t, serve(s, cookieReq("/app.js", "")), 200)
}

// The full round trip through real headers: one client, one cookie jar, no token
// in the asset URLs, which is how a browser asks for the SPA subresources.
func TestAuthCookieRoundTripWithClient(t *testing.T) {
	dir := staticDir(t)
	s, _ := newTestServer(t, func(c *Config) {
		c.Addr = "10.0.0.5:8765"
		c.LookupEnv = envWith(testToken)
		c.Static = StaticHandler(dir)
	})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	client := &http.Client{Jar: newTestJar()}

	res, err := client.Get(ts.URL + "/?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("document status = %d, want 200", res.StatusCode)
	}
	for _, path := range []string{"/app.js", "/style.css"} {
		sub, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		sub.Body.Close()
		if sub.StatusCode != 200 {
			t.Errorf("%s status = %d, want 200 from the seeded cookie", path, sub.StatusCode)
		}
	}
	bare, err := http.Get(ts.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	bare.Body.Close()
	if bare.StatusCode != 401 {
		t.Errorf("a client without the cookie got %d, want 401", bare.StatusCode)
	}
}
