package deploy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/deploy/deploytest"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

const body = "archive-bytes"

type fx struct {
	t    *testing.T
	fake *deploytest.Fake
	d    *deploy.Deployer
	file string
	rec  artifact.Record
	mu   sync.Mutex
	logs []string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "app.tar.gz")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	f := &fx{t: t, fake: &deploytest.Fake{}, file: file,
		rec: artifact.Record{Name: "app", File: "app.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}}
	f.d = &deploy.Deployer{Remote: f.fake, Targets: deploy.NewTargets()}
	return f
}

var ev = provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: "0123456789abcdef0123456789abcdef01234567", Branch: "main"}

func target() deploy.Target {
	return deploy.Target{Name: "production", Host: "h", User: "deploy", UploadDir: "up",
		Allow: webhook.Policy{Rules: []webhook.Rule{{Provider: "github", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}}}}}
}

func definition(t *testing.T, deployYAML string) *manifest.Deploy {
	t.Helper()
	m, err := manifest.Parse([]byte("version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{run: \"true\"}]\nartifact: {paths: [a]}\n" + deployYAML))
	if err != nil {
		t.Fatal(err)
	}
	return m.Deploy
}

func (f *fx) run(ctx context.Context, d *manifest.Deploy) (int, error) {
	rep := runner.NewLineReporter(func(s runner.Stream, line string) {
		f.mu.Lock()
		f.logs = append(f.logs, string(s)+": "+line)
		f.mu.Unlock()
	})
	defer rep.Close()
	return f.d.Run(ctx, deploy.Request{Target: target(), Deploy: d, Event: ev, JobID: "job/1", Record: f.rec, File: f.file, Log: rep})
}

func (f *fx) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

const simple = `deploy:
  target: production
  steps:
    - {name: install, run: "install it", env: {EXTRA: "1"}}
    - {name: start, run: "start it"}
`

func TestADeploymentUploadsVerifiesRunsAndCleansUp(t *testing.T) {
	f := newFx(t)
	code, err := f.run(context.Background(), definition(t, simple))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "install it|start it" {
		t.Fatalf("scripts: %s", got)
	}
	remote := "up/job-1-app.tar.gz"
	var env map[string]string
	for _, r := range f.fake.Runs {
		if r.Script == "install it" {
			env = r.Env
		}
	}
	for k, v := range map[string]string{
		"DYMPLOY_ARTIFACT": remote, "DYMPLOY_ARTIFACT_SHA256": f.rec.SHA256, "DYMPLOY_TARGET": "production", "DYMPLOY_COMMIT": ev.Commit,
		"DYMPLOY_SHORT_COMMIT": ev.Commit[:12], "DYMPLOY_OWNER": "acme", "DYMPLOY_REPO": "app", "DYMPLOY_BRANCH": "main", "DYMPLOY_JOB": "job/1", "EXTRA": "1",
	} {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["DYMPLOY_TAG"]; ok {
		t.Error("no tag was pushed")
	}
	if len(f.fake.Removed) != 1 || f.fake.Removed[0] != remote || f.fake.Closed != 1 {
		t.Fatalf("clean-up: removed=%v closed=%d", f.fake.Removed, f.fake.Closed)
	}
	for _, want := range []string{"deploying to production (deploy@h)", "upload verified", "[1/2] install", "[2/2] start", "deployed to production"} {
		if !strings.Contains(f.log(), want) {
			t.Errorf("log lacks %q:\n%s", want, f.log())
		}
	}
}

func TestStepsCannotOverrideTheVariablesDymployProvides(t *testing.T) {
	f := newFx(t)
	// the manifest validator refuses DYMPLOY_ names, so this checks the second line of defence
	d := definition(t, "deploy:\n  target: production\n  steps: [{run: \"x\"}]\n")
	d.Steps[0].Env = map[string]string{"DYMPLOY_TARGET": "evil"}
	if code, err := f.run(context.Background(), d); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	for _, r := range f.fake.Runs {
		if r.Script == "x" && r.Env["DYMPLOY_TARGET"] != "production" {
			t.Fatalf("overridden: %q", r.Env["DYMPLOY_TARGET"])
		}
	}
}

func TestAFailingStepStopsTheDeploymentAndNothingIsUndone(t *testing.T) {
	f := newFx(t)
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "install it" {
			return 3, "", false
		}
		return 0, "", false
	}
	d := definition(t, simple[:strings.Index(simple, "    - {name: start")]+"  rollback:\n    steps: [{run: \"undo\"}]\n")
	code, err := f.run(context.Background(), d)
	if err != nil || code != deploy.ExitStep {
		t.Fatalf("code=%d err=%v", code, err)
	}
	for _, s := range f.fake.Scripts() {
		if s == "undo" {
			t.Fatal("rollback must not be run")
		}
	}
	if !strings.Contains(f.log(), "nothing was undone") || !strings.Contains(f.log(), "does not run it") || !strings.Contains(f.log(), "exit code 3") {
		t.Fatalf("the log must say what is true after a failure:\n%s", f.log())
	}
	if len(f.fake.Removed) != 1 {
		t.Fatal("the upload is removed also after a failure")
	}
}

