package clone

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/checkout"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/provider/fake"
	"github.com/dymmer-code/dymploy/internal/runner"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// world builds git hosting for a fake provider: <base>/acme/app.git with one commit.
type world struct {
	base, commit string
	prov         *fake.Provider
	mgr          *checkout.Manager
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "acme", "app.git")
	work := filepath.Join(t.TempDir(), "work")
	_ = os.MkdirAll(filepath.Dir(bare), 0o755)
	git(t, base, "init", "-q", "--bare", "-b", "main", bare)
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, base, "init", "-q", "-b", "main", work)
	git(t, work, "remote", "add", "origin", bare)
	_ = os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hi"), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "first")
	git(t, work, "push", "-q", "origin", "main")
	m, err := checkout.New(checkout.Config{Root: filepath.Join(t.TempDir(), "repos"), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return &world{base: base, commit: git(t, work, "rev-parse", "HEAD"), prov: fake.New("tok").WithBase(base), mgr: m}
}

func (w *world) runner() *Runner {
	return New(w.mgr, func(name string) (provider.Cloner, bool) {
		if name == "fake" {
			return w.prov, true
		}
		return nil, false
	})
}

func (w *world) task(t *testing.T, ev provider.Event, params string) (runner.Task, *[]string) {
	t.Helper()
	input, _ := json.Marshal(ev)
	var lines []string
	rep := runner.NewLineReporter(func(s runner.Stream, l string) { lines = append(lines, string(s)+":"+l) })
	task := runner.Task{JobID: "j1", Name: "fake/acme/app", Kind: "clone", Input: input, Log: rep}
	if params != "" {
		task.Params = json.RawMessage(params)
	}
	return task, &lines
}

func (w *world) event() provider.Event {
	return provider.Event{Provider: "fake", Kind: provider.KindPush, Owner: "acme", Repo: "app", FullName: "acme/app",
		Ref: "refs/heads/main", Branch: "main", Commit: w.commit}
}

func TestCloneChecksOutTheEventCommitAndSaysSo(t *testing.T) {
	w := newWorld(t)
	task, lines := w.task(t, w.event(), "")
	res, err := w.runner().Run(context.Background(), task)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%v %v", res, err)
	}
	story := strings.Join(*lines, "\n")
	for _, want := range []string{"acme/app", w.commit[:12], "checked out"} {
		if !strings.Contains(story, want) {
			t.Fatalf("missing %q in:\n%s", want, story)
		}
	}
	if b, err := os.ReadFile(filepath.Join(w.mgr.Root(), "fake", "acme", "app", "hello.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the working copy must exist: %v", err)
	}
}

func TestParamsAreOptionalAndOpaqueToTheCaller(t *testing.T) {
	w := newWorld(t)
	task, _ := w.task(t, w.event(), `{"depth":1,"submodules":false}`)
	if _, err := w.runner().Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	bad, _ := w.task(t, w.event(), `{"depth":"deep"}`)
	if _, err := w.runner().Run(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "params") {
		t.Fatalf("badly shaped parameters must be reported as such: %v", err)
	}
	unknown, _ := w.task(t, w.event(), `{"depht":1}`)
	if _, err := w.runner().Run(context.Background(), unknown); err == nil {
		t.Fatal("a typo in the parameters must not be silently ignored")
	}
}

func TestInputProblems(t *testing.T) {
	w := newWorld(t)
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	for name, input := range map[string]string{"not json": "{", "empty": "", "no commit": `{"Provider":"fake","Owner":"acme","Repo":"app"}`} {
		_, err := w.runner().Run(context.Background(), runner.Task{Kind: "clone", Input: json.RawMessage(input), Log: rep})
		if err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	ev := w.event()
	ev.Provider = "gitlab"
	task, _ := w.task(t, ev, "")
	if _, err := w.runner().Run(context.Background(), task); err == nil || !strings.Contains(err.Error(), "gitlab") {
		t.Fatalf("a provider that cannot clone must be named: %v", err)
	}
}

type brokenCreds struct{ *fake.Provider }

func (brokenCreds) CloneCredentials(context.Context, *provider.Event) (provider.Credentials, error) {
	return provider.Credentials{}, errors.New("github: the event has no installation to authenticate with")
}

func TestCredentialProblemsAreErrorsOfTheRunner(t *testing.T) {
	w := newWorld(t)
	r := New(w.mgr, func(string) (provider.Cloner, bool) { return brokenCreds{w.prov}, true })
	task, _ := w.task(t, w.event(), "")
	if _, err := r.Run(context.Background(), task); err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatalf("%v", err)
	}
}

func TestAMissingCommitIsAFailureWithAStory(t *testing.T) {
	w := newWorld(t)
	ev := w.event()
	ev.Commit = strings.Repeat("b", 40)
	task, lines := w.task(t, ev, "")
	_, err := w.runner().Run(context.Background(), task)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "fetching") {
		t.Fatalf("the log must show how far it got: %v", *lines)
	}
}

func TestTheWorkingCopyIsReleasedWhenTheRunEnds(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 2; i++ {
		task, _ := w.task(t, w.event(), "")
		ctx, cancel := context.WithTimeout(context.Background(), 20*1000*1000*1000)
		_, err := w.runner().Run(ctx, task)
		cancel()
		if err != nil {
			t.Fatalf("run %d: the copy must be free again after the previous run: %v", i, err)
		}
	}
}

func TestCancellation(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	task, _ := w.task(t, w.event(), "")
	if _, err := w.runner().Run(ctx, task); err == nil {
		t.Fatal("a cancelled run must report it")
	}
}
