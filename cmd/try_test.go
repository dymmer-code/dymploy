package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project is a git repository with a tracked file, an ignored secret and, after the commit, one change
// and one new file that are not committed.
func project(t *testing.T, remote string) string {
	t.Helper()
	dir := tmpDir(t)
	sh(t, dir, "git", "init", "-q", "-b", "trunk")
	if remote != "" {
		sh(t, dir, "git", "remote", "add", "origin", remote)
	}
	write(t, dir, ".gitignore", "secret.key\nbuild/\n")
	write(t, dir, "app/main.txt", "v1")
	write(t, dir, "gone.txt", "to be deleted")
	write(t, dir, "secret.key", "TOP SECRET")
	write(t, dir, "build/out", "output")
	sh(t, dir, "git", "add", ".gitignore", "app", "gone.txt")
	sh(t, dir, "git", "commit", "-q", "-m", "first")
	write(t, dir, "app/main.txt", "v2 uncommitted")
	write(t, dir, "dymploy.yaml", "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{name: tests, run: \"make test\"}]\n")
	os.Remove(filepath.Join(dir, "gone.txt"))
	os.Symlink("main.txt", filepath.Join(dir, "app", "link"))
	return dir
}

func TestTryTakesWhatAJobWouldSee(t *testing.T) {
	dir := project(t, "git@github.com:acme/shop")
	src, err := snapshot(context.Background(), dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(src)
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(src, rel)); return string(b) }
	if read("app/main.txt") != "v2 uncommitted" || read("dymploy.yaml") == "" {
		t.Fatal("changes and new files that are not ignored are included: the definition need not be committed yet")
	}
	for _, absent := range []string{"secret.key", "build/out", "gone.txt", ".git"} {
		if _, err := os.Lstat(filepath.Join(src, absent)); err == nil {
			t.Errorf("%s must not be in the copy", absent)
		}
	}
	if l, err := os.Readlink(filepath.Join(src, "app", "link")); err != nil || l != "main.txt" {
		t.Fatalf("links are kept as links: %q %v", l, err)
	}
}

func TestTryWorksOutWhichRepositoryItIs(t *testing.T) {
	ctx := context.Background()
	for remote, want := range map[string]string{
		"git@github.com:acme/shop":              "github/acme/shop",
		"https://github.com/globex/dymploy.git": "github/globex/dymploy",
		"ssh://git@github.com/acme/app.git":     "github/acme/app",
		"git@gitlab.example.com:acme/app.git":   "local/local/",
		"":                                      "local/local/",
	} {
		ev, err := identify(ctx, project(t, remote), "", "")
		if err != nil {
			t.Fatal(err)
		}
		got := ev.Provider + "/" + ev.Owner + "/" + ev.Repo
		if !strings.HasPrefix(got, want) || ev.Branch != "trunk" || ev.Ref != "refs/heads/trunk" || len(ev.Commit) != 40 || ev.FullName != ev.Owner+"/"+ev.Repo {
			t.Errorf("%q → %q %+v", remote, got, ev)
		}
	}
	ev, err := identify(ctx, project(t, ""), "github/acme/other", "release")
	if err != nil || ev.Owner != "acme" || ev.Repo != "other" || ev.Branch != "release" {
		t.Fatalf("%v %+v", err, ev)
	}
	for _, bad := range []string{"acme/app", "a/b/c/d", "//x", "github//x"} {
		if _, err := identify(ctx, project(t, ""), bad, ""); err == nil {
			t.Errorf("--name %q accepted", bad)
		}
	}
	if _, err := identify(ctx, tmpDir(t), "", ""); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("%v", err)
	}
}

// fakeEngine puts a docker command in the PATH that answers well enough for a build step to "run".
func fakeEngine(t *testing.T, exit string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  create) echo abc123 ;;\n  wait) echo " + exit + " ;;\n  logs) echo \"running the tests\"; echo \"1 failure\" >&2 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git, _ := exec.LookPath("git")
	os.Symlink(git, filepath.Join(bin, "git"))
	t.Setenv("PATH", bin)
}

func TestTryRunsTheDefinitionAndShowsTheLog(t *testing.T) {
	dir := project(t, "git@github.com:acme/shop")
	fakeEngine(t, "0")
	out, _, err := run(t, context.Background(), "try", dir, "--out", t.TempDir())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"# trying github/acme/shop (trunk at ", "[1/1] tests", "running the tests", "! 1 failure", "no artifact declared", "# the run succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	fakeEngine(t, "2")
	out, _, err = run(t, context.Background(), "try", dir, "--out", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("a failing step fails the command with the step's code: %v\n%s", err, out)
	}
}

func TestTryLeavesTheDeployAloneUnlessAsked(t *testing.T) {
	dir := project(t, "git@github.com:acme/shop")
	write(t, dir, "dymploy.yaml", "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{run: \"true\"}]\nartifact: {paths: [dymploy.yaml]}\ndeploy:\n  target: production\n  steps: [{run: \"install\"}]\n")
	fakeEngine(t, "0")
	out, _, err := run(t, context.Background(), "try", dir, "--out", t.TempDir())
	if err != nil || !strings.Contains(out, "deploys to production: not delivered in this run") || !strings.Contains(out, "artifact ") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, _, err := run(t, context.Background(), "try", dir, "--deploy"); err == nil || !strings.Contains(err.Error(), "--deploy needs --config") {
		t.Fatalf("%v", err)
	}
}

