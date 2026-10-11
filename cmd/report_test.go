package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
)

// statusProvider is a provider that records the statuses it is asked to publish.
type statusProvider struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (*statusProvider) Name() string { return "fake" }
func (*statusProvider) ParseWebhook(*http.Request) (*provider.Event, error) {
	return nil, provider.ErrIgnored
}
func (s *statusProvider) PublishStatus(_ context.Context, ev *provider.Event, st provider.State, text, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev.FullName+"@"+ev.Commit[:7]+" "+string(st)+": "+text)
	return s.fail
}

func (s *statusProvider) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func runEvent(t *testing.T, typ queue.EventType, state job.State, n int, code *int) queue.Event {
	t.Helper()
	in, _ := json.Marshal(provider.Event{Provider: "fake", Owner: "acme", Repo: "app", FullName: "acme/app", Commit: "0123456789abcdef0123456789abcdef01234567"})
	return queue.Event{Type: typ, Job: job.Job{ID: "j", Name: "fake/acme/app", Number: n, State: state, ExitCode: code, Input: in, Error: "dial tcp 10.0.0.20:22: refused", StopReason: "maintenance of the secret server"}}
}

func TestTheStateOfARunIsShownNextToItsCommit(t *testing.T) {
	sp := &statusProvider{}
	cfg := &config.Config{Webhooks: map[string]config.WebhookConfig{"fake": {ReportStatus: true}}}
	rep := newStatusReporter(cfg, map[string]provider.Provider{"fake": sp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rep == nil {
		t.Fatal("a provider that can publish and is set to report gets a reporter")
	}
	five := 5
	for _, ev := range []queue.Event{
		runEvent(t, queue.EventQueued, job.Queued, 7, nil),
		runEvent(t, queue.EventStarted, job.Running, 7, nil),
		runEvent(t, queue.EventStopping, job.Running, 7, nil), // not worth a status
		runEvent(t, queue.EventOutput, job.Running, 7, nil),   // never
		runEvent(t, queue.EventFinished, job.Succeeded, 7, nil),
		runEvent(t, queue.EventFinished, job.Failed, 8, &five),
		runEvent(t, queue.EventFinished, job.Failed, 9, nil),
		runEvent(t, queue.EventFinished, job.Aborted, 10, nil),
		runEvent(t, queue.EventFinished, job.Interrupted, 11, nil),
		runEvent(t, queue.EventFinished, job.Queued, 12, nil), // not a verdict
	} {
		rep.Observe(ev)
	}
	want := []string{
		"acme/app@0123456 pending: Run #7 is queued",
		"acme/app@0123456 pending: Run #7 is running",
		"acme/app@0123456 success: Run #7 succeeded",
		"acme/app@0123456 failure: Run #8 failed (exit code 5)",
		"acme/app@0123456 error: Run #9 could not be completed",
		"acme/app@0123456 error: Run #10 was stopped before it finished",
		"acme/app@0123456 error: Run #11 was interrupted",
	}
	if got := strings.Join(sp.list(), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	for _, line := range sp.list() {
		if strings.Contains(line, "10.0") || strings.Contains(line, "secret") {
			t.Fatalf("what is published leaves dymploy: no hosts, errors or stop reasons in it: %q", line)
		}
	}
}

func TestOnlyRunsFromProvidersSetToReportAreReportedAndFailuresAreOnlyWarnings(t *testing.T) {
	sp := &statusProvider{fail: errors.New("GitHub is down")}
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	providers := map[string]provider.Provider{"fake": sp}
	if newStatusReporter(&config.Config{Webhooks: map[string]config.WebhookConfig{"fake": {}}}, providers, log) != nil {
		t.Fatal("not set to report: no reporter")
	}
	if newStatusReporter(&config.Config{Webhooks: map[string]config.WebhookConfig{"fake": {ReportStatus: true}}}, map[string]provider.Provider{"fake": noResolver{}}, log) != nil {
		t.Fatal("a provider that cannot publish statuses gets no reporter")
	}
	rep := newStatusReporter(&config.Config{Webhooks: map[string]config.WebhookConfig{"fake": {ReportStatus: true}}}, providers, log)
	rep.Observe(runEvent(t, queue.EventStarted, job.Running, 1, nil))
	if !strings.Contains(logs.String(), "could not publish the status of a run") || !strings.Contains(logs.String(), "GitHub is down") {
		t.Fatalf("a provider that does not answer is a warning:\n%s", logs.String())
	}
	// jobs that did not come from a repository, or from another provider, are left alone
	sp2 := &statusProvider{}
	rep2 := newStatusReporter(&config.Config{Webhooks: map[string]config.WebhookConfig{"fake": {ReportStatus: true}}}, map[string]provider.Provider{"fake": sp2}, log)
	rep2.Observe(queue.Event{Type: queue.EventStarted, Job: job.Job{ID: "x", Input: json.RawMessage(`{"not":"an event"}`)}})
	rep2.Observe(queue.Event{Type: queue.EventStarted, Job: job.Job{ID: "y", Input: json.RawMessage(`garbage`)}})
	ev := runEvent(t, queue.EventStarted, job.Running, 1, nil)
	var pe provider.Event
	json.Unmarshal(ev.Job.Input, &pe)
	pe.Provider = "gitlab"
	ev.Job.Input, _ = json.Marshal(pe)
	rep2.Observe(ev)
	if len(sp2.list()) != 0 {
		t.Fatalf("%v", sp2.list())
	}
}

func TestRunsWithoutANumberAreStillDescribed(t *testing.T) {
	if _, text, ok := describe(queue.Event{Type: queue.EventStarted, Job: job.Job{}}); !ok || text != "Run is running" {
		t.Fatalf("%q", text)
	}
}

func TestReportStatusNeedsTheApp(t *testing.T) {
	c := config.Default()
	c.Webhooks = map[string]config.WebhookConfig{"github": {SecretFile: "/s", ReportStatus: true}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "report_status needs app_id") {
		t.Fatalf("%v", err)
	}
	c.Webhooks["github"] = config.WebhookConfig{SecretFile: "/s", AppID: 1, PrivateKeyFile: "/k", ReportStatus: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}
