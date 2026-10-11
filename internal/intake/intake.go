// Package intake turns the events the webhook receiver accepts into jobs for the queue. It is the
// only place that knows both sides: the queue does not know about providers, nor the receiver about
// the queue.
package intake

import (
	"context"
	"encoding/json"
	"errors"

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
	sub    Submitter
	kind   string
	params json.RawMessage
}

// New returns an Enqueuer that submits jobs of the given runner kind. params are the runner's
// configuration, carried opaque to every job.
func New(sub Submitter, kind string, params json.RawMessage) *Enqueuer {
	return &Enqueuer{sub: sub, kind: kind, params: params}
}

// Enqueue implements webhook.Enqueuer. Jobs are named after the repository, so two pushes to the
// same repository never run at the same time. The whole event travels as the job's opaque input.
func (e *Enqueuer) Enqueue(ctx context.Context, ev provider.Event) error {
	_, err := e.Submit(ctx, ev)
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
