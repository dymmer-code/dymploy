package cmd

import (
	"context"
	"github.com/dymmer-code/dymploy/internal/control"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/runnertest"
)

// holdRig runs a real daemon whose jobs wait for a gate and, when told to stop, take a little while to
// clean up, so that what happens in between can be observed.
type holdRig struct {
	cfg, addr, dir string
	gate           *runnertest.Gate
	done           chan error
	ended          chan struct{}
	cancel         context.CancelFunc
	out            *syncBuffer
}

func startHold(t *testing.T, extra string) *holdRig {
	t.Helper()
	return startHoldSlow(t, extra, 300*time.Millisecond)
}

// startHoldSlow is startHold with a runner that takes cleanup to stop after being asked.
func startHoldSlow(t *testing.T, extra string, cleanup time.Duration) *holdRig {
	t.Helper()
	r := &holdRig{gate: runnertest.NewGate(), dir: tmpDir(t), done: make(chan error, 1), ended: make(chan struct{}), out: &syncBuffer{}}
	extraRunners = []func(*runner.Registry){func(reg *runner.Registry) {
		reg.MustRegister("hold", runnertest.New(func(ctx context.Context, tk runner.Task) (runner.Result, error) {
			tk.Log.System("working")
			select {
			case <-r.gate.Chan():
				return runner.Result{}, nil
			case <-ctx.Done():
				tk.Log.System("cleaning up after the stop")
				time.Sleep(cleanup)
				return runner.Result{}, ctx.Err()
			}
		}))
	}}
	t.Cleanup(func() { extraRunners = nil })
	r.addr = freeAddr(t)
	r.cfg = hookConfig(t, r.dir, r.addr, "runner:\n  kind: hold\n"+extra)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		root := NewRootCmd("v")
		root.SetOut(r.out)
		root.SetErr(r.out)
		root.SetArgs([]string{"serve", "--config", r.cfg})
		r.done <- root.ExecuteContext(ctx)
		close(r.ended)
	}()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(r.dir, "c.sock")); return err == nil })
	t.Cleanup(func() {
		r.gate.Open()
		cancel()
		select {
		case <-r.ended:
		case <-time.After(10 * time.Second):
		}
	})
	return r
}

func (r *holdRig) cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, _, err := run(t, context.Background(), append(args, "--config", r.cfg)...)
	return out, err
}

func (r *holdRig) push(t *testing.T, id, repo string) int {
	t.Helper()
	resp := signedPush(t, r.addr, id, repo, "refs/heads/main", hookSecret)
	defer resp.Body.Close()
	return resp.StatusCode
}

