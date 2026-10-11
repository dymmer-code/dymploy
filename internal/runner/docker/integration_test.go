//go:build integration

// These tests need a working Docker and network access to pull alpine. Run them with
//
//	go test -tags integration ./internal/runner/docker/
//
// Bind mounts must be visible to the engine: with a VM-based engine (Colima, Docker Desktop) set
// DYMPLOY_IT_DIR to a directory under your home.
package docker_test

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/docker"
)

func realRig(t *testing.T, manifest string, files map[string]string) (*docker.Runner, *rig) {
	t.Helper()
	cli, err := docker.NewCLI("")
	if err != nil {
		t.Skip(err)
	}
	if err := cli.Ping(context.Background()); err != nil {
		t.Skip(err)
	}
	base := os.Getenv("DYMPLOY_IT_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache", "dymploy-it")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	r := &rig{t: t, repo: filepath.Join(root, "repo"), arts: filepath.Join(root, "arts"), cache: filepath.Join(root, "cache")}
	os.MkdirAll(r.repo, 0o755)
	r.file("dymploy.yaml", manifest)
	for k, v := range files {
		r.file(k, v)
	}
	u, _ := user.Current()
	run, err := docker.New(docker.Config{
		Engine: cli, ArtifactsDir: r.arts, CacheDir: r.cache, User: u.Uid + ":" + u.Gid,
		Source: func(context.Context, runner.Task) (string, func(), provider.Event, error) {
			return r.repo, func() {}, provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: commit, Tag: "v1"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.run = run
	return run, r
}

func TestRealBuildRunsUnprivilegedKeepsCachesAndPacks(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  network: none
  limits: {memory: 256m, timeout: 3m}
  cache: [{name: counter, path: /cache}]
  steps:
    - name: identity
      run: |
        test "$(id -u)" != 0
        echo "tag=$DYMPLOY_TAG short=$DYMPLOY_SHORT_COMMIT"
        mkdir -p out
        echo "$DYMPLOY_COMMIT" > out/commit.txt
    - name: no network
      run: "! wget -q -T 3 -O /dev/null http://example.com"
    - name: cache
      run: |
        n=$(cat /cache/n 2>/dev/null || echo 0); echo $((n+1)) > /cache/n; cp /cache/n out/n
    - name: no privileges
      run: "! chown root /work 2>/dev/null; ! ls /var/run/docker.sock 2>/dev/null"
artifact: {name: app, paths: [out], exclude: ["*.tmp"]}
`, nil)
	for i, wantN := range []string{"1", "2"} {
		res, err := r.do(context.Background())
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("run %d: %+v %v\n%s", i, res, err, r.log())
		}
		if i == 0 && !strings.Contains(r.log(), "tag=v1 short="+commit[:12]) {
			t.Fatalf("env not visible:\n%s", r.log())
		}
		b, err := os.ReadFile(filepath.Join(r.repo, "out", "n"))
		if err != nil || strings.TrimSpace(string(b)) != wantN {
			t.Fatalf("run %d: cache counter %q (%v): the cache must survive between builds", i, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.arts, "job-1", "app.tar.gz")); err != nil {
		t.Fatal(err)
	}
}

func TestRealFailureAndTimeout(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  steps: [{name: bad, run: "echo before; exit 9"}]
artifact: {paths: [x]}
`, nil)
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 9 || !strings.Contains(r.log(), "stdout: before") {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	_, r = realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  limits: {timeout: 2s}
  steps: [{name: slow, run: "sleep 60"}]
artifact: {paths: [x]}
`, nil)
	start := time.Now()
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != docker.ExitTimeout || time.Since(start) > 40*time.Second {
		t.Fatalf("%+v %v after %s\n%s", res, err, time.Since(start), r.log())
	}
}

func TestRealCancellationLeavesNoContainerBehind(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  steps: [{name: slow, run: "sleep 60"}]
artifact: {paths: [x]}
`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(4*time.Second, cancel)
	if _, err := r.do(ctx); err == nil || ctx.Err() == nil {
		t.Fatalf("err=%v", err)
	}
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=dymploy.job=job-1").Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("containers left behind: %q (%v)", out, err)
	}
}

func TestRealDockerfileBuild(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {dockerfile: ci/Dockerfile, context: ci, args: {GREETING: hola}}
  steps: [{run: "mkdir -p out && cat /greeting > out/g"}]
artifact: {name: app, paths: [out/g]}
`, map[string]string{"ci/Dockerfile": "FROM alpine:3\nARG GREETING\nRUN echo $GREETING > /greeting\n"})
	if res, err := r.do(context.Background()); err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out", "g")); strings.TrimSpace(string(b)) != "hola" {
		t.Fatalf("got %q", b)
	}
}

// The daemon runs git on the working copy after the build: a build must not be able to plant a
// configuration or a hook in it.
func TestRealBuildCannotTouchTheGitDirectory(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  limits: {memory: 256m, cpus: 1, timeout: 3m}
  steps:
    - name: try to plant things
      run: |
        mkdir -p out
        if echo 'fsmonitor = /tmp/evil' >> .git/config 2>/dev/null; then echo PLANTED > out/result; exit 0; fi
        if mkdir -p .git/hooks 2>/dev/null && echo x > .git/hooks/post-checkout 2>/dev/null; then echo PLANTED > out/result; exit 0; fi
        if touch .git/new-file 2>/dev/null; then echo PLANTED > out/result; exit 0; fi
        echo REFUSED > out/result
        # the working tree itself is still writable
        echo ok > out/tree
artifact: {name: app, paths: [out]}
`, map[string]string{".git/config": "[core]\n\trepositoryformatversion = 0\n"})
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, r.log())
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out", "result")); strings.TrimSpace(string(b)) != "REFUSED" {
		t.Fatalf("the build changed the git directory: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, ".git", "config")); strings.Contains(string(b), "evil") {
		t.Fatalf("config was altered: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out", "tree")); strings.TrimSpace(string(b)) != "ok" {
		t.Fatal("the working tree must stay writable")
	}
}

