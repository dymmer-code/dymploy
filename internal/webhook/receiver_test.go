package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/provider/fake"
)

type recordingQueue struct {
	mu     sync.Mutex
	events []provider.Event
	err    error
}

func (q *recordingQueue) Enqueue(_ context.Context, ev provider.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.events = append(q.events, ev)
	return nil
}
func (q *recordingQueue) count() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.events) }

// stub lets a test choose exactly what the provider answers.
type stub struct {
	ev  *provider.Event
	err error
}

func (stub) Name() string { return "stub" }
func (s stub) ParseWebhook(*http.Request) (*provider.Event, error) {
	return s.ev, s.err
}

const token = "tok-shared-secret"

func fakeBody(delivery, ref string) string {
	return `{"delivery":"` + delivery + `","owner":"acme","repo":"app","ref":"` + ref + `","commit":"abc123"}`
}

type fixture struct {
	rc   *Receiver
	q    *recordingQueue
	logs *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	q := &recordingQueue{}
	logs := &bytes.Buffer{}
	return &fixture{
		q: q, logs: logs,
		rc: &Receiver{
			Providers: map[string]provider.Provider{"fake": fake.New(token), "stub": stub{}},
			Policy:    Policy{Rules: []Rule{{Provider: "fake", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}}}},
			Queue:     q,
			Dedupe:    NewDedupe(time.Hour, 100),
			Limiter:   NewLimiter(0, time.Minute),
			MaxBody:   1 << 10,
			Log:       slog.New(slog.NewTextHandler(logs, nil)),
		},
	}
}

func (f *fixture) post(path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.rc.Handler().ServeHTTP(w, r)
	return w
}

func (f *fixture) postFake(body string) *httptest.ResponseRecorder {
	return f.post("/hooks/fake", body, map[string]string{"X-Fake-Token": token})
}

func status(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("responses are JSON, got %q", ct)
	}
	var b map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return b["status"]
}

func TestAcceptedEventIsQueued(t *testing.T) {
	f := newFixture(t)
	w := f.postFake(fakeBody("d1", "refs/heads/main"))
	if w.Code != 202 || status(t, w) != "accepted" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f.q.count() != 1 || f.q.events[0].Repo != "app" || f.q.events[0].Commit != "abc123" || f.q.events[0].Branch != "main" {
		t.Fatalf("queued: %+v", f.q.events)
	}
}

func TestUnknownProvider(t *testing.T) {
	f := newFixture(t)
	if w := f.post("/hooks/gitlab", "{}", nil); w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestOnlyPost(t *testing.T) {
	f := newFixture(t)
	w := httptest.NewRecorder()
	f.rc.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks/fake", nil))
	if w.Code != 405 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestBadSignatureIs401AndLeaksNothing(t *testing.T) {
	f := newFixture(t)
	w := f.post("/hooks/fake", fakeBody("d1", "refs/heads/main"), map[string]string{"X-Fake-Token": "wrong"})
	if w.Code != 401 || status(t, w) != "unauthorized" || f.q.count() != 0 {
		t.Fatalf("%d %s queued=%d", w.Code, w.Body, f.q.count())
	}
}

func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	w := f.postFake(`{"delivery":"` + strings.Repeat("x", 4096) + `"}`)
	if w.Code != 413 || f.q.count() != 0 {
		t.Fatalf("%d", w.Code)
	}
}

func TestMalformedAndIgnored(t *testing.T) {
	f := newFixture(t)
	if w := f.postFake(`{"owner":"acme"}`); w.Code != 400 || status(t, w) != "malformed" {
		t.Fatalf("malformed: %d %s", w.Code, w.Body)
	}
	f.rc.Providers["stub"] = stub{err: provider.ErrIgnored}
	if w := f.post("/hooks/stub", "{}", nil); w.Code != 200 || status(t, w) != "ignored" {
		t.Fatalf("ignored: %d %s", w.Code, w.Body)
	}
	if f.q.count() != 0 {
		t.Fatal("neither may be queued")
	}
}

func TestDeletedRefsAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.rc.Providers["stub"] = stub{ev: &provider.Event{Provider: "fake", Owner: "acme", Repo: "app", Branch: "main", Deleted: true}}
	w := f.post("/hooks/stub", "{}", nil)
	if w.Code != 200 || status(t, w) != "ignored" || f.q.count() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestPolicyRefusal(t *testing.T) {
	f := newFixture(t)
	w := f.postFake(fakeBody("d1", "refs/heads/feature/x"))
	if w.Code != 403 || status(t, w) != "forbidden" || f.q.count() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "branch") {
		t.Fatal("the reason goes to the log, not to the caller")
	}
	if !strings.Contains(f.logs.String(), "branch feature/x not allowed") {
		t.Fatalf("the refusal must be logged with its reason:\n%s", f.logs)
	}
}

func TestDuplicateDelivery(t *testing.T) {
	f := newFixture(t)
	if w := f.postFake(fakeBody("d1", "refs/heads/main")); w.Code != 202 {
		t.Fatal(w.Code)
	}
	w := f.postFake(fakeBody("d1", "refs/heads/main"))
	if w.Code != 200 || status(t, w) != "duplicate" || f.q.count() != 1 {
		t.Fatalf("%d %s queued=%d", w.Code, w.Body, f.q.count())
	}
}

func TestFullQueueIs503AndTheRetryIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.q.err = ErrQueueFull
	w := f.postFake(fakeBody("d1", "refs/heads/main"))
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	f.q.err = nil
	if w := f.postFake(fakeBody("d1", "refs/heads/main")); w.Code != 202 || f.q.count() != 1 {
		t.Fatalf("the retry of a delivery that could not be queued must not be a duplicate: %d", w.Code)
	}
}

func TestOtherQueueErrorsAre500(t *testing.T) {
	f := newFixture(t)
	f.q.err = errors.New("disk on fire")
	w := f.postFake(fakeBody("d1", "refs/heads/main"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "disk") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.rc.Limiter = NewLimiter(1, time.Minute)
	if w := f.postFake(fakeBody("d1", "refs/heads/main")); w.Code != 202 {
		t.Fatal(w.Code)
	}
	w := f.postFake(fakeBody("d2", "refs/heads/main"))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", w.Code)
	}
}

func TestLogsNeverContainSecretsOrPayloads(t *testing.T) {
	f := newFixture(t)
	f.postFake(fakeBody("d1", "refs/heads/main"))
	f.post("/hooks/fake", fakeBody("d2", "refs/heads/main"), map[string]string{"X-Fake-Token": "guessed-token"})
	f.postFake(fakeBody("d3", "refs/heads/feature/x"))
	for _, secret := range []string{token, "guessed-token", `"commit"`, `"delivery"`} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatalf("%q must not appear in the log:\n%s", secret, f.logs)
		}
	}
}
