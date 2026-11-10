package httpapi

import (
	"net/http"
	"path/filepath"
)

// StaticHandler serves the web UI from dir: "/" maps to dir/index.html, the
// rest to a plain file server.
func StaticHandler(dir string) http.Handler {
	index := filepath.Join(dir, "index.html")
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, index)
			return
		}
		fs.ServeHTTP(w, r)
	})
}
