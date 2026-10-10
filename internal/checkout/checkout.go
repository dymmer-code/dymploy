// Package checkout keeps local working copies of repositories and brings them to an exact commit,
// for builds. Everything that touches git lives here.
//
// Security rules it enforces: credentials travel to git in environment variables scoped to the
// repository's own host (never in arguments, URLs or files); git runs with a private HOME and with the
// user's and the system's configuration ignored; hooks never run; submodules are fetched only when
// asked; every value that reaches git is validated first; and the tree is cleaned before every build.
package checkout

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// usedMarker is touched (inside .git) every time a copy is handed out; Clean uses it.
const usedMarker = "dymploy-used"

// Config sets up a Manager.
type Config struct {
	// Root is the absolute directory that holds all working copies.
	Root string
	// Git is the git binary; empty means "git" from the PATH.
	Git string
	// Insecure also accepts http://, file:// and local paths as repository URLs. For tests only.
	Insecure bool
}

// Auth is a read-only credential for the repository's host.
type Auth struct {
	Username string
	Password string
}

// Request asks for a working copy of one commit.
type Request struct {
	Provider string // namespaces the directory ("github")
	Owner    string
	Repo     string
	URL      string // where to fetch from; no credentials in it
	Commit   string // full hex id to check out
	// Ref, if set, is fetched when the host refuses to serve a bare commit id.
	Ref        string
	Depth      int // 0 = full history
	Submodules bool
	Auth       *Auth
	// Report, if set, receives a line per step. It never receives credentials.
	Report func(string)
}

// Checkout is a working copy handed out to one user at a time. Call Release when done.
type Checkout struct {
	Dir    string // the working copy
	Commit string // the commit that is checked out
	m      *Manager
	key    string
	once   sync.Once
}

// Release returns the working copy so the next build of the same repository can use it.
func (c *Checkout) Release() {
	c.once.Do(func() { c.m.unlock(c.key) })
}

// Manager hands out working copies.
type Manager struct {
	cfg  Config
	git  string
	home string

	mu    sync.Mutex
	locks map[string]chan struct{}

	now        func() time.Time
	protocolV0 bool // tests: behave like a host that only speaks the old protocol
}

