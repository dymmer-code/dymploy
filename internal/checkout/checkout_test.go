package checkout

import (
	"context"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a real git origin to talk to ---

type origin struct {
	t    *testing.T
	bare string // the repository clients clone from
	work string // where commits are made
}

func run(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	base := t.TempDir()
	o := &origin{t: t, bare: filepath.Join(base, "origin.git"), work: filepath.Join(base, "work")}
	run(t, base, nil, "init", "-q", "--bare", "-b", "main", o.bare)
	run(t, o.bare, nil, "config", "uploadpack.allowAnySHA1InWant", "true")
	run(t, base, nil, "init", "-q", "-b", "main", o.work)
	run(t, o.work, nil, "remote", "add", "origin", o.bare)
	return o
}

// commit writes a file, commits and pushes; it returns the commit id.
func (o *origin) commit(file, content, msg string) string {
	o.t.Helper()
	p := filepath.Join(o.work, file)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		o.t.Fatal(err)
	}
	run(o.t, o.work, nil, "add", "-A")
	run(o.t, o.work, nil, "commit", "-q", "-m", msg)
	run(o.t, o.work, nil, "push", "-q", "origin", "main")
	return run(o.t, o.work, nil, "rev-parse", "HEAD")
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(Config{Root: filepath.Join(t.TempDir(), "repos"), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (o *origin) req(commit string) Request {
	return Request{Provider: "github", Owner: "acme", Repo: "app", URL: o.bare, Commit: commit}
}

func acquire(t *testing.T, m *Manager, r Request) *Checkout {
	t.Helper()
	c, err := m.Acquire(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	return c
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- cloning and updating ---

func TestFirstAcquireClonesTheCommit(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	c := acquire(t, m, o.req(c1))
	if c.Commit != c1 || read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatalf("%+v", c)
	}
	if want := filepath.Join(m.Root(), "github", "acme", "app"); c.Dir != want {
		t.Fatalf("one directory per repository, got %s want %s", c.Dir, want)
	}
	if got := run(t, c.Dir, nil, "rev-parse", "HEAD"); got != c1 {
		t.Fatalf("HEAD %s", got)
	}
}

func TestLaterAcquiresUpdateToTheRequestedCommit(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	c2 := o.commit("a.txt", "two", "second")
	c := acquire(t, m, o.req(c1))
	c.Release()
	c = acquire(t, m, o.req(c2))
	if read(t, filepath.Join(c.Dir, "a.txt")) != "two" || c.Commit != c2 {
		t.Fatal("must update to the newer commit")
	}
	c.Release()
	c = acquire(t, m, o.req(c1)) // a retry of an older job builds the older commit
	if read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatal("must be able to go back to an older commit")
	}
}

func TestTheTreeIsCleanedBetweenBuilds(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	o.commit(".gitignore", "*.log\n", "ignore logs")
	c3 := o.commit("b.txt", "b", "third")
	c := acquire(t, m, o.req(c3))
	for name, content := range map[string]string{"leftover.txt": "x", "build.log": "ignored file", "a.txt": "modified"} {
		if err := os.WriteFile(filepath.Join(c.Dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(c.Dir, "node_modules", "x"), 0o755)
	c.Release()
	c = acquire(t, m, o.req(c3))
	for _, gone := range []string{"leftover.txt", "build.log", "node_modules"} {
		if _, err := os.Stat(filepath.Join(c.Dir, gone)); err == nil {
			t.Errorf("%s survived: a build must never see what a previous one left", gone)
		}
	}
	if read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatal("modified tracked files must be restored")
	}
	_ = c1
}

func TestShallowClone(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	o.commit("a", "1", "1")
	o.commit("a", "2", "2")
	c3 := o.commit("a", "3", "3")
	r := o.req(c3)
	r.Depth = 1
	c := acquire(t, m, r)
	if n := run(t, c.Dir, nil, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("depth 1 must fetch one commit, got %s", n)
	}
}

func TestUnknownCommitIsAnErrorAndTheCopyStaysUsable(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	c := acquire(t, m, o.req(c1))
	c.Release()
	_, err := m.Acquire(context.Background(), o.req(strings.Repeat("a", 40)))
	if err == nil {
		t.Fatal("a commit the repository does not have must fail")
	}
	c = acquire(t, m, o.req(c1))
	if read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatal("the failure must not damage the working copy")
	}
}

func TestFallbackToTheRefWhenTheHostDoesNotServeBareCommits(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	run(t, o.bare, nil, "config", "uploadpack.allowAnySHA1InWant", "false")
	run(t, o.bare, nil, "config", "uploadpack.allowReachableSHA1InWant", "false")
	c1 := o.commit("a.txt", "one", "first")
	o.commit("a.txt", "two", "second") // c1 is no longer the tip, so a host like this refuses to serve it by id
	m.protocolV0 = true
	direct := o.req(c1)
	if _, err := m.Acquire(context.Background(), direct); err == nil {
		t.Fatal("test premise: asking for a non-tip commit by id should fail against this host")
	}
	r := o.req(c1)
	r.Ref = "refs/heads/main"
	c := acquire(t, m, r)
	if c.Commit != c1 || read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatalf("the ref fallback must still end at the requested commit: %+v", c)
	}
}

func TestFallbackWithAMissingRefFailsClearly(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	run(t, o.bare, nil, "config", "uploadpack.allowAnySHA1InWant", "false")
	c1 := o.commit("a.txt", "one", "first")
	o.commit("a.txt", "two", "second")
	m.protocolV0 = true
	r := o.req(c1)
	r.Ref = "refs/heads/does-not-exist"
	if _, err := m.Acquire(context.Background(), r); err == nil {
		t.Fatal("expected an error")
	}
}

func TestReportReceivesTheSteps(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	var steps []string
	r := o.req(o.commit("a", "1", "1"))
	r.Report = func(s string) { steps = append(steps, s) }
	acquire(t, m, r).Release()
	all := strings.Join(steps, "|")
	for _, want := range []string{"fresh working copy", "fetching", "checking out"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %v", want, steps)
		}
	}
}

func TestARootThatCannotBeCreated(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := New(Config{Root: filepath.Join(file, "sub")}); err == nil {
		t.Fatal("a root below a regular file cannot work")
	}
}

func TestCancellingStopsGit(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a", "1", "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Acquire(ctx, o.req(c1)); err == nil {
		t.Fatal("a cancelled context must stop the work")
	}
}

func TestACorruptedCopyIsRebuilt(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	c := acquire(t, m, o.req(c1))
	dir := c.Dir
	c.Release()
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = acquire(t, m, o.req(c1))
	if read(t, filepath.Join(c.Dir, "a.txt")) != "one" {
		t.Fatal("a broken working copy must be thrown away and cloned again")
	}
}

func TestAMovedRepositoryIsFollowed(t *testing.T) {
	o1, o2, m := newOrigin(t), newOrigin(t), newManager(t)
	c1 := o1.commit("a.txt", "one", "first")
	c2 := o2.commit("a.txt", "from the new place", "x")
	acquire(t, m, o1.req(c1)).Release()
	c := acquire(t, m, o2.req(c2))
	if read(t, filepath.Join(c.Dir, "a.txt")) != "from the new place" {
		t.Fatal("the remote URL is the one in the request, every time")
	}
}

func TestSubmodulesOnlyWhenAsked(t *testing.T) {
	lib, o, m := newOrigin(t), newOrigin(t), newManager(t)
	libCommit := lib.commit("lib.txt", "library", "lib")
	run(t, o.work, []string{"GIT_ALLOW_PROTOCOL=file"}, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib.bare, "vendor/lib")
	run(t, o.work, nil, "commit", "-q", "-m", "add submodule")
	run(t, o.work, nil, "push", "-q", "origin", "main")
	head := run(t, o.work, nil, "rev-parse", "HEAD")

	c := acquire(t, m, o.req(head))
	if _, err := os.Stat(filepath.Join(c.Dir, "vendor", "lib", "lib.txt")); err == nil {
		t.Fatal("submodules are not fetched unless the build asks for them")
	}
	c.Release()
	r := o.req(head)
	r.Submodules = true
	c = acquire(t, m, r)
	if read(t, filepath.Join(c.Dir, "vendor", "lib", "lib.txt")) != "library" {
		t.Fatal("submodule content expected")
	}
	if got := run(t, filepath.Join(c.Dir, "vendor", "lib"), nil, "rev-parse", "HEAD"); got != libCommit {
		t.Fatalf("submodule at %s", got)
	}
}

// --- safety ---

func TestRepositoryHooksNeverRun(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	c2 := o.commit("a.txt", "two", "second")
	c := acquire(t, m, o.req(c1))
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	for _, h := range []string{"post-checkout", "post-merge", "reference-transaction"} {
		_ = os.MkdirAll(filepath.Join(c.Dir, ".git", "hooks"), 0o755)
		if err := os.WriteFile(filepath.Join(c.Dir, ".git", "hooks", h), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c.Release()
	acquire(t, m, o.req(c2))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook in the working copy was executed")
	}
}

func TestTheHostGitConfigurationIsIgnored(t *testing.T) {
	evil := filepath.Join(t.TempDir(), "gitconfig")
	marker := filepath.Join(t.TempDir(), "evil-ran")
	_ = os.WriteFile(evil, []byte("[core]\n\tfsmonitor = touch "+marker+"\n[url \"https://evil.example/\"]\n\tinsteadOf = "+"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", evil)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(evil))
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "one", "first")
	acquire(t, m, o.req(c1))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the user's git configuration must not apply to builds")
	}
}

