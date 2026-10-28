package docker_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
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
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/docker"
	"github.com/dymmer-code/dymploy/internal/runner/docker/dockertest"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

type rig struct {
	t        *testing.T
	repo     string
	arts     string
	cache    string
	released int
	eng      *dockertest.Fake
	run      *docker.Runner
	mu       sync.Mutex
	lines    []string
}

func newRig(t *testing.T, manifest string) *rig {
	t.Helper()
	r := &rig{t: t, repo: t.TempDir(), arts: t.TempDir(), cache: t.TempDir(), eng: &dockertest.Fake{}}
	if manifest != "" {
		r.file("dymploy.yaml", manifest)
	}
	run, err := docker.New(docker.Config{
		Engine: r.eng, ArtifactsDir: r.arts, CacheDir: r.cache, User: "505:20", StopGrace: time.Second,
		Source: func(_ context.Context, _ runner.Task) (string, func(), provider.Event, error) {
			return r.repo, func() { r.released++ }, provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: commit, Branch: "main"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.run = run
	return r
}

func (r *rig) file(rel, body string) {
	p := filepath.Join(r.repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) do(ctx context.Context) (runner.Result, error) {
	rep := runner.NewLineReporter(func(s runner.Stream, line string) {
		r.mu.Lock()
		r.lines = append(r.lines, string(s)+": "+line)
		r.mu.Unlock()
	})
	defer rep.Close()
	return r.run.Run(ctx, runner.Task{JobID: "job-1", Name: "github/acme/app", Kind: docker.Kind, Log: rep})
}

func (r *rig) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func writeOut(path string) func(docker.ContainerSpec) {
	return func(s docker.ContainerSpec) {
		for _, m := range s.Mounts {
			if m.Container == "/work" {
				p := filepath.Join(m.Host, path)
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte("built"), 0o755)
			}
		}
	}
}

const basic = `version: 1
build:
  image: {from: "alpine:3"}
  steps:
    - {name: compile, run: "make"}
artifact: {name: app, paths: [out/app]}
`

func TestSuccessfulBuildPacksTheArtifactAndNarrates(t *testing.T) {
	r := newRig(t, basic)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Stdout: "compiling\n", Stderr: "warning\n", Effect: writeOut("out/app")}
	}
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
	tgz := filepath.Join(r.arts, "job-1", "app.tar.gz")
	f, err := os.Open(tgz)
	if err != nil {
		t.Fatalf("artifact missing: %v\n%s", err, r.log())
	}
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	h, err := tar.NewReader(gz).Next()
	if err != nil || h.Name != "app" {
		t.Fatalf("archive: %v %v", h, err)
	}
	log := r.log()
	for _, want := range []string{"stdout: compiling", "stderr: warning", "system: [1/1] compile", "system: artifact ", "sha256 "} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if r.released != 1 {
		t.Fatalf("working copy released %d times", r.released)
	}
	want := "ping|pull alpine:3|create c1|start c1|stop c1|rm c1"
	if got := strings.Join(r.eng.Snapshot(), "|"); got != want {
		t.Fatalf("calls: %s", got)
	}
}

func TestContainersAreUnprivilegedBoundedAndGetTheDeclaredEnvironment(t *testing.T) {
	r := newRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  env: {A: base, B: base, TZ: UTC}
  network: none
  limits: {cpus: 1.5, memory: 2g}
  cache:
    - {name: deps, path: /root/.cache}
  steps:
    - {name: one, run: "true", env: {B: step}, workdir: sub}
    - {name: two, run: "true", image: "debian:13"}
artifact: {name: app, paths: [out/app]}
`)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	if len(r.eng.Created) != 2 {
		t.Fatalf("created %d containers", len(r.eng.Created))
	}
	c := r.eng.Created[0]
	if c.User != "505:20" || c.Network != "none" || c.CPUs != 1.5 || c.Memory != "2g" || c.Workdir != "/work/sub" || c.Image != "alpine:3" {
		t.Fatalf("spec: %+v", c)
	}
	if strings.Join(c.Cmd, "|") != "sh|-ec|true" {
		t.Fatalf("cmd %v", c.Cmd)
	}
	if c.Env["A"] != "base" || c.Env["B"] != "step" || c.Env["TZ"] != "UTC" || c.Env["DYMPLOY_COMMIT"] != commit ||
		c.Env["DYMPLOY_SHORT_COMMIT"] != commit[:12] || c.Env["DYMPLOY_OWNER"] != "acme" || c.Env["DYMPLOY_REPO"] != "app" ||
		c.Env["DYMPLOY_BRANCH"] != "main" || c.Env["DYMPLOY_JOB"] != "job-1" || c.Env["HOME"] != "/tmp" {
		t.Fatalf("env: %v", c.Env)
	}
	if _, ok := c.Env["DYMPLOY_TAG"]; ok {
		t.Fatal("no tag was pushed")
	}
	if len(c.Mounts) != 2 || c.Mounts[0].Container != "/work" || c.Mounts[0].Host != r.repo || c.Mounts[0].ReadOnly ||
		c.Mounts[1].Container != "/root/.cache" || c.Mounts[1].Host != filepath.Join(r.cache, "github", "acme", "app", "deps") {
		t.Fatalf("mounts: %+v", c.Mounts)
	}
	if fi, err := os.Stat(c.Mounts[1].Host); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v %v", fi, err)
	}
	if r.eng.Created[1].Image != "debian:13" || r.eng.Created[1].Workdir != "/work" {
		t.Fatalf("second step: %+v", r.eng.Created[1])
	}
	if c.Labels["dymploy.job"] != "job-1" || !strings.HasPrefix(c.Name, "dymploy-job-1-") {
		t.Fatalf("identity: %+v", c)
	}
}

func TestAFailingStepStopsTheBuildWithItsExitCode(t *testing.T) {
	r := newRig(t, strings.Replace(basic, `- {name: compile, run: "make"}`, `- {name: compile, run: "make"}
    - {name: after, run: "true"}`, 1))
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{ExitCode: 7, Stderr: "boom\n"}
	}
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(r.eng.Created) != 1 {
		t.Fatalf("the second step ran after the first failed")
	}
	if !strings.Contains(r.log(), "failed with exit code 7") {
		t.Fatalf("log:\n%s", r.log())
	}
	if _, err := os.Stat(filepath.Join(r.arts, "job-1")); err == nil {
		t.Fatal("a failed build must not leave an artifact")
	}
}

func TestMissingOrInvalidManifestFailsTheJobWithoutRunningAnything(t *testing.T) {
	for name, tc := range map[string]struct{ manifest, want string }{
		"missing":                      {"", "no dymploy.yaml"},
		"invalid":                      {"version: 1\nbuild: {}\n", "dymploy.yaml:"},
		"dockerfile that is not there": {"version: 1\nbuild:\n  image: {dockerfile: Dockerfile}\n  steps: [{run: \"true\"}]\nartifact: {paths: [a]}\n", "dockerfile"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tc.manifest)
			res, err := r.do(context.Background())
			if err != nil || res.ExitCode != docker.ExitBadManifest {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if !strings.Contains(r.log(), tc.want) || len(r.eng.Created) != 0 || r.released != 1 {
				t.Fatalf("created=%d released=%d log:\n%s", len(r.eng.Created), r.released, r.log())
			}
		})
	}
}

func TestAManifestThatIsALinkIsNotFollowed(t *testing.T) {
	r := newRig(t, "")
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("version: 1 # secret"), 0o600)
	if err := os.Symlink(secret, filepath.Join(r.repo, "dymploy.yaml")); err != nil {
		t.Fatal(err)
	}
	res, _ := r.do(context.Background())
	if res.ExitCode != docker.ExitBadManifest || !strings.Contains(r.log(), "regular file") || strings.Contains(r.log(), "secret") {
		t.Fatalf("res=%+v log:\n%s", res, r.log())
	}
}

func TestTheEngineBeingUnavailableIsAnErrorOfTheRunnerNotAFailedJob(t *testing.T) {
	for name, set := range map[string]func(*dockertest.Fake){
		"ping":   func(f *dockertest.Fake) { f.PingErr = errors.New("daemon down") },
		"pull":   func(f *dockertest.Fake) { f.PullErr = errors.New("no such image") },
		"create": func(f *dockertest.Fake) { f.CreateErr = errors.New("name in use") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, basic)
			set(r.eng)
			res, err := r.do(context.Background())
			if err == nil || res.ExitCode != 0 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if r.released != 1 && name != "ping" {
				t.Fatalf("released %d", r.released)
			}
		})
	}
}

func TestExceedingTheTimeoutStopsTheContainerAndFailsTheJob(t *testing.T) {
	r := newRig(t, strings.Replace(basic, "build:\n", "build:\n  limits: {timeout: 1s}\n", 1))
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior { return dockertest.Behavior{Hang: true} }
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != docker.ExitTimeout {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	calls := strings.Join(r.eng.Snapshot(), "|")
	if !strings.Contains(calls, "stop c1|rm c1") || !strings.Contains(r.log(), "time limit") {
		t.Fatalf("calls %s\n%s", calls, r.log())
	}
}

func TestCancellingTheJobStopsTheContainerAndReturnsTheContextError(t *testing.T) {
	r := newRig(t, basic)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior { return dockertest.Behavior{Hang: true} }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(r.eng.Snapshot()) < 4 { // ping, pull, create, start
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := r.do(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if calls := strings.Join(r.eng.Snapshot(), "|"); !strings.HasSuffix(calls, "stop c1|rm c1") {
		t.Fatalf("calls %s", calls)
	}
}

func TestADockerfileImageIsBuiltForTheJobAndRemovedAfterwards(t *testing.T) {
	r := newRig(t, `version: 1
build:
  image: {dockerfile: ci/Dockerfile, context: ci, args: {V: "1"}}
  steps: [{run: "true"}]
artifact: {name: app, paths: [out/app]}
`)
	r.file("ci/Dockerfile", "FROM alpine:3\n")
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	b := r.eng.Builds[0]
	realRepo, _ := filepath.EvalSymlinks(r.repo)
	if b.Dockerfile != filepath.Join(realRepo, "ci/Dockerfile") || b.Context != filepath.Join(realRepo, "ci") || b.Args["V"] != "1" || b.Tag != "dymploy-build-job-1" {
		t.Fatalf("build: %+v", b)
	}
	if r.eng.Created[0].Image != b.Tag {
		t.Fatalf("the step ran in %s, not in the image built", r.eng.Created[0].Image)
	}
	calls := r.eng.Snapshot()
	if calls[len(calls)-1] != "rmi "+b.Tag {
		t.Fatalf("image not removed: %v", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "pull") {
			t.Fatalf("nothing should be pulled when the image is built: %v", calls)
		}
	}
}

func TestADockerfileThatLeadsOutsideTheRepositoryIsRefused(t *testing.T) {
	r := newRig(t, `version: 1
build:
  image: {dockerfile: Dockerfile}
  steps: [{run: "true"}]
artifact: {name: app, paths: [out/app]}
`)
	outside := filepath.Join(t.TempDir(), "Dockerfile")
	os.WriteFile(outside, []byte("FROM scratch\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(r.repo, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	_, err := r.do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside the repository") || len(r.eng.Builds) != 0 {
		t.Fatalf("err=%v builds=%d", err, len(r.eng.Builds))
	}
}

func TestABuildThatProducesNoArtifactFailsTheJob(t *testing.T) {
	r := newRig(t, basic)
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != docker.ExitNoArtifact || !strings.Contains(r.log(), "could not be packed") {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
}

func TestEachImageIsPreparedOnce(t *testing.T) {
	r := newRig(t, strings.Replace(basic, `- {name: compile, run: "make"}`, `- {name: a, run: "true"}
    - {name: b, run: "true"}
    - {name: c, run: "true", image: "debian:13"}
    - {name: d, run: "true", image: "debian:13"}`, 1))
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if _, err := r.do(context.Background()); err != nil {
		t.Fatal(err)
	}
	pulls := 0
	for _, c := range r.eng.Snapshot() {
		if strings.HasPrefix(c, "pull") {
			pulls++
		}
	}
	if pulls != 2 {
		t.Fatalf("pulls=%d: %v", pulls, r.eng.Snapshot())
	}
}

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	if _, err := docker.New(docker.Config{}); err == nil {
		t.Fatal("no engine, no source")
	}
	src := func(context.Context, runner.Task) (string, func(), provider.Event, error) {
		return "", nil, provider.Event{}, nil
	}
	if _, err := docker.New(docker.Config{Engine: &dockertest.Fake{}, Source: src, ArtifactsDir: "rel", CacheDir: "/x"}); err == nil {
		t.Fatal("relative directories")
	}
}

func TestASourceErrorIsReturnedAsIs(t *testing.T) {
	boom := errors.New("clone failed")
	run, _ := docker.New(docker.Config{Engine: &dockertest.Fake{}, ArtifactsDir: "/a", CacheDir: "/c",
		Source: func(context.Context, runner.Task) (string, func(), provider.Event, error) {
			return "", nil, provider.Event{}, boom
		}})
	_, err := run.Run(context.Background(), runner.Task{JobID: "j"})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}

const withDeploy = `version: 1
build:
  image: {from: "alpine:3"}
  steps: [{name: compile, run: "make"}]
artifact: {name: app, paths: [out/app]}
deploy:
  target: production
  steps:
    - {name: install, run: "install it"}
  check: {run: "probe", attempts: 1}
`

type deployRig struct {
	*rig
	remote *deploytest.Fake
}

func newDeployRig(t *testing.T, manifest string, allow bool) *deployRig {
	t.Helper()
	r := newRig(t, manifest)
	remote := &deploytest.Fake{}
	ts := deploy.NewTargets()
	rules := []webhook.Rule{{Provider: "github", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}}}
	if !allow {
		rules = []webhook.Rule{{Provider: "github", Owner: "acme", Repositories: []string{"other"}, Branches: []string{"main"}}}
	}
	ts.Set(map[string]deploy.Target{"production": {Name: "production", Host: "h", User: "deploy", Allow: webhook.Policy{Rules: rules}}})
	run, err := docker.New(docker.Config{
		Engine: r.eng, ArtifactsDir: r.arts, CacheDir: r.cache, User: "505:20", StopGrace: time.Second,
		Deployer: &deploy.Deployer{Remote: remote, Targets: ts},
		Source: func(_ context.Context, _ runner.Task) (string, func(), provider.Event, error) {
			return r.repo, func() { r.released++ }, provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: commit, Branch: "main"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.run = run
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	return &deployRig{rig: r, remote: remote}
}

func TestBuildThenDeployAsTheLastStepOfTheJob(t *testing.T) {
	r := newDeployRig(t, withDeploy, true)
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
	if got := strings.Join(r.remote.Scripts(), "|"); got != "install it|probe" {
		t.Fatalf("scripts: %s", got)
	}
	dir := filepath.Join(r.arts, "job-1")
	rec, _, err := artifact.ReadRecord(dir)
	if err != nil || rec.Job != "job-1" || rec.Commit != commit || rec.Owner != "acme" || rec.Repo != "app" || rec.Name != "app" {
		t.Fatalf("record %+v %v", rec, err)
	}
	var env map[string]string
	for _, run := range r.remote.Runs {
		if run.Script == "install it" {
			env = run.Env
		}
	}
	if env["DYMPLOY_ARTIFACT_SHA256"] != rec.SHA256 || env["DYMPLOY_TARGET"] != "production" {
		t.Fatalf("env: %v", env)
	}
	log := r.log()
	if strings.Index(log, "artifact ") > strings.Index(log, "deploying to production") || !strings.Contains(log, "deployed to production") {
		t.Fatalf("the deploy comes after the build in the story:\n%s", log)
	}
}

func TestAFailedDeployFailsTheJobWithTheDeployCode(t *testing.T) {
	r := newDeployRig(t, withDeploy, true)
	r.remote.Handle = func(string) (int, string, bool) { return 1, "", false }
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != deploy.ExitStep {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
}

func TestATargetTheRepositoryMayNotUseFailsBeforeBuilding(t *testing.T) {
	r := newDeployRig(t, withDeploy, false)
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != deploy.ExitTargetDenied || !strings.Contains(r.log(), "not allowed to deploy") || !strings.Contains(r.log(), "nothing was built") {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
	if len(r.eng.Created) != 0 || len(r.remote.Dialed) != 0 || r.released != 1 {
		t.Fatalf("nothing may run: containers=%d dialed=%d released=%d", len(r.eng.Created), len(r.remote.Dialed), r.released)
	}
	r = newDeployRig(t, strings.Replace(withDeploy, "target: production", "target: nowhere", 1), true)
	if res, _ := r.do(context.Background()); res.ExitCode != deploy.ExitTargetDenied || !strings.Contains(r.log(), "not configured") {
		t.Fatalf("%+v\n%s", res, r.log())
	}
}

func TestADefinitionThatDeploysNeedsAServerThatCan(t *testing.T) {
	r := newRig(t, withDeploy) // no Deployer
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != deploy.ExitTargetDenied || !strings.Contains(r.log(), "not set up to deploy") || len(r.eng.Created) != 0 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
}

func TestAJobWithoutDeployDoesNotNeedTargets(t *testing.T) {
	r := newDeployRig(t, basic, true)
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 || len(r.remote.Dialed) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(r.arts, "job-1", artifact.RecordFile)); err != nil {
		t.Fatalf("every artifact gets its record: %v", err)
	}
}

func TestTheServersLimitsAreAppliedAndCannotBeExceeded(t *testing.T) {
	r := newRig(t, basic)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if c := r.eng.Created[0]; c.Memory != "4g" || c.CPUs != 2 {
		t.Fatalf("a definition with no limits gets the server's defaults, not unlimited: %+v", c)
	}

	r = newRig(t, strings.Replace(basic, "build:\n", "build:\n  limits: {memory: 64g}\n", 1))
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != docker.ExitBadManifest || !strings.Contains(r.log(), "more than this server allows") || len(r.eng.Created) != 0 {
		t.Fatalf("res=%+v err=%v\n%s", res, err, r.log())
	}
}

func TestTheOpenFilesLimitReachesTheContainersOnlyWhenAsked(t *testing.T) {
	r := newRig(t, basic)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if c := r.eng.Created[0]; c.NoFile != 0 {
		t.Fatalf("nothing asked and the server sets nothing: the engine keeps its own limit: %+v", c)
	}

	r = newRig(t, strings.Replace(basic, "build:\n", "build:\n  limits: {nofile: 65536}\n", 1))
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, c := range r.eng.Created {
		if c.NoFile != 65536 {
			t.Fatalf("every step container gets the limit the definition asked for: %+v", c)
		}
	}
}

func TestBadServerParamsAreErrorsOfTheRunner(t *testing.T) {
	r := newRig(t, basic)
	_, err := r.run.Run(context.Background(), runner.Task{JobID: "j", Params: json.RawMessage(`{"submodules":true}`), Log: runner.NewLineReporter(func(runner.Stream, string) {})})
	if err == nil || !strings.Contains(err.Error(), "submodules are not supported") {
		t.Fatalf("%v", err)
	}
}

func TestTheCloneStepOnlyReceivesItsOwnParams(t *testing.T) {
	r := newRig(t, basic)
	var got json.RawMessage
	r.run, _ = docker.New(docker.Config{
		Engine: r.eng, ArtifactsDir: r.arts, CacheDir: r.cache, User: "1:1",
		Source: func(_ context.Context, t runner.Task) (string, func(), provider.Event, error) {
			got = t.Params
			return r.repo, func() {}, provider.Event{Provider: "github", Owner: "a", Repo: "b", Commit: commit}, nil
		},
	})
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	if _, err := r.run.Run(context.Background(), runner.Task{JobID: "j", Params: json.RawMessage(`{"depth":3,"limits":{"memory":"1g"}}`), Log: rep}); err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"depth":3}` {
		t.Fatalf("the clone saw %s", got)
	}
}

func TestTheGitDirectoryIsReadOnlyInsideTheBuild(t *testing.T) {
	r := newRig(t, basic)
	os.MkdirAll(filepath.Join(r.repo, ".git"), 0o755)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	var found bool
	for _, m := range r.eng.Created[0].Mounts {
		if m.Container == "/work/.git" {
			found = true
			if !m.ReadOnly || m.Host != filepath.Join(r.repo, ".git") {
				t.Fatalf("%+v", m)
			}
		}
	}
	if !found {
		t.Fatal(".git must be mounted read-only over the working copy")
	}
	// a repository without a .git directory (a gitlink file, for instance) gets no extra mount
	r = newRig(t, basic)
	os.WriteFile(filepath.Join(r.repo, ".git"), []byte("gitdir: elsewhere"), 0o644)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Effect: writeOut("out/app")}
	}
	r.do(context.Background())
	if len(r.eng.Created[0].Mounts) != 1 {
		t.Fatalf("%+v", r.eng.Created[0].Mounts)
	}
}

