package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateArgsAreUnprivilegedAndComplete(t *testing.T) {
	args, err := createArgs(ContainerSpec{
		Name: "n", Image: "alpine:3", Cmd: []string{"sh", "-ec", "echo hi"}, Workdir: "/work", User: "1:2",
		Env: map[string]string{"B": "2", "A": "1"}, Network: "none", CPUs: 1.5, Memory: "512m", NoFile: 65536,
		Labels: map[string]string{"k": "v"},
		Mounts: []Mount{{Host: "/h/repo", Container: "/work"}, {Host: "/h/c", Container: "/c", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"create --init --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=1024", "--name n", "--user 1:2", "--workdir /work",
		"--network none", "--cpus 1.5", "--memory 512m --memory-swap 512m", "--ulimit nofile=65536:65536", "--label k=v", "--env A=1 --env B=2",
		"--mount type=bind,source=/h/repo,target=/work --mount type=bind,source=/h/c,target=/c,readonly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, " alpine:3 sh -ec echo hi") {
		t.Errorf("image and command must come last: %s", got)
	}
	for _, bad := range []string{"--privileged", "docker.sock", "--device", "--publish", "--network host"} {
		if strings.Contains(got, bad) {
			t.Errorf("must never contain %q", bad)
		}
	}
}

func TestCreateArgsRefuseWhatCouldBeSmuggledIn(t *testing.T) {
	base := ContainerSpec{Image: "alpine:3", Cmd: []string{"true"}}
	for name, mod := range map[string]func(*ContainerSpec){
		"comma in a path":    func(c *ContainerSpec) { c.Mounts = []Mount{{Host: "/a,b", Container: "/w"}} },
		"newline":            func(c *ContainerSpec) { c.Mounts = []Mount{{Host: "/a", Container: "/w\n"}} },
		"relative host path": func(c *ContainerSpec) { c.Mounts = []Mount{{Host: "a", Container: "/w"}} },
		"host network":       func(c *ContainerSpec) { c.Network = "host" },
		"no command":         func(c *ContainerSpec) { c.Cmd = nil },
		"no image":           func(c *ContainerSpec) { c.Image = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			mod(&c)
			if _, err := createArgs(c); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// fakeDocker installs a script named docker that records its arguments and behaves as DOCKER_FAKE_* say.
func fakeDocker(t *testing.T) (cli *CLI, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1" in
  version) [ -n "$DOCKER_FAKE_DOWN" ] && { echo "Cannot connect to the daemon" >&2; exit 1; }; echo 29.0.0 ;;
  image) case "$2" in inspect) [ -n "$DOCKER_FAKE_HAVE" ] || { echo "Error: No such image" >&2; exit 1; } ;; rm) [ -n "$DOCKER_FAKE_NOSUCH" ] && { echo "Error: No such image: x" >&2; exit 1; } ;; esac ;;
  pull) [ -n "$DOCKER_FAKE_PULLFAIL" ] && { echo "pull access denied" >&2; exit 1; }; echo pulled ;;
  build) echo "building"; echo "step" >&2; [ -n "$DOCKER_FAKE_BUILDFAIL" ] && exit 1 ;;
  create) echo abc123 ;;
  start|stop) echo ok ;;
  logs) echo out-line; echo err-line >&2; [ -n "$DOCKER_FAKE_LOGSFAIL" ] && exit 1 ;;
  wait) echo "${DOCKER_FAKE_EXIT:-0}" ;;
  rm) [ -n "$DOCKER_FAKE_NOSUCH" ] && { echo "Error: No such container: x" >&2; exit 1; }; [ -n "$DOCKER_FAKE_RMFAIL" ] && { echo "busy" >&2; exit 1; }; echo ok ;;
  sleep) exec sleep 30 ;;
esac
exit 0
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := NewCLI(bin)
	if err != nil {
		t.Fatal(err)
	}
	return c, func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func (c *CLI) setenv(k, v string) { c.env = append(c.env, k+"="+v) }

