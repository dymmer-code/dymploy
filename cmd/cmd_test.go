package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
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

// --- webhooks end to end ---

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

const hookSecret = "webhook-secret-for-tests"

func hookConfig(t *testing.T, dir, listen string, extra string) string {
	t.Helper()
	secretPath := filepath.Join(dir, "github.secret")
	if err := os.WriteFile(secretPath, []byte(hookSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "listen: " + listen + "\ncontrol_socket: " + filepath.Join(dir, "c.sock") + "\ndata_dir: " + filepath.Join(dir, "data") + `
webhooks:
  github:
    secret_file: ` + secretPath + `
policy:
  - provider: github
    owner: acme
    repositories: [shop]
    branches: [main]
` + extra
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func startWith(t *testing.T, cfgPath, sock string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := run(t, ctx, "serve", "--config", cfgPath); done <- err }()
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
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve must stop cleanly: %v", err)
		}
	}
}

func signedPush(t *testing.T, addr, delivery, repo, ref, secret string) *http.Response {
	t.Helper()
	body := `{"ref":"` + ref + `","after":"2222222222222222222222222222222222222222","repository":{"name":"` + repo +
		`","clone_url":"https://github.com/acme/` + repo + `.git","owner":{"login":"acme"}},"installation":{"id":1}}`
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/hooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestWebhooksEndToEnd(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	stop := startWith(t, hookConfig(t, dir, addr, ""), filepath.Join(dir, "c.sock"))
	defer stop()

	if r := signedPush(t, addr, "d-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("a signed push to an allowed repository must be accepted, got %d", r.StatusCode)
	}
	if r := signedPush(t, addr, "d-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 200 {
		t.Fatalf("the same delivery again is a duplicate (200), got %d", r.StatusCode)
	}
	if r := signedPush(t, addr, "d-2", "shop", "refs/heads/main", "not-the-secret"); r.StatusCode != 401 {
		t.Fatalf("a bad signature must be 401, got %d", r.StatusCode)
	}
	if r := signedPush(t, addr, "d-3", "secret-project", "refs/heads/main", hookSecret); r.StatusCode != 403 {
		t.Fatalf("a repository outside the policy must be 403, got %d", r.StatusCode)
	}
	if r := signedPush(t, addr, "d-4", "shop", "refs/heads/feature/x", hookSecret); r.StatusCode != 403 {
		t.Fatalf("a branch outside the policy must be 403, got %d", r.StatusCode)
	}
}

func TestServeRefusesAWorldReadableSecret(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	if err := os.Chmod(filepath.Join(dir, "github.secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // never wait forever if serve wrongly starts
	defer cancel()
	_, _, err := run(t, ctx, "serve", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("got %v", err)
	}
}

func TestServeRefusesAnUnsupportedProvider(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	b, _ := os.ReadFile(cfg)
	bad := strings.Replace(string(b), "webhooks:\n  github:", "webhooks:\n  sourcehut:", 1)
	bad = strings.Replace(bad, "provider: github", "provider: sourcehut", 1)
	if err := os.WriteFile(cfg, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := run(t, ctx, "serve", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "sourcehut") {
		t.Fatalf("an unknown provider must be named in the error, got %v", err)
	}
}
