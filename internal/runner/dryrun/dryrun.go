// Package dryrun is a runner that executes nothing: it narrates what it was asked to do and succeeds.
// It is the default while no real runner is configured, and it lets the whole chain (webhook, policy,
// queue, log, history) be checked end to end without side effects.
package dryrun

import (
	"context"
	"fmt"

	"github.com/dymmer-code/dymploy/internal/runner"
)

// Kind is the name this runner is registered under.
const Kind = "dryrun"

// Runner is the dry-run runner.
type Runner struct{}

// New returns a dry-run runner.
func New() *Runner { return &Runner{} }

// Run implements runner.Runner.
func (*Runner) Run(ctx context.Context, t runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, err
	}
	t.Log.System(fmt.Sprintf("dry run of %q", t.Name))
	t.Log.System(fmt.Sprintf("input: %s", t.Input))
	t.Log.System(fmt.Sprintf("params: %s", t.Params))
	t.Log.System("nothing was executed")
	return runner.Result{}, nil
}
