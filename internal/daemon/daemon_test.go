package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/job"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	d, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return &config.Config{Listen: "127.0.0.1:0", ControlSocket: filepath.Join(d, "c.sock"), DataDir: filepath.Join(d, "data")}
}

func run(t *testing.T, cfg *config.Config) (addr net.Addr, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() { done <- New(cfg, "v9.9.9").Run(ctx, func(a net.Addr) { ready <- a }) }()
	select {
	case a := <-ready:
		return a, func() error { cancel(); return <-done }
	case err := <-done:
		t.Fatalf("daemon stopped early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	return nil, nil
}

func TestHealthz(t *testing.T) {
	addr, stop := run(t, testConfig(t))
	defer stop()
	resp, err := http.Get("http://" + addr.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil || body["status"] != "ok" {
		t.Fatalf("healthz: %d %v", resp.StatusCode, body)
	}
}

func TestStatusOverTheControlSocket(t *testing.T) {
	cfg := testConfig(t)
	_, stop := run(t, cfg)
	defer stop()
	st, err := (&control.Client{Socket: cfg.ControlSocket}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "v9.9.9" || st.PID != os.Getpid() || time.Since(st.StartedAt) > time.Minute {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestDataDirIsCreatedPrivate(t *testing.T) {
	cfg := testConfig(t)
	_, stop := run(t, cfg)
	defer stop()
	fi, err := os.Stat(cfg.DataDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("data dir: %v %v", fi, err)
	}
}

func TestGracefulShutdown(t *testing.T) {
	cfg := testConfig(t)
	addr, stop := run(t, cfg)
	if err := stop(); err != nil {
		t.Fatalf("shutdown must be clean: %v", err)
	}
	if _, err := os.Stat(cfg.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("the control socket must be removed, stat err = %v", err)
	}
	if _, err := http.Get("http://" + addr.String() + "/healthz"); err == nil {
		t.Fatal("the HTTP listener must be closed after shutdown")
	}
}

func TestListenFailureIsReported(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := testConfig(t)
	cfg.Listen = l.Addr().String() // already taken
	err = New(cfg, "v").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("a port in use must be an error")
	}
	if _, serr := os.Stat(cfg.ControlSocket); !os.IsNotExist(serr) {
		t.Fatalf("a failed start must leave no control socket behind, stat err = %v", serr)
	}
}

func TestExtraHandlersAreServed(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	d := New(cfg, "v").Handle("/hooks/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	go func() { done <- d.Run(ctx, func(a net.Addr) { ready <- a }) }()
	addr := <-ready
	defer func() { cancel(); <-done }()
	resp, err := http.Post("http://"+addr.String()+"/hooks/anything", "text/plain", nil)
	if err != nil || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("%v %v", resp, err)
	}
	if r2, err := http.Get("http://" + addr.String() + "/healthz"); err != nil || r2.StatusCode != 200 {
		t.Fatalf("healthz must keep working: %v %v", r2, err)
	}
}

type noJobs struct{}

func (noJobs) List(context.Context, job.Filter) ([]job.Job, error) {
	return []job.Job{{ID: "x", Name: "n", State: job.Succeeded}}, nil
}
func (noJobs) Get(context.Context, job.ID) (job.Job, error)   { return job.Job{}, job.ErrNotFound }
func (noJobs) Logs(job.ID, int64, int) ([]job.Entry, error)   { return nil, nil }
func (noJobs) Cancel(context.Context, job.ID) error           { return nil }
func (noJobs) Retry(context.Context, job.ID) (job.Job, error) { return job.Job{}, nil }
func (noJobs) Subscribe() (<-chan control.Event, func())      { return make(chan control.Event), func() {} }

func TestJobsAndReloadAreOfferedOnTheControlSocket(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	reloaded := 0
	go func() {
		done <- New(cfg, "v").WithJobs(noJobs{}).OnReload(func() (string, error) { reloaded++; return "ok", nil }).Run(ctx, func(a net.Addr) { ready <- a })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon stopped early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	c := &control.Client{Socket: cfg.ControlSocket}
	if jobs, err := c.Jobs(context.Background(), job.Filter{}); err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", jobs, err)
	}
	if msg, err := c.Reload(context.Background()); err != nil || msg != "ok" || reloaded != 1 {
		t.Fatalf("%q %v %d", msg, err, reloaded)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestASlowClientCannotHoldAConnectionForEver(t *testing.T) {
	old := readTimeout
	readTimeout = 300 * time.Millisecond
	defer func() { readTimeout = old }()
	addr, stop := run(t, testConfig(t))
	defer stop()
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// a request that promises a body and never sends it
	conn.Write([]byte("POST /hooks/github HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\nabc"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	start := time.Now()
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the server kept waiting for a body that never came")
			}
			break // closed by the server
		}
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestOversizedHeadersAreRefused(t *testing.T) {
	addr, stop := run(t, testConfig(t))
	defer stop()
	req, _ := http.NewRequest("GET", "http://"+addr.String()+"/healthz", nil)
	req.Header.Set("X-Big", strings.Repeat("a", 200<<10))
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("got %d", resp.StatusCode)
		}
	}
}

// emitter is a Jobs whose events the test controls.
type emitter struct {
	noJobs
	ch chan control.Event
}

func (e emitter) Subscribe() (<-chan control.Event, func()) { return e.ch, func() {} }

func TestTheLiveChannelOutlivesTheServerTimeouts(t *testing.T) {
	oldR, oldW, oldI := readTimeout, writeTimeout, idleTimeout
	readTimeout, writeTimeout, idleTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	defer func() { readTimeout, writeTimeout, idleTimeout = oldR, oldW, oldI }()
	em := emitter{ch: make(chan control.Event, 4)}
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	d := New(cfg, "v").Handle("/v1/", (&control.API{Jobs: em}).Handler())
	go func() { done <- d.Run(ctx, func(a net.Addr) { ready <- a }) }()
	addr := <-ready
	defer func() { cancel(); <-done }()

	c, _, err := websocket.Dial(ctx, "ws://"+addr.String()+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, _, err := c.Read(ctx); err != nil { // the snapshot
		t.Fatal(err)
	}
	time.Sleep(time.Second) // several times the timeouts
	em.ch <- control.Event{Type: "queued", Job: job.Job{ID: "late", State: job.Queued}}
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()
	_, data, err := c.Read(rctx)
	if err != nil || !strings.Contains(string(data), "late") {
		t.Fatalf("the channel must survive the HTTP timeouts: %v %s", err, data)
	}
}