func (r *holdRig) jobs(t *testing.T, state string) []job.Job {
	t.Helper()
	cl, _ := clientFor("", r.cfg)
	var st []job.State
	if state != "" {
		st = []job.State{job.State(state)}
	}
	jobs, err := cl.Jobs(context.Background(), job.Filter{States: st})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func (r *holdRig) waitServeEnds(t *testing.T) {
	t.Helper()
	select {
	case <-r.ended:
		if err := <-r.done; err != nil {
			t.Fatalf("serve must end cleanly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}

// storedJobs reads the state the stopped daemon left behind.
func (r *holdRig) storedJobs(t *testing.T) map[string]job.Job {
	t.Helper()
	st, err := job.OpenBolt(filepath.Join(r.dir, "data", "state", "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := st.List(context.Background(), job.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]job.Job{}
	for _, j := range all {
		m[j.Name+"#"+string(j.State)] = j
	}
	return m
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGracefulStopFinishesTheWorkInHandRefusesNewJobsAndThenLeaves(t *testing.T) {
	r := startHold(t, "queue:\n  max_concurrent: 1\n  max_pending: 10\n  on_busy: queue\n  log_max_bytes: 1000000\n")
	dropIn(t, r.cfg, "50-more.yaml", "policy:\n  - {provider: github, owner: acme, repositories: [other], branches: [main]}\n")
	if _, err := r.cli(t, "reload"); err != nil {
		t.Fatal(err)
	}
	if r.push(t, "g-1", "shop") != 202 || r.push(t, "g-2", "other") != 202 {
		t.Fatal("two jobs: one runs, one waits")
	}
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 && len(r.jobs(t, "queued")) == 1 })

	out, err := r.cli(t, "stop", "--graceful", "--no-wait")
	if err != nil || !strings.Contains(out, "draining") || !strings.Contains(out, "1 running job, 1 waiting job") {
		t.Fatalf("%v %q", err, out)
	}
	// new work is refused, with a message, while the rest is still answered
	resp := signedPush(t, r.addr, "g-3", "shop", "refs/heads/main", hookSecret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(body), "not accepting new jobs") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if code, _ := get(t, "http://"+r.addr+"/healthz"); code != 200 {
		t.Fatalf("the daemon still answers while it drains: %d", code)
	}
	if _, err := r.cli(t, "queue", "github/acme/shop", "--ref", "main"); err == nil {
		t.Fatal("a job started by hand is a new job too")
	}
	if out, _ := r.cli(t, "stop", "--graceful", "--no-wait"); !strings.Contains(out, "already draining") {
		t.Fatalf("asking again is harmless: %q", out)
	}
	if st, _ := r.cli(t, "status"); !strings.Contains(st, "draining") {
		t.Fatalf("status says so: %q", st)
	}
	select {
	case <-r.ended:
		t.Fatal("it must not leave while work remains")
	case <-time.After(400 * time.Millisecond):
	}

	r.gate.Open() // both jobs now run to the end, the waiting one included
	r.waitServeEnds(t)
	stored := r.storedJobs(t)
	if len(stored) != 2 || stored["github/acme/shop#succeeded"].ID == "" || stored["github/acme/other#succeeded"].ID == "" {
		t.Fatalf("a graceful stop loses nothing: %v", keys(stored))
	}
}

func keys(m map[string]job.Job) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStopAbortsRunningJobsWritesTheReasonRefusesEverythingAndKeepsTheWaitingOnes(t *testing.T) {
	r := startHold(t, "queue:\n  max_concurrent: 1\n  max_pending: 10\n  on_busy: queue\n  log_max_bytes: 1000000\n")
	dropIn(t, r.cfg, "50-more.yaml", "policy:\n  - {provider: github, owner: acme, repositories: [other], branches: [main]}\n")
	if _, err := r.cli(t, "reload"); err != nil {
		t.Fatal(err)
	}
	r.push(t, "s-1", "shop")
	r.push(t, "s-2", "other")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 && len(r.jobs(t, "queued")) == 1 })

	out, err := r.cli(t, "stop", "--reason", "maintenance window", "--no-wait")
	if err != nil || !strings.Contains(out, "stopping: aborting 1 running job") || !strings.Contains(out, "1 waiting job stay queued") {
		t.Fatalf("%v %q", err, out)
	}
	// while the job cleans up, everything incoming is refused
	for _, path := range []string{"/healthz", "/hooks/github"} {
		code, body := get(t, "http://"+r.addr+path)
		if code != 503 || !strings.Contains(body, "dymploy is stopping") {
			t.Fatalf("%s: %d %s", path, code, body)
		}
	}
	if code := r.push(t, "s-3", "shop"); code != 503 {
		t.Fatalf("a push while stopping: %d", code)
	}
	if st, _ := r.cli(t, "status"); !strings.Contains(st, "stopping") {
		t.Fatalf("the control socket keeps answering: %q", st)
	}
	r.waitServeEnds(t)

	stored := r.storedJobs(t)
	aborted := stored["github/acme/shop#aborted"]
	if aborted.ID == "" || aborted.StopReason != "maintenance window" || aborted.StopRequestedAt.IsZero() || aborted.FinishedAt.IsZero() {
		t.Fatalf("the running job is aborted, with the reason: %v", keys(stored))
	}
	if q := stored["github/acme/other#queued"]; q.ID == "" {
		t.Fatalf("the waiting job stays queued for the next start: %v", keys(stored))
	}
	story := readJobLog(t, r.dir, string(aborted.ID))
	for _, want := range []string{"stop requested: maintenance window", "cleaning up after the stop", "job finished: aborted (maintenance window)"} {
		if !strings.Contains(story, want) {
			t.Fatalf("the log lacks %q:\n%s", want, story)
		}
	}
}

func readJobLog(t *testing.T, dir, id string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "data", "logs", "*"+id+"*.jsonl"))
	if len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(dir, "data", "logs", "*.jsonl"))
	}
	var all strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all.Write(b)
	}
	return all.String()
}

