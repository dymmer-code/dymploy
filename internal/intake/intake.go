// Package intake turns the events the webhook receiver accepts into jobs for the queue. It is the
// only place that knows both sides: the queue does not know about providers, nor the receiver about
// the queue.
package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

// Submitter is the part of the queue that intake needs.
type Submitter interface {
	Submit(ctx context.Context, r queue.Request) (job.Job, error)
}

// Enqueuer implements webhook.Enqueuer on top of a queue.
type Enqueuer struct {
	scope  func(provider.Event) (*job.Origin, []string)
	sub    Submitter
	kind   string
	params json.RawMessage
	fresh  func(ctx context.Context, ev provider.Event) error
}

// Fresh makes Enqueue check each event with fn before it is queued; an error stops it. Submit, which is
// how a person starts a job by hand, is never checked: that person chose the commit.
func (e *Enqueuer) Fresh(fn func(ctx context.Context, ev provider.Event) error) *Enqueuer {
	e.fresh = fn
	return e
}

// tipTimeout bounds the question to the provider so a slow answer cannot hold a webhook open.
const tipTimeout = 10 * time.Second

// Tip returns a check that an event's commit is still where its branch or tag points. A webhook is signed,
// but a signature proves who sent it, not when: a captured delivery can be sent again, and deliveries can
// arrive out of order, so asking the provider is the only way to know the event is still current.
// enabled says, per provider, whether to check at all; find returns the provider's resolver, if it has one.
func Tip(find func(name string) (provider.Resolver, bool), enabled func(name string) bool) func(ctx context.Context, ev provider.Event) error {
	return func(ctx context.Context, ev provider.Event) error {
		if ev.Ref == "" || !enabled(ev.Provider) {
			return nil
		}
		res, ok := find(ev.Provider)
		if !ok {
			return nil // this provider cannot say where a ref points: there is nothing to compare with
		}
		ctx, cancel := context.WithTimeout(ctx, tipTimeout)
		defer cancel()
		cur, err := res.ResolveRef(ctx, ev.Owner, ev.Repo, ev.Ref)
		switch {
		case errors.Is(err, provider.ErrRefNotFound):
			return fmt.Errorf("%w: %s no longer exists", webhook.ErrStale, ev.Ref)
		case err != nil:
			return fmt.Errorf("%w: %v", webhook.ErrCannotVerify, err)
		case cur.Commit != ev.Commit:
			return fmt.Errorf("%w: %s points at %.12s now", webhook.ErrStale, ev.Ref, cur.Commit)
		}
		return nil
	}
}

// New returns an Enqueuer that submits jobs of the given runner kind. params are the runner's
// configuration, carried opaque to every job.
func New(sub Submitter, kind string, params json.RawMessage) *Enqueuer {
	return &Enqueuer{sub: sub, kind: kind, params: params}
}

// Enqueue implements webhook.Enqueuer. Jobs are named after the repository, so two pushes to the
// same repository never run at the same time. The whole event travels as the job's opaque input.
func (e *Enqueuer) Enqueue(ctx context.Context, ev provider.Event) error {
	if e.fresh != nil {
		if err := e.fresh(ctx, ev); err != nil {
			return err
		}
	}
	var err error
	if e.scope != nil {
		origin, targets := e.scope(ev)
		if origin != nil {
			_, err = e.SubmitScoped(ctx, ev, *origin, targets)
		} else {
			_, err = e.Submit(ctx, ev)
		}
	} else {
		_, err = e.Submit(ctx, ev)
	}
	switch {
	case errors.Is(err, queue.ErrQueueFull):
		return webhook.ErrQueueFull
	case errors.Is(err, queue.ErrRejected):
		return webhook.ErrDropped
	case errors.Is(err, queue.ErrDraining), errors.Is(err, queue.ErrClosed):
		return webhook.ErrShuttingDown
	}
	return err
}

// Submit is Enqueue for callers that want the job back and the queue's own errors.
func (e *Enqueuer) Submit(ctx context.Context, ev provider.Event) (job.Job, error) {
	input, err := json.Marshal(ev)
	if err != nil {
		return job.Job{}, err
	}
	return e.sub.Submit(ctx, queue.Request{
		Name: ev.Provider + "/" + ev.FullName, Kind: e.kind, Params: e.params, Input: input,
	})
}

// SubmitScoped preserves local runner parameters and adds the app's target restriction.
func (e *Enqueuer) SubmitScoped(ctx context.Context, ev provider.Event, origin job.Origin, targets []string) (job.Job, error) {
	origin.Commit = ev.Commit
	if origin.Trigger == "" {
		origin.Trigger = "user"
	}
	if e.kind != "docker" {
		return job.Job{}, errors.New("application jobs require the docker runner")
	}
	params := map[string]any{}
	if len(e.params) > 0 {
		if err := json.Unmarshal(e.params, &params); err != nil {
			return job.Job{}, err
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	if targets == nil {
		targets = []string{}
	}
	params["allowed_targets"] = targets
	raw, err := json.Marshal(params)
	if err != nil {
		return job.Job{}, err
	}
	input, err := json.Marshal(ev)
	if err != nil {
		return job.Job{}, err
	}
	return e.sub.Submit(ctx, queue.Request{Name: ev.Provider + "/" + ev.FullName, Kind: e.kind, Params: raw, Input: input, Origin: &origin})
}

// WithScope adds app provenance to matching webhook jobs. Configure before starting intake.
func (e *Enqueuer) WithScope(fn func(provider.Event) (*job.Origin, []string)) *Enqueuer {
	e.scope = fn
	return e
}