func TestTheHealthCheckIsRetriedUntilItPasses(t *testing.T) {
	f := newFx(t)
	calls := 0
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "probe" {
			calls++
			if calls < 2 {
				return 1, "", false
			}
		}
		return 0, "", false
	}
	d := definition(t, "deploy:\n  target: production\n  steps: [{run: \"go\"}]\n  check: {run: probe, attempts: 3, interval: 1s}\n")
	if code, err := f.run(context.Background(), d); err != nil || code != 0 || calls != 2 {
		t.Fatalf("code=%d err=%v probes=%d\n%s", code, err, calls, f.log())
	}
	if !strings.Contains(f.log(), "attempt 1/3") || !strings.Contains(f.log(), "passed (attempt 2/3)") {
		t.Fatalf("%s", f.log())
	}
}

func TestAHealthCheckThatNeverPassesFailsTheJob(t *testing.T) {
	f := newFx(t)
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "probe" {
			return 1, "", false
		}
		return 0, "", false
	}
	d := definition(t, "deploy:\n  target: production\n  steps: [{run: \"go\"}]\n  check: {run: probe, attempts: 2, interval: 1s}\n  after:\n    steps: [{run: notify}]\n")
	code, err := f.run(context.Background(), d)
	if err != nil || code != deploy.ExitCheck {
		t.Fatalf("code=%d err=%v", code, err)
	}
	for _, s := range f.fake.Scripts() {
		if s == "notify" {
			t.Fatal("`after` runs only when the deployment is healthy")
		}
	}
}

func TestAFailingAfterStepFailsTheJobButIsNotADeployFailure(t *testing.T) {
	f := newFx(t)
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "notify" {
			return 1, "", false
		}
		return 0, "", false
	}
	d := definition(t, "deploy:\n  target: production\n  steps: [{run: \"go\"}]\n  after:\n    steps: [{name: tell, run: notify}]\n")
	code, err := f.run(context.Background(), d)
	if err != nil || code != deploy.ExitAfter || !strings.Contains(f.log(), "nothing is undone") {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
	f = newFx(t)
	if code, err := f.run(context.Background(), definition(t, "deploy:\n  target: production\n  steps: [{run: \"go\"}]\n  after:\n    steps: [{run: ok}]\n")); err != nil || code != 0 {
		t.Fatalf("a passing after: %d %v", code, err)
	}
}

func TestADamagedUploadRunsNothing(t *testing.T) {
	f := newFx(t)
	f.fake.Corrupt = true
	_, err := f.run(context.Background(), definition(t, simple))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
	if len(f.fake.Scripts()) != 0 {
		t.Fatalf("nothing may run on a damaged artifact: %v", f.fake.Scripts())
	}
}

func TestTargetsThatCannotBeReachedAreRunnerErrors(t *testing.T) {
	for name, set := range map[string]func(*deploytest.Fake){
		"dial":   func(f *deploytest.Fake) { f.DialErr = errors.New("connection refused") },
		"upload": func(f *deploytest.Fake) { f.UploadErr = errors.New("disk full") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			set(f.fake)
			code, err := f.run(context.Background(), definition(t, simple))
			if err == nil || code != 0 {
				t.Fatalf("code=%d err=%v", code, err)
			}
		})
	}
	f := newFx(t)
	f.file = filepath.Join(t.TempDir(), "missing")
	if _, err := f.run(context.Background(), definition(t, simple)); err == nil || !strings.Contains(err.Error(), "opening the artifact") {
		t.Fatalf("%v", err)
	}
}

func TestADeploymentThatTakesTooLongIsStopped(t *testing.T) {
	f := newFx(t)
	f.fake.Handle = func(s string) (int, string, bool) { return 0, "", s == "slow" }
	d := definition(t, "deploy:\n  target: production\n  timeout: 1s\n  steps: [{run: slow}]\n")
	code, err := f.run(context.Background(), d)
	if err != nil || code != deploy.ExitTimeout || !strings.Contains(f.log(), "time limit") {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
}

func TestCancellingStopsTheDeploymentWithTheContextError(t *testing.T) {
	f := newFx(t)
	f.fake.Handle = func(s string) (int, string, bool) { return 0, "", s == "slow" }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := f.run(ctx, definition(t, "deploy:\n  target: production\n  steps: [{run: slow}]\n"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestAuthorize(t *testing.T) {
	f := newFx(t)
	if _, err := f.d.Authorize("production", ev); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("%v", err)
	}
	f.d.Targets.Set(map[string]deploy.Target{"production": target()})
	if tg, err := f.d.Authorize("production", ev); err != nil || tg.Name != "production" {
		t.Fatalf("%v", err)
	}
	other := ev
	other.Repo = "secret-project"
	if _, err := f.d.Authorize("production", other); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("%v", err)
	}
	tag := ev
	tag.Branch, tag.Tag = "", "v1"
	if _, err := f.d.Authorize("production", tag); err == nil || !strings.Contains(err.Error(), "tag v1") {
		t.Fatalf("tags were not allowed: %v", err)
	}
	noRef := ev
	noRef.Branch = ""
	noRef.Ref = "refs/heads/odd"
	if _, err := f.d.Authorize("production", noRef); err == nil || !strings.Contains(err.Error(), "refs/heads/odd") {
		t.Fatalf("%v", err)
	}
}
