package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/runnertest"
)

// --- helpers ---

type env struct {
	m     *Manager
	store *job.MemStore
	logs  *job.MemLogs
	fake  *runnertest.Fake
}

var idSeq atomic.Int64

func newEnv(t *testing.T, cfg Config, script runnertest.Script) *env {
	t.Helper()
	e := &env{store: job.NewMemStore(), logs: job.NewMemLogs(), fake: runnertest.New(script)}
	e.m = e.build(t, cfg, e.store, e.logs)
	return e
}

func (e *env) build(t *testing.T, cfg Config, store job.Store, logs job.Logs) *Manager {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register("fake", e.fake); err != nil {
		t.Fatal(err)
	}
	var tick atomic.Int64
	m, err := New(cfg, Deps{
		Store: store, Logs: logs, Runners: reg,
		Now:   func() time.Time { return time.Unix(1_700_000_000+tick.Add(1), 0).UTC() },
		NewID: func(time.Time) job.ID { return job.ID(fmt.Sprintf("j%04d", idSeq.Add(1))) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) start(t *testing.T) {
	t.Helper()
	if err := e.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.m.Shutdown(ctx)
	})
}

func req(name string) Request { return Request{Name: name, Kind: "fake"} }

func (e *env) submit(t *testing.T, name string) job.Job {
	t.Helper()
	j, err := e.m.Submit(context.Background(), req(name))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) waitState(t *testing.T, id job.ID, want job.State) job.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		j, err := e.m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: state %s, wanted %s", id, j.State, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *env) waitStarted(t *testing.T) runner.Task {
	t.Helper()
	select {
	case task := <-e.fake.Started():
		return task
	case <-time.After(3 * time.Second):
		t.Fatal("no job started")
		return runner.Task{}
	}
}

func (e *env) expectNoStart(t *testing.T) {
	t.Helper()
	select {
	case task := <-e.fake.Started():
		t.Fatalf("job %s (%s) must not have started", task.JobID, task.Name)
	case <-time.After(60 * time.Millisecond):
	}
}

func lines(entries []job.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = string(e.Stream) + ":" + e.Text
	}
	return out
}

// --- running a job ---

func TestAJobRunsAndRecordsItsOutput(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Writes("building\n", "warning\n", 0))
	e.start(t)
	j := e.submit(t, "github/a/b")
	if j.State != job.Queued || j.ID == "" || j.CreatedAt.IsZero() {
		t.Fatalf("a submitted job is returned queued: %+v", j)
	}
	done := e.waitState(t, j.ID, job.Succeeded)
	if done.ExitCode == nil || *done.ExitCode != 0 || done.StartedAt.IsZero() || !done.FinishedAt.After(done.StartedAt) {
		t.Fatalf("%+v", done)
	}
	got, _ := e.m.Logs(j.ID, 0, 0)
	text := strings.Join(lines(got), "|")
	for _, want := range []string{"system:job started", "stdout:building", "stderr:warning", "system:job finished: succeeded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the log must tell the whole story, missing %q in:\n%s", want, text)
		}
	}
	for i, en := range got {
		if en.Seq != int64(i+1) || en.Time.IsZero() {
			t.Fatalf("entries are numbered 1..n with a time: %+v", got)
		}
	}
	if !strings.HasPrefix(got[0].Text, "job started") || !strings.HasPrefix(got[len(got)-1].Text, "job finished") {
		t.Fatalf("started first, finished last: %v", lines(got))
	}
}

func TestExitCodeFailureIsNotARunnerError(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(7))
	e.start(t)
	j := e.waitState(t, e.submit(t, "n").ID, job.Failed)
	if j.ExitCode == nil || *j.ExitCode != 7 || j.Error != "" {
		t.Fatalf("a failing exit code is a result, not an error: %+v", j)
	}
}

func TestRunnerErrorIsRecorded(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Fail(errors.New("docker daemon unreachable")))
	e.start(t)
	j := e.waitState(t, e.submit(t, "n").ID, job.Failed)
	if j.ExitCode != nil || !strings.Contains(j.Error, "docker daemon unreachable") {
		t.Fatalf("%+v", j)
	}
	got, _ := e.m.Logs(j.ID, 0, 0)
	if !strings.Contains(strings.Join(lines(got), "|"), "docker daemon unreachable") {
		t.Fatalf("the log must say why: %v", lines(got))
	}
}

func TestAPanickingRunnerDoesNotTakeTheDaemonDown(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, func(context.Context, runner.Task) (runner.Result, error) { panic("kaboom") })
	e.start(t)
	j := e.waitState(t, e.submit(t, "n").ID, job.Failed)
	if !strings.Contains(j.Error, "panic") || !strings.Contains(j.Error, "kaboom") {
		t.Fatalf("%+v", j)
	}
}

