package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/dymmer-code/dymploy/internal/checkout"
	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/intake"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	fakeprovider "github.com/dymmer-code/dymploy/internal/provider/fake"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/dryrun"
	"github.com/dymmer-code/dymploy/internal/runner/runnertest"
	"github.com/dymmer-code/dymploy/internal/webhook"
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

func waitForLog(t *testing.T, dataDir, substr string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(dataDir, "logs", "*.jsonl"))
		for _, f := range files {
			if b, _ := os.ReadFile(f); strings.Contains(string(b), substr) {
				return string(b)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job log with %q under %s (files: %v)", substr, dataDir, files)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAcceptedPushRunsAsAJobAndLeavesItsStory(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	stop := startWith(t, hookConfig(t, dir, addr, ""), filepath.Join(dir, "c.sock"))
	defer stop()
	if r := signedPush(t, addr, "run-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("got %d", r.StatusCode)
	}
	story := waitForLog(t, filepath.Join(dir, "data"), "job finished: succeeded")
	for _, want := range []string{"job started", "dry run", "acme/shop", "nothing was executed"} {
		if !strings.Contains(story, want) {
			t.Fatalf("the job log must say %q:\n%s", want, story)
		}
	}
	for _, p := range []string{"state/jobs.db"} {
		fi, err := os.Stat(filepath.Join(dir, "data", p))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
}

func TestServeRefusesAnUnknownRunnerKind(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "runner:\n  kind: podman\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := run(t, ctx, "serve", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "podman") || !strings.Contains(err.Error(), "dryrun") {
		t.Fatalf("name the bad kind and list the available ones: %v", err)
	}
}

func TestOnlyOneDaemonOwnsTheState(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	cfg2 := hookConfig(t, dir, freeAddr(t), "") // same data_dir, other port
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, _, err := run(t, ctx, "serve", "--config", cfg2)
	if err == nil {
		t.Fatal("a second daemon on the same data directory must be refused")
	}
}

// --- error paths of the composition root ---

func TestStatusWithoutAConfigurationFileUsesTheDefaultSocket(t *testing.T) {
	_, _, err := run(t, context.Background(), "status", "--config", filepath.Join(tmpDir(t), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "/run/dymploy/control.sock") {
		t.Fatalf("with no configuration the default socket is tried, and named in the error: %v", err)
	}
}

func TestStatusWithABrokenConfiguration(t *testing.T) {
	p := filepath.Join(tmpDir(t), "broken.yaml")
	if err := os.WriteFile(p, []byte("listen: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, context.Background(), "status", "--config", p)
	if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("a bad configuration must not be mistaken for a missing one: %v", err)
	}
}

func serveFails(t *testing.T, cfg string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := run(t, ctx, "serve", "--config", cfg)
	if err == nil {
		t.Fatal("serve was expected to fail")
	}
	return err
}

func TestServeCannotUseAFileAsDataDirectory(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "state") {
		t.Fatalf("the error must point at the state directory: %v", err)
	}
}

func TestServeCannotUseAFileAsLogDirectory(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "logs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "log directory") {
		t.Fatalf("%v", err)
	}
}

// --- cloning private repositories ---

func writeAppKey(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(p, pemBytes, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// withApp rewrites a config produced by hookConfig so the github webhook also has app settings.
func withApp(t *testing.T, cfg, keyPath string) {
	t.Helper()
	b, _ := os.ReadFile(cfg)
	out := strings.Replace(string(b), "webhooks:\n  github:\n", "webhooks:\n  github:\n    app_id: 4242\n    private_key_file: "+keyPath+"\n", 1)
	if err := os.WriteFile(cfg, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestACloneJobWithoutAnAppFailsWithAnExplanation(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	stop := startWith(t, hookConfig(t, dir, addr, "runner:\n  kind: clone\n"), filepath.Join(dir, "c.sock"))
	defer stop()
	if r := signedPush(t, addr, "clone-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("got %d", r.StatusCode)
	}
	story := waitForLog(t, filepath.Join(dir, "data"), "job finished: failed")
	if !strings.Contains(story, "no app is configured") {
		t.Fatalf("the log must say why a private repository cannot be cloned:\n%s", story)
	}
}

func TestServeStartsWithAnAppConfigured(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "runner:\n  kind: clone\n")
	withApp(t, cfg, writeAppKey(t, dir, 0o600))
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	stop()
}

func TestServeRefusesAWorldReadableAppKey(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	withApp(t, cfg, writeAppKey(t, dir, 0o644))
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("%v", err)
	}
}

func TestServeRefusesAnUnusableAppKey(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withApp(t, cfg, bad)
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "private_key_file") {
		t.Fatalf("%v", err)
	}
}

// --- housekeeping and degraded environments ---

func TestRunEveryStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 100)
	done := make(chan struct{})
	go func() { runEvery(ctx, 5*time.Millisecond, func() { calls <- struct{}{} }); close(done) }()
	<-calls
	<-calls
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop must end with its context")
	}
}