var (
	namePart   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	providerRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	commitRE   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	refRE      = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9._/-]{1,200}$`)
)

// New validates the configuration and prepares the root (mode 0700).
func New(cfg Config) (*Manager, error) {
	if cfg.Root == "" || !filepath.IsAbs(cfg.Root) {
		return nil, errors.New("checkout: the root directory must be an absolute path")
	}
	gitBin := cfg.Git
	if gitBin == "" {
		gitBin = "git"
	}
	resolved, err := exec.LookPath(gitBin)
	if err != nil {
		return nil, fmt.Errorf("checkout: git is required but was not found: %w", err)
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("checkout: creating %s: %w", cfg.Root, err)
	}
	if err := os.Chmod(cfg.Root, 0o700); err != nil {
		return nil, err
	}
	home := filepath.Join(cfg.Root, ".home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, git: resolved, home: home, locks: map[string]chan struct{}{}, now: time.Now}, nil
}

// Root is the directory that holds the working copies.
func (m *Manager) Root() string { return m.cfg.Root }

func (m *Manager) validate(r Request) error {
	switch {
	case !providerRE.MatchString(r.Provider):
		return fmt.Errorf("invalid provider name %q", r.Provider)
	case !validName(r.Owner):
		return fmt.Errorf("invalid owner %q", r.Owner)
	case !validName(r.Repo):
		return fmt.Errorf("invalid repository name %q", r.Repo)
	case !commitRE.MatchString(r.Commit):
		return fmt.Errorf("invalid commit id %q (a full hexadecimal id is required)", r.Commit)
	case r.Ref != "" && (!refRE.MatchString(r.Ref) || strings.Contains(r.Ref, "..") || strings.HasSuffix(r.Ref, ".lock")):
		return fmt.Errorf("invalid ref %q", r.Ref)
	case r.Depth < 0:
		return errors.New("depth must not be negative")
	}
	return m.validateURL(r.URL)
}

func validName(s string) bool { return s != "." && s != ".." && namePart.MatchString(s) }

func (m *Manager) validateURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\r\n\x00") || strings.Contains(raw, "::") {
		return fmt.Errorf("invalid repository URL %q", raw)
	}
	if m.cfg.Insecure && filepath.IsAbs(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("repository URL %q must be https://host/path", raw)
	}
	if u.Scheme != "https" && !(m.cfg.Insecure && (u.Scheme == "http" || u.Scheme == "file")) {
		return fmt.Errorf("repository URL %q must use https", raw)
	}
	if u.User != nil {
		return errors.New("the repository URL must not contain credentials")
	}
	return nil
}

// Acquire returns the working copy of the repository at the requested commit. The copy belongs to
// the caller until Release; other callers for the same repository wait (or give up with ctx).
func (m *Manager) Acquire(ctx context.Context, r Request) (*Checkout, error) {
	if err := m.validate(r); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.cfg.Root, r.Provider, r.Owner, r.Repo)
	if err := m.lock(ctx, dir); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			m.unlock(dir)
		}
	}()
	say := func(format string, a ...any) {
		if r.Report != nil {
			r.Report(fmt.Sprintf(format, a...))
		}
	}
	if err := m.prepare(ctx, dir, r, say); err != nil {
		return nil, err
	}
	ok = true
	return &Checkout{Dir: dir, Commit: r.Commit, m: m, key: dir}, nil
}

func (m *Manager) prepare(ctx context.Context, dir string, r Request, say func(string, ...any)) error {
	s := m.session(r)
	for _, d := range []string{filepath.Join(m.cfg.Root, r.Provider), filepath.Join(m.cfg.Root, r.Provider, r.Owner)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if !m.healthy(ctx, dir, s) {
		say("preparing a fresh working copy of %s/%s", r.Owner, r.Repo)
		if err := m.reset(dir); err != nil {
			return err
		}
		if _, err := m.run(ctx, dir, s, "init", "-q"); err != nil {
			return err
		}
		if _, err := m.run(ctx, dir, s, "remote", "add", "origin", r.URL); err != nil {
			return err
		}
	} else if _, err := m.run(ctx, dir, s, "remote", "set-url", "origin", r.URL); err != nil {
		return err
	}

	say("fetching %s", short(r.Commit))
	if err := m.fetch(ctx, dir, s, r); err != nil {
		return err
	}
	if _, err := m.run(ctx, dir, s, "cat-file", "-e", r.Commit+"^{commit}"); err != nil {
		return fmt.Errorf("the repository does not have commit %s", short(r.Commit))
	}
	say("checking out %s and cleaning the tree", short(r.Commit))
	if _, err := m.run(ctx, dir, s, "checkout", "--force", "--detach", r.Commit); err != nil {
		return err
	}
	if _, err := m.run(ctx, dir, s, "reset", "--hard", "-q", r.Commit); err != nil {
		return err
	}
	if _, err := m.run(ctx, dir, s, "clean", "-ffdxq"); err != nil {
		return err
	}
	if r.Submodules {
		say("updating submodules")
		args := []string{"submodule", "update", "--init", "--recursive", "--force"}
		if r.Depth > 0 {
			args = append(args, "--depth", strconv.Itoa(r.Depth))
		}
		if _, err := m.run(ctx, dir, s, "submodule", "sync", "--recursive"); err != nil {
			return err
		}
		if _, err := m.run(ctx, dir, s, args...); err != nil {
			return err
		}
	}
	head, err := m.run(ctx, dir, s, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != r.Commit {
		return fmt.Errorf("the working copy is at %q, not at the requested commit", strings.TrimSpace(head))
	}
	now := m.now()
	marker := filepath.Join(dir, ".git", usedMarker)
	if err := os.WriteFile(marker, nil, 0o600); err == nil {
		_ = os.Chtimes(marker, now, now)
	}
	return nil
}

// healthy says whether dir is a usable repository.
func (m *Manager) healthy(ctx context.Context, dir string, s session) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	out, err := m.run(ctx, dir, s, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// reset empties dir (only ever inside Root) and recreates it.
func (m *Manager) reset(dir string) error {
	rel, err := filepath.Rel(m.cfg.Root, dir)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return fmt.Errorf("refusing to remove %s", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

func (m *Manager) fetch(ctx context.Context, dir string, s session, r Request) error {
	args := []string{"fetch", "--no-tags", "--no-recurse-submodules", "--force", "-q"}
	if r.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(r.Depth))
	}
	direct := append(append([]string{}, args...), "origin", r.Commit)
	_, err := m.run(ctx, dir, s, direct...)
	if err == nil || r.Ref == "" {
		return err
	}
	// Some hosts do not serve a bare commit id: ask for the branch or tag it was pushed to instead.
	byRef := append(append([]string{}, args...), "origin", "+"+r.Ref+":refs/dymploy/fetched")
	if _, err2 := m.run(ctx, dir, s, byRef...); err2 != nil {
		return err2
	}
	return nil
}

// --- running git ---

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (m *Manager) run(ctx context.Context, dir string, s session, args ...string) (string, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null", // a repository's hooks never run
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=", // nothing may store the token
		"-c", "advice.detachedHead=false",
		"-c", "gc.auto=0",
		"-c", "transfer.fsckObjects=true",
		"-c", "submodule.recurse=false",
	}
	if m.protocolV0 {
		base = append(base, "-c", "protocol.version=0")
	} else {
		base = append(base, "-c", "protocol.version=2")
	}
	if m.cfg.Insecure {
		base = append(base, "-c", "protocol.file.allow=always")
	}
	cmd := exec.CommandContext(ctx, m.git, append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = m.env(s)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := out.String()
	if s.auth != nil {
		text = scrub(text, s.auth)
	}
	if err != nil {
		if ctx.Err() != nil {
			return text, ctx.Err()
		}
		return text, fmt.Errorf("git %s failed: %s", args[0], lastLines(text, 6))
	}
	return text, nil
}

// env builds the environment of a git process from scratch: nothing is inherited except PATH.
func (m *Manager) env(s session) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + m.home,
		"LANG=C", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if !m.cfg.Insecure {
		env = append(env, "GIT_ALLOW_PROTOCOL=https")
	} else {
		env = append(env, "GIT_ALLOW_PROTOCOL=https:http:file")
	}
	if s.auth != nil && s.scope != "" {
		// Scoped to the repository's host: submodules or redirects elsewhere never receive it.
		token := base64.StdEncoding.EncodeToString([]byte(s.auth.Username + ":" + s.auth.Password))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+s.scope+".extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+token,
		)
	}
	return env
}

// session is the credentials of one request and the URL prefix they may be sent to.
type session struct {
	auth  *Auth
	scope string // "https://host/" or "", when there are no credentials to send
}

func (m *Manager) session(r Request) session {
	if r.Auth == nil {
		return session{}
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" {
		return session{auth: r.Auth}
	}
	return session{auth: r.Auth, scope: u.Scheme + "://" + u.Host + "/"}
}

// scrub removes every form of the credential from text that may be shown to someone.
func scrub(text string, a *Auth) string {
	enc := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
	for _, secret := range []string{a.Password, enc} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// --- one user per repository ---

func (m *Manager) lockFor(key string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = make(chan struct{}, 1)
		m.locks[key] = l
	}
	return l
}

func (m *Manager) lock(ctx context.Context, key string) error {
	select {
	case m.lockFor(key) <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the working copy of %s: %w", filepath.Base(key), ctx.Err())
	}
}

func (m *Manager) unlock(key string) { <-m.lockFor(key) }

// Clean removes working copies that were last handed out more than olderThan ago and are not in
// use, and returns how many it removed. Copies are cheap to recreate; disk is not.
func (m *Manager) Clean(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := m.now().Add(-olderThan)
	removed := 0
	providers, _ := os.ReadDir(m.cfg.Root)
	for _, prov := range providers {
		if !prov.IsDir() || strings.HasPrefix(prov.Name(), ".") {
			continue
		}
		owners, _ := os.ReadDir(filepath.Join(m.cfg.Root, prov.Name()))
		for _, own := range owners {
			repos, _ := os.ReadDir(filepath.Join(m.cfg.Root, prov.Name(), own.Name()))
			for _, repo := range repos {
				if err := ctx.Err(); err != nil {
					return removed, err
				}
				dir := filepath.Join(m.cfg.Root, prov.Name(), own.Name(), repo.Name())
				fi, err := os.Stat(filepath.Join(dir, ".git", usedMarker))
				if err != nil || fi.ModTime().After(cutoff) {
					continue
				}
				l := m.lockFor(dir)
				select {
				case l <- struct{}{}: // not in use
				default:
					continue
				}
				err = os.RemoveAll(dir)
				<-l
				if err != nil {
					return removed, err
				}
				removed++
			}
		}
	}
	return removed, nil
}
