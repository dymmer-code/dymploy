package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/dymmer-code/dymploy/internal/checkout"
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
		if err != nil || !strings.Contains(out, "OK") || !strings.Contains(out, "deploy: production") || !strings.Contains(out, "rollback: none") {
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
