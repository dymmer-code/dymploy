package deploy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// TargetResult is public execution metadata; no command output, credentials or host addresses.
type TargetResult struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

type Outcome struct {
	ExitCode    int            `json:"exit_code"`
	Targets     []TargetResult `json:"targets"`
	ArtifactJob string         `json:"artifact_job,omitempty"`
}

// AuthorizeMany checks the entire list before a build or deployment can change anything.
func (d *Deployer) AuthorizeMany(def *manifest.Deploy, ev provider.Event) ([]Target, error) {
	var out []Target
	for _, name := range def.TargetNames() {
		one := *def
		one.Target = name
		one.Targets = nil
		t, err := d.AuthorizeDeploy(&one, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RunMany delivers one artifact. After any failure no new target starts; already running targets
// finish their own lifecycle. Reporting errors stop dispatch too, so progress is never silently lost.
func (d *Deployer) RunMany(ctx context.Context, r Request, targets []Target, report func(Outcome) error) (Outcome, error) {
	out := Outcome{Targets: make([]TargetResult, len(targets))}
	for i, t := range targets {
		out.Targets[i] = TargetResult{Name: t.Name, State: "skipped"}
	}
	var mu sync.Mutex
	var firstErr error
	next := 0
	failed := false
	emit := func() {
		if report != nil {
			snapshot := out
			snapshot.Targets = append([]TargetResult(nil), out.Targets...)
			if err := report(snapshot); err != nil {
				failed = true
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	emit()
	n := 1
	if r.Deploy.MaxParallel != nil {
		n = *r.Deploy.MaxParallel
	}
	n = min(n, len(targets))
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if failed || next == len(targets) || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				out.Targets[i].State = "running"
				out.Targets[i].StartedAt = time.Now().UTC()
				emit()
				stop := failed
				mu.Unlock()
				if stop {
					return
				}
				req := r
				req.Target = targets[i]
				def := *r.Deploy
				def.Target = targets[i].Name
				def.Targets = nil
				req.Deploy = &def
				var log *runner.LineReporter
				if len(targets) > 1 {
					prefix := "[" + targets[i].Name + "] "
					log = runner.NewLineReporter(func(s runner.Stream, line string) {
						switch s {
						case runner.Stdout:
							fmt.Fprintln(r.Log.Stdout(), prefix+line)
						case runner.Stderr:
							fmt.Fprintln(r.Log.Stderr(), prefix+line)
						default:
							r.Log.System(prefix + line)
						}
					})
					req.Log = log
				}
				var code int
				var err error
				if r.SecretsOnly {
					code, err = d.RunSecrets(ctx, SecretsRequest{Target: req.Target, Deploy: req.Deploy, Event: req.Event, JobID: req.JobID, Log: req.Log})
				} else {
					code, err = d.Run(ctx, req)
				}
				if log != nil {
					log.Close()
				}
				mu.Lock()
				result := &out.Targets[i]
				result.ExitCode = code
				result.FinishedAt = time.Now().UTC()
				result.State = "succeeded"
				if err != nil || code != 0 {
					result.State = "failed"
					failed = true
					if out.ExitCode == 0 {
						out.ExitCode = code
					}
					if err != nil && firstErr == nil {
						firstErr = err
					}
					if ctx.Err() != nil {
						result.State = "aborted"
					}
				}
				emit()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, firstErr
}