func TestAJobThatOnlyChecksTheCodePacksAndDeploysNothing(t *testing.T) {
	r := newDeployRig(t, "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{name: tests, run: \"make test\"}]\n", true)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{Stdout: "12 tests, 0 failures\n"}
	}
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != 0 || !strings.Contains(r.log(), "stdout: 12 tests, 0 failures") || !strings.Contains(r.log(), "no artifact declared") {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	if _, err := os.Stat(filepath.Join(r.arts, "job-1")); err == nil || len(r.remote.Dialed) != 0 {
		t.Fatal("no artifact directory, no deploy")
	}
	// and when the tests fail, so does the job, with the tests' own exit code
	r = newDeployRig(t, "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{name: tests, run: \"make test\"}]\n", true)
	r.eng.Script = func(docker.ContainerSpec) dockertest.Behavior {
		return dockertest.Behavior{ExitCode: 2, Stderr: "1 failure\n"}
	}
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFailedDeploymentRetryReusesVerifiedArtifact(t *testing.T) {
	r := newDeployRig(t, withDeploy, true)
	r.remote.Handle = func(s string) (int, string, bool) {
		if s == "probe" {
			return 1, "", false
		}
		return 0, "", false
	}
	result, err := r.do(context.Background())
	if err != nil || result.ExitCode != deploy.ExitCheck {
		t.Fatalf("%+v %v", result, err)
	}
	input, _ := json.Marshal(provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: commit, Branch: "main"})
	task := runner.Task{JobID: "job-1", Name: "github/acme/app", Input: input}
	next, err := r.run.RetryInput(context.Background(), task, result.Details)
	if err != nil {
		t.Fatal(err)
	}
	r.eng.PingErr = errors.New("engine offline: retry must not build")
	r.remote.Handle = nil
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	defer rep.Close()
	task.JobID = "job-2"
	task.Input = next
	task.Log = rep
	res, err := r.run.Run(context.Background(), task)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	fresh, _, err := artifact.ReadRecord(filepath.Join(r.arts, "job-2"))
	if err != nil || fresh.Job != "job-2" || time.Since(fresh.CreatedAt) > time.Minute {
		t.Fatalf("retry did not get independent retention: %+v %v", fresh, err)
	}
	if len(r.eng.Created) != 1 || r.released != 1 {
		t.Fatal("retry rebuilt or cloned")
	}
	// Integrity is checked even for retry planning.
	var out deploy.Outcome
	json.Unmarshal(result.Details, &out)
	rec, file, err := artifact.ReadRecord(filepath.Join(r.arts, out.ArtifactJob))
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	os.WriteFile(file, []byte("corrupt"), 0600)
	if _, err := r.run.RetryInput(context.Background(), task, result.Details); err == nil {
		t.Fatal("accepted corrupt artifact")
	}
}