func TestInvalidRequestsAreRefused(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	good := o.commit("a", "1", "1")
	cases := map[string]func(*Request){
		"owner with ..":      func(r *Request) { r.Owner = ".." },
		"owner with slash":   func(r *Request) { r.Owner = "a/b" },
		"empty repo":         func(r *Request) { r.Repo = "" },
		"dot repo":           func(r *Request) { r.Repo = "." },
		"repo with newline":  func(r *Request) { r.Repo = "a\nb" },
		"provider upper":     func(r *Request) { r.Provider = "GitHub" },
		"provider with path": func(r *Request) { r.Provider = "../x" },
		"short commit":       func(r *Request) { r.Commit = "abc123" },
		"commit as option":   func(r *Request) { r.Commit = "--upload-pack=touch /tmp/pwned" },
		"commit uppercase":   func(r *Request) { r.Commit = strings.ToUpper(good) },
		"empty url":          func(r *Request) { r.URL = "" },
		"url with password":  func(r *Request) { r.URL = "https://user:secret@github.com/a/b.git" },
		"url as option":      func(r *Request) { r.URL = "--upload-pack=touch /tmp/pwned" },
		"ext transport":      func(r *Request) { r.URL = "ext::sh -c touch% /tmp/pwned" },
		"ref as option":      func(r *Request) { r.Ref = "--output=/tmp/pwned" },
		"negative depth":     func(r *Request) { r.Depth = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := o.req(good)
			mutate(&r)
			if c, err := m.Acquire(context.Background(), r); err == nil {
				c.Release()
				t.Fatal("must be refused")
			}
		})
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("an option injection reached git")
	}
}