func TestRealBuildIsHeldToItsMemoryLimit(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  limits: {memory: 64m, timeout: 2m}
  steps:
    - name: eat memory
      run: |
        # a shell cannot hold 300 MB in 64 MB: the kernel kills it
        head -c 300000000 /dev/zero | tail -c 1 > /dev/null
        x=$(head -c 300000000 /dev/zero | tr '\0' 'a'); echo ${#x}
artifact: {paths: [x]}
`, nil)
	res, err := r.do(context.Background())
	if err != nil || res.ExitCode == 0 {
		t.Fatalf("a build that exceeds its memory must fail: %+v %v\n%s", res, err, r.log())
	}
}

// A stop is a request: the container's process gets SIGTERM and the chance to finish what it is doing,
// and only after the grace period is it killed.
func TestRealStopLetsTheProcessCleanUp(t *testing.T) {
	_, r := realRig(t, `version: 1
build:
  image: {from: "alpine:3"}
  steps:
    - name: long work
      run: |
        trap 'echo GOT-TERM-CLEANING-UP; exit 0' TERM
        echo working
        sleep 120 &
        wait
artifact: {paths: [x]}
`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200 && !strings.Contains(r.log(), "working"); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	if _, err := r.do(ctx); err == nil || ctx.Err() == nil {
		t.Fatalf("err=%v", err)
	}
	if d := time.Since(start); d > 9*time.Second {
		t.Fatalf("the container was killed after the whole grace period instead of ending when asked: %s", d)
	}
	log := r.log()
	if !strings.Contains(log, "GOT-TERM-CLEANING-UP") || !strings.Contains(log, "stopping the container") {
		t.Fatalf("the process must see SIGTERM and the story must tell it:\n%s", log)
	}
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=dymploy.job=job-1").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("containers left behind: %q", out)
	}
}