func TestUnknownKindIsRefusedAtSubmit(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	_, err := e.m.Submit(context.Background(), Request{Name: "n", Kind: "podman"})
	if !errors.Is(err, runner.ErrUnknownKind) {
		t.Fatalf("a job that can never run must not be accepted: %v", err)
	}
	if l, _ := e.m.List(context.Background(), job.Filter{}); len(l) != 0 {
		t.Fatal("and nothing is stored")
	}
}

func TestSubmitValidatesTheRequest(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	for _, r := range []Request{{Kind: "fake"}, {Name: "n"}} {
		if _, err := e.m.Submit(context.Background(), r); err == nil {
			t.Fatalf("%+v must be refused", r)
		}
	}
}

func TestOpaqueDataReachesTheRunnerUntouched(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	params := json.RawMessage(`{"image":"debian:13","anything":[1,{"x":null}]}`)
	input := json.RawMessage(`{"sha":"abc","note":"ñ"}`)
	j, err := e.m.Submit(context.Background(), Request{Name: "n", Kind: "fake", Params: params, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	task := e.waitStarted(t)
	if task.JobID != string(j.ID) || task.Name != "n" || task.Kind != "fake" || string(task.Params) != string(params) || string(task.Input) != string(input) {
		t.Fatalf("the queue must carry Params and Input as they came: %+v", task)
	}
	stored, _ := e.m.Get(context.Background(), j.ID)
	if string(stored.Params) != string(params) || string(stored.Input) != string(input) {
		t.Fatalf("and keep them: %+v", stored)
	}
}

// --- limits ---

func TestMaxConcurrentIsHonoured(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 2}, runnertest.Hold(gate, 0))
	e.start(t)
	var ids []job.ID
	for i := 0; i < 6; i++ {
		ids = append(ids, e.submit(t, fmt.Sprintf("job-%d", i)).ID)
	}
	e.waitStarted(t)
	e.waitStarted(t)
	e.expectNoStart(t)
	if q, _ := e.m.List(context.Background(), job.Filter{States: []job.State{job.Queued}}); len(q) != 4 {
		t.Fatalf("4 must be waiting, %d are", len(q))
	}
	gate.Open()
	for _, id := range ids {
		e.waitState(t, id, job.Succeeded)
	}
	if e.fake.MaxConcurrent() != 2 {
		t.Fatalf("never more than 2 at once, saw %d", e.fake.MaxConcurrent())
	}
}

func TestOneJobPerNameAndFIFOWithinAName(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 4}, runnertest.Hold(gate, 0))
	e.start(t)
	a1 := e.submit(t, "repo-a")
	a2 := e.submit(t, "repo-a")
	a3 := e.submit(t, "repo-a")
	e.waitStarted(t)
	e.expectNoStart(t) // 3 slots free, but the same name must wait
	gate.Open()
	for _, j := range []job.Job{a1, a2, a3} {
		e.waitState(t, j.ID, job.Succeeded)
	}
	if e.fake.MaxConcurrentFor("repo-a") != 1 {
		t.Fatal("two jobs of the same name ran at once")
	}
	calls := e.fake.Calls()
	if calls[0].JobID != string(a1.ID) || calls[1].JobID != string(a2.ID) || calls[2].JobID != string(a3.ID) {
		t.Fatalf("FIFO within a name: %v %v %v", calls[0].JobID, calls[1].JobID, calls[2].JobID)
	}
}

func TestABlockedNameDoesNotBlockOthers(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 2}, func(ctx context.Context, t runner.Task) (runner.Result, error) {
		if t.Name == "slow" {
			return runnertest.Hold(gate, 0)(ctx, t)
		}
		return runner.Result{}, nil
	})
	e.start(t)
	e.submit(t, "slow")
	e.waitStarted(t)
	e.submit(t, "slow")          // waits behind the first: head of the queue, but blocked
	other := e.submit(t, "fast") // must not wait for it
	e.waitState(t, other.ID, job.Succeeded)
	gate.Open()
}

func TestQueueFull(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1, MaxPending: 2}, runnertest.Hold(gate, 0))
	e.start(t)
	e.submit(t, "running")
	e.waitStarted(t)
	e.submit(t, "w1")
	e.submit(t, "w2")
	_, err := e.m.Submit(context.Background(), req("w3"))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("got %v", err)
	}
	gate.Open()
}

// --- what to do with a new job whose name is busy ---

