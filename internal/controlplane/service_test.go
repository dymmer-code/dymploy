package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
)

type memoryJobs struct {
	control.Jobs
	store   *job.MemStore
	logs    *job.MemLogs
	aborted []job.ID
	events  chan control.Event
}

func (m *memoryJobs) List(ctx context.Context, f job.Filter) ([]job.Job, error) {
	return m.store.List(ctx, f)
}
func (m *memoryJobs) Get(ctx context.Context, id job.ID) (job.Job, error) {
	return m.store.Get(ctx, id)
}
func (m *memoryJobs) Logs(id job.ID, after int64, limit int) ([]job.Entry, error) {
	return m.logs.Read(id, after, limit)
}
func (m *memoryJobs) Abort(_ context.Context, id job.ID, _ string) error {
	m.aborted = append(m.aborted, id)
	return nil
}
func (m *memoryJobs) Subscribe() (<-chan control.Event, func()) { return m.events, func() {} }
func testService(t *testing.T) (*Service, *memoryJobs, Snapshot) {
	t.Helper()
	c := Catalog{Path: filepath.Join(t.TempDir(), "catalog.db"), Source: "builder-a"}
	a := Application{ID: "app-a", ProjectID: "project", OwnerID: "owner", Repository: Repository{Provider: "github", Owner: "acme", Name: "app", Branches: []string{"main"}}}
	snapshot := Snapshot{SchemaVersion: 1, BuilderID: c.Source, Applications: []Application{a}}
	if err := c.Update(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := c.Accept(a.ID, a.Digest()); err != nil {
		t.Fatal(err)
	}
	jobs := &memoryJobs{store: job.NewMemStore(), logs: job.NewMemLogs(), events: make(chan control.Event, 10)}
	return &Service{Catalog: c, Jobs: jobs}, jobs, snapshot
}
func TestServiceCannotReadOrAbortAnotherApplicationsJob(t *testing.T) {
	s, jobs, _ := testService(t)
	ctx := context.Background()
	for _, j := range []job.Job{
		{ID: "own", Name: "github/acme/app", Origin: &job.Origin{Owner: "owner", Project: "project", Source: "builder-a", App: "app-a"}, Params: json.RawMessage(`{"secret":"never"}`), Input: json.RawMessage(`{"password":"never"}`)},
		{ID: "other", Name: "github/acme/app", Origin: &job.Origin{Owner: "owner", Project: "project", Source: "builder-a", App: "app-b"}},
		{ID: "legacy", Name: "github/acme/app"},
	} {
		if err := jobs.store.Put(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"other", "legacy", "missing"} {
		for _, action := range []string{"job", "logs", "abort"} {
			if r := s.Handle(ctx, Command{App: "app-a", Job: id, Action: action}); r.Error != "not_found" {
				t.Fatalf("%s %s: %+v", action, id, r)
			}
		}
	}
	if len(jobs.aborted) != 0 {
		t.Fatal("cross-app abort")
	}
	r := s.Handle(ctx, Command{App: "app-a", Action: "history"})
	raw, _ := json.Marshal(r)
	if r.Error != "" || strings.Contains(string(raw), "never") || len(r.Data.(map[string]any)["jobs"].([]job.Job)) != 1 {
		t.Fatalf("history: %s", raw)
	}
	if err := s.Catalog.Revoke("app-a"); err != nil {
		t.Fatal(err)
	}
	if r := s.Handle(ctx, Command{App: "app-a", Job: "own", Action: "logs"}); r.Error != "pending_acceptance" {
		t.Fatal(r)
	}
}
func TestServiceEnqueueRestrictsRefsAndRecordsOrigin(t *testing.T) {
	s, _, _ := testService(t)
	called := false
	s.Enqueue = func(_ context.Context, a Application, o job.Origin) (job.Job, error) {
		called = true
		if o.App != a.ID || o.Source != "builder-a" || o.Request != "request-123" {
			t.Fatal(o)
		}
		return job.Job{ID: "queued", Origin: &o}, nil
	}
	for _, ref := range []string{"main", "refs/heads/other", "refs/tags/main", ""} {
		if r := s.Handle(context.Background(), Command{App: "app-a", Action: "enqueue", RequestID: "request-123", Ref: ref}); r.Error == "" {
			t.Fatal(ref)
		}
	}
	if called {
		t.Fatal("unauthorized ref enqueued")
	}
	r := s.Handle(context.Background(), Command{App: "app-a", Action: "enqueue", RequestID: "request-123", Ref: "refs/heads/main"})
	if r.Error != "" || !called {
		t.Fatal(r)
	}
}

func TestServiceCursorRevocationAndConfigurationRevision(t *testing.T) {
	s, jobs, snapshot := testService(t)
	ctx := context.Background()
	_ = jobs.store.Put(ctx, job.Job{ID: "own", State: job.Running, Origin: &job.Origin{Owner: "owner", Project: "project", Source: s.Catalog.Source, App: "app-a"}})
	for i := int64(1); i <= 3; i++ {
		_ = jobs.logs.Append("own", job.Entry{Seq: i, Text: "line"})
	}
	r := s.Handle(ctx, Command{Action: "logs", App: "app-a", Job: "own", After: 1, Limit: 1})
	entries := r.Data.(map[string]any)["entries"].([]job.Entry)
	if r.Error != "" || len(entries) != 1 || entries[0].Seq != 2 {
		t.Fatal(r)
	}
	if r := s.Handle(ctx, Command{Action: "logs", App: "app-a", Job: "own", After: -1}); r.Error == "" {
		t.Fatal("negative cursor")
	}
	if r := s.Handle(ctx, Command{Action: "abort", App: "app-a", Job: "own"}); r.Error != "" || len(jobs.aborted) != 1 {
		t.Fatal(r)
	}
	if r := s.Handle(ctx, Command{Action: "arbitrary", App: "app-a"}); r.Error == "" {
		t.Fatal(r)
	}
	if r := s.Handle(ctx, Command{Action: "status", App: "app-a"}); r.Data.(map[string]any)["accepted"] != true {
		t.Fatal(r)
	}
	snapshot.Revision = "new"
	_ = s.Catalog.Update(snapshot)
	if r := s.Handle(ctx, Command{Action: "status", App: "app-a"}); r.Error != "pending_acceptance" {
		t.Fatal(r)
	}
	if r := s.Handle(ctx, Command{Action: "status", App: "app-a", Revision: "new"}); r.Error != "" {
		t.Fatal(r)
	}
	_ = jobs.store.Delete(ctx, "own")
	if r := s.Handle(ctx, Command{Action: "logs", App: "app-a", Job: "own", Revision: "new"}); r.Error != "not_found" {
		t.Fatal("expired job", r)
	}
}

func TestUnavailableLocalCatalogueFailsClosed(t *testing.T) {
	s, _, _ := testService(t)
	s.Check = func(Application) error { return errors.New("local target changed") }
	r := s.Handle(context.Background(), Command{Action: "status", App: "app-a"})
	if r.Data.(map[string]any)["accepted"] != false {
		t.Fatal(r)
	}
	if r := s.Handle(context.Background(), Command{Action: "history", App: "app-a"}); r.Error == "" {
		t.Fatal(r)
	}
	if err := os.WriteFile(s.Catalog.Path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := s.Handle(context.Background(), Command{Action: "history", App: "app-a"}); r.Error != "unavailable" {
		t.Fatal(r)
	}
}

func TestTransferredApplicationCannotReadPreviousOwnersHistory(t *testing.T) {
	s, jobs, snapshot := testService(t)
	ctx := context.Background()
	_ = jobs.store.Put(ctx, job.Job{ID: "previous", Origin: &job.Origin{Source: s.Catalog.Source, App: "app-a", Owner: "owner", Project: "project"}})
	snapshot.Applications[0].OwnerID = "new-owner"
	_ = s.Catalog.Update(snapshot)
	a := snapshot.Applications[0]
	_ = s.Catalog.Accept(a.ID, a.Digest())
	if r := s.Handle(ctx, Command{Action: "job", App: a.ID, Job: "previous"}); r.Error != "not_found" {
		t.Fatal(r)
	}
	r := s.Handle(ctx, Command{Action: "history", App: a.ID})
	if r.Error != "" || len(r.Data.(map[string]any)["jobs"].([]job.Job)) != 0 {
		t.Fatal(r)
	}
}
func TestWebhookAssociationRequiresOneAcceptedMatch(t *testing.T) {
	s, _, snapshot := testService(t)
	ev := provider.Event{Provider: "github", FullName: "acme/app", Ref: "refs/heads/main", DeliveryID: "delivery"}
	origin, targets := s.Associate(ev)
	if origin == nil || origin.Owner != "owner" || origin.Request != "hook:delivery" || targets == nil {
		t.Fatal(origin, targets)
	}
	other := snapshot.Applications[0]
	other.ID = "app-b"
	snapshot.Applications = append(snapshot.Applications, other)
	_ = s.Catalog.Update(snapshot)
	_ = s.Catalog.Accept(other.ID, other.Digest())
	if origin, _ := s.Associate(ev); origin != nil {
		t.Fatal("ambiguous repository assigned", origin)
	}
	_ = s.Catalog.Revoke(other.ID)
	if origin, _ := s.Associate(ev); origin == nil {
		t.Fatal("unique association lost")
	}
	ev.Ref = "refs/heads/private"
	if origin, _ := s.Associate(ev); origin != nil {
		t.Fatal(origin)
	}
}

func TestHistoryPagesWithoutCrossingTenantBoundary(t *testing.T) {
	s, jobs, _ := testService(t)
	ctx := context.Background()
	for i, id := range []job.ID{"001", "002", "003"} {
		_ = jobs.store.Put(ctx, job.Job{ID: id, CreatedAt: time.Unix(int64(i), 0), Origin: &job.Origin{Source: s.Catalog.Source, App: "app-a", Owner: "owner", Project: "project"}})
	}
	r := s.Handle(ctx, Command{Action: "history", App: "app-a", Limit: 2})
	data := r.Data.(map[string]any)
	if data["next_cursor"] != "002" || len(data["jobs"].([]job.Job)) != 2 {
		t.Fatal(r)
	}
	r = s.Handle(ctx, Command{Action: "history", App: "app-a", Limit: 2, Before: "002"})
	data = r.Data.(map[string]any)
	if data["next_cursor"] != "" || data["jobs"].([]job.Job)[0].ID != "001" {
		t.Fatal(r)
	}
}

func TestPublicJobKeepsOnlyTypedDeploymentResults(t *testing.T) {
	j := safeJob(job.Job{Details: json.RawMessage(`{"secret":"never","targets":[{"name":"rowan","state":"succeeded","password":"never"}]}`)})
	if strings.Contains(string(j.Details), "never") || !strings.Contains(string(j.Details), "rowan") {
		t.Fatal(string(j.Details))
	}
	if j := safeJob(job.Job{Details: json.RawMessage(`{`)}); j.Details != nil {
		t.Fatal(j)
	}
}
func TestWebhookAssociationRefusesChangedLocalPolicyAndInstallation(t *testing.T) {
	s, _, snapshot := testService(t)
	ev := provider.Event{Provider: "github", FullName: "acme/app", Ref: "refs/heads/main", InstallationID: 9}
	s.Check = func(Application) error { return errors.New("changed") }
	if origin, _ := s.Associate(ev); origin != nil {
		t.Fatal(origin)
	}
	s.Check = nil
	snapshot.Applications[0].Repository.InstallationIDs = []int64{10}
	_ = s.Catalog.Update(snapshot)
	a := snapshot.Applications[0]
	_ = s.Catalog.Accept(a.ID, a.Digest())
	if origin, _ := s.Associate(ev); origin != nil {
		t.Fatal(origin)
	}
	ev.InstallationID = 10
	if origin, _ := s.Associate(ev); origin == nil || origin.Trigger != "webhook" {
		t.Fatal(origin)
	}
	_ = s.Catalog.Revoke(a.ID)
	if origin, _ := s.Associate(ev); origin != nil {
		t.Fatal(origin)
	}
}

func TestLargeLogsArePagedByBytesWithoutPrematureCompletion(t *testing.T) {
	s, jobs, _ := testService(t)
	ctx := context.Background()
	_ = jobs.store.Put(ctx, job.Job{ID: "own", State: job.Succeeded, Origin: &job.Origin{Source: s.Catalog.Source, App: "app-a", Owner: "owner", Project: "project"}})
	for i := int64(1); i <= 8; i++ {
		_ = jobs.logs.Append("own", job.Entry{Seq: i, Text: strings.Repeat("x", 200000)})
	}
	r := s.Handle(ctx, Command{Action: "logs", App: "app-a", Job: "own"})
	data := r.Data.(map[string]any)
	entries := data["entries"].([]job.Entry)
	if len(entries) == 8 || data["complete"] != false {
		t.Fatal("unbounded or prematurely complete")
	}
	raw, _ := json.Marshal(r)
	if len(raw) > 2<<20 {
		t.Fatal("frame too large")
	}
	r = s.Handle(ctx, Command{Action: "logs", App: "app-a", Job: "own", After: entries[len(entries)-1].Seq})
	if r.Data.(map[string]any)["complete"] != true {
		t.Fatal(r)
	}
}
