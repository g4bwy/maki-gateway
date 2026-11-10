// Package httpapi serves the gateway HTTP/SSE API and the static web UI.
// Contract: docs/CONTRACTS.md section 2; behavior: docs/DESIGN_CORE.md.
package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"maki-gateway/internal/model"
	"maki-gateway/internal/registry"
)

const (
	tokenEnv       = "MAKI_GATEWAY_TOKEN"
	defaultVersion = "dev"
	defaultLast    = 200
	defaultDays    = 30
	searchCap      = 50
	sseRetry       = "retry: 3000\n\n"
	ssePing        = ": ping\n\n"
	pingInterval   = 20 * time.Second
)

// Config wires the server to its dependencies (DESIGN_CORE).
type Config struct {
	Hub       *model.Hub
	Registry  *registry.Registry
	MakiPath  string
	Addr      string
	Version   string
	LookupEnv func(string) string
	Static    http.Handler
	Engines   []model.Engine
	Ctx       context.Context
}

type authChecker struct {
	required bool
	token    string
}

// newAuth decides whether addr is a public bind. An empty host (bare ":8765")
// counts as public. Public binds refuse to start without MAKI_GATEWAY_TOKEN.
func newAuth(addr string, getenv func(string) string) (authChecker, error) {
	host := addrHost(addr)
	if host == "localhost" {
		return authChecker{}, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return authChecker{}, nil
	}
	token := getenv(tokenEnv)
	if token == "" {
		return authChecker{}, errors.New("httpapi: bind " + addr + " is not loopback; set " + tokenEnv)
	}
	return authChecker{required: true, token: token}, nil
}

func addrHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return host
}

// Server is the authenticated, logged, routed gateway handler.
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	auth authChecker

	makiOnce sync.Once
	makiVer  string
}

func New(cfg Config) (*Server, error) {
	if cfg.Hub == nil || cfg.Registry == nil {
		return nil, errors.New("httpapi: Config.Hub and Config.Registry are required")
	}
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.Getenv
	}
	a, err := newAuth(cfg.Addr, cfg.LookupEnv)
	if err != nil {
		return nil, err
	}
	if cfg.MakiPath == "" {
		cfg.MakiPath = "maki"
	}
	if cfg.Version == "" {
		cfg.Version = defaultVersion
	}
	s := &Server{cfg: cfg, auth: a}
	s.routes()
	return s, nil
}

// ServeHTTP enforces auth, then serves through a status-logging wrapper.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.auth.required && !s.auth.allows(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
}

func (a authChecker) allows(r *http.Request) bool {
	got := ""
	if v := r.URL.Query().Get("token"); v != "" {
		got = v
	} else if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			got = cred
		}
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

// MakiVersion caches one `maki --version` execution for the process lifetime.
// "" when the command fails. Exported so main.go can warm it before serving.
func (s *Server) MakiVersion() string {
	s.makiOnce.Do(func() {
		out, err := exec.CommandContext(s.cfg.Ctx, s.cfg.MakiPath, "--version").Output()
		if err == nil {
			s.makiVer = strings.TrimSpace(string(out))
		}
	})
	return s.makiVer
}

// statusWriter captures the response status and keeps Flush and
// ResponseController working through Unwrap (SSE needs both).
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func parseQueryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func (s *Server) row(id string) (model.Session, bool) { return s.cfg.Registry.Find(id) }
