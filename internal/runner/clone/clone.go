// Package clone is a runner that checks out the commit of an event and reports it. It is the first
// step of any build, and on its own it proves the chain from webhook to a real working copy.
//
// It is the one runner that understands what its Input is (a provider.Event) and what its Params are;
// the queue and everything else still treat both as opaque.
package clone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dymmer-code/dymploy/internal/checkout"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// Kind is the name this runner is registered under.
const Kind = "clone"

// Finder returns the provider with the given name if it can clone.
type Finder func(provider string) (provider.Cloner, bool)

// Runner checks out the commit of a push event.
type Runner struct {
	mgr  *checkout.Manager
	find Finder
}

// New returns a clone runner.
func New(mgr *checkout.Manager, find Finder) *Runner { return &Runner{mgr: mgr, find: find} }

// params are what the runner accepts in its configuration. Unknown keys are an error.
type params struct {
	Depth      int  `json:"depth"`
	Submodules bool `json:"submodules"`
}

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, t runner.Task) (runner.Result, error) {
	var p params
	if len(t.Params) > 0 {
		dec := json.NewDecoder(bytes.NewReader(t.Params))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return runner.Result{}, fmt.Errorf("the clone runner's params are invalid: %w", err)
		}
	}
	var ev provider.Event
	if err := json.Unmarshal(t.Input, &ev); err != nil {
		return runner.Result{}, fmt.Errorf("the job input is not a repository event: %w", err)
	}
	if ev.Commit == "" || ev.Owner == "" || ev.Repo == "" || ev.Provider == "" {
		return runner.Result{}, errors.New("the job input lacks provider, owner, repository or commit")
	}
	cloner, ok := r.find(ev.Provider)
	if !ok {
		return runner.Result{}, fmt.Errorf("provider %q cannot clone repositories", ev.Provider)
	}

	t.Log.System(fmt.Sprintf("preparing %s/%s at %.12s", ev.Owner, ev.Repo, ev.Commit))
	creds, err := cloner.CloneCredentials(ctx, &ev)
	if err != nil {
		return runner.Result{}, fmt.Errorf("getting read-only credentials: %w", err)
	}
	co, err := r.mgr.Acquire(ctx, checkout.Request{
		Provider: ev.Provider, Owner: ev.Owner, Repo: ev.Repo,
		URL: cloner.RepositoryURL(ev.Owner, ev.Repo), Commit: ev.Commit, Ref: ev.Ref,
		Depth: p.Depth, Submodules: p.Submodules,
		Auth:   &checkout.Auth{Username: creds.Username, Password: creds.Password},
		Report: t.Log.System,
	})
	if err != nil {
		return runner.Result{}, err
	}
	defer co.Release()
	t.Log.System(fmt.Sprintf("checked out %.12s in %s", co.Commit, co.Dir))
	return runner.Result{}, nil
}
