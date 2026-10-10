// Package daemon runs dymploy as a service: the HTTP listener for webhooks and the API, and the
// control socket for the CLI. Jobs, queue and providers plug in here as they are built.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
)

// Daemon is one running instance.
type Daemon struct {
	cfg       *config.Config
	version   string
	startedAt time.Time
}

// New prepares a daemon; nothing starts until Run.
func New(cfg *config.Config, version string) *Daemon {
	return &Daemon{cfg: cfg, version: version}
}

// Run serves until ctx is cancelled and then shuts everything down. ready, if not nil, receives the
// address of the HTTP listener once both listeners are up.
func (d *Daemon) Run(ctx context.Context, ready func(net.Addr)) error {
	if err := os.MkdirAll(d.cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("creating the data directory: %w", err)
	}
	if err := os.Chmod(d.cfg.DataDir, 0o750); err != nil {
		return err
	}
	// Bind the public listener first: if the port is taken we fail before creating any socket file.
	l, err := net.Listen("tcp", d.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", d.cfg.Listen, err)
	}
	d.startedAt = time.Now().UTC()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.Serve(l) }()

	ctlReady := make(chan struct{})
	ctlErr := make(chan error, 1)
	go func() {
		ctlErr <- control.Serve(ctx, d.cfg.ControlSocket, d.status, func() { close(ctlReady) })
	}()

	select {
	case <-ctlReady:
		if ready != nil {
			ready(l.Addr())
		}
	case err := <-ctlErr:
		srv.Close()
		return err
	}

	var first error
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		if !errors.Is(err, http.ErrServerClosed) {
			first = err
		}
		cancel()
	case err := <-ctlErr:
		first = err
		cancel()
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := srv.Shutdown(shutdownCtx); err != nil && first == nil {
		first = err
	}
	select {
	case err := <-ctlErr:
		if err != nil && first == nil {
			first = err
		}
	case <-time.After(10 * time.Second):
	}
	return first
}

func (d *Daemon) status() control.Status {
	return control.Status{Version: d.version, PID: os.Getpid(), StartedAt: d.startedAt}
}
