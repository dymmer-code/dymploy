package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
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

// ErrStale is what the enqueuer returns when the event's commit is no longer where its branch or tag
// points: a delivery that arrives late, out of order or replayed must not build old code.
var ErrStale = errors.New("the commit is no longer the tip of its branch or tag")

// ErrCannotVerify is returned when the check above could not be made (the provider did not answer). The
// event is not queued; the provider can deliver it again.
var ErrCannotVerify = errors.New("could not verify that the commit is still the tip")

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
	// IPLimiter limits each client address (see ClientIP) before the shared Limiter counts anything, so that
	// one address cannot use up what everybody else shares. TrustedProxies are the proxies whose
	// X-Forwarded-For is believed.
	IPLimiter      *IPLimiter
	TrustedProxies []netip.Prefix
	MaxBody        int64
	Log            *slog.Logger

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
	client := ClientIP(r, rc.TrustedProxies)
	log := rc.Log.With("provider", name, "remote", client)

	// Cheapest checks first: flooding must not cost a signature check per request.
	if !rc.IPLimiter.Allow(client) {
		w.Header().Set("Retry-After", "60")
		reply(w, http.StatusTooManyRequests, "rate limited")
		log.Warn("webhook refused", "reason", "rate limited (this address)")
		return
	}
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
	case errors.Is(err, ErrStale):
		// Answered 200 and kept as seen: it is not a failure of the sender and it must not be retried.
		reply(w, http.StatusOK, "stale")
		log.Info("webhook ignored", "why", "the commit is not the tip any more", "commit", short(ev.Commit))
	case errors.Is(err, ErrCannotVerify):
		rc.Dedupe.Release(ev.Provider, ev.DeliveryID)
		w.Header().Set("Retry-After", "30")
		reply(w, http.StatusServiceUnavailable, "could not verify")
		log.Warn("webhook not queued", "reason", "could not verify the tip", "error", err)
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
