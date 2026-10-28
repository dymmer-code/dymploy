package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// ExitNoSecretsSteps is the exit code of a secrets job whose definition does not say what to do with the
// secrets (it has no deploy.secrets_steps): nothing was fetched and nothing touched.
const ExitNoSecretsSteps = 9

// undoTimeout bounds the steps that put things back, which run even when the deploy time limit is over.
const undoTimeout = 2 * time.Minute

// ErrNoSecretsSteps says a definition cannot be used to change only the secrets.
var ErrNoSecretsSteps = errors.New("the definition has no deploy.secrets_steps: it does not say how to put new secrets in place")

// AuthorizeSecrets is AuthorizeDeploy for a job that changes only the secrets: the event's repository must be
// allowed to use the target, the target must be a machine with a secrets source, and the definition must say
// what to do with them (deploy.secrets and deploy.secrets_steps). It is checked before anything is fetched.
func (d *Deployer) AuthorizeSecrets(def *manifest.Deploy, ev provider.Event) (Target, error) {
	t, err := d.Authorize(def.Target, ev)
	if err != nil {
		return Target{}, err
	}
	switch {
	case t.Kind == KindS3:
		return Target{}, fmt.Errorf("target %q is a bucket: it has no secrets", def.Target)
	case len(def.SecretsSteps) == 0:
		return Target{}, ErrNoSecretsSteps
	case !def.Secrets:
		return Target{}, fmt.Errorf("deploy.secrets_steps needs deploy.secrets: true")
	case t.Secrets == nil || d.Secrets == nil:
		return Target{}, fmt.Errorf("target %q has no secrets source configured on this server", def.Target)
	}
	return t, nil
}

// SecretsRequest is one change of secrets: no artifact, the release already on the target stays as it is.
type SecretsRequest struct {
	Target Target
	Deploy *manifest.Deploy
	Event  provider.Event
	JobID  string
	Log    runner.Reporter
}

// RunSecrets fetches the target's secrets, uploads them, runs deploy.secrets_steps with the path in
// $DYMPLOY_SECRETS_FILE, then the health check. If a step or the check fails, deploy.secrets_undo runs, so
// the definition can put back what it replaced. Like Run, it returns an exit code for what ran and failed and
// an error only when it could not do its work.
func (d *Deployer) RunSecrets(parent context.Context, r SecretsRequest) (int, error) {
	log := r.Log
	ctx, cancel := context.WithTimeout(parent, r.Deploy.Timeout.D())
	defer cancel()
	ended := func(err error) (int, error) {
		if parent.Err() != nil {
			return 0, fmt.Errorf("secrets change interrupted: %w", parent.Err())
		}
		if ctx.Err() != nil {
			log.System(fmt.Sprintf("the change exceeded its time limit (%s) and was stopped", r.Deploy.Timeout.D()))
			return ExitTimeout, nil
		}
		return 0, err
	}

	t := r.Target
	if t.Secrets == nil || d.Secrets == nil {
		return 0, fmt.Errorf("target %s has no secrets source configured", t.Name)
	}
	// Fetched before touching the target: if the source fails, nothing has changed there.
	secrets, err := d.Secrets.Fetch(ctx, *t.Secrets)
	if err != nil {
		return ended(fmt.Errorf("fetching the secrets for %s: %w", t.Name, err))
	}
	log.System(fmt.Sprintf("secrets fetched from the source of target %s (%d bytes, never logged)", t.Name, len(secrets)))
	log.System(fmt.Sprintf("changing the secrets of %s (%s@%s); no build, no new release", t.Name, t.User, t.Host))
	conn, err := d.Remote.Dial(ctx, t)
	if err != nil {
		return ended(fmt.Errorf("connecting to target %s: %w", t.Name, err))
	}
	defer conn.Close()

	dir := t.UploadDir
	if dir == "" {
		dir = DefaultUploadDir
	}
	remote := path.Join(dir, unsafe.ReplaceAllString(r.JobID, "-")+"-secrets")
	defer func() { // best effort, also when ctx is over
		cctx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		_, _ = conn.Run(cctx, "rm -f -- \"$DYMPLOY_ARTIFACT\"", map[string]string{"DYMPLOY_ARTIFACT": remote}, io.Discard, io.Discard)
	}()
	if err := d.uploadSecrets(ctx, conn, log, secrets, remote); err != nil {
		return ended(err)
	}

	req := Request{Target: t, Deploy: r.Deploy, Event: r.Event, JobID: r.JobID, Log: log}
	env := req.baseEnv("")
	delete(env, "DYMPLOY_ARTIFACT") // there is no artifact in this job
	delete(env, "DYMPLOY_ARTIFACT_SHA256")
	env["DYMPLOY_SECRETS_FILE"] = remote

	fail := func(code int) (int, error) {
		d.undoSecrets(parent, conn, r, env)
		return code, nil
	}
	n := len(r.Deploy.SecretsSteps)
	for i, s := range r.Deploy.SecretsSteps {
		name := stepName(s.Name, i)
		log.System(fmt.Sprintf("secrets [%d/%d] %s", i+1, n, name))
		code, err := conn.Run(ctx, s.Run, merge(s.Env, env), log.Stdout(), log.Stderr())
		if err != nil {
			return ended(fmt.Errorf("running %q on %s: %w", name, t.Name, err))
		}
		if code != 0 {
			log.System(fmt.Sprintf("secrets [%d/%d] %s failed with exit code %d", i+1, n, name, code))
			return fail(ExitStep)
		}
	}
	if r.Deploy.Check != nil {
		ok, err := d.check(ctx, conn, req, env)
		if err != nil {
			return ended(err)
		}
		if !ok {
			return fail(ExitCheck)
		}
	}
	if d.SecretState != nil {
		if err := d.SecretState.Record(t.Name, secrets); err != nil {
			log.System("could not record the secrets fingerprint; status may be stale")
		}
	}
	log.System("secrets changed on " + t.Name)
	return 0, nil
}

// undoSecrets runs deploy.secrets_undo after a failure. It is best effort: it says in the log what it did,
// and what it could not do.
func (d *Deployer) undoSecrets(parent context.Context, conn Conn, r SecretsRequest, env map[string]string) {
	log := r.Log
	steps := r.Deploy.SecretsUndo
	if len(steps) == 0 {
		log.System("nothing was undone (the definition has no deploy.secrets_undo): what the steps already did stays as it is on " + r.Target.Name)
		return
	}
	if parent.Err() != nil {
		log.System("the job was stopped: deploy.secrets_undo was not run")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), undoTimeout)
	defer cancel()
	for i, s := range steps {
		name := stepName(s.Name, i)
		log.System(fmt.Sprintf("undo [%d/%d] %s", i+1, len(steps), name))
		code, err := conn.Run(ctx, s.Run, merge(s.Env, env), log.Stdout(), log.Stderr())
		if err != nil || code != 0 {
			log.System(fmt.Sprintf("undo %s did not work (exit code %d, error %v): stopped, check %s by hand", name, code, err, r.Target.Name))
			return
		}
	}
	log.System("the secrets steps were undone")
}
