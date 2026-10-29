package queue

import (
	"context"
	"errors"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner/runnertest"
	"testing"
)

func TestOriginIdempotencyAndIsolation(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	r := req("github/acme/app")
	r.Origin = &job.Origin{Source: "builder", App: "app-a", Request: "request-1", Ref: "refs/heads/main"}
	first, err := e.m.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.m.Submit(context.Background(), r)
	if err != nil || first.ID != second.ID {
		t.Fatalf("duplicate: %v %v", second, err)
	}
	r.Origin = &job.Origin{Source: "builder", App: "app-a", Request: "request-1", Ref: "refs/heads/other"}
	if _, err := e.m.Submit(context.Background(), r); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	r.Origin.App = "app-b"
	other, err := e.m.Submit(context.Background(), r)
	if err != nil || other.ID == first.ID {
		t.Fatalf("isolation: %v", err)
	}
	got, err := e.m.List(context.Background(), job.Filter{Source: "builder", App: "app-a"})
	if err != nil || len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("filter: %v %v", got, err)
	}
}

func TestRetryPreservesTenantAndDoesNotReuseRequestKey(t *testing.T) {
	e := newEnv(t, Config{MaxConcurrent: 1}, runnertest.Exit(0))
	e.start(t)
	r := req("github/acme/app")
	r.Origin = &job.Origin{Source: "builder", App: "app", Owner: "owner", Project: "project", Ref: "refs/heads/main", Request: "request-1"}
	j, err := e.m.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(t, j.ID, job.Succeeded)
	retried, err := e.m.Retry(context.Background(), j.ID)
	if err != nil || retried.ID == j.ID || retried.Origin.Owner != "owner" || retried.Origin.Request != "" {
		t.Fatal(retried, err)
	}
}
