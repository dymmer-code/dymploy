// Package runnertest gives tests a scriptable Runner, so the queue can be tested without any real
// execution mechanism and with full control of timing, output and failures.
package runnertest

import (
	"context"
	"sync"

	"github.com/dymmer-code/dymploy/internal/runner"
)

// Script decides what one run does.
type Script func(ctx context.Context, t runner.Task) (runner.Result, error)

// Exit returns a script that ends immediately with the given exit code.
func Exit(code int) Script {
	return func(context.Context, runner.Task) (runner.Result, error) { return runner.Result{ExitCode: code}, nil }
}

// Fail returns a script whose run cannot be carried out.
func Fail(err error) Script {
	return func(context.Context, runner.Task) (runner.Result, error) { return runner.Result{}, err }
}

// Fake is a runner driven by a Script. It records every task and how many ran at the same time.
type Fake struct {
	script Script

	mu         sync.Mutex
	calls      []runner.Task
	running    map[string]int // by task name
	perNameMax map[string]int
	total      int
	max        int
	started    chan runner.Task
}

// New returns a fake that runs script for every task.
func New(script Script) *Fake {
	return &Fake{script: script, running: map[string]int{}, perNameMax: map[string]int{}, started: make(chan runner.Task, 1024)}
}

// Run implements runner.Runner.
func (f *Fake) Run(ctx context.Context, t runner.Task) (runner.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, t)
	f.running[t.Name]++
	if f.running[t.Name] > f.perNameMax[t.Name] {
		f.perNameMax[t.Name] = f.running[t.Name]
	}
	f.total++
	if f.total > f.max {
		f.max = f.total
	}
	f.mu.Unlock()
	f.started <- t
	defer func() {
		f.mu.Lock()
		f.running[t.Name]--
		f.total--
		f.mu.Unlock()
	}()
	return f.script(ctx, t)
}

// Started delivers each task as its run begins.
func (f *Fake) Started() <-chan runner.Task { return f.started }

// Calls returns the tasks seen so far, in order.
func (f *Fake) Calls() []runner.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.Task(nil), f.calls...)
}

// MaxConcurrent is the highest number of runs that were active at the same time.
func (f *Fake) MaxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.max
}

// MaxConcurrentFor is how many runs with the same name were ever active at once (should be 1).
func (f *Fake) MaxConcurrentFor(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perNameMax[name]
}

// Gate holds runs until the test opens it.
type Gate struct{ ch chan struct{} }

// NewGate returns a closed gate.
func NewGate() *Gate { return &Gate{ch: make(chan struct{})} }

// Chan is closed once the gate is open.
func (g *Gate) Chan() <-chan struct{} { return g.ch }

// Open lets every waiting and future run through. It is safe to call more than once.
func (g *Gate) Open() {
	select {
	case <-g.ch:
	default:
		close(g.ch)
	}
}

// Hold returns a script that waits for the gate (or for cancellation, which it reports as the
// context's error, as a real runner must) and then ends with code.
func Hold(g *Gate, code int) Script {
	return func(ctx context.Context, _ runner.Task) (runner.Result, error) {
		select {
		case <-g.ch:
			return runner.Result{ExitCode: code}, nil
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
}

// Writes returns a script that writes to the task's log and ends with code.
func Writes(stdout, stderr string, code int) Script {
	return func(_ context.Context, t runner.Task) (runner.Result, error) {
		if stdout != "" {
			_, _ = t.Log.Stdout().Write([]byte(stdout))
		}
		if stderr != "" {
			_, _ = t.Log.Stderr().Write([]byte(stderr))
		}
		return runner.Result{ExitCode: code}, nil
	}
}
