// Command gateway serves the maki web gateway: HTTP/SSE API plus static UI.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"maki-gateway/internal/acpengine"
	"maki-gateway/internal/attach"
	"maki-gateway/internal/httpapi"
	"maki-gateway/internal/model"
	"maki-gateway/internal/registry"
	"maki-gateway/internal/store"
)

const (
	defaultAddr    = "127.0.0.1:8765"
	shutdownBudget = 3 * time.Second
)

func main() {
	makiPath := flag.String("maki", "maki", "path to the maki binary")
	addr := flag.String("addr", defaultAddr, "listen address")
	state := flag.String("state", defaultStateDir(), "maki state directory")
	webDir := flag.String("web", "web", "directory holding the static UI")
	sockDir := flag.String("sockdir", "", "attach socket directory (default <state>/gateway)")
	flag.Parse()
	if *sockDir == "" {
		*sockDir = filepath.Join(*state, "gateway")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := model.NewHub()
	acp := acpengine.New(*makiPath, hub)
	att := attach.New(*sockDir, hub)
	engines := []model.Engine{acp, att}
	st, err := store.New(*state)
	if err != nil {
		slog.Warn("store engine disabled", "err", err)
	} else {
		engines = append(engines, st)
	}

	reg := registry.New(hub, engines...)
	reg.Start(ctx)

	srv, err := httpapi.New(httpapi.Config{
		Hub:       hub,
		Registry:  reg,
		MakiPath:  *makiPath,
		Addr:      *addr,
		LookupEnv: os.Getenv,
		Static:    httpapi.StaticHandler(*webDir),
		Engines:   engines,
		Ctx:       ctx,
	})
	if err != nil {
		slog.Error("refusing to start", "err", err)
		os.Exit(1)
	}
	srv.MakiVersion()

	httpSrv := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			stop()
		}
	}()
	slog.Info("gateway listening", "addr", *addr, "state", *state, "sockdir", *sockDir)

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Warn("graceful shutdown failed, forcing close", "err", err)
	}
	httpSrv.Close()
}

func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "maki")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "maki")
	}
	return filepath.Join(".local", "state", "maki")
}