func TestCoalesceReplacesTheWaitingJob(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1, OnBusy: Coalesce}, runnertest.Hold(gate, 0))
	e.start(t)
	running := e.submit(t, "repo")
	e.waitStarted(t)
	old := e.submit(t, "repo")
	newest := e.submit(t, "repo")
	sup := e.waitState(t, old.ID, job.Aborted)
	if !strings.Contains(sup.StopReason, string(newest.ID)) || !strings.Contains(sup.StopReason, "superseded") {
		t.Fatalf("a replaced job says by whom: %+v", sup)
	}
	gate.Open()
	e.waitState(t, running.ID, job.Succeeded) // the running one is never touched
	e.waitState(t, newest.ID, job.Succeeded)
	if n := len(e.fake.Calls()); n != 2 {
		t.Fatalf("the replaced job must never run, %d ran", n)
	}
}

func TestRejectRefusesWhileTheNameIsBusy(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 2, OnBusy: Reject}, runnertest.Hold(gate, 0))
	e.start(t)
	e.submit(t, "repo")
	e.waitStarted(t)
	if _, err := e.m.Submit(context.Background(), req("repo")); !errors.Is(err, ErrRejected) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.m.Submit(context.Background(), req("other")); err != nil {
		t.Fatalf("other names are not affected: %v", err)
	}
	gate.Open()
}

func TestUnknownPolicyIsRefused(t *testing.T) {
	_, err := New(Config{MaxConcurrent: 1, OnBusy: "sometimes"}, Deps{Store: job.NewMemStore(), Logs: job.NewMemLogs(), Runners: runner.NewRegistry()})
	if err == nil {
		t.Fatal("a typo in the policy must not silently become the default")
	}
}

// --- control ---

func TestAbortQueuedAndRunning(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e.start(t)
	running := e.submit(t, "a")
	e.waitStarted(t)
	queued := e.submit(t, "b")

	if err := e.m.Abort(context.Background(), queued.ID, "not needed any more"); err != nil {
		t.Fatal(err)
	}
	q := e.waitState(t, queued.ID, job.Aborted)
	if q.StopReason != "not needed any more" || q.StopRequestedAt.IsZero() || q.FinishedAt.IsZero() {
		t.Fatalf("a job that never ran records why it was stopped: %+v", q)
	}

	if err := e.m.Abort(context.Background(), running.ID, "the deploy target is being replaced"); err != nil {
		t.Fatal(err)
	}
	j := e.waitState(t, running.ID, job.Aborted)
	if j.FinishedAt.IsZero() || j.StopReason != "the deploy target is being replaced" || j.StopRequestedAt.IsZero() {
		t.Fatalf("an aborted job has its end time and the reason: %+v", j)
	}
	got, _ := e.m.Logs(running.ID, 0, 0)
	story := strings.Join(lines(got), "|")
	for _, want := range []string{"stop requested: the deploy target is being replaced", "job finished: aborted (the deploy target is being replaced)"} {
		if !strings.Contains(story, want) {
			t.Fatalf("the log tells it: want %q in %s", want, story)
		}
	}
	if len(e.fake.Calls()) != 1 {
		t.Fatal("the queued job that was aborted must never run")
	}
}

func TestAStopIsARequestThatTheRunnerFulfilsAndTheJobSaysSoWhileItStops(t *testing.T) {
	// a runner that needs a moment to clean up after being told to stop
	released := make(chan struct{})
	cleaning := make(chan struct{})
	e := newEnv(t, Config{MaxConcurrent: 1}, func(ctx context.Context, tk runner.Task) (runner.Result, error) {
		<-ctx.Done()
		close(cleaning)
		<-released // stopping its processes takes time
		return runner.Result{}, ctx.Err()
	})
	e.start(t)
	events, cancel := e.m.Subscribe()
	defer cancel()
	j := e.submit(t, "a")
	e.waitStarted(t)

	if err := e.m.Abort(context.Background(), j.ID, "please stop"); err != nil {
		t.Fatal(err)
	}
	<-cleaning
	mid, _ := e.m.Get(context.Background(), j.ID)
	if mid.State != job.Running || mid.StopReason != "please stop" || mid.StopRequestedAt.IsZero() {
		t.Fatalf("while the runner is stopping the job is still running, with the request recorded: %+v", mid)
	}
	if err := e.m.Abort(context.Background(), j.ID, "a second opinion"); err != nil {
		t.Fatalf("asking twice is harmless: %v", err)
	}
	close(released)
	done := e.waitState(t, j.ID, job.Aborted)
	if done.StopReason != "please stop" {
		t.Fatalf("the first reason stays: %+v", done)
	}
	var seen []EventType
	for len(seen) < 4 {
		select {
		case ev := <-events:
			if ev.Type != EventOutput {
				seen = append(seen, ev.Type)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("events: %v", seen)
		}
	}
	if seen[0] != EventQueued || seen[1] != EventStarted || seen[2] != EventStopping || seen[3] != EventFinished {
		t.Fatalf("watchers see the stop request before the end: %v", seen)
	}
}

func TestAnAbortedJobWhoseRunnerReportsSomethingElseIsStillAborted(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, func(ctx context.Context, tk runner.Task) (runner.Result, error) {
		<-ctx.Done()
		return runner.Result{ExitCode: 137}, nil // killed: the work reports an exit code, not an error
	})
	_ = gate
	e.start(t)
	j := e.submit(t, "a")
	e.waitStarted(t)
	e.m.Abort(context.Background(), j.ID, "stop")
	got := e.waitState(t, j.ID, job.Aborted)
	if got.ExitCode == nil || *got.ExitCode != 137 {
		t.Fatalf("the exit code of the stopped work is kept: %+v", got)
	}
}

