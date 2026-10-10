// Package queue schedules jobs: it accepts them, applies the concurrency limits and the policy for
// busy names, hands each one to the runner registered for its kind, records what the runner reports,
// and keeps the state of every job so that nothing is lost across restarts.
//
// It depends on three contracts only: runner.Runner (how work is executed), job.Store (where job
// state lives) and job.Logs (where the story of each job lives). It knows nothing about Docker,
// GitHub or the data it carries (Params and Input are opaque).
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// Policy says what happens to a new job whose name already has work waiting or running.
type Policy string

// The policies.
const (
	// Queue (default): the new job waits its turn behind the others of its name.
	Queue Policy = "queue"
	// Coalesce: the new job replaces any job of the same name that is still waiting; the one already
	// running is not touched. Only the latest request of a burst runs.
	Coalesce Policy = "coalesce"
	// Reject: the new job is refused while one of its name is waiting or running.
	Reject Policy = "reject"
)

// Config sets the limits.
type Config struct {
	// MaxConcurrent is how many jobs may run at once (at least 1). Jobs with the same name never
	// run at the same time whatever this is.
	MaxConcurrent int
	// MaxPending is how many jobs may wait; 0 means DefaultMaxPending.
	MaxPending int
	// OnBusy is the policy for a name that is busy; empty means Queue.
	OnBusy Policy
}

// DefaultMaxPending bounds the waiting list when the configuration does not.
const DefaultMaxPending = 1000

// Deps are the collaborators of a Manager.
type Deps struct {
	Store   job.Store
	Logs    job.Logs
	Runners *runner.Registry
	Now     func() time.Time
	NewID   func(time.Time) job.ID
	Log     *slog.Logger
}

// Errors returned by the Manager.
var (
	ErrQueueFull   = errors.New("the queue is full")
	ErrRejected    = errors.New("a job with this name is already waiting or running")
	ErrFinished    = errors.New("the job has already finished")
	ErrNotFinished = errors.New("only a finished job can be retried")
	ErrClosed      = errors.New("the queue is not accepting jobs")

	errCanceledByUser = errors.New("canceled by user")
	errShutdown       = errors.New("daemon stopped")
)

// Request describes a job to submit.
type Request struct {
	Name string // jobs with the same name run one at a time
	Kind string // selects the runner
	// Params and Input are carried to the runner without being looked at.
	Params  json.RawMessage
	Input   json.RawMessage
	RetryOf job.ID
}

// EventType tells what happened.
type EventType string

// The events.
const (
	EventQueued   EventType = "queued"
	EventStarted  EventType = "started"
	EventOutput   EventType = "output"
	EventFinished EventType = "finished"
)

// Event is published to subscribers as jobs move. Subscribers that cannot keep up lose events
// (they can resynchronize from the log by sequence number); the queue never waits for them.
type Event struct {
	Type  EventType
	Job   job.Job
	Entry *job.Entry // set for EventOutput
}

type running struct {
	job    job.Job
	cancel context.CancelCauseFunc
}

// Manager is the queue.
type Manager struct {
	cfg  Config
	d    Deps
	log  *slog.Logger
	wg   sync.WaitGroup
	subs subscribers

	mu       sync.Mutex
	open     bool
	started  bool
	root     context.Context
	rootStop context.CancelCauseFunc
	pending  []job.Job
	running  map[job.ID]*running
	busy     map[string]job.ID
}