func TestProductionManagersOnlyAcceptHTTPS(t *testing.T) {
	m, err := New(Config{Root: filepath.Join(t.TempDir(), "r")})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://github.com/a/b.git", "file:///etc", "/etc", "git://github.com/a/b.git", "ssh://git@github.com/a/b.git"} {
		_, err := m.Acquire(context.Background(), Request{Provider: "github", Owner: "a", Repo: "b", URL: u, Commit: strings.Repeat("a", 40)})
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestRootIsPrivate(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	acquire(t, m, o.req(o.commit("a", "1", "1")))
	for _, p := range []string{m.Root(), filepath.Join(m.Root(), "github"), filepath.Join(m.Root(), "github", "acme")} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
}

// --- credentials ---

const password = "ghs_super_secret_installation_token"

// httpOrigin serves the origin over the real smart-HTTP protocol and DEMANDS the credentials.
func httpOrigin(t *testing.T, o *origin, user, pass string) (url string, requests *[]string) {
	t.Helper()
	execPath := run(t, ".", nil, "--exec-path")
	var mu sync.Mutex
	var seen []string
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"), Root: "/git",
		Env:        []string{"GIT_PROJECT_ROOT=" + filepath.Dir(o.bare), "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{"PATH", "HOME"},
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" auth="+map[bool]string{true: "ok", false: "none-or-wrong"}[r.Header.Get("Authorization") == want])
		mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/git/origin.git", &seen
}

func TestCredentialsReachGitButLeaveNoTrace(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "private", "first")
	url, _ := httpOrigin(t, o, "x-access-token", password)
	var steps []string
	r := Request{Provider: "github", Owner: "acme", Repo: "app", URL: url, Commit: c1,
		Auth: &Auth{Username: "x-access-token", Password: password}, Report: func(s string) { steps = append(steps, s) }}
	c := acquire(t, m, r)
	if read(t, filepath.Join(c.Dir, "a.txt")) != "private" {
		t.Fatal("authenticated clone failed")
	}
	// Nothing under the working copy, the manager's root or the home it gives git may contain the secret.
	_ = filepath.WalkDir(m.Root(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + password))
		if strings.Contains(string(b), password) || strings.Contains(string(b), enc) {
			t.Errorf("the token (or its encoding) was written to %s", p)
		}
		return nil
	})
	for _, s := range steps {
		if strings.Contains(s, password) {
			t.Errorf("the token reached the progress report: %s", s)
		}
	}
	cfg := read(t, filepath.Join(c.Dir, ".git", "config"))
	if !strings.Contains(cfg, url) || strings.Contains(cfg, "@") {
		t.Fatalf("the remote is stored without credentials:\n%s", cfg)
	}
}