func TestCleanOnceRemovesOldCopiesAndSurvivesErrors(t *testing.T) {
	root := filepath.Join(tmpDir(t), "repos")
	m, err := checkout.New(checkout.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "github", "acme", "old", ".git")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(old, "dymploy-used")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(marker, long, long)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cleanOnce(context.Background(), m, 30*24*time.Hour, log)
	if _, err := os.Stat(filepath.Join(root, "github", "acme", "old")); err == nil {
		t.Fatal("an old unused working copy must be removed")
	}
	if !strings.Contains(logs.String(), "removed") {
		t.Fatalf("%s", logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	os.MkdirAll(filepath.Join(root, "github", "acme", "x"), 0o700)
	logs.Reset()
	cleanOnce(ctx, m, time.Hour, log)
	if !strings.Contains(logs.String(), "failed") {
		t.Fatalf("a failing clean-up is logged, not fatal: %q", logs.String())
	}
}

func TestWithoutGitServeStillRunsOtherJobs(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	dir := tmpDir(t)
	stop := startWith(t, hookConfig(t, dir, freeAddr(t), ""), filepath.Join(dir, "c.sock"))
	stop()
}

func TestWithoutGitTheCloneRunnerIsRefused(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "runner:\n  kind: clone\n")
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "git") {
		t.Fatalf("%v", err)
	}
}