func TestStopWaitsForTheDaemonToGoAndShowsTheProgress(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "w-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	out, err := r.cli(t, "stop", "--reason", "bye")
	if err != nil || !strings.Contains(out, "stopping: aborting 1 running job") || !strings.Contains(out, "dymploy has stopped") {
		t.Fatalf("%v\n%s", err, out)
	}
	r.waitServeEnds(t)
	if _, err := r.cli(t, "status"); err == nil {
		t.Fatal("the daemon is gone")
	}
	if _, err := r.cli(t, "stop"); err == nil {
		t.Fatal("nothing to stop")
	}
}

func TestStopGracefullyWithATimeoutSaysHowToAbort(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "t-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	_, err := r.cli(t, "stop", "--graceful", "--timeout", "700ms")
	if err == nil || !strings.Contains(err.Error(), "still draining") || !strings.Contains(err.Error(), "dymploy stop") {
		t.Fatalf("%v", err)
	}
	// stopping while it drains turns the drain into an abort
	out, err := r.cli(t, "stop", "--reason", "enough waiting")
	if err != nil || !strings.Contains(out, "stopping: aborting 1 running job") {
		t.Fatalf("%v %q", err, out)
	}
	r.waitServeEnds(t)
	if s := r.storedJobs(t)["github/acme/shop#aborted"]; s.StopReason != "enough waiting" {
		t.Fatalf("%+v", s)
	}
}

func TestSignalsStopAndDrainTheSameWay(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "sig-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r.waitServeEnds(t)
	if s := r.storedJobs(t)["github/acme/shop#aborted"]; !strings.Contains(s.StopReason, "terminated") {
		t.Fatalf("SIGTERM aborts the running job and says why: %+v", s)
	}

	r = startHold(t, "")
	r.push(t, "sig-2", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { st, _ := r.cli(t, "status"); return strings.Contains(st, "draining") })
	r.gate.Open()
	r.waitServeEnds(t)
	if r.storedJobs(t)["github/acme/shop#succeeded"].ID == "" {
		t.Fatal("SIGQUIT lets the job finish")
	}
}

func TestACancelledContextStopsTheDaemonPolitely(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "ctx-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	r.cancel()
	r.waitServeEnds(t)
	if s := r.storedJobs(t)["github/acme/shop#aborted"]; s.ID == "" || s.StopReason == "" {
		t.Fatalf("the job is aborted, not left running: %+v", s)
	}
}

func TestAReloadThatRemovesARepositoryStopsItsJobsProperly(t *testing.T) {
	r := startHold(t, "queue:\n  max_concurrent: 1\n  max_pending: 10\n  on_busy: queue\n  log_max_bytes: 1000000\n")
	dropIn(t, r.cfg, "50-other.yaml", "policy:\n  - {provider: github, owner: acme, repositories: [other], branches: [main]}\n")
	if _, err := r.cli(t, "reload"); err != nil {
		t.Fatal(err)
	}
	r.push(t, "rl-1", "other") // runs
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	r.push(t, "rl-3", "other")   // waits behind the first one of its repository
	r.push(t, "rl-2", "shop") // another repository, also waiting
	waitFor(t, func() bool { return len(r.jobs(t, "queued")) == 2 })

	if err := os.Remove(filepath.Join(r.dir, "config.d", "50-other.yaml")); err != nil {
		t.Fatal(err)
	}
	out, err := r.cli(t, "reload")
	if err != nil || !strings.Contains(out, "stop requested for 2 jobs the new configuration no longer allows") {
		t.Fatalf("the running job and the one waiting behind it both go: %v %q", err, out)
	}
	waitFor(t, func() bool { return len(r.jobs(t, "aborted")) == 2 })
	for _, a := range r.jobs(t, "aborted") {
		if a.Name != "github/acme/other" || !strings.Contains(a.StopReason, "no longer allowed by the configuration after a reload") || a.FinishedAt.IsZero() {
			t.Fatalf("%+v", a)
		}
	}
	for _, q := range r.jobs(t, "queued") { // the other repository's job may still be waiting for its turn
		if q.Name == "github/acme/other" {
			t.Fatalf("nothing of the removed repository stays in the queue: %+v", q)
		}
	}
	// the one the policy still allows carries on
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	if j := r.jobs(t, "running")[0]; j.Name != "github/acme/shop" {
		t.Fatalf("%+v", j)
	}
	_ = sync.Mutex{}
}

