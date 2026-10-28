// Package secretsjob is the runner of the jobs that change only the secrets of a target: it reads dymploy.yaml
// from the commit it is asked about, and runs the definition's deploy.secrets_steps with the target's current
// secrets. It builds nothing and delivers no artifact, so it needs git but not the container engine.
package secretsjob

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// Kind is the name this runner is registered under.
const Kind = "secrets"

// ExitBadManifest is the exit code for a dymploy.yaml that is missing or invalid (the same as a build's).
const ExitBadManifest = 2

// Source provides the working copy of the commit a task is about, and the event that caused the task. dir is
// where the repository is; release gives it back and must be called exactly once.
type Source func(ctx context.Context, t runner.Task) (dir string, release func(), ev provider.Event, err error)

// Config sets up a Runner.
type Config struct {
	Source   Source
	Deployer *deploy.Deployer
}

// Runner implements runner.Runner.
type Runner struct{ cfg Config }

// New returns a runner of secrets jobs.
func New(cfg Config) (*Runner, error) {
	if cfg.Source == nil || cfg.Deployer == nil {
		return nil, errors.New("secrets runner: a source and a deployer are required")
	}
	return &Runner{cfg: cfg}, nil
}

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, t runner.Task) (runner.Result, error) {
	dir, release, ev, err := r.cfg.Source(ctx, t)
	if err != nil {
		return runner.Result{}, err
	}
	defer release()
	log := t.Log

	m, bad := manifest.ReadDir(dir)
	if bad != "" {
		log.Stderr().Write([]byte(bad + "\n"))
		log.System("dymploy.yaml is not usable: nothing was changed")
		return runner.Result{ExitCode: ExitBadManifest}, nil
	}
	if !m.HasDeploy() {
		log.Stderr().Write([]byte("this definition has no deploy section: there are no secrets to change\n"))
		return runner.Result{ExitCode: deploy.ExitNoSecretsSteps}, nil
	}
	var targets []deploy.Target
	for _, name := range m.Deploy.TargetNames() {
		one := *m.Deploy
		one.Target = name
		one.Targets = nil
		target, err := r.cfg.Deployer.AuthorizeSecrets(&one, ev)
		if err != nil {
			log.Stderr().Write([]byte(err.Error() + "\n"))
			log.System("nothing was changed")
			if errors.Is(err, deploy.ErrNoSecretsSteps) {
				return runner.Result{ExitCode: deploy.ExitNoSecretsSteps}, nil
			}
			return runner.Result{ExitCode: deploy.ExitTargetDenied}, nil
		}
		targets = append(targets, target)
	}
	out, err := r.cfg.Deployer.RunMany(ctx, deploy.Request{SecretsOnly: true, Deploy: m.Deploy, Event: ev, JobID: t.JobID, Log: log}, targets, func(out deploy.Outcome) error {
		raw, _ := json.Marshal(out)
		if t.Report != nil {
			return t.Report(raw)
		}
		return nil
	})
	raw, _ := json.Marshal(out)
	return runner.Result{ExitCode: out.ExitCode, Details: raw}, err
}
