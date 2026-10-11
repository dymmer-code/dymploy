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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
)

// Timeouts of the public listener. A client that is slow to send its request must not be able to hold
// a connection (and a goroutine) for ever. Connections taken over for the live channel are not subject
// to them once upgraded.
var (
	readTimeout  = 30 * time.Second
	writeTimeout = 60 * time.Second
	idleTimeout  = 2 * time.Minute
)

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    64 << 10,
	}
}

// Mode is what the daemon is willing to do.
type Mode int32

// The modes. The daemon only moves forward: Running → Draining → Stopping.
const (
	// ModeRunning: business as usual.
	ModeRunning Mode = iota
	// ModeDraining: the work in hand is being finished; new jobs are refused (webhooks get 503) but
	// everything else, notably the API that shows the progress, keeps answering.
	ModeDraining
	// ModeStopping: running jobs are being aborted; every request on the public listener is refused
	// with 503 until the daemon is clean and goes away. The control socket keeps answering.
	ModeStopping
)

func (m Mode) String() string {
	switch m {
	case ModeDraining:
		return "draining"
	case ModeStopping:
		return "stopping"
	}
	return "running"
}

// Daemon is one running instance.
type Daemon struct {
	cfg       *config.Config
	version   string
	startedAt time.Time
	handlers  []route
	reload    func() (string, error)
	jobs      control.Jobs
	stop      func(graceful bool, reason string) (string, error)
	stats     func() (queued, running int)

	mode    atomic.Int32
	closing chan struct{}
	once    sync.Once
}

// SetMode moves the daemon forward; going back is ignored. Entering ModeStopping ends the live
// channels ("going away").
func (d *Daemon) SetMode(m Mode) {
	for {
		cur := d.mode.Load()
		if Mode(cur) >= m || d.mode.CompareAndSwap(cur, int32(m)) {
			break
		}
	}
	if m == ModeStopping {
		d.once.Do(func() { close(d.closing) })
	}
}

// Mode returns the current mode.
func (d *Daemon) Mode() Mode { return Mode(d.mode.Load()) }

// Closing is closed when the daemon starts to stop.
func (d *Daemon) Closing() <-chan struct{} { return d.closing }

// WithStats says how to count the work, for the status.
func (d *Daemon) WithStats(f func() (queued, running int)) *Daemon {
	d.stats = f
	return d
}

// OnStop sets what `dymploy stop` does. Call it before Run.
func (d *Daemon) OnStop(f func(graceful bool, reason string) (string, error)) *Daemon {
	d.stop = f
	return d
}

// gate refuses requests according to the mode.
func (d *Daemon) gate(next http.Handler) http.Handler {
	refuse := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch d.Mode() {
		case ModeStopping:
			refuse(w, "dymploy is stopping")
			return
		case ModeDraining:
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/hooks/") {
				refuse(w, "dymploy is shutting down and not accepting new jobs")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type route struct {
	pattern string
	handler http.Handler
}

// Handle mounts an extra handler (for example the webhook receiver) on the HTTP listener. Call it
// before Run.
func (d *Daemon) Handle(pattern string, h http.Handler) *Daemon {
	d.handlers = append(d.handlers, route{pattern, h})
	return d
}

// WithJobs makes the jobs available on the control socket. Call it before Run.
func (d *Daemon) WithJobs(j control.Jobs) *Daemon {
	d.jobs = j
	return d
}

// OnReload sets what `dymploy reload` does. Call it before Run.
func (d *Daemon) OnReload(f func() (string, error)) *Daemon {
	d.reload = f
	return d
}

// New prepares a daemon; nothing starts until Run.
func New(cfg *config.Config, version string) *Daemon {
	return &Daemon{cfg: cfg, version: version, closing: make(chan struct{})}
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
	for _, r := range d.handlers {
		mux.Handle(r.pattern, r.handler)
	}
	srv := newServer(d.gate(mux))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.Serve(l) }()

	ctlReady := make(chan struct{})
	ctlErr := make(chan error, 1)
	go func() {
		ctlErr <- control.Serve(ctx, d.cfg.ControlSocket, control.Handlers{Status: d.status, Reload: d.reload, Jobs: d.jobs, Stop: d.stop, Closing: d.closing}, func() { close(ctlReady) })
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
	st := control.Status{Version: d.version, PID: os.Getpid(), StartedAt: d.startedAt, State: d.Mode().String()}
	if d.stats != nil {
		st.Queued, st.Running = d.stats()
	}
	return st
}
