package intake

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

type submitter struct {
	got queue.Request
	err error
}

func (s *submitter) Submit(_ context.Context, r queue.Request) (job.Job, error) {
	s.got = r
	return job.Job{ID: "j1"}, s.err
}

var ev = provider.Event{Provider: "github", FullName: "acme/shop", Owner: "acme", Repo: "shop", Branch: "main", Commit: "abc"}

func TestAnEventBecomesAJobNamedAfterTheRepository(t *testing.T) {
	s := &submitter{}
	e := New(s, "docker", json.RawMessage(`{"image":"x"}`))
	if err := e.Enqueue(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.got.Name != "github/acme/shop" || s.got.Kind != "docker" || string(s.got.Params) != `{"image":"x"}` {
		t.Fatalf("%+v", s.got)
	}
	var back provider.Event
	if err := json.Unmarshal(s.got.Input, &back); err != nil || back != ev {
		t.Fatalf("the event travels intact as the job's opaque input: %v %+v", err, back)
	}
}

func TestQueueErrorsAreTranslatedForTheWebhookLayer(t *testing.T) {
	cases := map[string]struct {
		in   error
		want error
	}{
		"full":     {queue.ErrQueueFull, webhook.ErrQueueFull},
		"rejected": {queue.ErrRejected, webhook.ErrDropped},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := New(&submitter{err: c.in}, "k", nil).Enqueue(context.Background(), ev)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	boom := errors.New("disk")
	if err := New(&submitter{err: boom}, "k", nil).Enqueue(context.Background(), ev); !errors.Is(err, boom) {
		t.Fatalf("anything else passes through: %v", err)
	}
}

func TestSubmitReturnsTheJobAndTheQueuesOwnErrors(t *testing.T) {
	s := &submitter{}
	j, err := New(s, "docker", nil).Submit(context.Background(), ev)
	if err != nil || j.ID != "j1" || s.got.Name != "github/acme/shop" {
		t.Fatalf("%v %+v", err, j)
	}
	s.err = queue.ErrRejected
	if _, err := New(s, "docker", nil).Submit(context.Background(), ev); !errors.Is(err, queue.ErrRejected) {
		t.Fatalf("callers that start jobs by hand get the queue's errors, not the webhook's: %v", err)
	}
}

func TestAStoppingQueueIsTranslatedForTheWebhookLayer(t *testing.T) {
	for _, err := range []error{queue.ErrDraining, queue.ErrClosed} {
		s := &submitter{err: err}
		if got := New(s, "docker", nil).Enqueue(context.Background(), ev); !errors.Is(got, webhook.ErrShuttingDown) {
			t.Errorf("%v → %v", err, got)
		}
	}
}