func TestWrongOrMissingCredentialsFailWithoutLeaking(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "private", "first")
	url, _ := httpOrigin(t, o, "x-access-token", password)
	for name, auth := range map[string]*Auth{"none": nil, "wrong": {Username: "x-access-token", Password: "another-wrong-secret"}} {
		t.Run(name, func(t *testing.T) {
			_, err := m.Acquire(context.Background(), Request{Provider: "github", Owner: "acme", Repo: "app", URL: url, Commit: c1, Auth: auth})
			if err == nil {
				t.Fatal("private repository cloned without the right credentials")
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "another-wrong-secret") {
				t.Fatalf("the error leaks a credential: %v", err)
			}
		})
	}
}

func TestCredentialsAreOnlySentToTheirOwnHost(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a.txt", "x", "first")
	url, seen := httpOrigin(t, o, "x-access-token", password)
	acquire(t, m, Request{Provider: "github", Owner: "acme", Repo: "app", URL: url, Commit: c1, Auth: &Auth{Username: "x-access-token", Password: password}})
	if len(*seen) == 0 {
		t.Fatal("the server saw nothing")
	}
	for _, s := range *seen {
		if !strings.Contains(s, "auth=ok") {
			t.Fatalf("every request of the clone carries the credentials: %v", *seen)
		}
	}
}

// --- concurrency and housekeeping ---

func TestOneAcquirePerRepositoryAtATime(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a", "1", "1")
	first, err := m.Acquire(context.Background(), o.req(c1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx, o.req(c1)); err == nil {
		t.Fatal("the same repository must not be handed out twice")
	}
	other := o.req(c1)
	other.Repo = "other"
	c, err := m.Acquire(context.Background(), other)
	if err != nil {
		t.Fatalf("other repositories are independent: %v", err)
	}
	c.Release()
	first.Release()
	first.Release() // idempotent
	c, err = m.Acquire(context.Background(), o.req(c1))
	if err != nil {
		t.Fatalf("after Release it is available again: %v", err)
	}
	c.Release()
}

func TestAnAcquireWaitsForTheRelease(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a", "1", "1")
	first := acquire(t, m, o.req(c1))
	got := make(chan error, 1)
	go func() {
		c, err := m.Acquire(context.Background(), o.req(c1))
		if err == nil {
			c.Release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("must wait while the repository is in use")
	case <-time.After(100 * time.Millisecond):
	}
	first.Release()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

func TestCleanRemovesOnlyOldUnusedCopies(t *testing.T) {
	o, m := newOrigin(t), newManager(t)
	c1 := o.commit("a", "1", "1")
	now := time.Now()
	m.now = func() time.Time { return now }
	for _, repo := range []string{"old", "recent", "busy"} {
		r := o.req(c1)
		r.Repo = repo
		c, err := m.Acquire(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if repo != "busy" {
			c.Release()
		} else {
			t.Cleanup(c.Release)
		}
	}
	long := now.Add(-45 * 24 * time.Hour)
	for _, repo := range []string{"old", "busy"} {
		if err := os.Chtimes(filepath.Join(m.Root(), "github", "acme", repo, ".git", usedMarker), long, long); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := m.Clean(context.Background(), 30*24*time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	for repo, want := range map[string]bool{"old": false, "recent": true, "busy": true} {
		_, err := os.Stat(filepath.Join(m.Root(), "github", "acme", repo))
		if (err == nil) != want {
			t.Errorf("%s present=%v want %v", repo, err == nil, want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a root is required")
	}
	if _, err := New(Config{Root: "relative/path"}); err == nil {
		t.Fatal("the root must be absolute")
	}
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "r"), Git: "no-such-git-binary"}); err == nil {
		t.Fatal("a missing git binary should be noticed at start, not at the first job")
	}
}
