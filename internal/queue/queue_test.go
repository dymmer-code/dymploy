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
	sup := e.waitState(t, old.ID, job.Canceled)
	if !strings.Contains(sup.Error, string(newest.ID)) || !strings.Contains(sup.Error, "superseded") {
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

func TestCancelQueuedAndRunning(t *testing.T) {
	gate := runnertest.NewGate()
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Hold(gate, 0))
	e.start(t)
	running := e.submit(t, "a")
	e.waitStarted(t)
	queued := e.submit(t, "b")

	if err := e.m.Cancel(context.Background(), queued.ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(t, queued.ID, job.Canceled)

	if err := e.m.Cancel(context.Background(), running.ID); err != nil {
		t.Fatal(err)
	}
	j := e.waitState(t, running.ID, job.Canceled)
	if j.FinishedAt.IsZero() {
		t.Fatal("a cancelled job has an end time")
	}
	got, _ := e.m.Logs(running.ID, 0, 0)
	if !strings.Contains(strings.Join(lines(got), "|"), "canceled") {
		t.Fatalf("%v", lines(got))
	}
	if len(e.fake.Calls()) != 1 {
		t.Fatal("the queued job that was cancelled must never run")
	}
}

func TestCancelErrors(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	j := e.submit(t, "a")
	e.waitState(t, j.ID, job.Succeeded)
	if err := e.m.Cancel(context.Background(), j.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("got %v", err)
	}
	if err := e.m.Cancel(context.Background(), "ghost"); !errors.Is(err, job.ErrNotFound) {
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
	if got.State != job.Interrupted {
		t.Fatalf("a shutdown interrupts what is running: %+v", got)
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
