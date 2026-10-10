package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dymmer-code/dymploy/internal/job"
)

// Event is what the live channel carries about a job.
type Event struct {
	Type  string     // queued, started, output, finished
	Job   job.Job    // the job as it was when the event happened
	Entry *job.Entry // for output events: the log line
}

// Jobs is what the daemon offers about its jobs. The queue implements it (through a small adapter in
// the composition root, so that this package does not depend on the queue).
type Jobs interface {
	List(ctx context.Context, f job.Filter) ([]job.Job, error)
	Get(ctx context.Context, id job.ID) (job.Job, error)
	Logs(id job.ID, after int64, limit int) ([]job.Entry, error)
	Cancel(ctx context.Context, id job.ID) error
	Retry(ctx context.Context, id job.ID) (job.Job, error)
	// Subscribe returns a channel of events and a function that ends the subscription. A subscriber
	// that cannot keep up loses events: output events carry their sequence number so that it can
	// notice and read the log again.
	Subscribe() (<-chan Event, func())
}

// ErrConflict is what Jobs methods return when the request makes no sense in the job's state (for
// example cancelling a finished job). The API answers 409.
var ErrConflict = errors.New("conflict")

// Message is one JSON message of the live channel.
type Message struct {
	// Type is snapshot, queued, started, output, finished or error.
	Type  string     `json:"type"`
	Job   *job.Job   `json:"job,omitempty"`
	Jobs  []job.Job  `json:"jobs,omitempty"` // snapshot: the jobs waiting or running now
	Entry *job.Entry `json:"entry,omitempty"`
	Error string     `json:"error,omitempty"`
}

// API serves the jobs over HTTP. The same handler runs on the control socket (full access) and, read
// only and behind a token, on the public listener.
type API struct {
	Jobs Jobs
	// ReadOnly leaves out cancelling and retrying.
	ReadOnly bool
	// OriginPatterns are the web origins allowed to open the live channel from a browser; clients that
	// are not browsers send no Origin and are not affected.
	OriginPatterns []string
	// MaxStreams bounds the live channels open at once; 0 means 16.
	MaxStreams int

	streams atomic.Int64
}

// Handler returns the routes, under /v1/.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs", a.list)
	mux.HandleFunc("GET /v1/jobs/{id}", a.get)
	mux.HandleFunc("GET /v1/jobs/{id}/logs", a.logs)
	mux.HandleFunc("GET /v1/events", a.events)
	if !a.ReadOnly {
		mux.HandleFunc("POST /v1/jobs/{id}/cancel", a.cancel)
		mux.HandleFunc("POST /v1/jobs/{id}/retry", a.retry)
	}
	return mux
}

// public hides what only the runner needs: the parameters and input of a job can carry repository
// details and are of no use to a reader.
func public(j job.Job) job.Job {
	j.Params, j.Input = nil, nil
	return j
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, job.ErrNotFound):
		reply(w, http.StatusNotFound, map[string]string{"error": "job not found"})
	case errors.Is(err, ErrConflict):
		reply(w, http.StatusConflict, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrConflict.Error()+": ")})
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := job.Filter{Name: q.Get("name")}
	for _, s := range strings.Split(q.Get("state"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			state := job.State(s)
			switch state {
			case job.Queued, job.Running, job.Succeeded, job.Failed, job.Canceled, job.Interrupted:
				f.States = append(f.States, state)
			default:
				reply(w, http.StatusBadRequest, map[string]string{"error": "unknown state " + strconv.Quote(s)})
				return
			}
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1000 {
			reply(w, http.StatusBadRequest, map[string]string{"error": "limit must be a number between 0 and 1000"})
			return
		}
		f.Limit = n
	}
	jobs, err := a.Jobs.List(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	for i := range jobs {
		jobs[i] = public(jobs[i])
	}
	if jobs == nil {
		jobs = []job.Job{}
	}
	reply(w, http.StatusOK, jobs)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	j, err := a.Jobs.Get(r.Context(), job.ID(r.PathValue("id")))
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, public(j))
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	id := job.ID(r.PathValue("id"))
	if _, err := a.Jobs.Get(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	after, limit, ok := logRange(w, r)
	if !ok {
		return
	}
	entries, err := a.Jobs.Logs(id, after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	if entries == nil {
		entries = []job.Entry{}
	}
	reply(w, http.StatusOK, entries)
}

func logRange(w http.ResponseWriter, r *http.Request) (after int64, limit int, ok bool) {
	q := r.URL.Query()
	var err error
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			reply(w, http.StatusBadRequest, map[string]string{"error": "after must be a sequence number"})
			return 0, 0, false
		}
	}
	limit = 1000
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 10000 {
			reply(w, http.StatusBadRequest, map[string]string{"error": "limit must be a number between 1 and 10000"})
			return 0, 0, false
		}
	}
	return after, limit, true
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	id := job.ID(r.PathValue("id"))
	if err := a.Jobs.Cancel(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"message": "cancelling " + string(id)})
}