func TestEveryDaemonCommandReportsABrokenConfiguration(t *testing.T) {
	p := filepath.Join(tmpDir(t), "broken.yaml")
	os.WriteFile(p, []byte("listen: nope\n"), 0o600)
	for _, args := range [][]string{{"queue"}, {"stop"}, {"abort", "x"}, {"retry", "x"}, {"job", "x"}, {"logs", "x"}, {"watch"}, {"jobs"}} {
		if _, _, err := run(t, context.Background(), append(args, "--config", p)...); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestStopOfADaemonThatCannotBeStoppedThroughTheSocket(t *testing.T) {
	sock := filepath.Join(tmpDir(t), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	go control.Serve(ctx, sock, control.Handlers{Status: func() control.Status { return control.Status{} }}, func() { close(ready) })
	<-ready
	if _, _, err := run(t, context.Background(), "stop", "--socket", sock); err == nil || !strings.Contains(err.Error(), "cannot be stopped") {
		t.Fatalf("%v", err)
	}
}

func TestPressingCtrlCWhileWaitingLeavesTheDaemonDoingWhatWasAsked(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "cc-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(800*time.Millisecond, cancel)
	out, _, err := run(t, ctx, "stop", "--graceful", "--config", r.cfg)
	if err != nil || !strings.Contains(out, "no longer waiting") {
		t.Fatalf("%v %q", err, out)
	}
	if st, _ := r.cli(t, "status"); !strings.Contains(st, "draining") {
		t.Fatalf("it keeps draining: %q", st)
	}
}

func TestStoppingTwiceAndTheOtherWayRound(t *testing.T) {
	r := startHold(t, "")
	r.push(t, "tw-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	if out, _ := r.cli(t, "stop", "--no-wait"); !strings.Contains(out, "stopping:") {
		t.Fatalf("%q", out)
	}
	if out, err := r.cli(t, "stop", "--no-wait"); err == nil && !strings.Contains(out, "already stopping") {
		t.Fatalf("a second stop says it is already under way: %v %q", err, out)
	}
	if _, err := r.cli(t, "stop", "--graceful", "--no-wait"); err != nil && !strings.Contains(err.Error(), "already stopping") && !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("a graceful request while stopping is refused clearly: %v", err)
	}
	r.waitServeEnds(t)
}

func TestJobsThatDoNotStopInTimeAreLeftBehindAfterTheShutdownTimeout(t *testing.T) {
	r := startHoldSlow(t, "shutdown_timeout: 1s\n", 5*time.Second) // takes five seconds to stop, is given one
	r.push(t, "to-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	start := time.Now()
	if _, err := r.cli(t, "stop", "--reason", "now", "--no-wait"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.ended:
		if err := <-r.done; err == nil || !strings.Contains(err.Error(), "waiting for running jobs to stop") {
			t.Fatalf("an unclean stop is an error, so that whoever supervises dymploy notices: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not leave")
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 4500*time.Millisecond {
		t.Fatalf("the daemon waits for shutdown_timeout and then leaves: %s", d)
	}
	if !strings.Contains(r.out.String(), "some jobs did not stop in time") {
		t.Fatalf("and says so:\n%s", r.out.String())
	}
}

func TestASecondSignalLeavesAtOnce(t *testing.T) {
	r := startHold(t, "shutdown_timeout: 30s\n")
	r.push(t, "sg-1", "shop")
	waitFor(t, func() bool { return len(r.jobs(t, "running")) == 1 })
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	r.waitServeEnds(t)
}