// New validates the configuration and the dependencies.
func New(cfg Config, d Deps) (*Manager, error) {
	if cfg.MaxConcurrent < 1 {
		return nil, fmt.Errorf("max_concurrent must be at least 1, is %d", cfg.MaxConcurrent)
	}
	if cfg.MaxPending < 0 {
		return nil, fmt.Errorf("max_pending must not be negative, is %d", cfg.MaxPending)
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = DefaultMaxPending
	}
	switch cfg.OnBusy {
	case "":
		cfg.OnBusy = Queue
	case Queue, Coalesce, Reject:
	default:
		return nil, fmt.Errorf("unknown policy %q (use queue, coalesce or reject)", cfg.OnBusy)
	}
	if d.Store == nil || d.Logs == nil || d.Runners == nil {
		return nil, errors.New("the queue needs a Store, Logs and Runners")
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = job.NewID
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Manager{cfg: cfg, d: d, log: d.Log, running: map[job.ID]*running{}, busy: map[string]job.ID{}}, nil
}

// Start recovers the state left by a previous run and begins dispatching. Jobs that were running
// when the daemon stopped are marked interrupted (a half-done deploy must not be repeated blindly);
// jobs that were waiting resume, oldest first.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("the queue was already started")
	}
	was, err := m.d.Store.List(ctx, job.Filter{States: []job.State{job.Running}})
	if err != nil {
		return fmt.Errorf("recovering running jobs: %w", err)
	}
	now := m.d.Now()
	for _, j := range was {
		j.State, j.FinishedAt, j.Error = job.Interrupted, now, "the daemon stopped while the job was running"
		if err := m.d.Store.Put(ctx, j); err != nil {
			return fmt.Errorf("marking %s interrupted: %w", j.ID, err)
		}
		m.log.Warn("job interrupted by a restart", "job", j.ID, "name", j.Name)
	}
	waiting, err := m.d.Store.List(ctx, job.Filter{States: []job.State{job.Queued}})
	if err != nil {
		return fmt.Errorf("recovering waiting jobs: %w", err)
	}
	for i := len(waiting) - 1; i >= 0; i-- { // List is newest first; resume oldest first
		m.pending = append(m.pending, waiting[i])
	}
	m.started, m.open = true, true
	m.root, m.rootStop = context.WithCancelCause(context.Background())
	m.dispatch()
	return nil
}

// Shutdown stops accepting jobs, interrupts the running ones and waits for their runners to return
// (or for ctx). Waiting jobs stay queued for the next start. It is safe to call more than once.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.open = false
	stop := m.rootStop
	m.mu.Unlock()
	if stop != nil {
		stop(errShutdown)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for running jobs to stop: %w", ctx.Err())
	}
}

