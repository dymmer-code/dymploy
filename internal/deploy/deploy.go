// Package deploy carries a built artifact to a target and runs the deployment steps there.
//
// What a target is (host, user, key, who may use it) comes from the daemon's configuration; the
// repository's dymploy.yaml only names one. The mechanism that reaches the target hides behind Remote
// (SSH in production, a fake in tests), so this package decides what to do and in which order, and
// knows nothing about SSH.
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

// Exit codes of a job that failed while deploying, for reasons of the repository's definition.
const (
	ExitTargetDenied = 4   // the target does not exist or this repository may not use it
	ExitStep         = 5   // a deploy step failed
	ExitCheck        = 6   // the health check never succeeded
	ExitAfter        = 7   // the build is deployed and healthy, but an `after` step failed
	ExitTimeout      = 124 // deploy.timeout ran out (same code as timeout(1))
)

// DefaultUploadDir is where artifacts are copied when the target does not say.
const DefaultUploadDir = "dymploy-uploads"

// Target is a place to deploy to.
type Target struct {
	Name       string
	Host       string
	Port       int
	User       string
	KeyFile    string
	KnownHosts string
	UploadDir  string
	Allow      webhook.Policy
}

// Conn is an open connection to a target.
type Conn interface {
	// Upload stores the content at remotePath (created with mode 0600, directories as needed).
	Upload(ctx context.Context, remotePath string, r io.Reader) error
	// Run executes a script with `sh -ec` and the given environment, streaming its output, and returns
	// its exit code. An error means the command could not be run or the connection broke.
	Run(ctx context.Context, script string, env map[string]string, stdout, stderr io.Writer) (int, error)
	Close() error
}

// Remote opens connections to targets.
type Remote interface {
	// Check verifies, without connecting, that the target can be used: its files exist and are usable.
	Check(t Target) error
	Dial(ctx context.Context, t Target) (Conn, error)
}

// Targets is the set of targets currently configured. It can be replaced while jobs run.
type Targets struct {
	mu sync.RWMutex
	m  map[string]Target
}

// NewTargets returns an empty set.
func NewTargets() *Targets { return &Targets{m: map[string]Target{}} }

// Set replaces all the targets.
func (ts *Targets) Set(m map[string]Target) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.m = m
}

// Get returns a target by name.
func (ts *Targets) Get(name string) (Target, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.m[name]
	return t, ok
}

// Deployer deploys artifacts.
type Deployer struct {
	Remote  Remote
	Targets *Targets
}

// Authorize says whether the event's repository may deploy to the named target. It is checked before
// anything is built, so a definition that asks for a target it cannot use fails at once.
func (d *Deployer) Authorize(name string, ev provider.Event) (Target, error) {
	t, ok := d.Targets.Get(name)
	if !ok {
		return Target{}, fmt.Errorf("target %q is not configured on this server", name)
	}
	if allowed, _ := t.Allow.Allow(&ev); !allowed {
		return Target{}, fmt.Errorf("%s/%s (%s) is not allowed to deploy to target %q", ev.Owner, ev.Repo, refName(ev), name)
	}
	return t, nil
}

func refName(ev provider.Event) string {
	switch {
	case ev.Branch != "":
		return "branch " + ev.Branch
	case ev.Tag != "":
		return "tag " + ev.Tag
	}
	return ev.Ref
}

