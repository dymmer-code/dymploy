package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
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