func TestRetryRefusesChangedIdentityOrTarget(t *testing.T) {
	r := newDeployRig(t, withDeploy, true)
	r.remote.Handle = func(s string) (int, string, bool) {
		if s == "probe" {
			return 1, "", false
		}
		return 0, "", false
	}
	res, err := r.do(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var details deploy.Outcome
	json.Unmarshal(res.Details, &details)
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	defer rep.Close()
	base := map[string]any{"provider": "github", "owner": "acme", "repo": "app", "commit": commit, "branch": "main"}
	for _, mode := range []string{"identity", "target", "path", "duplicate", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			in := map[string]any{}
			for k, v := range base {
				in[k] = v
			}
			req := map[string]any{"artifact_job": details.ArtifactJob, "targets": []string{"production"}}
			switch mode {
			case "identity":
				in["owner"] = "other"
			case "target":
				req["targets"] = []string{"unauthorized"}
			case "path":
				req["artifact_job"] = "../outside"
			case "duplicate":
				req["targets"] = []string{"production", "production"}
			case "manifest":
				os.WriteFile(filepath.Join(r.arts, details.ArtifactJob, "deploy-plan.yaml"), []byte("broken"), 0600)
			}
			in["deploy_retry"] = req
			raw, _ := json.Marshal(in)
			if _, err := r.run.Run(context.Background(), runner.Task{JobID: "retry", Input: raw, Log: rep}); err == nil {
				t.Fatal("unsafe retry accepted")
			}
		})
	}
}