func TestNewCLIFailsWithoutDocker(t *testing.T) {
	if _, err := NewCLI("/nonexistent/docker"); err == nil || !strings.Contains(err.Error(), "docker is required") {
		t.Fatalf("err=%v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := NewCLI(""); err == nil {
		t.Fatal("docker is not in the PATH")
	}
}

func TestPing(t *testing.T) {
	c, _ := fakeDocker(t)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.setenv("DOCKER_FAKE_DOWN", "1")
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "not reachable") || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureImagePullsOnlyWhenMissing(t *testing.T) {
	c, calls := fakeDocker(t)
	var out bytes.Buffer
	c.setenv("DOCKER_FAKE_HAVE", "1")
	if err := c.EnsureImage(context.Background(), "alpine:3", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(calls(), "|"), "pull") {
		t.Fatal("pulled an image that is already there")
	}
	c.setenv("DOCKER_FAKE_HAVE", "")
	if err := c.EnsureImage(context.Background(), "alpine:3", &out); err != nil || !strings.Contains(out.String(), "pulled") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	c.setenv("DOCKER_FAKE_PULLFAIL", "1")
	if err := c.EnsureImage(context.Background(), "alpine:3", &out); err == nil || !strings.Contains(err.Error(), "pulling image alpine:3") {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildImage(t *testing.T) {
	c, calls := fakeDocker(t)
	var out, errb bytes.Buffer
	err := c.BuildImage(context.Background(), BuildSpec{Tag: "t", Dockerfile: "/r/Dockerfile", Context: "/r", Args: map[string]string{"B": "2", "A": "1"}}, &out, &errb)
	if err != nil || out.String() != "building\n" || errb.String() != "step\n" {
		t.Fatalf("err=%v out=%q err=%q", err, out.String(), errb.String())
	}
	if last := calls()[len(calls())-1]; last != "build --progress=plain --tag t --file /r/Dockerfile --build-arg A=1 --build-arg B=2 /r" {
		t.Fatalf("command: %s", last)
	}
	c.setenv("DOCKER_FAKE_BUILDFAIL", "1")
	if err := c.BuildImage(context.Background(), BuildSpec{Tag: "t", Dockerfile: "/r/D", Context: "/r"}, &out, &errb); err == nil {
		t.Fatal("a failed build must fail")
	}
}

func TestRemoveImageIgnoresAMissingOne(t *testing.T) {
	c, _ := fakeDocker(t)
	if err := c.RemoveImage(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	c.setenv("DOCKER_FAKE_NOSUCH", "1")
	if err := c.RemoveImage(context.Background(), "x"); err != nil {
		t.Fatalf("missing image: %v", err)
	}
}

func TestContainerLifecycle(t *testing.T) {
	c, calls := fakeDocker(t)
	ctx := context.Background()
	id, err := c.Create(ctx, ContainerSpec{Image: "alpine:3", Cmd: []string{"true"}})
	if err != nil || id != "abc123" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := c.Create(ctx, ContainerSpec{}); err == nil {
		t.Fatal("an empty spec")
	}
	if err := c.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	c.setenv("DOCKER_FAKE_EXIT", "3")
	code, err := c.Follow(ctx, id, &out, &errb)
	if err != nil || code != 3 || out.String() != "out-line\n" || errb.String() != "err-line\n" {
		t.Fatalf("code=%d err=%v out=%q err=%q", code, err, out.String(), errb.String())
	}
	if err := c.Stop(ctx, id, 7*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls(), "|")
	for _, want := range []string{"start abc123", "logs --follow abc123", "wait abc123", "stop --time 7 abc123", "rm --force --volumes abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	c.setenv("DOCKER_FAKE_NOSUCH", "1")
	if err := c.Remove(ctx, id); err != nil {
		t.Fatalf("missing container: %v", err)
	}
	c.setenv("DOCKER_FAKE_NOSUCH", "")
	c.setenv("DOCKER_FAKE_RMFAIL", "1")
	if err := c.Remove(ctx, id); err == nil {
		t.Fatal("a real failure must be reported")
	}
}

func TestFollowErrors(t *testing.T) {
	c, _ := fakeDocker(t)
	var b bytes.Buffer
	c.setenv("DOCKER_FAKE_LOGSFAIL", "1")
	if _, err := c.Follow(context.Background(), "x", &b, &b); err == nil || !strings.Contains(err.Error(), "following") {
		t.Fatalf("err=%v", err)
	}
	c.setenv("DOCKER_FAKE_LOGSFAIL", "")
	c.setenv("DOCKER_FAKE_EXIT", "not-a-number")
	if _, err := c.Follow(context.Background(), "x", &b, &b); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Fatalf("err=%v", err)
	}
}

func TestACancelledContextAbortsTheCommand(t *testing.T) {
	c, _ := fakeDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var b bytes.Buffer
	start := time.Now()
	_, err := c.run(ctx, "sleep")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := c.Follow(ctx2, "x", &b, &b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if err := c.EnsureImage(ctx2, "x", &b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if err := c.BuildImage(ctx2, BuildSpec{Tag: "t", Dockerfile: "/d", Context: "/"}, &b, &b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestTheEngineGetsASmallEnvironment(t *testing.T) {
	t.Setenv("DYMPLOY_SECRET_TOKEN", "must-not-leak")
	t.Setenv("DOCKER_HOST", "unix:///x")
	c, _ := fakeDocker(t)
	env := strings.Join(c.env, "\n")
	if strings.Contains(env, "must-not-leak") || !strings.Contains(env, "DOCKER_HOST=unix:///x") || !strings.Contains(env, "PATH=") {
		t.Fatalf("env:\n%s", env)
	}
}

func TestBuildImageLimits(t *testing.T) {
	c, calls := fakeDocker(t)
	var b bytes.Buffer
	if err := c.BuildImage(context.Background(), BuildSpec{Tag: "t", Dockerfile: "/r/D", Context: "/r", Memory: "1g", CPUs: 1.5, NoFile: 4096}, &b, &b); err != nil {
		t.Fatal(err)
	}
	last := calls()[len(calls())-1]
	for _, want := range []string{"--memory 1g --memory-swap 1g", "--cpu-period 100000 --cpu-quota 150000", "--ulimit nofile=4096:4096"} {
		if !strings.Contains(last, want) {
			t.Errorf("%q lacks %q", last, want)
		}
	}
}

func TestNoFileIsOnlyPassedWhenSet(t *testing.T) {
	args, err := createArgs(ContainerSpec{Name: "n", Image: "alpine:3", Cmd: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); strings.Contains(got, "--ulimit") {
		t.Errorf("no limit was asked for, the engine keeps its own: %s", got)
	}
}
