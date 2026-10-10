// Package runner is the contract between dymploy's queue and whatever actually executes a job.
//
// The queue knows nothing about Docker, SSH or any other mechanism: it hands a Task to the Runner
// registered for the task's Kind and collects what comes back (output, exit code, errors). Params and
// Input are opaque to everything except that Runner: the configuration can carry data all the way to
// the executor without the layers in between having to understand it.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Task is one unit of work.
type Task struct {
	JobID string
	Name  string
	Kind  string
	// Params come from the configuration (how to run jobs of this Kind). Opaque.
	Params json.RawMessage
	// Input is the data of this particular job, for example the event that triggered it. Opaque.
	Input json.RawMessage
	// Log is where the runner reports the execution as it happens. It must be used continuously,
	// not once at the end: whatever the runner reports is what ends up in the job's log and in the
	// live stream, so it is how anyone later knows what happened. The runner must not keep it after
	// Run returns.
	Log Reporter
}

// Result is how a task that ran ended.
type Result struct {
	// ExitCode of the work. Non-zero means the job failed; it is not an error of the runner.
	ExitCode int
}

// Runner executes tasks of the kinds it is registered for.
//
// Run returns a non-nil error only when it could not carry the task out: the mechanism was
// unavailable, the task was malformed, or ctx was cancelled (in which case the runner must stop the
// work, release what it holds and return an error satisfying errors.Is(err, ctx.Err())). A task that
// ran and failed is reported through Result.ExitCode, not through the error.
type Runner interface {
	Run(ctx context.Context, t Task) (Result, error)
}

// ErrUnknownKind is returned for a kind nothing is registered for.
var ErrUnknownKind = errors.New("no runner registered for this kind")

// Registry maps kinds to runners.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]Runner
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{runners: map[string]Runner{}} }

// Register adds a runner for kind.
func (r *Registry) Register(kind string, run Runner) error {
	if kind == "" {
		return errors.New("runner kind must not be empty")
	}
	if run == nil {
		return fmt.Errorf("runner for kind %q is nil", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.runners[kind]; dup {
		return fmt.Errorf("a runner for kind %q is already registered", kind)
	}
	r.runners[kind] = run
	return nil
}

// MustRegister is Register for wiring code, where a duplicate or empty kind is a programming error
// and must stop the program at start, not be handled.
func (r *Registry) MustRegister(kind string, run Runner) {
	if err := r.Register(kind, run); err != nil {
		panic(err)
	}
}

// Get returns the runner for kind.
func (r *Registry) Get(kind string) (Runner, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runners[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return run, nil
}

// Kinds lists the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.runners))
	for k := range r.runners {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