func TestTryKeepsTheSourcesOnRequest(t *testing.T) {
	dir := project(t, "")
	fakeEngine(t, "0")
	out, _, err := run(t, context.Background(), "try", dir, "--keep", "--out", t.TempDir())
	if err != nil || !strings.Contains(out, "# the sources of this run are in ") {
		t.Fatalf("%v\n%s", err, out)
	}
	path := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "# the sources of this run are in ", 2)[1], "\n", 2)[0])
	if _, err := os.Stat(filepath.Join(path, "dymploy.yaml")); err != nil {
		t.Fatalf("the copy should still be there: %v", err)
	}
	os.RemoveAll(path)
}

func TestTryNeedsDockerAndAUsableConfiguration(t *testing.T) {
	dir := project(t, "")
	t.Setenv("PATH", "/nonexistent")
	if _, _, err := run(t, context.Background(), "try", dir); err == nil || !strings.Contains(err.Error(), "docker is required") {
		t.Fatalf("%v", err)
	}
	if _, _, err := run(t, context.Background(), "try", dir, "--config", filepath.Join(tmpDir(t), "none.yaml")); err == nil {
		t.Fatal("a configuration that is not there")
	}
}

func TestTryDeployUsesTheConfigurationsTargetsAndTheirRules(t *testing.T) {
	dir := project(t, "git@github.com:acme/shop")
	write(t, dir, "dymploy.yaml", "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{run: \"true\"}]\nartifact: {paths: [dymploy.yaml]}\ndeploy:\n  target: production\n  steps: [{run: \"install\"}]\n")
	fakeEngine(t, "0")
	cfgDir := tmpDir(t)
	secret := filepath.Join(cfgDir, "webhook.secret")
	os.WriteFile(secret, []byte("s"), 0o600)
	config := filepath.Join(cfgDir, "config.yaml")
	body := "webhooks:\n  github:\n    secret_file: " + secret + "\n" + targetFiles(t, cfgDir, "production")
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// the repository named is not one the target allows: the run stops before anything is delivered
	out, _, err := run(t, context.Background(), "try", dir, "--deploy", "--config", config, "--name", "github/someone/else", "--out", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "exit code 4") || !strings.Contains(out, "not allowed to deploy to target") {
		t.Fatalf("a try cannot do what a push could not: %v\n%s", err, out)
	}
	// a target whose key is gone is refused up front, as the daemon would at start
	os.Remove(filepath.Join(cfgDir, "production.key"))
	if _, _, err := run(t, context.Background(), "try", dir, "--deploy", "--config", config, "--out", t.TempDir()); err == nil || !strings.Contains(err.Error(), "targets.production") {
		t.Fatalf("%v", err)
	}
}

func TestTryReportsAnInterruptionAndAnUnusableOutputDirectory(t *testing.T) {
	dir := project(t, "")
	fakeEngine(t, "0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := run(t, ctx, "try", dir, "--out", t.TempDir()); err == nil {
		t.Fatal("a run that is cancelled before it starts does not succeed")
	}
	file := filepath.Join(tmpDir(t), "afile")
	os.WriteFile(file, nil, 0o600)
	if _, _, err := run(t, context.Background(), "try", dir, "--out", file); err == nil {
		t.Fatal("an output directory that is a file")
	}
}

func TestSnapshotFailuresAreReportedAndCleanedUp(t *testing.T) {
	ctx := context.Background()
	dir := project(t, "")
	// the place for the copy cannot be created
	file := filepath.Join(tmpDir(t), "afile")
	os.WriteFile(file, nil, 0o600)
	if _, err := snapshot(ctx, dir, file); err == nil {
		t.Fatal("a parent that is a file")
	}
	// a file that cannot be read stops the copy and leaves nothing behind
	if os.Getuid() != 0 {
		write(t, dir, "app/unreadable", "x")
		os.Chmod(filepath.Join(dir, "app", "unreadable"), 0o000)
		parent := t.TempDir()
		if _, err := snapshot(ctx, dir, parent); err == nil {
			t.Fatal("an unreadable file")
		}
		if left, _ := os.ReadDir(parent); len(left) != 0 {
			t.Fatalf("a failed snapshot removes its copy: %v", left)
		}
		os.Chmod(filepath.Join(dir, "app", "unreadable"), 0o644)
	}
	// not a repository
	if _, err := snapshot(ctx, tmpDir(t), t.TempDir()); err == nil {
		t.Fatal("not a git repository")
	}
	if err := copyFile(filepath.Join(dir, "nothing"), filepath.Join(t.TempDir(), "x"), 0o644); err == nil {
		t.Fatal("a missing source")
	}
	if err := copyFile(filepath.Join(dir, "dymploy.yaml"), filepath.Join(t.TempDir(), "no", "such", "dir", "x"), 0o644); err == nil {
		t.Fatal("a destination that cannot be created")
	}
}