func TestAbortErrors(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
	if err := e.m.Abort(context.Background(), j.ID, "x"); !errors.Is(err, ErrFinished) {
		t.Fatalf("got %v", err)
	}
	if err := e.m.Abort(context.Background(), "ghost", "x"); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestRetryMakesANewJobFromAFinishedOne(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(1))
	e.start(t)
	params, input := json.RawMessage(`{"p":1}`), json.RawMessage(`{"i":2}`)
	j, _ := e.m.Submit(context.Background(), Request{Name: "n", Kind: "fake", Params: params, Input: input})
	e.waitState(t, j.ID, job.Failed)
	again, err := e.m.Retry(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == j.ID || again.RetryOf != j.ID || again.Name != "n" || string(again.Params) != string(params) || string(again.Input) != string(input) {
		t.Fatalf("%+v", again)
	}
	e.waitState(t, again.ID, job.Failed)
}

func TestRetryOfAJobThatIsNotFinished(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e.start(t)
	j := e.submit(t, "a")
	e.waitStarted(t)
	if _, err := e.m.Retry(context.Background(), j.ID); err == nil {
		t.Fatal("a job still running cannot be retried")
	}
	if _, err := e.m.Retry(context.Background(), "ghost"); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	gate.Open()
}

// --- restarts ---

func TestRecoveryAfterARestart(t *testing.T) {
	store, logs := job.NewMemStore(), job.NewMemLogs()
	ctx := context.Background()
	_ = store.Put(ctx, job.Job{ID: "old-running", Name: "a", Kind: "fake", State: job.Running, CreatedAt: time.Unix(100, 0)})
	_ = store.Put(ctx, job.Job{ID: "old-queued-1", Name: "b", Kind: "fake", State: job.Queued, CreatedAt: time.Unix(200, 0)})
	_ = store.Put(ctx, job.Job{ID: "old-queued-2", Name: "b", Kind: "fake", State: job.Queued, CreatedAt: time.Unix(300, 0)})
	e := &env{store: store, logs: logs, fake: runnertest.New(runnertest.Exit(0))}
	e.m = e.build(t, Config{MaxConcurrent: 1}, store, logs)
	e.start(t)

	intr, _ := e.m.Get(ctx, "old-running")
	if intr.State != job.Interrupted || !strings.Contains(intr.Error, "stopped") || intr.FinishedAt.IsZero() {
		t.Fatalf("a job that was running when the daemon died is interrupted, not retried blindly: %+v", intr)
	}
	e.waitState(t, "old-queued-1", job.Succeeded)
	e.waitState(t, "old-queued-2", job.Succeeded)
	calls := e.fake.Calls()
	if len(calls) != 2 || calls[0].JobID != "old-queued-1" || calls[1].JobID != "old-queued-2" {
		t.Fatalf("queued jobs resume in their original order, and the interrupted one does not run: %v", calls)
	}
}

func TestShutdownInterruptsRunningJobsAndStopsAccepting(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	if err := e.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	j := e.submit(t, "a")
	e.waitStarted(t)
	queued := e.submit(t, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Get(context.Background(), j.ID)
	if got.State != job.Aborted || got.StopReason != "dymploy was stopped" {
		t.Fatalf("a shutdown aborts what is running, with the reason: %+v", got)
	}
	q, _ := e.m.Get(context.Background(), queued.ID)
	if q.State != job.Queued {
		t.Fatalf("what was waiting stays queued for the next start: %+v", q)
	}
	if _, err := e.m.Submit(context.Background(), req("c")); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	if err := e.m.Shutdown(ctx); err != nil {
		t.Fatalf("a second Shutdown is harmless: %v", err)
	}
}

func TestSubmitBeforeStartIsRefused(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	if _, err := e.m.Submit(context.Background(), req("a")); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

// --- failures of the infrastructure under the queue ---

type failingStore struct {
	job.Store
	fail atomic.Bool
}

func (f *failingStore) Put(ctx context.Context, j job.Job) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.Store.Put(ctx, j)
}

func TestSubmitFailsCleanlyWhenTheStoreFails(t *testing.T) {
	fs := &failingStore{Store: job.NewMemStore()}
	e := &env{logs: job.NewMemLogs(), fake: runnertest.New(runnertest.Exit(0))}
	e.m = e.build(t, Config{MaxConcurrent: 1}, fs, e.logs)
	e.start(t)
	fs.fail.Store(true)
	if _, err := e.m.Submit(context.Background(), req("a")); err == nil {
		t.Fatal("a job that could not be stored must not be accepted")
	}
	fs.fail.Store(false)
	e.expectNoStart(t)
	j, err := e.m.Submit(context.Background(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(t, j.ID, job.Succeeded)
}

type failingLogs struct{ job.Logs }

func (failingLogs) Append(job.ID, job.Entry) error { return errors.New("log disk gone") }

func TestALogFailureDoesNotKillTheJob(t *testing.T) {
	e := &env{store: job.NewMemStore(), fake: runnertest.New(runnertest.Writes("x\n", "", 0))}
	e.m = e.build(t, Config{MaxConcurrent: 1}, e.store, failingLogs{job.NewMemLogs()})
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
}

// --- watching ---

func collectEvents(t *testing.T, m *Manager, until func(Event) bool) []Event {
	t.Helper()
	ch, cancel := m.Subscribe()
	defer cancel()
	var out []Event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
			if until(ev) {
				return out
			}
		case <-deadline:
			t.Fatalf("events so far: %v", out)
		}
	}
}

func TestSubscribersSeeTheWholeLifeOfAJob(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Writes("hello\n", "", 0))
	e.start(t)
	ch, cancel := e.m.Subscribe()
	defer cancel()
	j := e.submit(t, "a")
	var kinds []string
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev := <-ch:
			if ev.Job.ID != j.ID {
				continue
			}
			kinds = append(kinds, string(ev.Type))
			if ev.Type == EventFinished {
				break loop
			}
		case <-deadline:
			t.Fatalf("so far: %v", kinds)
		}
	}
	got := strings.Join(kinds, ",")
	if !strings.HasPrefix(got, "queued,started,output") || !strings.HasSuffix(got, "output,finished") {
		t.Fatalf("queued, started, the output lines, finished: %s", got)
	}
}