func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	j, err := a.Jobs.Retry(r.Context(), job.ID(r.PathValue("id")))
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusAccepted, public(j))
}

const writeTimeout = 10 * time.Second

// events is the live channel (a WebSocket). Without ?job it sends a snapshot of what is waiting or
// running and then every event. With ?job=ID[&after=SEQ] it first replays that job's log from SEQ and
// then follows the job until it finishes, so that a client that reconnects loses nothing.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	max := int64(a.MaxStreams)
	if max == 0 {
		max = 16
	}
	if a.streams.Add(1) > max {
		a.streams.Add(-1)
		w.Header().Set("Retry-After", "5")
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "too many live channels open"})
		return
	}
	defer a.streams.Add(-1)

	q := r.URL.Query()
	watched := job.ID(q.Get("job"))
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			reply(w, http.StatusBadRequest, map[string]string{"error": "after must be a sequence number"})
			return
		}
		after = n
	}
	if watched != "" {
		if _, err := a.Jobs.Get(r.Context(), watched); err != nil {
			fail(w, err)
			return
		}
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: a.OriginPatterns})
	if err != nil {
		return // Accept has already answered
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // answers pings and notices the client leaving
	send := func(m Message) error {
		data, _ := json.Marshal(m)
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, data)
	}

	// Subscribe before reading the past, so that nothing falls between the two; entries seen in both
	// are told apart by their sequence number.
	events, unsubscribe := a.Jobs.Subscribe()
	defer unsubscribe()

	last := after
	if watched == "" {
		jobs, err := a.Jobs.List(ctx, job.Filter{States: []job.State{job.Queued, job.Running}})
		if err != nil {
			_ = send(Message{Type: "error", Error: err.Error()})
			c.Close(websocket.StatusInternalError, "internal error")
			return
		}
		for i := range jobs {
			jobs[i] = public(jobs[i])
		}
		if jobs == nil {
			jobs = []job.Job{}
		}
		if send(Message{Type: "snapshot", Jobs: jobs}) != nil {
			return
		}
	} else {
		for {
			entries, err := a.Jobs.Logs(watched, last, 500)
			if err != nil {
				_ = send(Message{Type: "error", Error: err.Error()})
				c.Close(websocket.StatusInternalError, "internal error")
				return
			}
			for i := range entries {
				e := entries[i]
				if send(Message{Type: "output", Entry: &e}) != nil {
					return
				}
				last = e.Seq
			}
			if len(entries) < 500 {
				break
			}
		}
		if j, err := a.Jobs.Get(ctx, watched); err == nil && j.State.Final() {
			pj := public(j)
			_ = send(Message{Type: "finished", Job: &pj})
			c.Close(websocket.StatusNormalClosure, "job finished")
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if watched != "" && ev.Job.ID != watched {
				continue
			}
			m := Message{Type: ev.Type}
			pj := public(ev.Job)
			m.Job = &pj
			if ev.Entry != nil {
				if watched != "" && ev.Entry.Seq <= last {
					continue // already replayed
				}
				e := *ev.Entry
				m.Entry = &e
				last = e.Seq
			}
			if send(m) != nil {
				return
			}
			if watched != "" && ev.Type == "finished" {
				c.Close(websocket.StatusNormalClosure, "job finished")
				return
			}
		}
	}
}

// RequireToken wraps h so that only requests carrying `Authorization: Bearer <token>` get through.
// The comparison takes the same time whatever the guess. denied, if not nil, is told about every
// refused request (for the audit log).
func RequireToken(token string, h http.Handler, denied func(*http.Request)) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			if denied != nil {
				denied(r)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="dymploy"`)
			reply(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h.ServeHTTP(w, r)
	})
}
