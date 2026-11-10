package httpapi

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticHandler(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"index.html": "<html>gateway-ui</html>",
		"app.js":     "console.log(1)",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := StaticHandler(dir)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	wantStatus(t, rec, 200)
	if rec.Body.String() != files["index.html"] {
		t.Fatalf("/ served %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app.js", nil))
	wantStatus(t, rec, 200)
	if rec.Body.String() != files["app.js"] {
		t.Fatalf("/app.js served %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/missing.txt", nil))
	wantStatus(t, rec, 404)
}

func TestStaticMountedAtRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("root-ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	acp, att, st := trio()
	s, _ := newTestServer(t, func(c *Config) { c.Static = StaticHandler(dir) }, acp, att, st)
	rec := do(s, "GET", "/", "")
	wantStatus(t, rec, 200)
	if rec.Body.String() != "root-ok" {
		t.Fatalf("root served %q", rec.Body.String())
	}
	wantStatus(t, do(s, "GET", "/api/unknown", ""), 404)
}