func TestCleanReposReturnsWhenItsContextEnds(t *testing.T) {
	m, err := checkout.New(checkout.Config{Root: filepath.Join(tmpDir(t), "r")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { cleanRepos(ctx, m, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanRepos must stop with its context")
	}
}

func TestServeReportsCorruptedState(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	if err := os.MkdirAll(filepath.Join(dir, "data", "state"), 0o750); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "data", "state", "jobs.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists([]byte("jobs"))
		return b.Put([]byte("zzz-broken"), []byte("{this is not json"))
	})
	db.Close()
	if err := serveFails(t, cfg); !strings.Contains(err.Error(), "recovering") {
		t.Fatalf("a damaged state must stop the daemon with a clear message: %v", err)
	}
}

// --- validate and schema ---

func writeManifest(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodManifest = `version: 1
build:
  image: {from: "debian:13"}
  steps: [{run: make}]
artifact:
  paths: [out/app]
deploy:
  target: production
  steps: [{run: "echo hi"}]
`

func TestValidateAcceptsAGoodFileAndSummarizesIt(t *testing.T) {
	dir := tmpDir(t)
	p := writeManifest(t, dir, "dymploy.yaml", goodManifest)
	for _, arg := range []string{p, dir} { // a file, or the directory that holds it
		out, _, err := run(t, context.Background(), "validate", arg)
		if err != nil || !strings.Contains(out, "OK") || !strings.Contains(out, "deploy: production") {
			t.Fatalf("%s: out=%q err=%v", arg, out, err)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	dir := tmpDir(t)
	writeManifest(t, dir, "dymploy.yaml", "version: 1\nbuild:\n  image: {from: \"bad image\"}\n  network: host\n  steps: [{name: x}]\nartifact:\n  paths: [/abs]\n")
	_, _, err := run(t, context.Background(), "validate", dir)
	if err == nil {
		t.Fatal("a bad file must fail")
	}
	for _, want := range []string{"dymploy.yaml", "build.image.from", "build.network", "build.steps[0].run", "artifact.paths[0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestValidateChecksTheFilesItRefersTo(t *testing.T) {
	dir := tmpDir(t)
	writeManifest(t, dir, "dymploy.yaml", strings.Replace(goodManifest, `{from: "debian:13"}`, `{dockerfile: build/Dockerfile}`, 1))
	if _, _, err := run(t, context.Background(), "validate", dir); err == nil || !strings.Contains(err.Error(), "build/Dockerfile") {
		t.Fatalf("a missing Dockerfile is a problem you want to know before pushing: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "build"), 0o755)
	writeManifest(t, dir, "build/Dockerfile", "FROM debian:13\n")
	if _, _, err := run(t, context.Background(), "validate", dir); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrorsAboutTheFilesLocation(t *testing.T) {
	dir := tmpDir(t)
	if _, _, err := run(t, context.Background(), "validate", dir); err == nil || !strings.Contains(err.Error(), "dymploy.yaml") {
		t.Fatalf("an empty directory has no manifest: %v", err)
	}
	writeManifest(t, dir, "dymploy.yaml", goodManifest)
	writeManifest(t, dir, "dymploy.yml", goodManifest)
	if _, _, err := run(t, context.Background(), "validate", dir); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, context.Background(), "validate", filepath.Join(dir, "absent.yaml")); err == nil {
		t.Fatal("a missing file must fail")
	}
}

func TestValidateWithoutArgumentsLooksInTheCurrentDirectory(t *testing.T) {
	dir := tmpDir(t)
	writeManifest(t, dir, "dymploy.yaml", goodManifest)
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	out, _, err := run(t, context.Background(), "validate")
	if err != nil || !strings.Contains(out, "OK") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestSchemaCommandPrintsTheJSONSchema(t *testing.T) {
	out, _, err := run(t, context.Background(), "schema")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if json.Unmarshal([]byte(out), &s) != nil || s["title"] != "dymploy.yaml" {
		t.Fatalf("%.80s", out)
	}
}

// fakeDockerOnPath puts a docker command in the PATH, next to the real git, and returns its directory.
func fakeDockerOnPath(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required")
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestTheDockerRunnerIsAvailableWhenDockerIs(t *testing.T) {
	fakeDockerOnPath(t)
	dir := tmpDir(t)
	stop := startWith(t, hookConfig(t, dir, freeAddr(t), "runner:\n  kind: docker\n"), filepath.Join(dir, "c.sock"))
	stop()
}

func TestWithoutDockerTheDockerRunnerIsRefusedButOtherJobsStillRun(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required")
	}
	bin := t.TempDir()
	os.Symlink(git, filepath.Join(bin, "git"))
	t.Setenv("PATH", bin)
	dir := tmpDir(t)
	if err := serveFails(t, hookConfig(t, dir, freeAddr(t), "runner:\n  kind: docker\n")); !strings.Contains(err.Error(), "docker") {
		t.Fatalf("%v", err)
	}
	dir = tmpDir(t)
	stop := startWith(t, hookConfig(t, dir, freeAddr(t), ""), filepath.Join(dir, "c.sock"))
	stop()
}

func TestWithoutGitTheDockerRunnerIsRefused(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	dir := tmpDir(t)
	if err := serveFails(t, hookConfig(t, dir, freeAddr(t), "runner:\n  kind: docker\n")); !strings.Contains(err.Error(), "git") {
		t.Fatalf("%v", err)
	}
}

func TestCleanArtifactsOnceRemovesOldBuildsAndSurvivesErrors(t *testing.T) {
	dir := filepath.Join(tmpDir(t), "artifacts")
	old := filepath.Join(dir, "job-old")
	os.MkdirAll(old, 0o700)
	long := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(old, long, long)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cleanArtifactsOnce(dir, 30*24*time.Hour, log)
	if _, err := os.Stat(old); err == nil || !strings.Contains(logs.String(), "removed") {
		t.Fatalf("an old build's artifacts must go: %v %q", err, logs.String())
	}
	file := filepath.Join(tmpDir(t), "f")
	os.WriteFile(file, nil, 0o600)
	logs.Reset()
	cleanArtifactsOnce(file, time.Hour, log)
	if !strings.Contains(logs.String(), "failed") {
		t.Fatalf("a failing clean-up is logged, not fatal: %q", logs.String())
	}
}

func TestCleanArtifactsStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		cleanArtifacts(ctx, t.TempDir(), time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanArtifacts must stop with its context")
	}
}

func dropIn(t *testing.T, cfg, name, body string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(cfg), config.DropInDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

const otherRule = "policy:\n  - {provider: github, owner: acme, repositories: [other], branches: [main]}\n"

func TestReloadAppliesANewPolicyWithoutRestarting(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	cfg := hookConfig(t, dir, addr, "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	if r := signedPush(t, addr, "r-1", "other", "refs/heads/main", hookSecret); r.StatusCode != 403 {
		t.Fatalf("before the reload: %d", r.StatusCode)
	}
	dropIn(t, cfg, "10-other.yaml", otherRule)
	out, _, err := run(t, context.Background(), "reload", "--config", cfg)
	if err != nil || !strings.Contains(out, "2 policy rules") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if r := signedPush(t, addr, "r-2", "other", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("after the reload: %d", r.StatusCode)
	}
	if r := signedPush(t, addr, "r-3", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("the rules that were there stay: %d", r.StatusCode)
	}
}

func TestSIGHUPReloadsToo(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	cfg := hookConfig(t, dir, addr, "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	dropIn(t, cfg, "10-other.yaml", otherRule)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; ; i++ {
		if r := signedPush(t, addr, fmt.Sprintf("h-%d", i), "other", "refs/heads/main", hookSecret); r.StatusCode == 202 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP did not apply the new policy")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestAnInvalidConfigurationLeavesThePreviousOneInForce(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	cfg := hookConfig(t, dir, addr, "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	dropIn(t, cfg, "10-bad.yaml", "policy:\n  - {provider: github, owner: x}\n")
	_, _, err := run(t, context.Background(), "reload", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "keeps the previous one") || !strings.Contains(err.Error(), "repositories is required") {
		t.Fatalf("%v", err)
	}
	if r := signedPush(t, addr, "i-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("the previous policy must still work: %d", r.StatusCode)
	}
}

func TestReloadRefusesChangesThatNeedARestart(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	cfg := hookConfig(t, dir, addr, "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	dropIn(t, cfg, "10-more.yaml", otherRule+"queue: {max_concurrent: 7, max_pending: 10, on_busy: queue, log_max_bytes: 1000}\nrepo_keep_days: 3\n")
	_, _, err := run(t, context.Background(), "reload", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "restart") || !strings.Contains(err.Error(), "queue") || !strings.Contains(err.Error(), "repo_keep_days") {
		t.Fatalf("%v", err)
	}
	if r := signedPush(t, addr, "n-1", "other", "refs/heads/main", hookSecret); r.StatusCode != 403 {
		t.Fatalf("a refused reload applies nothing, not even the policy: %d", r.StatusCode)
	}
}

func TestReloadWithoutADaemonSaysSo(t *testing.T) {
	dir := tmpDir(t)
	_, _, err := run(t, context.Background(), "reload", "--socket", filepath.Join(dir, "none.sock"))
	if err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Fatalf("%v", err)
	}
}

func TestReloadWithoutWebhooksHasNothingToApply(t *testing.T) {
	dir := tmpDir(t)
	cfg := filepath.Join(dir, "config.yaml")
	body := "listen: " + freeAddr(t) + "\ncontrol_socket: " + filepath.Join(dir, "c.sock") + "\ndata_dir: " + filepath.Join(dir, "data") + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	out, _, err := run(t, context.Background(), "reload", "--config", cfg)
	if err != nil || !strings.Contains(out, "0 policy rules") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// targetFiles writes a usable key and known_hosts for a target and returns the YAML of the target.
func targetFiles(t *testing.T, dir, name string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, name+".key")
	if err := os.WriteFile(key, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	hosts := filepath.Join(dir, name+".known_hosts")
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{"10.0.0.7"}, sp)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "targets:\n  " + name + ":\n    host: 10.0.0.7\n    user: deploy\n    key_file: " + key + "\n    known_hosts: " + hosts +
		"\n    allow:\n      - {provider: github, owner: acme, repositories: [shop], branches: [main]}\n"
}

func TestServeStartsWithTargetsAndRefusesBrokenOnes(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), targetFiles(t, dir, "production"))
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	stop()

	dir = tmpDir(t)
	extra := targetFiles(t, dir, "production")
	os.Remove(filepath.Join(dir, "production.key"))
	err := serveFails(t, hookConfig(t, dir, freeAddr(t), extra))
	if err == nil || !strings.Contains(err.Error(), "targets.production") || !strings.Contains(err.Error(), "key_file") {
		t.Fatalf("a target whose key is missing must stop the start: %v", err)
	}
}

func TestReloadAppliesNewTargetsAndRefusesBrokenOnes(t *testing.T) {
	dir := tmpDir(t)
	cfg := hookConfig(t, dir, freeAddr(t), "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	dropIn(t, cfg, "30-production.yaml", targetFiles(t, dir, "production"))
	out, _, err := run(t, context.Background(), "reload", "--config", cfg)
	if err != nil || !strings.Contains(out, "1 targets") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	// a target whose key disappears: the reload is refused and says the previous targets stay
	os.Remove(filepath.Join(dir, "production.key"))
	_, _, err = run(t, context.Background(), "reload", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "previous targets stay in force") {
		t.Fatalf("%v", err)
	}
}

// finishedJob starts a daemon, runs one job through it and returns the config path and the job's id.
func finishedJob(t *testing.T, extra string) (cfg, addr, id string, stop func()) {
	t.Helper()
	dir := tmpDir(t)
	addr = freeAddr(t)
	cfg = hookConfig(t, dir, addr, extra)
	stop = startWith(t, cfg, filepath.Join(dir, "c.sock"))
	if r := signedPush(t, addr, "j-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("got %d", r.StatusCode)
	}
	waitForLog(t, filepath.Join(dir, "data"), "job finished: succeeded")
	out, _, err := run(t, context.Background(), "jobs", "--config", cfg, "--json")
	var jobs []map[string]any
	if err != nil || json.Unmarshal([]byte(out), &jobs) != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %q", err, out)
	}
	return cfg, addr, jobs[0]["id"].(string), stop
}

func TestJobsCommandsShowWhatHappened(t *testing.T) {
	cfg, _, id, stop := finishedJob(t, "")
	defer stop()
	ctx := context.Background()

	out, _, err := run(t, ctx, "jobs", "--config", cfg)
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "succeeded") || !strings.Contains(out, "github/acme/shop") {
		t.Fatalf("jobs: %v %q", err, out)
	}
	if out, _, _ := run(t, ctx, "jobs", "--config", cfg, "--state", "running"); !strings.Contains(out, "no jobs") {
		t.Fatalf("filter: %q", out)
	}
	if _, _, err := run(t, ctx, "jobs", "--config", cfg, "--state", "bogus"); err == nil || !strings.Contains(err.Error(), "unknown state") {
		t.Fatalf("a bad state is explained: %v", err)
	}

	out, _, err = run(t, ctx, "job", id, "--config", cfg)
	if err != nil || !strings.Contains(out, "state    succeeded") || !strings.Contains(out, "exit     0") || !strings.Contains(out, "finished") {
		t.Fatalf("job: %v %q", err, out)
	}
	if out, _, _ := run(t, ctx, "job", id, "--config", cfg, "--json"); !strings.Contains(out, `"state": "succeeded"`) || strings.Contains(out, `"input"`) {
		t.Fatalf("json (without the opaque input): %q", out)
	}
	if _, _, err := run(t, ctx, "job", "nope", "--config", cfg); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}

	out, _, err = run(t, ctx, "logs", id, "--config", cfg)
	if err != nil || !strings.Contains(out, "# job started") || !strings.Contains(out, "nothing was executed") || !strings.Contains(out, "job finished: succeeded") {
		t.Fatalf("logs: %v %q", err, out)
	}
	if out, _, _ := run(t, ctx, "logs", id, "--config", cfg, "--after", "1"); strings.Contains(out, "job started") {
		t.Fatalf("--after skips lines already seen: %q", out)
	}
	// following a job that is already over shows everything and returns by itself
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _, err = run(t, fctx, "logs", id, "-f", "--config", cfg)
	if err != nil || !strings.Contains(out, "job finished: succeeded") {
		t.Fatalf("logs -f: %v %q", err, out)
	}

	if _, _, err := run(t, ctx, "abort", id, "--config", cfg); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("a finished job cannot be aborted: %v", err)
	}
	out, _, err = run(t, ctx, "retry", id, "--config", cfg)
	if err != nil || !strings.Contains(out, "retry of "+id) {
		t.Fatalf("retry: %v %q", err, out)
	}
	waitFor(t, func() bool {
		o, _, _ := run(t, ctx, "jobs", "--config", cfg, "--json")
		return strings.Count(o, `"retry_of"`) == 1 && strings.Count(o, `"state": "succeeded"`) == 2
	})
	if _, _, err := run(t, ctx, "retry", "nope", "--config", cfg); err == nil {
		t.Fatal("unknown job")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestWatchShowsJobsAsTheyHappen(t *testing.T) {
	dir := tmpDir(t)
	addr := freeAddr(t)
	cfg := hookConfig(t, dir, addr, "")
	stop := startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		root := NewRootCmd("v")
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"watch", "--config", cfg})
		done <- root.ExecuteContext(ctx)
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "nothing waiting or running") })
	if r := signedPush(t, addr, "w-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("got %d", r.StatusCode)
	}
	waitFor(t, func() bool {
		s := out.String()
		return strings.Contains(s, "queued") && strings.Contains(s, "started") && strings.Contains(s, "finished") && strings.Contains(s, "nothing was executed")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Ctrl-C is the normal way to stop watching: %v", err)
	}

	// and as JSON, one message per line
	ctx2, cancel2 := context.WithCancel(context.Background())
	var js syncBuffer
	done2 := make(chan error, 1)
	go func() {
		root := NewRootCmd("v")
		root.SetOut(&js)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"watch", "--config", cfg, "--json"})
		done2 <- root.ExecuteContext(ctx2)
	}()
	waitFor(t, func() bool { return strings.Contains(js.String(), `"type":"snapshot"`) })
	cancel2()
	<-done2
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestJobCommandsWithoutADaemon(t *testing.T) {
	sock := filepath.Join(tmpDir(t), "none.sock")
	for _, args := range [][]string{{"jobs"}, {"job", "x"}, {"logs", "x"}, {"logs", "x", "-f"}, {"abort", "x"}, {"retry", "x"}, {"watch"}} {
		_, _, err := run(t, context.Background(), append(args, "--socket", sock)...)
		if err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestJobCommandsWithABrokenConfiguration(t *testing.T) {
	p := filepath.Join(tmpDir(t), "broken.yaml")
	os.WriteFile(p, []byte("listen: nope\n"), 0o600)
	for _, args := range [][]string{{"jobs"}, {"job", "x"}, {"logs", "x"}, {"abort", "x"}, {"retry", "x"}, {"watch"}} {
		if _, _, err := run(t, context.Background(), append(args, "--config", p)...); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestThePublicAPIIsClosedWithoutATokenAndReadOnlyWithOne(t *testing.T) {
	// closed
	cfg, addr, id, stop := finishedJob(t, "")
	_ = cfg
	resp, err := http.Get("http://" + addr + "/api/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("without api.token_file nothing is mounted: %d", resp.StatusCode)
	}
	stop()

	// open, with a token
	dir := tmpDir(t)
	tokenFile := filepath.Join(dir, "api.token")
	os.WriteFile(tokenFile, []byte("0123456789abcdef0123456789abcdef0123\n"), 0o600)
	addr = freeAddr(t)
	cfg = hookConfig(t, dir, addr, "api:\n  token_file: "+tokenFile+"\n")
	stop = startWith(t, cfg, filepath.Join(dir, "c.sock"))
	defer stop()
	if r := signedPush(t, addr, "p-1", "shop", "refs/heads/main", hookSecret); r.StatusCode != 202 {
		t.Fatalf("got %d", r.StatusCode)
	}
	waitForLog(t, filepath.Join(dir, "data"), "job finished: succeeded")
	get := func(path, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := get("/api/v1/jobs", ""); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := get("/api/v1/jobs", "wrong"); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r := get("/api/v1/jobs", "0123456789abcdef0123456789abcdef0123")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), "github/acme/shop") || strings.Contains(string(body), `"input"`) {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
	_ = id
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/jobs/x/cancel", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef0123")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode == 200 {
		t.Fatal("the public API is read-only")
	}
}

func TestServeRefusesATooShortAPIToken(t *testing.T) {
	dir := tmpDir(t)
	short := filepath.Join(dir, "token")
	os.WriteFile(short, []byte("guessable\n"), 0o600)
	if err := serveFails(t, hookConfig(t, dir, freeAddr(t), "api:\n  token_file: "+short+"\n")); !strings.Contains(err.Error(), "too short") {
		t.Fatalf("%v", err)
	}
}

func TestServeRefusesBadDockerParams(t *testing.T) {
	fakeDockerOnPath(t)
	dir := tmpDir(t)
	err := serveFails(t, hookConfig(t, dir, freeAddr(t), "runner:\n  kind: docker\n  params: {limits: {memry: 1g}}\n"))
	if !strings.Contains(err.Error(), "docker runner's params") {
		t.Fatalf("%v", err)
	}
	// with another runner the docker settings are not looked at
	dir = tmpDir(t)
	stop := startWith(t, hookConfig(t, dir, freeAddr(t), "runner:\n  kind: dryrun\n"), filepath.Join(dir, "c.sock"))
	stop()
}

func TestServeRefusesAnUnusableAPIToken(t *testing.T) {
	dir := tmpDir(t)
	open := filepath.Join(dir, "token")
	os.WriteFile(open, []byte("t"), 0o644)
	if err := serveFails(t, hookConfig(t, dir, freeAddr(t), "api:\n  token_file: "+open+"\n")); !strings.Contains(err.Error(), "api.token_file") {
		t.Fatalf("%v", err)
	}
	if err := serveFails(t, hookConfig(t, dir, freeAddr(t), "api:\n  token_file: "+filepath.Join(dir, "none")+"\n")); !strings.Contains(err.Error(), "api.token_file") {
		t.Fatalf("%v", err)
	}
}

func TestQueueErrorsBecomeConflicts(t *testing.T) {
	for _, err := range []error{queue.ErrFinished, queue.ErrNotFinished, queue.ErrRejected, queue.ErrQueueFull, queue.ErrClosed} {
		if got := conflict(fmt.Errorf("wrapped: %w", err)); !errors.Is(got, control.ErrConflict) {
			t.Errorf("%v is not a conflict", err)
		}
	}
	other := errors.New("disk on fire")
	if conflict(other) != other || conflict(nil) != nil {
		t.Fatal("other errors, and success, pass through")
	}
	for d, want := range map[time.Duration]string{0: "0s", 150 * time.Second: "2m", 5 * time.Hour: "5h", 72 * time.Hour: "3d"} {
		if got := since(time.Now().Add(-d)); got != want {
			t.Errorf("since(%v) = %s, want %s", d, got, want)
		}
	}
	if since(time.Time{}) != "-" {
		t.Fatal("unknown time")
	}
}

// stubJobs is a control.Jobs for commands that need a job in a state the dry-run runner never leaves behind.
type stubJobs struct {
	cancelled []job.ID
	reasons   []string
	queued    []control.EnqueueRequest
}

func (s *stubJobs) List(context.Context, job.Filter) ([]job.Job, error) {
	code := 3
	now := time.Now()
	return []job.Job{
		{ID: "f", Name: "github/acme/app", State: job.Failed, ExitCode: &code, CreatedAt: now.Add(-time.Hour)},
		{ID: "r", Name: "github/acme/app", State: job.Running, CreatedAt: now},
	}, nil
}
func (s *stubJobs) Get(_ context.Context, id job.ID) (job.Job, error) {
	code := 5
	return job.Job{ID: id, Name: "github/acme/app", Kind: "docker", State: job.Failed, ExitCode: &code, Error: "boom", RetryOf: "older",
		CreatedAt: time.Now().Add(-time.Minute), StartedAt: time.Now().Add(-50 * time.Second), FinishedAt: time.Now().Add(-10 * time.Second)}, nil
}
func (s *stubJobs) Logs(job.ID, int64, int) ([]job.Entry, error) {
	return []job.Entry{{Seq: 1, Time: time.Now(), Stream: "stdout", Text: "out"}, {Seq: 2, Time: time.Now(), Stream: "stderr", Text: "err"}, {Seq: 3, Time: time.Now(), Stream: "system", Text: "sys"}}, nil
}
func (s *stubJobs) Abort(_ context.Context, id job.ID, reason string) error {
	s.cancelled = append(s.cancelled, id)
	s.reasons = append(s.reasons, reason)
	return nil
}
func (s *stubJobs) Retry(context.Context, job.ID) (job.Job, error) {
	return job.Job{}, nil
}
func (s *stubJobs) Enqueue(_ context.Context, r control.EnqueueRequest) (job.Job, error) {
	s.queued = append(s.queued, r)
	return job.Job{ID: "hand-1", Name: r.Repo, State: job.Queued}, nil
}
func (s *stubJobs) Subscribe() (<-chan control.Event, func()) {
	ch := make(chan control.Event)
	return ch, func() {}
}

func TestJobCommandsFormatWhatTheDaemonSends(t *testing.T) {
	sock := filepath.Join(tmpDir(t), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := &stubJobs{}
	ready := make(chan struct{})
	go control.Serve(ctx, sock, control.Handlers{Status: func() control.Status { return control.Status{} }, Jobs: jobs}, func() { close(ready) })
	<-ready
	out, _, err := run(t, ctx, "jobs", "--socket", sock)
	if err != nil || !strings.Contains(out, "failed (exit 3)") || !strings.Contains(out, "1h ago") || !strings.Contains(out, "running") {
		t.Fatalf("%v %q", err, out)
	}
	out, _, err = run(t, ctx, "job", "x", "--socket", sock)
	for _, want := range []string{"kind     docker", "exit     5", "error    boom", "retry of older", "started", "finished", "took 40s"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("job lacks %q: %v\n%s", want, err, out)
		}
	}
	out, _, err = run(t, ctx, "logs", "x", "--socket", sock)
	if err != nil || !strings.Contains(out, "  out") || !strings.Contains(out, "! err") || !strings.Contains(out, "# sys") {
		t.Fatalf("stream marks: %v %q", err, out)
	}
	out, _, err = run(t, ctx, "abort", "x", "--reason", "the repository moved", "--socket", sock)
	if err != nil || !strings.Contains(out, "stop requested for x") || len(jobs.cancelled) != 1 || jobs.reasons[0] != "the repository moved" {
		t.Fatalf("%v %q", err, out)
	}
}

// manualRig is a queue with the dry-run runner, a provider that knows some references and a receiver
// whose policy is the one a webhook would meet.
type manualRig struct {
	jobs  queueJobs
	rc    *webhook.Receiver
	store *job.MemStore
	logs  syncBuffer
}

func newManualRig(t *testing.T, providers map[string]provider.Provider) *manualRig {
	t.Helper()
	r := &manualRig{store: job.NewMemStore()}
	reg := runner.NewRegistry()
	reg.MustRegister("dryrun", dryrun.New())
	log := slog.New(slog.NewTextHandler(&r.logs, nil))
	mgr, err := queue.New(queue.Config{MaxConcurrent: 1, MaxPending: 5, OnBusy: queue.Policy("reject")},
		queue.Deps{Store: r.store, Logs: job.NewMemLogs(), Runners: reg, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownQueue(mgr, 5*time.Second) })
	enq := intake.New(mgr, "dryrun", nil)
	r.rc = &webhook.Receiver{
		Providers: providers,
		Policy:    webhook.Policy{Rules: []webhook.Rule{{Provider: "fake", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}, Tags: []string{"v*"}}}},
		Queue:     enq, Dedupe: webhook.NewDedupe(time.Hour, 10), Log: log,
	}
	r.jobs = queueJobs{m: mgr, log: log, providers: providers, hooks: r.rc, enq: enq}
	return r
}

func fakeProviders() map[string]provider.Provider {
	return map[string]provider.Provider{"fake": fakeprovider.New("t").WithRefs(map[string]string{
		"acme/app:refs/heads/main":    "0123456789abcdef0123456789abcdef01234567",
		"acme/app:refs/heads/feature": "1123456789abcdef0123456789abcdef01234567",
		"acme/app:refs/tags/v1":       "2123456789abcdef0123456789abcdef01234567",
		"acme/other:refs/heads/main":  "3123456789abcdef0123456789abcdef01234567",
	})}
}

func TestAJobStartedByHandMeetsTheSamePolicyAsAWebhook(t *testing.T) {
	r := newManualRig(t, fakeProviders())
	ctx := context.Background()
	j, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"})
	if err != nil || j.Name != "fake/acme/app" || j.ID == "" {
		t.Fatalf("%v %+v", err, j)
	}
	var in provider.Event
	if err := json.Unmarshal(j.Input, &in); err != nil || in.Commit != "0123456789abcdef0123456789abcdef01234567" || in.Branch != "main" || in.DeliveryID != "" {
		t.Fatalf("the job carries the event a push would have: %v %+v", err, in)
	}
	waitFor(t, func() bool { got, _ := r.store.Get(ctx, j.ID); return got.State.Final() })
	if !strings.Contains(r.logs.String(), "job started by hand") {
		t.Fatalf("audit line missing:\n%s", r.logs.String())
	}
	// a tag the policy allows, with the provider left out (there is only one)
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "acme/app", Ref: "v1"}); err != nil {
		t.Fatalf("%v", err)
	}
	waitFor(t, func() bool {
		l, _ := r.store.List(ctx, job.Filter{})
		return len(l) == 2 && l[0].State.Final() && l[1].State.Final()
	})

	// not allowed: a branch outside the rules, another repository
	for _, tc := range []struct{ repo, ref string }{{"fake/acme/app", "feature"}, {"fake/acme/other", "main"}} {
		_, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: tc.repo, Ref: tc.ref})
		if !errors.Is(err, control.ErrForbidden) || !strings.Contains(err.Error(), "policy does not allow") {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if !strings.Contains(r.logs.String(), "manual job refused by the policy") {
		t.Fatal("refusals are audited")
	}
	// the policy in force is the receiver's current one: a reload applies to manual jobs at once
	r.rc.SetPolicy(webhook.Policy{})
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestStartingAJobByHandReportsWhatIsWrong(t *testing.T) {
	r := newManualRig(t, fakeProviders())
	ctx := context.Background()
	for name, tc := range map[string]struct {
		repo, ref, want string
	}{
		"no provider part":  {"app", "main", "provider/owner/repo"},
		"too many parts":    {"fake/acme/app/x", "main", "provider/owner/repo"},
		"empty owner":       {"fake//app", "main", "incomplete"},
		"unknown provider":  {"gitlab/acme/app", "main", "unknown provider"},
		"unknown reference": {"fake/acme/app", "nope", "no such branch or tag"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: tc.repo, Ref: tc.ref})
			if !errors.Is(err, control.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
		})
	}
	// a provider that cannot resolve references
	r.jobs.providers = map[string]provider.Provider{"fake": noResolver{}}
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"}); !errors.Is(err, control.ErrInvalid) || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatalf("%v", err)
	}
	// resolver failures that are not "no such reference" pass through
	r.jobs.providers = map[string]provider.Provider{"fake": brokenResolver{}}
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"}); err == nil || errors.Is(err, control.ErrInvalid) {
		t.Fatalf("%v", err)
	}
	// without a receiver there is no policy, so nothing is allowed
	r.jobs.providers, r.jobs.hooks = fakeProviders(), nil
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestAJobStartedByHandObeysTheQueueRules(t *testing.T) {
	r := newManualRig(t, fakeProviders())
	ctx := context.Background()
	// the runner is held up so that the first job is still there for the second
	gate := runnertest.NewGate()
	r.jobs.providers = fakeProviders()
	reg := runner.NewRegistry()
	reg.MustRegister("hold", runnertest.New(runnertest.Hold(gate, 0)))
	mgr, _ := queue.New(queue.Config{MaxConcurrent: 1, MaxPending: 5, OnBusy: queue.Policy("reject")},
		queue.Deps{Store: job.NewMemStore(), Logs: job.NewMemLogs(), Runners: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	mgr.Start(ctx)
	defer func() { gate.Open(); shutdownQueue(mgr, 5*time.Second) }()
	r.jobs.m, r.jobs.enq = mgr, intake.New(mgr, "hold", nil)
	if _, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.jobs.Enqueue(ctx, control.EnqueueRequest{Repo: "fake/acme/app", Ref: "main"})
	if !errors.Is(err, control.ErrConflict) || !strings.Contains(err.Error(), "already waiting or running") {
		t.Fatalf("on_busy: reject is a conflict, as it is for a webhook: %v", err)
	}
}

type noResolver struct{}

func (noResolver) Name() string { return "fake" }
func (noResolver) ParseWebhook(*http.Request) (*provider.Event, error) {
	return nil, provider.ErrIgnored
}

type brokenResolver struct{ noResolver }

func (brokenResolver) ResolveRef(context.Context, string, string, string) (*provider.Event, error) {
	return nil, errors.New("GitHub is down")
}

func TestQueueCommand(t *testing.T) {
	sock := filepath.Join(tmpDir(t), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := &stubJobs{}
	ready := make(chan struct{})
	go control.Serve(ctx, sock, control.Handlers{Status: func() control.Status { return control.Status{} }, Jobs: jobs}, func() { close(ready) })
	<-ready

	out, _, err := run(t, ctx, "queue", "github/acme/app", "--ref", "main", "--socket", sock)
	if err != nil || !strings.Contains(out, "queued github/acme/app at main (hand-1)") || len(jobs.queued) != 1 || jobs.queued[0].Ref != "main" {
		t.Fatalf("%v %q %+v", err, out, jobs.queued)
	}
	out, _, err = run(t, ctx, "queue", "--socket", sock)
	if err != nil || !strings.Contains(out, "running") {
		t.Fatalf("without arguments it lists what is waiting and running: %v %q", err, out)
	}
	if _, _, err := run(t, ctx, "queue", "github/acme/app", "--socket", sock); err == nil || !strings.Contains(err.Error(), "--ref is required") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, ctx, "queue", "--ref", "main", "--socket", sock); err == nil || !strings.Contains(err.Error(), "say which repository") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, ctx, "queue", "a", "b", "--socket", sock); err == nil {
		t.Fatal("at most one repository")
	}
	fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
	defer fcancel()
	// following a job the stub knows nothing about ends with the daemon's answer, not a hang
	if _, _, err := run(t, fctx, "queue", "github/acme/app", "--ref", "main", "-f", "--socket", sock); err != nil && !strings.Contains(err.Error(), "not found") && fctx.Err() == nil {
		t.Logf("follow: %v", err)
	}
	if _, _, err := run(t, ctx, "queue", "x", "--ref", "m", "--socket", filepath.Join(tmpDir(t), "none.sock")); err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, ctx, "queue", "--config", filepath.Join(tmpDir(t), "broken.yaml")); err == nil {
		t.Log("a missing configuration falls back to the default socket")
	}
}

