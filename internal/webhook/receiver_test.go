package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestDroppedByPolicyIs200AndNotRetried(t *testing.T) {
	f := newFixture(t)
	f.q.err = ErrDropped
	w := f.postFake(fakeBody("d1", "refs/heads/main"))
	if w.Code != 200 || status(t, w) != "dropped" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	f.q.err = nil
	if w := f.postFake(fakeBody("d1", "refs/heads/main")); w.Code != 202 {
		t.Fatalf("a dropped delivery is released like any other that was not queued: %d", w.Code)
	}
}

func TestAStaleDeliveryIs200AndIsNotRetriedNorQueued(t *testing.T) {
	f := newFixture(t)
	f.q.err = ErrStale
	w := f.postFake(fakeBody("s1", "refs/heads/main"))
	if w.Code != 200 || status(t, w) != "stale" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	f.q.err = nil
	if w := f.postFake(fakeBody("s1", "refs/heads/main")); w.Code != 200 || status(t, w) != "duplicate" {
		t.Fatalf("a stale delivery stays seen, so sending it again changes nothing: %d %s", w.Code, w.Body)
	}
	if f.q.count() != 0 {
		t.Fatal("a stale event reached the queue")
	}
}

func TestWhenTheTipCannotBeVerifiedIs503AndTheRetryIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.q.err = ErrCannotVerify
	w := f.postFake(fakeBody("v1", "refs/heads/main"))
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || status(t, w) != "could not verify" {
		t.Fatalf("%d %s %v", w.Code, w.Body, w.Header())
	}
	f.q.err = nil
	if w := f.postFake(fakeBody("v1", "refs/heads/main")); w.Code != 202 {
		t.Fatalf("an unverified delivery is released so it can be sent again: %d", w.Code)
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

func TestOneAddressCannotUseUpWhatEverybodySharesAndTheProxyHeaderIsOnlyBelievedFromTheProxy(t *testing.T) {
	f := newFixture(t)
	f.rc.IPLimiter = NewIPLimiter(2, time.Minute)
	f.rc.Limiter = NewLimiter(100, time.Minute)
	f.rc.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	send := func(remote, xff string) int {
		r := httptest.NewRequest("POST", "/hooks/fake", strings.NewReader(fakeBody("x", "refs/heads/main")))
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		r.Header.Set("X-Fake-Token", "t")
		w := httptest.NewRecorder()
		f.rc.Handler().ServeHTTP(w, r)
		return w.Code
	}
	// a flood from one real client, seen through the proxy
	send("192.0.2.1:1", "203.0.113.9")
	send("192.0.2.1:2", "203.0.113.9")
	if code := send("192.0.2.1:3", "203.0.113.9"); code != 429 {
		t.Fatalf("the flooder is limited: %d", code)
	}
	if code := send("192.0.2.1:4", "198.51.100.7"); code == 429 {
		t.Fatal("another client behind the same proxy is not affected")
	}
	// somebody who is not the proxy cannot claim to be another address
	send("203.0.113.50:1", "198.51.100.7")
	send("203.0.113.50:2", "198.51.100.7")
	if code := send("203.0.113.50:3", "198.51.100.200"); code != 429 {
		t.Fatalf("a forged X-Forwarded-For must not give a fresh budget: %d", code)
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

func TestReplacingThePolicyAppliesToTheNextEvents(t *testing.T) {
	f := newFixture(t)
	if w := f.postFake(fakeBody("d1", "refs/heads/feature/x")); w.Code != 403 {
		t.Fatalf("before: %d", w.Code)
	}
	f.rc.SetPolicy(Policy{Rules: []Rule{{Provider: "fake", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"feature/*"}}}})
	if w := f.postFake(fakeBody("d2", "refs/heads/feature/x")); w.Code != 202 {
		t.Fatalf("after: %d %s", w.Code, w.Body)
	}
	f.rc.SetPolicy(Policy{})
	if w := f.postFake(fakeBody("d3", "refs/heads/main")); w.Code != 403 {
		t.Fatalf("an empty policy builds nothing: %d", w.Code)
	}
}

func TestAllowedAnswersWithTheCurrentPolicy(t *testing.T) {
	f := newFixture(t)
	ev := &provider.Event{Provider: "fake", Owner: "acme", Repo: "app", Branch: "main", Ref: "refs/heads/main"}
	if ok, why := f.rc.Allowed(ev); !ok || why != "" {
		t.Fatalf("%v %q", ok, why)
	}
	other := *ev
	other.Branch = "feature/x"
	if ok, why := f.rc.Allowed(&other); ok || why == "" {
		t.Fatalf("a refusal says why: %v %q", ok, why)
	}
	f.rc.SetPolicy(Policy{})
	if ok, _ := f.rc.Allowed(ev); ok {
		t.Fatal("the replaced policy applies to manual jobs too")
	}
}

func TestAShuttingDownQueueIs503WithAMessageAndTheRetryIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.q.err = ErrShuttingDown
	w := f.postFake(fakeBody("d-stop", "refs/heads/main"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "shutting down") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", w.Code, w.Body, w.Header())
	}
	f.q.err = nil
	if w := f.postFake(fakeBody("d-stop", "refs/heads/main")); w.Code != 202 {
		t.Fatalf("the same delivery, once dymploy is back, is not a duplicate: %d", w.Code)
	}
}
