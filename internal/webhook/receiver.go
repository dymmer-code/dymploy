package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// DefaultMaxBody is the largest webhook body accepted when the configuration does not say otherwise.
const DefaultMaxBody = 5 << 20

// ErrQueueFull is what a queue returns when it cannot take more work right now.
var ErrQueueFull = errors.New("queue full")

// ErrShuttingDown is what a queue returns when dymploy is stopping and takes no new work. The sender
// should try again later: the answer is 503 with a message that says why.
var ErrShuttingDown = errors.New("dymploy is shutting down")

// ErrDropped is what a queue returns when its policy discards the job on purpose. It is not an
// error for the sender: retrying would be refused again.
var ErrDropped = errors.New("dropped by the queue policy")

// Enqueuer is where accepted events go. The job queue implements it.
type Enqueuer interface {
	Enqueue(ctx context.Context, ev provider.Event) error
}

// Receiver serves POST /hooks/{provider}.
type Receiver struct {
	Providers map[string]provider.Provider
	Policy    Policy // the initial policy; SetPolicy replaces it while running
	Queue     Enqueuer
	Dedupe    *Dedupe
	Limiter   *Limiter
	MaxBody   int64
	Log       *slog.Logger

	current atomic.Pointer[Policy]
}

// SetPolicy replaces the policy for the events that arrive from now on. It is safe to call while
// the receiver is serving.
func (rc *Receiver) SetPolicy(p Policy) { rc.current.Store(&p) }

// Allowed says whether an event passes the current policy, with the reason when it does not. Jobs
// started by hand go through it too: they are no way around the policy.
func (rc *Receiver) Allowed(ev *provider.Event) (bool, string) { return rc.policy().Allow(ev) }

func (rc *Receiver) policy() Policy {
	if p := rc.current.Load(); p != nil {
		return *p
	}
	return rc.Policy
}

// Handler returns the HTTP handler for the webhook routes.
func (rc *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{provider}", rc.serve)
	return mux
}

func (rc *Receiver) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	log := rc.Log.With("provider", name, "remote", r.RemoteAddr)

	// Cheapest check first: flooding must not cost a signature check per request.
	if rc.Limiter != nil && !rc.Limiter.Allow() {
		w.Header().Set("Retry-After", "60")
		reply(w, http.StatusTooManyRequests, "rate limited")
		log.Warn("webhook refused", "reason", "rate limited")
		return
	}
	p, ok := rc.Providers[name]
	if !ok {
		reply(w, http.StatusNotFound, "unknown provider")
		return
	}
	max := rc.MaxBody
	if max <= 0 {
		max = DefaultMaxBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)

	ev, err := p.ParseWebhook(r)
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, provider.ErrUnauthorized):
		reply(w, http.StatusUnauthorized, "unauthorized")
		log.Warn("webhook refused", "reason", "not authenticated")
		return
	case errors.As(err, &tooBig):
		reply(w, http.StatusRequestEntityTooLarge, "too large")
		log.Warn("webhook refused", "reason", "body too large", "limit", max)
		return
	case errors.Is(err, provider.ErrIgnored):
		reply(w, http.StatusOK, "ignored")
		log.Debug("webhook ignored", "why", err.Error())
		return
	case errors.Is(err, provider.ErrMalformed):
		reply(w, http.StatusBadRequest, "malformed")
		log.Warn("webhook refused", "reason", "malformed", "why", err.Error())
		return
	case err != nil:
		reply(w, http.StatusBadRequest, "unreadable")
		log.Warn("webhook refused", "reason", "unreadable body")
		return
	}

	log = log.With("delivery", ev.DeliveryID, "repo", ev.FullName, "ref", ev.Ref)
	if ev.Deleted {
		reply(w, http.StatusOK, "ignored")
		log.Debug("webhook ignored", "why", "ref deleted")
		return
	}
	if allowed, why := rc.policy().Allow(ev); !allowed {
		reply(w, http.StatusForbidden, "forbidden")
		log.Warn("webhook refused", "reason", why)
		return
	}
	if !rc.Dedupe.Claim(ev.Provider, ev.DeliveryID) {
		reply(w, http.StatusOK, "duplicate")
		log.Info("webhook ignored", "why", "duplicate delivery")
		return
	}
	switch err := rc.Queue.Enqueue(r.Context(), *ev); {
	case errors.Is(err, ErrQueueFull):
		rc.Dedupe.Release(ev.Provider, ev.DeliveryID) // the provider will retry; that is not a duplicate
		w.Header().Set("Retry-After", "30")
		reply(w, http.StatusServiceUnavailable, "busy")
		log.Warn("webhook not queued", "reason", "queue full")
	case errors.Is(err, ErrShuttingDown):
		rc.Dedupe.Release(ev.Provider, ev.DeliveryID)
		w.Header().Set("Retry-After", "60")
		reply(w, http.StatusServiceUnavailable, "shutting down: not accepting new jobs")
		log.Warn("webhook not queued", "reason", "shutting down")
	case errors.Is(err, ErrDropped):
		rc.Dedupe.Release(ev.Provider, ev.DeliveryID)
		reply(w, http.StatusOK, "dropped")
		log.Info("webhook dropped", "reason", "queue policy")
	case err != nil:
		rc.Dedupe.Release(ev.Provider, ev.DeliveryID)
		reply(w, http.StatusInternalServerError, "error")
		log.Error("webhook not queued", "error", err)
	default:
		reply(w, http.StatusAccepted, "accepted")
		log.Info("job queued", "commit", short(ev.Commit))
	}
}

func reply(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