func TestOutputEventsCarryTheEntry(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Writes("hello\n", "", 0))
	e.start(t)
	var seen []string
	var mu sync.Mutex
	ch, cancel := e.m.Subscribe()
	defer cancel()
	e.submit(t, "a")
	deadline := time.After(3 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-ch:
			if ev.Type == EventOutput {
				mu.Lock()
				seen = append(seen, ev.Entry.Text)
				mu.Unlock()
			}
			done = ev.Type == EventFinished
		case <-deadline:
			t.Fatal("timeout")
		}
	}
	if !strings.Contains(strings.Join(seen, "|"), "hello") {
		t.Fatalf("%v", seen)
	}
}

func TestASlowSubscriberNeverBlocksTheQueue(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 2}, runnertest.Writes(strings.Repeat("line\n", 600), "", 0))
	e.start(t)
	_, cancel := e.m.Subscribe() // never read
	defer cancel()
	var ids []job.ID
	for i := 0; i < 5; i++ {
		ids = append(ids, e.submit(t, fmt.Sprintf("n%d", i)).ID)
	}
	for _, id := range ids {
		e.waitState(t, id, job.Succeeded)
	}
}

func TestCancelledSubscriptionStopsReceiving(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	ch, cancel := e.m.Subscribe()
	cancel()
	cancel() // idempotent
	e.waitState(t, e.submit(t, "a").ID, job.Succeeded)
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("no events after cancel")
		}
	default:
	}
}

// --- housekeeping ---

