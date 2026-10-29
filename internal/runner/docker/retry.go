package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

type deployRetry struct {
	ArtifactJob string   `json:"artifact_job"`
	Targets     []string `json:"targets"`
}

func parseRetry(input json.RawMessage) (*deployRetry, error) {
	if len(input) == 0 {
		return nil, nil
	}
	var envelope struct {
		Retry *deployRetry `json:"deploy_retry"`
	}
	if err := json.Unmarshal(input, &envelope); err != nil {
		return nil, err
	}
	return envelope.Retry, nil
}

func (r *Runner) RetryInput(ctx context.Context, t runner.Task, details json.RawMessage) (json.RawMessage, error) {
	var out deploy.Outcome
	if len(details) == 0 || json.Unmarshal(details, &out) != nil || out.ArtifactJob == "" {
		return t.Input, nil
	}
	req := deployRetry{ArtifactJob: out.ArtifactJob}
	for _, target := range out.Targets {
		if target.State != "succeeded" {
			req.Targets = append(req.Targets, target.Name)
		}
	}
	if len(req.Targets) == 0 {
		return t.Input, nil
	}
	if _, _, _, err := r.retryPlan(req); err != nil {
		return nil, err
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(t.Input, &input); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("missing repository event")
	}
	input["deploy_retry"], _ = json.Marshal(req)
	return json.Marshal(input)
}

func (r *Runner) retryPlan(req deployRetry) (*manifest.Manifest, artifact.Record, string, error) {
	if req.ArtifactJob == "" || safe(req.ArtifactJob) != req.ArtifactJob || len(req.Targets) == 0 {
		return nil, artifact.Record{}, "", errors.New("invalid deployment retry")
	}
	dir := filepath.Join(r.cfg.ArtifactsDir, req.ArtifactJob)
	rec, file, err := artifact.ReadRecord(dir)
	if err != nil {
		return nil, rec, "", fmt.Errorf("previous artifact unavailable: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "deploy-plan.yaml"))
	if err != nil {
		return nil, rec, "", err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, rec, "", err
	}
	if m.Deploy == nil {
		return nil, rec, "", errors.New("previous artifact has no deployment plan")
	}
	allowed := map[string]bool{}
	for _, n := range m.Deploy.TargetNames() {
		allowed[n] = true
	}
	for _, n := range req.Targets {
		if !allowed[n] {
			return nil, rec, "", errors.New("retry target is absent or duplicated in the original plan")
		}
		allowed[n] = false
	}
	m.Deploy.Target = ""
	m.Deploy.Targets = req.Targets
	return m, rec, file, nil
}

func (r *Runner) retryDeploy(ctx context.Context, t runner.Task, req deployRetry) (runner.Result, error) {
	params, err := ParseParams(t.Params)
	if err != nil {
		return runner.Result{}, err
	}
	if err := params.authorizeTargets(req.Targets); err != nil {
		return runner.Result{}, err
	}
	m, rec, file, err := r.retryPlan(req)
	if err != nil {
		return runner.Result{}, err
	}
	var ev provider.Event
	if err := json.Unmarshal(t.Input, &ev); err != nil {
		return runner.Result{}, err
	}
	if ev.Provider != rec.Provider || ev.Owner != rec.Owner || ev.Repo != rec.Repo || ev.Commit != rec.Commit {
		return runner.Result{}, errors.New("retry event differs from the artifact identity")
	}
	if r.cfg.Deployer == nil {
		return runner.Result{}, errors.New("deployment is unavailable")
	}
	targets, err := r.cfg.Deployer.AuthorizeMany(m.Deploy, ev)
	if err != nil {
		return runner.Result{ExitCode: deploy.ExitTargetDenied}, nil
	}
	// Give the retry its own verified artifact so normal age-based pruning of the old run is harmless.
	id := safe(t.JobID)
	if id == "" || id == req.ArtifactJob {
		return runner.Result{}, errors.New("invalid retry job id")
	}
	dir := filepath.Join(r.cfg.ArtifactsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return runner.Result{}, err
	}
	src, err := os.Open(file)
	if err != nil {
		return runner.Result{}, err
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, rec.File), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return runner.Result{}, err
	}
	_, err = io.Copy(dst, src)
	closeErr := dst.Close()
	if err != nil {
		return runner.Result{}, err
	}
	if closeErr != nil {
		return runner.Result{}, closeErr
	}
	rec.Job = t.JobID
	rec.CreatedAt = time.Now().UTC()
	if err := artifact.WriteRecord(dir, rec); err != nil {
		return runner.Result{}, err
	}
	raw, err := os.ReadFile(filepath.Join(r.cfg.ArtifactsDir, req.ArtifactJob, "deploy-plan.yaml"))
	if err != nil {
		return runner.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy-plan.yaml"), raw, 0600); err != nil {
		return runner.Result{}, err
	}
	t.Log.System("retrying unfinished destinations with the previous artifact; no build")
	b := build{r: r, t: t, m: m, ev: ev, id: id}
	return b.deploy(ctx, targets)
}