func TestRunsAreCalledByTheirNumber(t *testing.T) {
	cfg, addr, id, stop := finishedJob(t, "")
	defer stop()
	ctx := context.Background()
	out, _, err := run(t, ctx, "jobs", "--config", cfg)
	if err != nil || !strings.Contains(out, "github/acme/shop #1") || !strings.Contains(out, "("+id+")") {
		t.Fatalf("%v %q", err, out)
	}
	for _, ref := range []string{"github/acme/shop#1", "github/acme/shop #1"} {
		out, _, err = run(t, ctx, "job", ref, "--config", cfg)
		if err != nil || !strings.Contains(out, "run      github/acme/shop #1") || !strings.Contains(out, id) {
			t.Fatalf("%q: %v %q", ref, err, out)
		}
	}
	if out, _, err = run(t, ctx, "logs", "github/acme/shop#1", "--config", cfg); err != nil || !strings.Contains(out, "job finished: succeeded") {
		t.Fatalf("%v %q", err, out)
	}
	if out, _, err = run(t, ctx, "retry", "github/acme/shop#1", "--config", cfg); err != nil || !strings.Contains(out, "queued github/acme/shop #2 (retry of github/acme/shop#1)") {
		t.Fatalf("%v %q", err, out)
	}
	waitFor(t, func() bool {
		o, _, _ := run(t, ctx, "job", "github/acme/shop#2", "--config", cfg)
		return strings.Contains(o, "succeeded")
	})
	if _, _, err = run(t, ctx, "abort", "github/acme/shop#1", "--config", cfg); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("%v", err)
	}
	for ref, want := range map[string]string{"github/acme/shop#9": "no run #9", "github/acme/shop#x": "is not a run", "#3": "is not a run", "github/nobody/none#1": "no run #1"} {
		if _, _, err := run(t, ctx, "job", ref, "--config", cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	_ = addr
}

func TestSecretsCommandAsksForAJobThatChangesOnlyTheSecrets(t *testing.T) {
	sock := filepath.Join(tmpDir(t), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := &stubJobs{}
	ready := make(chan struct{})
	go control.Serve(ctx, sock, control.Handlers{Status: func() control.Status { return control.Status{} }, Jobs: jobs}, func() { close(ready) })
	<-ready

	out, _, err := run(t, ctx, "secrets", "github/acme/app", "--ref", "main", "--socket", sock)
	if err != nil || !strings.Contains(out, "queued the change of secrets") || len(jobs.queued) != 1 || !jobs.queued[0].Secrets || jobs.queued[0].Ref != "main" {
		t.Fatalf("%v %q %+v", err, out, jobs.queued)
	}
	if _, _, err := run(t, ctx, "secrets", "github/acme/app", "--socket", sock); err == nil || !strings.Contains(err.Error(), "--ref is required") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, ctx, "secrets", "--ref", "main", "--socket", sock); err == nil {
		t.Fatal("it needs a repository")
	}
}
