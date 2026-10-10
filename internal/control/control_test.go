package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Unix socket paths are limited (~104 bytes on macOS): use a short temp dir.
func sockPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "c.sock")
}

func start(t *testing.T, path string, st func() Status) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- Serve(ctx, path, Handlers{Status: st}, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("server stopped before being ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not become ready")
	}
	return func() error { cancel(); return <-done }
}

func fixed() Status {
	return Status{Version: "v1.2.3", PID: 4242, StartedAt: time.Unix(1_700_000_000, 0).UTC()}
}

func TestStatusRoundTrip(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	got, err := (&Client{Socket: p}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1.2.3" || got.PID != 4242 || !got.StartedAt.Equal(fixed().StartedAt) {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestSocketIsPrivate(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the control socket must be 0600, is %o", perm)
	}
}

func TestSocketIsRemovedOnShutdown(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	if err := stop(); err != nil {
		t.Fatalf("clean shutdown must not error: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the socket file must be gone after shutdown, stat err = %v", err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	p := sockPath(t)
	if err := os.WriteFile(p, nil, 0o600); err != nil { // leftover from a crash: a file nobody listens on
		t.Fatal(err)
	}
	stop := start(t, p, fixed)
	defer stop()
	if _, err := (&Client{Socket: p}).Status(context.Background()); err != nil {
		t.Fatalf("a stale socket must not block a new server: %v", err)
	}
}

func TestSecondServerIsRefused(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	err := Serve(context.Background(), p, Handlers{Status: fixed}, nil)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a second server on a live socket must be refused, got %v", err)
	}
	// and the first one must still be alive
	if _, err := (&Client{Socket: p}).Status(context.Background()); err != nil {
		t.Fatalf("the running server was disturbed: %v", err)
	}
}

func TestClientWithoutServer(t *testing.T) {
	p := sockPath(t)
	_, err := (&Client{Socket: p}).Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dymploy serve") {
		t.Fatalf("the error must tell the user to start the daemon, got %v", err)
	}
}

func TestOnlyStatusIsServed(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	c := &Client{Socket: p}
	if err := c.get(context.Background(), "/v1/nope", &Status{}); err == nil {
		t.Fatal("unknown paths must return an error")
	}
}

func startWith(t *testing.T, path string, h Handlers) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- Serve(ctx, path, h, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("server stopped before being ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not become ready")
	}
	return func() error { cancel(); return <-done }
}

func TestReloadReportsWhatTheDaemonSays(t *testing.T) {
	p := sockPath(t)
	var fail bool
	stop := startWith(t, p, Handlers{Status: fixed, Reload: func() (string, error) {
		if fail {
			return "", errors.New("policy[0]: provider is required")
		}
		return "policy: 2 rules", nil
	}})
	defer stop()
	c := &Client{Socket: p}
	if msg, err := c.Reload(context.Background()); err != nil || msg != "policy: 2 rules" {
		t.Fatalf("msg=%q err=%v", msg, err)
	}
	fail = true
	_, err := c.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "keeps the previous one") || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("a refused configuration must come back with its reason: %v", err)
	}
}

func TestReloadWhenTheDaemonCannot(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	if _, err := (&Client{Socket: p}).Reload(context.Background()); !errors.Is(err, ErrNoReload) {
		t.Fatalf("err=%v", err)
	}
}

func TestReloadWithoutADaemon(t *testing.T) {
	if _, err := (&Client{Socket: sockPath(t)}).Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Fatalf("err=%v", err)
	}
}