// Request is one deployment.
type Request struct {
	Target Target
	Deploy *manifest.Deploy
	Event  provider.Event
	JobID  string
	Record artifact.Record
	File   string // local path of the archive
	Log    runner.Reporter
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Run uploads the archive, verifies it, runs the steps, the health check and the `after` steps, and
// returns the job's exit code. It returns an error only when it could not do its work (the target
// cannot be reached, the upload is damaged) or ctx was cancelled; a deployment that ran and failed is
// an exit code.
//
// There is no automatic undo: if a step or the check fails, whatever the steps already did stays as it is.
func (d *Deployer) Run(parent context.Context, r Request) (int, error) {
	log := r.Log
	ctx, cancel := context.WithTimeout(parent, r.Deploy.Timeout.D())
	defer cancel()
	// ended turns the end of ctx into the right outcome: cancelled by the caller is an error, the
	// deadline running out is a failed job.
	ended := func(err error) (int, error) {
		if parent.Err() != nil {
			return 0, fmt.Errorf("deploy interrupted: %w", parent.Err())
		}
		if ctx.Err() != nil {
			log.System(fmt.Sprintf("the deployment exceeded its time limit (%s) and was stopped", r.Deploy.Timeout.D()))
			return ExitTimeout, nil
		}
		return 0, err
	}

	t := r.Target
	log.System(fmt.Sprintf("deploying to %s (%s@%s)", t.Name, t.User, t.Host))
	conn, err := d.Remote.Dial(ctx, t)
	if err != nil {
		return ended(fmt.Errorf("connecting to target %s: %w", t.Name, err))
	}
	defer conn.Close()

	dir := t.UploadDir
	if dir == "" {
		dir = DefaultUploadDir
	}
	remote := path.Join(dir, unsafe.ReplaceAllString(r.JobID, "-")+"-"+r.Record.File)
	defer func() { // best effort, also when ctx is over
		cctx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		_, _ = conn.Run(cctx, "rm -f -- \"$DYMPLOY_ARTIFACT\"", map[string]string{"DYMPLOY_ARTIFACT": remote}, io.Discard, io.Discard)
	}()

	if err := d.upload(ctx, conn, r, remote); err != nil {
		return ended(err)
	}

	env := r.baseEnv(remote)
	n := len(r.Deploy.Steps)
	for i, s := range r.Deploy.Steps {
		name := stepName(s.Name, i)
		log.System(fmt.Sprintf("[%d/%d] %s", i+1, n, name))
		code, err := conn.Run(ctx, s.Run, merge(s.Env, env), log.Stdout(), log.Stderr())
		if err != nil {
			return ended(fmt.Errorf("running %q on %s: %w", name, t.Name, err))
		}
		if code != 0 {
			log.System(fmt.Sprintf("[%d/%d] %s failed with exit code %d", i+1, n, name, code))
			d.noUndo(r)
			return ExitStep, nil
		}
	}
	if c := r.Deploy.Check; c != nil {
		ok, err := d.check(ctx, conn, r, env)
		if err != nil {
			return ended(err)
		}
		if !ok {
			d.noUndo(r)
			return ExitCheck, nil
		}
	}
	if r.Deploy.After != nil {
		for i, s := range r.Deploy.After.Steps {
			name := stepName(s.Name, i)
			log.System(fmt.Sprintf("after [%d/%d] %s", i+1, len(r.Deploy.After.Steps), name))
			code, err := conn.Run(ctx, s.Run, merge(s.Env, env), log.Stdout(), log.Stderr())
			if err != nil {
				return ended(fmt.Errorf("running %q on %s: %w", name, t.Name, err))
			}
			if code != 0 {
				log.System(fmt.Sprintf("after %s failed with exit code %d: the deployment itself succeeded, nothing is undone", name, code))
				return ExitAfter, nil
			}
		}
	}
	log.System("deployed to " + t.Name)
	return 0, nil
}

// noUndo tells, in the log, what is true after a failure: nothing is rolled back.
func (d *Deployer) noUndo(r Request) {
	msg := "nothing was undone: what the steps already did stays as it is on " + r.Target.Name
	if r.Deploy.Rollback != nil {
		msg += " (deploy.rollback is defined but this version of dymploy does not run it)"
	}
	r.Log.System(msg)
}

func stepName(name string, i int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("step %d", i+1)
}

func (d *Deployer) upload(ctx context.Context, conn Conn, r Request, remote string) error {
	log := r.Log
	f, err := os.Open(r.File)
	if err != nil {
		return fmt.Errorf("opening the artifact: %w", err)
	}
	defer f.Close()
	log.System(fmt.Sprintf("uploading %s (%d bytes) to %s", r.Record.File, r.Record.Size, remote))
	if err := conn.Upload(ctx, remote, f); err != nil {
		return fmt.Errorf("uploading the artifact: %w", err)
	}
	var out, errb bytes.Buffer
	code, err := conn.Run(ctx, "sha256sum -- \"$DYMPLOY_ARTIFACT\"", map[string]string{"DYMPLOY_ARTIFACT": remote}, &out, &errb)
	if err != nil {
		return fmt.Errorf("verifying the upload: %w", err)
	}
	fields := strings.Fields(out.String())
	if code != 0 || len(fields) == 0 {
		return fmt.Errorf("could not verify the upload (sha256sum exited %d): %s", code, strings.TrimSpace(errb.String()))
	}
	if fields[0] != r.Record.SHA256 {
		return errors.New("the file on the target does not match the artifact (SHA-256 differs): nothing was run")
	}
	log.System("upload verified: sha256 " + r.Record.SHA256)
	return nil
}

func (d *Deployer) check(ctx context.Context, conn Conn, r Request, env map[string]string) (bool, error) {
	c, log := r.Deploy.Check, r.Log
	for attempt := 1; attempt <= c.Attempts; attempt++ {
		actx, cancel := context.WithTimeout(ctx, c.Timeout.D())
		code, err := conn.Run(actx, c.Run, env, log.Stdout(), log.Stderr())
		cancel()
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err == nil && code == 0:
			log.System(fmt.Sprintf("health check passed (attempt %d/%d)", attempt, c.Attempts))
			return true, nil
		}
		log.System(fmt.Sprintf("health check failed (attempt %d/%d)", attempt, c.Attempts))
		if attempt < c.Attempts {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(c.Interval.D()):
			}
		}
	}
	log.System("the health check never succeeded")
	return false, nil
}

func (r Request) baseEnv(remote string) map[string]string {
	ev := r.Event
	e := map[string]string{
		"DYMPLOY_COMMIT":          ev.Commit,
		"DYMPLOY_SHORT_COMMIT":    ev.Commit[:min(12, len(ev.Commit))],
		"DYMPLOY_OWNER":           ev.Owner,
		"DYMPLOY_REPO":            ev.Repo,
		"DYMPLOY_JOB":             r.JobID,
		"DYMPLOY_TARGET":          r.Target.Name,
		"DYMPLOY_ARTIFACT":        remote,
		"DYMPLOY_ARTIFACT_SHA256": r.Record.SHA256,
	}
	if ev.Branch != "" {
		e["DYMPLOY_BRANCH"] = ev.Branch
	}
	if ev.Tag != "" {
		e["DYMPLOY_TAG"] = ev.Tag
	}
	return e
}

// merge returns a copy of base overridden by over: the variables dymploy provides always win.
func merge(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
