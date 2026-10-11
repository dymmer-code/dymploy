package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
)

// statusReporter shows the state of each run next to its commit in the provider the push came from.
// It follows the queue's events (in order, from the queue's observer) and never gets in the way: a
// provider that does not answer costs a warning in the log, not a job.
type statusReporter struct {
	providers map[string]provider.Provider
	enabled   map[string]bool
	log       *slog.Logger
	timeout   time.Duration
}

// newStatusReporter returns nil when no provider is set to report.
func newStatusReporter(cfg *config.Config, providers map[string]provider.Provider, log *slog.Logger) *statusReporter {
	enabled := map[string]bool{}
	for name, w := range cfg.Webhooks {
		if _, ok := providers[name].(provider.StatusPublisher); ok && w.ReportStatus {
			enabled[name] = true
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	return &statusReporter{providers: providers, enabled: enabled, log: log, timeout: 20 * time.Second}
}

// describe turns a queue event into what the commit should show. ok is false for events that are not
// worth a status. What it says is deliberately generic: statuses leave dymploy, so no error text, no
// host names and no stop reasons go into them (the log has the details).
func describe(ev queue.Event) (state provider.State, text string, ok bool) {
	j := ev.Job
	run := fmt.Sprintf("Run #%d", j.Number)
	if j.Number == 0 {
		run = "Run"
	}
	switch ev.Type {
	case queue.EventQueued:
		return provider.StatePending, run + " is queued", true
	case queue.EventStarted:
		return provider.StatePending, run + " is running", true
	case queue.EventFinished:
		switch j.State {
		case job.Succeeded:
			return provider.StateSuccess, run + " succeeded", true
		case job.Failed:
			if j.ExitCode != nil {
				return provider.StateFailure, fmt.Sprintf("%s failed (exit code %d)", run, *j.ExitCode), true
			}
			return provider.StateError, run + " could not be completed", true
		case job.Aborted:
			return provider.StateError, run + " was stopped before it finished", true
		case job.Interrupted:
			return provider.StateError, run + " was interrupted", true
		}
	}
	return "", "", false
}

// Observe is the queue's observer.
func (r *statusReporter) Observe(ev queue.Event) {
	var pe provider.Event
	if json.Unmarshal(ev.Job.Input, &pe) != nil || pe.Provider == "" || !r.enabled[pe.Provider] {
		return // not a job that came from a repository, or its provider is not set to report
	}
	state, text, ok := describe(ev)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	pub := r.providers[pe.Provider].(provider.StatusPublisher)
	if err := pub.PublishStatus(ctx, &pe, state, text, ""); err != nil {
		r.log.Warn("could not publish the status of a run", "job", ev.Job.ID, "name", ev.Job.Name, "state", state, "error", err)
	}
}
