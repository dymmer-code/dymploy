package webhook

import (
	"context"
	"log/slog"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// LogQueue is a placeholder Enqueuer: it logs the accepted event and drops it. It exists only until
// the job queue replaces it; no job runs while it is in place.
type LogQueue struct{ Log *slog.Logger }

// Enqueue implements Enqueuer.
func (q LogQueue) Enqueue(_ context.Context, ev provider.Event) error {
	q.Log.Warn("event accepted but the job queue is not implemented yet: nothing will run",
		"provider", ev.Provider, "repo", ev.FullName, "ref", ev.Ref)
	return nil
}
