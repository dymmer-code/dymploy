package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd("v1.2.3")
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

func tmpDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func writeConfig(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	body := "listen: 127.0.0.1:0\ncontrol_socket: " + filepath.Join(dir, "c.sock") + "\ndata_dir: " + filepath.Join(dir, "data") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVersion(t *testing.T) {
	out, _, err := run(t, context.Background(), "version")
	if err != nil || strings.TrimSpace(out) != "dymploy v1.2.3" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// startDaemon runs `dymploy serve` in the background and waits for its control socket.
func startDaemon(t *testing.T) (cfgPath string, stop func()) {
	t.Helper()
	dir := tmpDir(t)
	cfgPath = writeConfig(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := run(t, ctx, "serve", "--config", cfgPath); done <- err }()
	sock := filepath.Join(dir, "c.sock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve stopped early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cfgPath, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve must stop cleanly on cancellation: %v", err)
		}
	}
}

func TestStatusAgainstARunningDaemon(t *testing.T) {
	cfg, stop := startDaemon(t)
	defer stop()
	out, _, err := run(t, context.Background(), "status", "--config", cfg)
	if err != nil || !strings.Contains(out, "dymploy v1.2.3") || !strings.Contains(out, "pid") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	jout, _, err := run(t, context.Background(), "status", "--config", cfg, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if json.Unmarshal([]byte(jout), &st) != nil || st["version"] != "v1.2.3" {
		t.Fatalf("--json must print the status as JSON, got %q", jout)
	}
}

func TestStatusWithExplicitSocket(t *testing.T) {
	cfg, stop := startDaemon(t)
	defer stop()
	sock := filepath.Join(filepath.Dir(cfg), "c.sock")
	out, _, err := run(t, context.Background(), "status", "--socket", sock)
	if err != nil || !strings.Contains(out, "v1.2.3") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestStatusWithoutDaemon(t *testing.T) {
	cfg := writeConfig(t, tmpDir(t))
	_, _, err := run(t, context.Background(), "status", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "dymploy serve") {
		t.Fatalf("a missing daemon must give an actionable error, got %v", err)
	}
}

func TestServeRejectsABadConfiguration(t *testing.T) {
	p := filepath.Join(tmpDir(t), "bad.yaml")
	if err := os.WriteFile(p, []byte("listen: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, context.Background(), "serve", "--config", p)
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("got %v", err)
	}
}

func TestServeNeedsAConfigurationFile(t *testing.T) {
	_, _, err := run(t, context.Background(), "serve", "--config", filepath.Join(tmpDir(t), "missing.yaml"))
	if err == nil {
		t.Fatal("serve must not start without a readable configuration")
	}
}
