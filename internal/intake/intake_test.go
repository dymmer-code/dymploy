package intake

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	fakeprovider "github.com/dymmer-code/dymploy/internal/provider/fake"
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

// --- the commit must still be the tip ---

type resolver struct {
	fake *fakeprovider.Provider
	err  error
}

func (r resolver) ResolveRef(ctx context.Context, owner, repo, ref string) (*provider.Event, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.fake.ResolveRef(ctx, owner, repo, ref)
}

func tipOf(refs map[string]string, err error, on bool) func(context.Context, provider.Event) error {
	r := resolver{fake: fakeprovider.New("t").WithRefs(refs), err: err}
	return Tip(func(string) (provider.Resolver, bool) { return r, true }, func(string) bool { return on })
}

var pushed = provider.Event{Provider: "fake", Owner: "acme", Repo: "shop", FullName: "acme/shop", Ref: "refs/heads/main", Branch: "main", Commit: "c2"}

func TestACurrentPushIsQueuedAndAStaleOneIsNot(t *testing.T) {
	s := &submitter{}
	e := New(s, "k", nil).Fresh(tipOf(map[string]string{"acme/shop:refs/heads/main": "c2"}, nil, true))
	if err := e.Enqueue(context.Background(), pushed); err != nil || s.got.Name == "" {
		t.Fatalf("the tip itself must pass: %v", err)
	}

	s = &submitter{}
	e = New(s, "k", nil).Fresh(tipOf(map[string]string{"acme/shop:refs/heads/main": "c3"}, nil, true))
	err := e.Enqueue(context.Background(), pushed)
	if !errors.Is(err, webhook.ErrStale) {
		t.Fatalf("an old commit must be stale: %v", err)
	}
	if s.got.Name != "" {
		t.Fatal("a stale event must not reach the queue")
	}
}

func TestADeletedRefIsStaleAndTagsAreChecked(t *testing.T) {
	e := New(&submitter{}, "k", nil).Fresh(tipOf(map[string]string{}, nil, true))
	if err := e.Enqueue(context.Background(), pushed); !errors.Is(err, webhook.ErrStale) {
		t.Fatalf("the branch is gone: %v", err)
	}
	tag := pushed
	tag.Ref, tag.Branch, tag.Tag = "refs/tags/v1", "", "v1"
	moved := New(&submitter{}, "k", nil).Fresh(tipOf(map[string]string{"acme/shop:refs/tags/v1": "other"}, nil, true))
	if err := moved.Enqueue(context.Background(), tag); !errors.Is(err, webhook.ErrStale) {
		t.Fatalf("a moved tag is stale too: %v", err)
	}
}

func TestIfTheProviderCannotAnswerNothingIsQueued(t *testing.T) {
	s := &submitter{}
	e := New(s, "k", nil).Fresh(tipOf(nil, errors.New("api down"), true))
	if err := e.Enqueue(context.Background(), pushed); !errors.Is(err, webhook.ErrCannotVerify) {
		t.Fatalf("an unanswered check must not let the event through: %v", err)
	}
	if s.got.Name != "" {
		t.Fatal("it was queued")
	}
}

func TestTheCheckCanBeSwitchedOffAndNeverAppliesToAPersonSubmitting(t *testing.T) {
	stale := tipOf(map[string]string{"acme/shop:refs/heads/main": "c3"}, nil, false)
	if err := New(&submitter{}, "k", nil).Fresh(stale).Enqueue(context.Background(), pushed); err != nil {
		t.Fatalf("switched off for this provider: %v", err)
	}
	on := tipOf(map[string]string{"acme/shop:refs/heads/main": "c3"}, nil, true)
	if _, err := New(&submitter{}, "k", nil).Fresh(on).Submit(context.Background(), pushed); err != nil {
		t.Fatalf("a person starting a job by hand chose that commit: %v", err)
	}
	// a provider that cannot resolve refs has nothing to compare: the event passes
	noResolver := Tip(func(string) (provider.Resolver, bool) { return nil, false }, func(string) bool { return true })
	if err := noResolver(context.Background(), pushed); err != nil {
		t.Fatal(err)
	}
}

func TestScopedSubmissionPreservesLimitsAndRestrictsTargets(t *testing.T) {
	s := &submitter{}
	origin := job.Origin{Source: "builder", App: "app", Ref: "refs/heads/main"}
	enq := New(s, "docker", json.RawMessage(`{"limits":{"memory":"2g"}}`))
	if _, err := enq.SubmitScoped(context.Background(), ev, origin, nil); err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	_ = json.Unmarshal(s.got.Params, &params)
	if s.got.Origin.App != "app" || params["limits"].(map[string]any)["memory"] != "2g" || len(params["allowed_targets"].([]any)) != 0 {
		t.Fatal(s.got)
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`)} {
		if _, err := New(s, "docker", raw).SubmitScoped(context.Background(), ev, origin, []string{"rowan"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(s, "docker", json.RawMessage(`{`)).SubmitScoped(context.Background(), ev, origin, nil); err == nil {
		t.Fatal("bad parameters")
	}
	if _, err := New(s, "dryrun", nil).SubmitScoped(context.Background(), ev, origin, nil); err == nil {
		t.Fatal("unsupported runner")
	}
}

func TestWebhookScopeAddsProvenanceWithoutChangingUnmatchedJobs(t *testing.T) {
	s := &submitter{}
	origin := job.Origin{Source: "builder", App: "app", Owner: "owner", Project: "project", Ref: "refs/heads/main"}
	e := New(s, "docker", nil).WithScope(func(provider.Event) (*job.Origin, []string) { return &origin, []string{"rowan"} })
	if err := e.Enqueue(context.Background(), ev); err != nil || s.got.Origin == nil || s.got.Origin.App != "app" {
		t.Fatal(s.got, err)
	}
	e.WithScope(func(provider.Event) (*job.Origin, []string) { return nil, nil })
	if err := e.Enqueue(context.Background(), ev); err != nil || s.got.Origin != nil {
		t.Fatal(s.got, err)
	}
}