// Submit accepts a job. The returned job is in state queued.
func (m *Manager) Submit(ctx context.Context, r Request) (job.Job, error) {
	if r.Name == "" || r.Kind == "" {
		return job.Job{}, errors.New("a job needs a name and a kind")
	}
	if _, err := m.d.Runners.Get(r.Kind); err != nil {
		return job.Job{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.open {
		return job.Job{}, ErrClosed
	}
	superseded := -1
	switch m.cfg.OnBusy {
	case Reject:
		if _, busy := m.busy[r.Name]; busy || m.pendingIndex(r.Name) >= 0 {
			return job.Job{}, fmt.Errorf("%w: %s", ErrRejected, r.Name)
		}
	case Coalesce:
		superseded = m.pendingIndex(r.Name)
	}
	if superseded < 0 && len(m.pending) >= m.cfg.MaxPending {
		return job.Job{}, ErrQueueFull
	}
	now := m.d.Now()
	j := job.Job{
		ID: m.d.NewID(now), Name: r.Name, Kind: r.Kind, State: job.Queued,
		Params: r.Params, Input: r.Input, CreatedAt: now, RetryOf: r.RetryOf,
	}
	if err := m.d.Store.Put(ctx, j); err != nil {
		return job.Job{}, fmt.Errorf("storing the job: %w", err)
	}
	if superseded >= 0 {
		old := m.pending[superseded]
		m.pending = append(m.pending[:superseded], m.pending[superseded+1:]...)
		m.finishWaiting(ctx, old, job.Canceled, fmt.Sprintf("superseded by %s", j.ID))
	}
	m.pending = append(m.pending, j)
	m.publish(Event{Type: EventQueued, Job: j})
	m.dispatch()
	return j, nil
}

// Cancel stops a job: a waiting one never runs; a running one gets its context cancelled and the
// runner is expected to stop it.
func (m *Manager) Cancel(ctx context.Context, id job.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.running[id]; ok {
		r.cancel(errCanceledByUser)
		return nil
	}
	for i, j := range m.pending {
		if j.ID == id {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			m.finishWaiting(ctx, j, job.Canceled, "canceled before it started")
			return nil
		}
	}
	j, err := m.d.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j.State.Final() {
		return ErrFinished
	}
	return fmt.Errorf("job %s is %s but not known to the queue", id, j.State)
}

// Retry submits a copy of a finished job.
func (m *Manager) Retry(ctx context.Context, id job.ID) (job.Job, error) {
	j, err := m.d.Store.Get(ctx, id)
	if err != nil {
		return job.Job{}, err
	}
	if !j.State.Final() {
		return job.Job{}, fmt.Errorf("%w: job %s is %s", ErrNotFinished, id, j.State)
	}
	return m.Submit(ctx, Request{Name: j.Name, Kind: j.Kind, Params: j.Params, Input: j.Input, RetryOf: j.ID})
}

// Get returns a job.
func (m *Manager) Get(ctx context.Context, id job.ID) (job.Job, error) { return m.d.Store.Get(ctx, id) }

// List returns jobs, newest first.
func (m *Manager) List(ctx context.Context, f job.Filter) ([]job.Job, error) {
	return m.d.Store.List(ctx, f)
}

// Logs returns the log of a job from a sequence number on.
func (m *Manager) Logs(id job.ID, after int64, limit int) ([]job.Entry, error) {
	return m.d.Logs.Read(id, after, limit)
}

// Prune deletes finished jobs and their logs: those older than maxAge (0 = no age limit) and those
// beyond the keep newest finished ones (0 = no count limit). Unfinished jobs are never touched.
func (m *Manager) Prune(ctx context.Context, maxAge time.Duration, keep int) (int, error) {
	all, err := m.d.Store.List(ctx, job.Filter{States: []job.State{job.Succeeded, job.Failed, job.Canceled, job.Interrupted}})
	if err != nil {
		return 0, err
	}
	cutoff := m.d.Now().Add(-maxAge)
	removed := 0
	for i, j := range all { // newest first
		end := j.FinishedAt
		if end.IsZero() {
			end = j.CreatedAt
		}
		if !((keep > 0 && i >= keep) || (maxAge > 0 && end.Before(cutoff))) {
			continue
		}
		if err := m.d.Logs.Delete(j.ID); err != nil {
			return removed, err
		}
		if err := m.d.Store.Delete(ctx, j.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// --- scheduling (callers hold m.mu) ---

func (m *Manager) pendingIndex(name string) int {
	for i, j := range m.pending {
		if j.Name == name {
			return i
		}
	}
	return -1
}

// dispatch starts as many waiting jobs as the limits allow. A job whose name is busy is skipped,
// not waited for: it must not hold back jobs of other names.
func (m *Manager) dispatch() {
	if !m.open {
		return
	}
	for len(m.running) < m.cfg.MaxConcurrent {
		idx := -1
		for i, j := range m.pending {
			if _, busy := m.busy[j.Name]; !busy {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		j := m.pending[idx]
		m.pending = append(m.pending[:idx], m.pending[idx+1:]...)
		m.begin(j)
	}
}

func (m *Manager) begin(j job.Job) {
	j.State, j.StartedAt = job.Running, m.d.Now()
	if err := m.d.Store.Put(context.Background(), j); err != nil {
		m.log.Error("could not record that the job started", "job", j.ID, "error", err)
	}
	ctx, cancel := context.WithCancelCause(m.root)
	r := &running{job: j, cancel: cancel}
	m.running[j.ID] = r
	m.busy[j.Name] = j.ID
	m.log.Info("job started", "job", j.ID, "name", j.Name, "kind", j.Kind)
	m.publish(Event{Type: EventStarted, Job: j})
	m.wg.Add(1)
	go m.execute(ctx, r)
}

// finishWaiting ends a job that never ran.
func (m *Manager) finishWaiting(ctx context.Context, j job.Job, state job.State, why string) {
	j.State, j.FinishedAt, j.Error = state, m.d.Now(), why
	if err := m.d.Store.Put(ctx, j); err != nil {
		m.log.Error("could not record the end of a waiting job", "job", j.ID, "error", err)
	}
	m.publish(Event{Type: EventFinished, Job: j})
}

// --- execution ---

func (m *Manager) execute(ctx context.Context, r *running) {
	defer m.wg.Done()
	j := r.job
	var seq int64
	logFailures := 0
	rep := runner.NewLineReporter(func(s runner.Stream, text string) {
		seq++
		e := job.Entry{Seq: seq, Time: m.d.Now(), Stream: s, Text: text}
		if err := m.d.Logs.Append(j.ID, e); err != nil {
			if logFailures++; logFailures == 1 {
				m.log.Warn("could not write the job log; the job continues", "job", j.ID, "error", err)
			}
		}
		m.publish(Event{Type: EventOutput, Job: j, Entry: &e})
	})
	rep.System("job started")

	res, runErr := m.run(ctx, j, rep)
	rep.Close()

	final := j
	final.FinishedAt = m.d.Now()
	cause := context.Cause(ctx)
	switch {
	case runErr == nil && res.ExitCode == 0:
		final.State = job.Succeeded
		code := 0
		final.ExitCode = &code
	case errors.Is(cause, errCanceledByUser):
		final.State = job.Canceled
		if runErr == nil {
			code := res.ExitCode
			final.ExitCode = &code
		}
	case errors.Is(cause, errShutdown):
		final.State, final.Error = job.Interrupted, "the daemon stopped while the job was running"
	case runErr != nil:
		final.State, final.Error = job.Failed, runErr.Error()
	default:
		final.State = job.Failed
		code := res.ExitCode
		final.ExitCode = &code
	}
	rep.System(finishLine(final))

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, j.ID)
	delete(m.busy, j.Name)
	r.cancel(nil)
	if err := m.d.Store.Put(context.Background(), final); err != nil {
		m.log.Error("could not record the end of the job", "job", j.ID, "error", err)
	}
	m.log.Info("job finished", "job", j.ID, "name", j.Name, "state", final.State, "error", final.Error)
	m.publish(Event{Type: EventFinished, Job: final})
	m.dispatch()
}

// run calls the runner, turning a panic into an error so one bad runner cannot take the daemon down.
func (m *Manager) run(ctx context.Context, j job.Job, rep runner.Reporter) (res runner.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the runner panicked: %v", p)
		}
	}()
	rn, err := m.d.Runners.Get(j.Kind)
	if err != nil {
		return runner.Result{}, err
	}
	return rn.Run(ctx, runner.Task{JobID: string(j.ID), Name: j.Name, Kind: j.Kind, Params: j.Params, Input: j.Input, Log: rep})
}

func finishLine(j job.Job) string {
	switch j.State {
	case job.Succeeded:
		return "job finished: succeeded (exit code 0)"
	case job.Canceled:
		return "job finished: canceled"
	case job.Interrupted:
		return "job finished: interrupted (" + j.Error + ")"
	}
	if j.ExitCode != nil {
		return fmt.Sprintf("job finished: failed (exit code %d)", *j.ExitCode)
	}
	return "job finished: failed: " + j.Error
}

// PruneLoop calls Prune every interval until ctx ends. Errors are logged; the loop goes on.
func (m *Manager) PruneLoop(ctx context.Context, interval time.Duration, maxAge time.Duration, keep int) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if n, err := m.Prune(ctx, maxAge, keep); err != nil {
				m.log.Warn("pruning the job history failed", "error", err)
			} else if n > 0 {
				m.log.Info("job history pruned", "removed", n)
			}
		}
	}
}
