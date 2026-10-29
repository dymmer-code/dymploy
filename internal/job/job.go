// Package job holds the data model of a job and the stores that keep it. It knows nothing about how
// jobs are run (see package runner) or scheduled (see package queue).
package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dymmer-code/dymploy/internal/runner"
)

// ID identifies a job. IDs sort by creation time.
type ID string

// NewID returns a time-ordered unique id.
func NewID(now time.Time) ID {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return ID(fmt.Sprintf("%012x-%s", now.UnixMilli(), hex.EncodeToString(b[:])))
}

// State of a job.
type State string

// The states of a job. Succeeded, Failed, Aborted and Interrupted are final.
const (
	Queued    State = "queued"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	// Aborted: the job was asked to stop (by a person, by a configuration that no longer allows it, or
	// because dymploy was stopped) and its processes were stopped. StopReason says why.
	Aborted State = "aborted"
	// Interrupted: dymploy died while the job was running, so nobody got to stop it properly. Found
	// when the next run starts; the job is NOT retried by itself.
	Interrupted State = "interrupted"
)

// Final reports whether a job in this state will not change any more.
func (s State) Final() bool {
	switch s {
	case Succeeded, Failed, Aborted, Interrupted:
		return true
	}
	return false
}

// Origin binds a remotely requested job to its control plane and application.
// Request is an idempotency key within retained history; Ref records the requested ref.
type Origin struct {
	Commit     string `json:"commit,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	Owner      string `json:"owner"`
	Project    string `json:"project"`
	Definition string `json:"definition,omitempty"`
	Source     string `json:"source"`
	App        string `json:"app"`
	Request    string `json:"request,omitempty"`
	Ref        string `json:"ref"`
}

// Job is one unit of work and its history.
type Job struct {
	Origin *Origin `json:"origin,omitempty"`
	ID     ID      `json:"id"`
	Name   string  `json:"name"`
	// Number is the run number within the name: the first run of github/acme/app is #1, the next #2…
	// Numbers are never reused, so "github/acme/app #12" always means the same run.
	Number int    `json:"number,omitempty"`
	Kind   string `json:"kind"`
	State  State  `json:"state"`
	// Params and Input are opaque to everything but the runner of Kind.
	Params json.RawMessage `json:"params,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// ExitCode is set when the work ran to the end.
	ExitCode *int `json:"exit_code,omitempty"`
	// Error says why a job ended without an exit code (or was interrupted by a crash).
	Error string `json:"error,omitempty"`
	// StopRequestedAt and StopReason are set the moment a stop is asked for, while the job is still
	// stopping, and stay with the job as the record of why it was aborted.
	StopRequestedAt time.Time       `json:"stop_requested_at,omitzero"`
	StopReason      string          `json:"stop_reason,omitempty"`
	Details         json.RawMessage `json:"details,omitempty"`
	RetryOf         ID              `json:"retry_of,omitempty"`
}

// Filter selects jobs for List.
type Filter struct {
	Before  ID
	Owner   string
	Project string
	Source  string
	App     string
	States  []State // any of these; empty means any
	Name    string  // exact name; empty means any
	Number  int     // run number within the name; 0 means any
	Limit   int     // 0 means no limit
}

// Matches reports whether j passes the filter (the limit aside).
func (f Filter) Matches(j Job) bool {
	if f.Before != "" && j.ID >= f.Before {
		return false
	}
	if f.Owner != "" && (j.Origin == nil || j.Origin.Owner != f.Owner) {
		return false
	}
	if f.Project != "" && (j.Origin == nil || j.Origin.Project != f.Project) {
		return false
	}
	if f.Source != "" && (j.Origin == nil || j.Origin.Source != f.Source) {
		return false
	}
	if f.App != "" && (j.Origin == nil || j.Origin.App != f.App) {
		return false
	}
	if f.Name != "" && j.Name != f.Name {
		return false
	}
	if f.Number != 0 && j.Number != f.Number {
		return false
	}
	if len(f.States) == 0 {
		return true
	}
	for _, s := range f.States {
		if s == j.State {
			return true
		}
	}
	return false
}

// ErrNotFound is returned for an id that is not stored.
var ErrNotFound = errors.New("job not found")

// Store keeps jobs.
type Store interface {
	// Put creates or replaces a job.
	Put(ctx context.Context, j Job) error
	Get(ctx context.Context, id ID) (Job, error)
	// List returns matching jobs, newest first.
	List(ctx context.Context, f Filter) ([]Job, error)
	Delete(ctx context.Context, id ID) error
	// NextNumber returns the next run number for a name: 1, 2, 3… It never repeats, not even after
	// the jobs that had the earlier numbers have been deleted.
	NextNumber(ctx context.Context, name string) (int, error)
}

// Entry is one line of a job's log.
type Entry struct {
	Seq    int64         `json:"seq"` // 1, 2, 3… per job; lets a reader resume where it stopped
	Time   time.Time     `json:"time"`
	Stream runner.Stream `json:"stream"`
	Text   string        `json:"text"`
}

// Logs keeps the log of each job.
type Logs interface {
	Append(id ID, e Entry) error
	// Read returns up to limit entries with Seq greater than after, in order (limit 0 means all).
	Read(id ID, after int64, limit int) ([]Entry, error)
	Delete(id ID) error
}