func TestPruneRemovesOldFinishedJobsAndTheirLogs(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Writes("x\n", "", 0))
	e.start(t)
	var ids []job.ID
	for i := 0; i < 4; i++ {
		j := e.submit(t, fmt.Sprintf("n%d", i))
		e.waitState(t, j.ID, job.Succeeded)
		ids = append(ids, j.ID)
	}
	removed, err := e.m.Prune(context.Background(), 0, 2) // keep the 2 newest finished jobs
	if err != nil || removed != 2 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	for _, id := range ids[:2] {
		if _, err := e.m.Get(context.Background(), id); !errors.Is(err, job.ErrNotFound) {
			t.Fatalf("%s should be gone: %v", id, err)
		}
		if l, _ := e.m.Logs(id, 0, 0); len(l) != 0 {
			t.Fatalf("%s: its log must go too", id)
		}
	}
	for _, id := range ids[2:] {
		if _, err := e.m.Get(context.Background(), id); err != nil {
			t.Fatalf("%s must stay: %v", id, err)
		}
	}
}

func TestPruneByAgeNeverTouchesUnfinishedJobs(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e.start(t)
	running := e.submit(t, "a")
	e.waitStarted(t)
	queued := e.submit(t, "b")
	removed, err := e.m.Prune(context.Background(), time.Nanosecond, 0)
	if err != nil || removed != 0 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	for _, id := range []job.ID{running.ID, queued.ID} {
		if _, err := e.m.Get(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	gate.Open()
}

// --- concurrency hygiene (run with -race) ---

func TestManyConcurrentSubmits(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 3, MaxPending: 1000}, runnertest.Exit(0))
	e.start(t)
	var wg sync.WaitGroup
	ids := make(chan job.ID, 200)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				j, err := e.m.Submit(context.Background(), req(fmt.Sprintf("n%d", (g+i)%5)))
				if err != nil {
					t.Error(err)
					return
				}
				ids <- j.ID
			}
		}(g)
	}
	wg.Wait()
	close(ids)
	for id := range ids {
		e.waitState(t, id, job.Succeeded)
	}
	if e.fake.MaxConcurrent() > 3 {
		t.Fatalf("saw %d at once", e.fake.MaxConcurrent())
	}
	for i := 0; i < 5; i++ {
		if e.fake.MaxConcurrentFor(fmt.Sprintf("n%d", i)) > 1 {
			t.Fatal("same name ran twice at once")
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []Config{{MaxConcurrent: 0}, {MaxConcurrent: -1}, {MaxConcurrent: 1, MaxPending: -1}} {
		if _, err := New(c, Deps{Store: job.NewMemStore(), Logs: job.NewMemLogs(), Runners: runner.NewRegistry()}); err == nil {
			t.Errorf("%+v must be refused", c)
		}
	}
	if _, err := New(Config{MaxConcurrent: 1}, Deps{}); err == nil {
		t.Error("missing dependencies must be refused")
	}
}

func TestPruneLoopRunsUntilCancelled(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	var ids []job.ID
	for i := 0; i < 4; i++ {
		j := e.submit(t, fmt.Sprintf("n%d", i))
		e.waitState(t, j.ID, job.Succeeded)
		ids = append(ids, j.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.m.PruneLoop(ctx, 5*time.Millisecond, 0, 1); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		l, _ := e.m.List(context.Background(), job.Filter{})
		if len(l) == 1 && l[0].ID == ids[3] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pruning did not happen: %d jobs", len(l))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop must stop when its context ends")
	}
}

// --- draining and halting ---

func TestDrainRefusesNewJobsAndWaitsForAllTheWorkInHand(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e.start(t)
	a := e.submit(t, "a")
	e.waitStarted(t)
	b := e.submit(t, "b") // waiting
	e.m.Drain()

	if st := e.m.Stats(); !st.Draining || st.Running != 1 || st.Queued != 1 || !st.Open {
		t.Fatalf("%+v", st)
	}
	if _, err := e.m.Submit(context.Background(), req("c")); !errors.Is(err, ErrDraining) {
		t.Fatalf("new jobs are refused with a message of their own: %v", err)
	}
	if _, err := e.m.Retry(context.Background(), a.ID); err == nil {
		t.Fatal("a retry is a new job too")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := e.m.WaitIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not idle while work remains: %v", err)
	}

	gate.Open()
	idle, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := e.m.WaitIdle(idle); err != nil {
		t.Fatalf("idle once everything, queued work included, has run: %v", err)
	}
	e.waitState(t, a.ID, job.Succeeded)
	e.waitState(t, b.ID, job.Succeeded)
	if st := e.m.Stats(); st.Running != 0 || st.Queued != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestHaltAbortsWhatRunsKeepsWhatWaitsAndAcceptsNothing(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 2}, runnertest.Hold(gate, 0))
	e.start(t)
	r1, r2 := e.submit(t, "a"), e.submit(t, "b")
	e.waitStarted(t)
	e.waitStarted(t)
	waiting := e.submit(t, "c")

	e.m.Halt("dymploy is stopping")
	if _, err := e.m.Submit(context.Background(), req("d")); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.m.WaitStopped(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []job.ID{r1.ID, r2.ID} {
		got, _ := e.m.Get(context.Background(), id)
		if got.State != job.Aborted || got.StopReason != "dymploy is stopping" {
			t.Fatalf("%+v", got)
		}
	}
	w, _ := e.m.Get(context.Background(), waiting.ID)
	if w.State != job.Queued {
		t.Fatalf("what was waiting stays queued for the next start, and does not start now: %+v", w)
	}
	if len(e.fake.Calls()) != 2 {
		t.Fatal("no waiting job may start once the queue is halted")
	}
}

func TestWaitersHonourTheirContextAndAnEmptyQueueIsIdleAtOnce(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.m.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.m.WaitStopped(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	gate := runnertest.NewGate()
	e2 := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e2.start(t)
	e2.submit(t, "a")
	e2.waitStarted(t)
	ccancel()
	if err := e2.m.WaitStopped(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	gate.Open()
}

func TestASlowStopIsComplainedAboutInTheDaemonLog(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	stuck := make(chan struct{})
	e := &env{store: job.NewMemStore(), logs: job.NewMemLogs(), fake: runnertest.New(func(ctx context.Context, _ runner.Task) (runner.Result, error) {
		<-ctx.Done()
		<-stuck
		return runner.Result{}, ctx.Err()
	})}
	reg := runner.NewRegistry()
	reg.MustRegister("fake", e.fake)
	m, err := New(Config{MaxConcurrent: 1, StopGrace: 50 * time.Millisecond}, Deps{
		Store: e.store, Logs: e.logs, Runners: reg,
		Log: slog.New(slog.NewTextHandler(writerFunc(func(p []byte) { mu.Lock(); logs.Write(p); mu.Unlock() }), nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.start(t)
	j := e.submit(t, "a")
	e.waitStarted(t)
	m.Abort(context.Background(), j.ID, "stop")
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := logs.String()
		mu.Unlock()
		if strings.Contains(got, "taking longer than expected to stop") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no complaint:\n%s", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stuck)
	e.waitState(t, j.ID, job.Aborted)
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func TestEveryRunOfANameIsNumbered(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 2}, runnertest.Exit(0))
	e.start(t)
	a1, b1, a2 := e.submit(t, "github/acme/a"), e.submit(t, "github/acme/b"), e.submit(t, "github/acme/a")
	if a1.Number != 1 || b1.Number != 1 || a2.Number != 2 {
		t.Fatalf("each repository counts its own runs: %d %d %d", a1.Number, b1.Number, a2.Number)
	}
	done := e.waitState(t, a1.ID, job.Succeeded)
	retry, err := e.m.Retry(context.Background(), done.ID)
	if err != nil || retry.Number != 3 || retry.RetryOf != done.ID {
		t.Fatalf("a retry is a new run with the next number: %v %+v", err, retry)
	}
	e.waitState(t, retry.ID, job.Succeeded)
	got, _ := e.m.List(context.Background(), job.Filter{Name: "github/acme/a", Number: 2})
	if len(got) != 1 || got[0].ID != a2.ID {
		t.Fatalf("a run is found by its number: %+v", got)
	}
}

// --- observer ---

type observed struct {
	mu  sync.Mutex
	got []string
}

func (o *observed) add(ev Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, string(ev.Type)+":"+string(ev.Job.State))
}

func (o *observed) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.got...)
}

func TestAnObserverSeesTheLifeOfAJobInOrderAndNeverItsOutput(t *testing.T) {
	var o observed
	e := &env{store: job.NewMemStore(), logs: job.NewMemLogs(), fake: runnertest.New(runnertest.Writes("hello\n", "", 0))}
	reg := runner.NewRegistry()
	reg.MustRegister("fake", e.fake)
	m, err := New(Config{MaxConcurrent: 1}, Deps{Store: e.store, Logs: e.logs, Runners: reg, Observer: o.add,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.list(), " "); got != "queued:queued started:running finished:succeeded" {
		t.Fatalf("%s", got)
	}
}

func TestShutdownLetsTheObserverSeeTheEndOfAbortedJobs(t *testing.T) {
	var o observed
	slow := make(chan struct{})
	gate := runnertest.NewGate()
	e := &env{store: job.NewMemStore(), logs: job.NewMemLogs(), fake: runnertest.New(runnertest.Hold(gate, 0))}
	reg := runner.NewRegistry()
	reg.MustRegister("fake", e.fake)
	m, err := New(Config{MaxConcurrent: 1}, Deps{Store: e.store, Logs: e.logs, Runners: reg,
		Observer: func(ev Event) {
			if ev.Type == EventFinished {
				<-slow // reporting the end takes a while
			}
			o.add(ev)
		}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.start(t)
	j := e.submit(t, "a")
	e.waitStarted(t)
	go func() { time.Sleep(200 * time.Millisecond); close(slow) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(o.list(), " ")
	if !strings.HasSuffix(got, "finished:aborted") || !strings.Contains(got, string(j.ID)[:0]) {
		t.Fatalf("Shutdown waits for the observer, so the aborted job's end is reported: %s", got)
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("a second Shutdown is harmless: %v", err)
	}
}

func TestAnObserverThatNeverCatchesUpDoesNotBlockShutdownForeverAndOverflowIsDropped(t *testing.T) {
	old := observerBuffer
	observerBuffer = 6
	defer func() { observerBuffer = old }()
	block := make(chan struct{})
	e := &env{store: job.NewMemStore(), logs: job.NewMemLogs(), fake: runnertest.New(runnertest.Exit(0))}
	reg := runner.NewRegistry()
	reg.MustRegister("fake", e.fake)
	var logs strings.Builder
	var mu sync.Mutex
	m, err := New(Config{MaxConcurrent: 1, MaxPending: 10000}, Deps{Store: e.store, Logs: e.logs, Runners: reg,
		Observer: func(Event) { <-block },
		Log:      slog.New(slog.NewTextHandler(writerFunc(func(p []byte) { mu.Lock(); logs.Write(p); mu.Unlock() }), nil))})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.start(t)
	for i := 0; i < 10; i++ { // more events than the buffer holds
		if _, err := m.Submit(context.Background(), req(fmt.Sprintf("n%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		dropped := strings.Contains(logs.String(), "too far behind")
		mu.Unlock()
		if dropped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("overflow must be dropped and logged, not block the queue")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); err == nil {
		t.Fatal("an observer that does not catch up is reported")
	}
	close(block)
}

// stateAtFinish remembers what the store says about a job at the moment its closing line reaches the log.
type stateAtFinish struct {
	job.Logs
	store job.Store
	seen  chan job.State
}

func (s *stateAtFinish) Append(id job.ID, e job.Entry) error {
	if strings.HasPrefix(e.Text, "job finished:") {
		j, err := s.store.Get(context.Background(), id)
		if err == nil {
			s.seen <- j.State
		}
	}
	return s.Logs.Append(id, e)
}

// Whoever waits for the closing line of a job's log and then asks for its state must not find it still running.
func TestTheStateIsFinalWhenTheClosingLineOfTheLogIsWritten(t *testing.T) {
	store := job.NewMemStore()
	logs := &stateAtFinish{Logs: job.NewMemLogs(), store: store, seen: make(chan job.State, 1)}
	e := &env{store: store, fake: runnertest.New(runnertest.Exit(0))}
	e.m = e.build(t, Config{MaxConcurrent: 1}, store, logs)
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
	select {
	case got := <-logs.seen:
		if got != job.Succeeded {
			t.Fatalf("the closing line was written while the store said %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the closing line was never written")
	}
}

// abortAtFinish asks the queue to abort a job at the moment its closing line reaches the log.
type abortAtFinish struct {
	job.Logs
	m      **Manager
	result chan error
}

func (a *abortAtFinish) Append(id job.ID, e job.Entry) error {
	if strings.HasPrefix(e.Text, "job finished:") {
		a.result <- (*a.m).Abort(context.Background(), id, "too late")
	}
	return a.Logs.Append(id, e)
}

// A stop request that arrives once the final state is recorded finds a finished job and changes nothing.
func TestAStopRequestAfterTheFinalStateIsRecordedChangesNothing(t *testing.T) {
	store := job.NewMemStore()
	e := &env{store: store, fake: runnertest.New(runnertest.Exit(0))}
	logs := &abortAtFinish{Logs: job.NewMemLogs(), m: &e.m, result: make(chan error, 1)}
	e.m = e.build(t, Config{MaxConcurrent: 1}, store, logs)
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
	select {
	case err := <-logs.result:
		if !errors.Is(err, ErrFinished) {
			t.Fatalf("Abort answered %v, want ErrFinished", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the closing line was never written")
	}
	got, err := store.Get(context.Background(), j.ID)
	if err != nil || got.State != job.Succeeded || got.StopReason != "" {
		t.Fatalf("the late stop request changed the job: %+v %v", got, err)
	}
}
