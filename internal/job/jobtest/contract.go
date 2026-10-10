// Package jobtest holds contract tests that every job.Store and job.Logs implementation must pass,
// so the in-memory versions used by other packages' tests behave exactly like the persistent ones.
package jobtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func mk(n int, name string, st job.State) job.Job {
	return job.Job{
		ID: job.ID("job-" + string(rune('a'+n))), Name: name, Kind: "docker", State: st,
		Params: json.RawMessage(`{"image":"x"}`), Input: json.RawMessage(`{"sha":"abc"}`),
		CreatedAt: t0.Add(time.Duration(n) * time.Minute),
	}
}

// RunStore checks a job.Store. factory returns a fresh, empty store; reopen, if not nil, returns a
// store over the same data after the previous one was closed (persistence across restarts).
func RunStore(t *testing.T, factory func(t *testing.T) job.Store, reopen func(t *testing.T, s job.Store) job.Store) {
	ctx := context.Background()
	t.Run("put and get keep every field", func(t *testing.T) {
		s := factory(t)
		code := 3
		j := mk(1, "github/a/b", job.Failed)
		j.StartedAt, j.FinishedAt, j.ExitCode, j.Error, j.RetryOf = t0.Add(time.Hour), t0.Add(2*time.Hour), &code, "boom", "job-x"
		if err := s.Put(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != j.ID || got.Name != j.Name || got.Kind != j.Kind || got.State != j.State ||
			string(got.Params) != string(j.Params) || string(got.Input) != string(j.Input) ||
			!got.CreatedAt.Equal(j.CreatedAt) || !got.StartedAt.Equal(j.StartedAt) || !got.FinishedAt.Equal(j.FinishedAt) ||
			got.ExitCode == nil || *got.ExitCode != 3 || got.Error != "boom" || got.RetryOf != "job-x" {
			t.Fatalf("got %+v want %+v", got, j)
		}
	})
	t.Run("an unset exit code stays unset", func(t *testing.T) {
		s := factory(t)
		_ = s.Put(ctx, mk(1, "n", job.Queued))
		got, _ := s.Get(ctx, "job-b")
		if got.ExitCode != nil || !got.StartedAt.IsZero() {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("put replaces", func(t *testing.T) {
		s := factory(t)
		j := mk(1, "n", job.Queued)
		_ = s.Put(ctx, j)
		j.State = job.Running
		_ = s.Put(ctx, j)
		got, _ := s.Get(ctx, j.ID)
		if got.State != job.Running {
			t.Fatal(got.State)
		}
		if l, _ := s.List(ctx, job.Filter{}); len(l) != 1 {
			t.Fatalf("replacing must not duplicate: %d", len(l))
		}
	})
	t.Run("missing job", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Get(ctx, "nope"); !errors.Is(err, job.ErrNotFound) {
			t.Fatalf("%v", err)
		}
		if err := s.Delete(ctx, "nope"); err != nil {
			t.Fatalf("deleting what is not there is not an error: %v", err)
		}
	})
	t.Run("list is newest first and filterable", func(t *testing.T) {
		s := factory(t)
		for i, st := range []job.State{job.Succeeded, job.Failed, job.Queued, job.Running} {
			name := "one"
			if i%2 == 1 {
				name = "two"
			}
			_ = s.Put(ctx, mk(i, name, st))
		}
		all, _ := s.List(ctx, job.Filter{})
		if len(all) != 4 || all[0].ID != "job-d" || all[3].ID != "job-a" {
			t.Fatalf("order: %+v", ids(all))
		}
		if l, _ := s.List(ctx, job.Filter{Limit: 2}); len(l) != 2 || l[0].ID != "job-d" {
			t.Fatalf("limit: %v", ids(l))
		}
		if l, _ := s.List(ctx, job.Filter{States: []job.State{job.Queued, job.Running}}); len(l) != 2 || l[0].ID != "job-d" || l[1].ID != "job-c" {
			t.Fatalf("states: %v", ids(l))
		}
		if l, _ := s.List(ctx, job.Filter{Name: "two"}); len(l) != 2 {
			t.Fatalf("name: %v", ids(l))
		}
		if l, _ := s.List(ctx, job.Filter{Name: "two", States: []job.State{job.Failed}, Limit: 1}); len(l) != 1 || l[0].ID != "job-b" {
			t.Fatalf("combined: %v", ids(l))
		}
	})
	t.Run("delete", func(t *testing.T) {
		s := factory(t)
		_ = s.Put(ctx, mk(1, "n", job.Queued))
		_ = s.Delete(ctx, "job-b")
		if _, err := s.Get(ctx, "job-b"); !errors.Is(err, job.ErrNotFound) {
			t.Fatal(err)
		}
	})
	if reopen != nil {
		t.Run("survives a restart", func(t *testing.T) {
			s := factory(t)
			_ = s.Put(ctx, mk(1, "n", job.Running))
			s2 := reopen(t, s)
			got, err := s2.Get(ctx, "job-b")
			if err != nil || got.State != job.Running {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func ids(l []job.Job) []job.ID {
	out := make([]job.ID, len(l))
	for i, j := range l {
		out[i] = j.ID
	}
	return out
}

// RunLogs checks a job.Logs implementation.
func RunLogs(t *testing.T, factory func(t *testing.T) job.Logs) {
	entry := func(seq int64, text string) job.Entry {
		return job.Entry{Seq: seq, Time: t0.Add(time.Duration(seq) * time.Second), Stream: runner.Stdout, Text: text}
	}
	t.Run("append and read in order", func(t *testing.T) {
		l := factory(t)
		for i, s := range []string{"a", "b", "c"} {
			if err := l.Append("j1", entry(int64(i+1), s)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := l.Read("j1", 0, 0)
		if err != nil || len(got) != 3 || got[0].Text != "a" || got[2].Seq != 3 || got[1].Stream != runner.Stdout || !got[0].Time.Equal(t0.Add(time.Second)) {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("resume after a sequence number", func(t *testing.T) {
		l := factory(t)
		for i := 1; i <= 5; i++ {
			_ = l.Append("j1", entry(int64(i), "x"))
		}
		got, _ := l.Read("j1", 3, 0)
		if len(got) != 2 || got[0].Seq != 4 {
			t.Fatalf("%+v", got)
		}
		got, _ = l.Read("j1", 0, 2)
		if len(got) != 2 || got[1].Seq != 2 {
			t.Fatalf("limit: %+v", got)
		}
	})
	t.Run("jobs have separate logs", func(t *testing.T) {
		l := factory(t)
		_ = l.Append("j1", entry(1, "one"))
		_ = l.Append("j2", entry(1, "two"))
		a, _ := l.Read("j1", 0, 0)
		b, _ := l.Read("j2", 0, 0)
		if len(a) != 1 || a[0].Text != "one" || len(b) != 1 || b[0].Text != "two" {
			t.Fatalf("%+v %+v", a, b)
		}
	})
	t.Run("an unknown job has an empty log", func(t *testing.T) {
		l := factory(t)
		if got, err := l.Read("ghost", 0, 0); err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		l := factory(t)
		_ = l.Append("j1", entry(1, "x"))
		if err := l.Delete("j1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := l.Read("j1", 0, 0); len(got) != 0 {
			t.Fatal("deleted")
		}
		if err := l.Delete("j1"); err != nil {
			t.Fatalf("deleting twice is fine: %v", err)
		}
	})
	t.Run("text with newlines, quotes and non-ASCII survives", func(t *testing.T) {
		l := factory(t)
		_ = l.Append("j1", entry(1, "ñandú \"quoted\" \t tab"))
		got, _ := l.Read("j1", 0, 0)
		if got[0].Text != "ñandú \"quoted\" \t tab" {
			t.Fatalf("%q", got[0].Text)
		}
	})
}
