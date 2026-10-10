package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// fakeJobs is an in-memory Jobs.
type fakeJobs struct {
	mu      sync.Mutex
	jobs    map[job.ID]job.Job
	order   []job.ID
	logs    map[job.ID][]job.Entry
	subs    map[int]chan Event
	next    int
	cancels []job.ID
}

func newFake() *fakeJobs {
	return &fakeJobs{jobs: map[job.ID]job.Job{}, logs: map[job.ID][]job.Entry{}, subs: map[int]chan Event{}}
}

func (f *fakeJobs) add(j job.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[j.ID]; !ok {
		f.order = append([]job.ID{j.ID}, f.order...)
	}
	f.jobs[j.ID] = j
}

func (f *fakeJobs) line(id job.ID, text string) job.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := job.Entry{Seq: int64(len(f.logs[id]) + 1), Time: time.Unix(1_700_000_000, 0).UTC(), Stream: runner.Stdout, Text: text}
	f.logs[id] = append(f.logs[id], e)
	return e
}

func (f *fakeJobs) emit(ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fakeJobs) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *fakeJobs) List(_ context.Context, fl job.Filter) ([]job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []job.Job
	for _, id := range f.order {
		if j := f.jobs[id]; fl.Matches(j) {
			out = append(out, j)
		}
	}
	if fl.Limit > 0 && len(out) > fl.Limit {
		out = out[:fl.Limit]
	}
	return out, nil
}

func (f *fakeJobs) Get(_ context.Context, id job.ID) (job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return job.Job{}, job.ErrNotFound
	}
	return j, nil
}

func (f *fakeJobs) Logs(id job.ID, after int64, limit int) ([]job.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []job.Entry
	for _, e := range f.logs[id] {
		if e.Seq > after && (limit == 0 || len(out) < limit) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeJobs) Cancel(_ context.Context, id job.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	switch {
	case !ok:
		return job.ErrNotFound
	case j.State.Final():
		return errors.New(ErrConflict.Error() + ": the job has already finished")
	}
	f.cancels = append(f.cancels, id)
	return nil
}

func (f *fakeJobs) Retry(_ context.Context, id job.ID) (job.Job, error) {
	f.mu.Lock()
	j, ok := f.jobs[id]
	f.mu.Unlock()
	switch {
	case !ok:
		return job.Job{}, job.ErrNotFound
	case !j.State.Final():
		return job.Job{}, errors.New(ErrConflict.Error() + ": only a finished job can be retried")
	}
	n := job.Job{ID: id + "-r", Name: j.Name, Kind: j.Kind, State: job.Queued, RetryOf: id, Params: j.Params, Input: j.Input}
	f.add(n)
	return n, nil
}

func (f *fakeJobs) Subscribe() (<-chan Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next
	f.next++
	ch := make(chan Event, 256)
	f.subs[id] = ch
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.subs[id]; ok {
			delete(f.subs, id)
			close(c)
		}
	}
}

func code(n int) *int { return &n }

func withJobs(t *testing.T) (*fakeJobs, *Client) {
	t.Helper()
	f := newFake()
	p := sockPath(t)
	stop := startWith(t, p, Handlers{Status: fixed, Jobs: f})
	t.Cleanup(func() { stop() })
	return f, &Client{Socket: p}
}

func TestJobsListGetLogsOverTheSocket(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "a", Name: "github/acme/one", Kind: "docker", State: job.Succeeded, ExitCode: code(0), Params: json.RawMessage(`{"secret":"p"}`), Input: json.RawMessage(`{"x":1}`)})
	f.add(job.Job{ID: "b", Name: "github/acme/two", Kind: "docker", State: job.Running})
	f.add(job.Job{ID: "c", Name: "github/acme/one", Kind: "docker", State: job.Failed, ExitCode: code(5)})
	for i := 1; i <= 5; i++ {
		f.line("a", "line")
	}
	ctx := context.Background()

	all, err := c.Jobs(ctx, job.Filter{})
	if err != nil || len(all) != 3 || all[0].ID != "c" {
		t.Fatalf("%v %+v", err, all)
	}
	if got, _ := c.Jobs(ctx, job.Filter{States: []job.State{job.Running, job.Failed}}); len(got) != 2 {
		t.Fatalf("state filter: %+v", got)
	}
	if got, _ := c.Jobs(ctx, job.Filter{Name: "github/acme/one", Limit: 1}); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("name and limit: %+v", got)
	}
	if got, err := c.Jobs(ctx, job.Filter{Name: "nothing"}); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty list is an empty list: %#v %v", got, err)
	}
	j, err := c.Job(ctx, "a")
	if err != nil || j.Name != "github/acme/one" || *j.ExitCode != 0 {
		t.Fatalf("%v %+v", err, j)
	}
	if j.Params != nil || j.Input != nil {
		t.Fatalf("the parameters and input of a job are not part of the API: %s %s", j.Params, j.Input)
	}
	if _, err := c.Job(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	logs, err := c.Logs(ctx, "a", 2, 2)
	if err != nil || len(logs) != 2 || logs[0].Seq != 3 {
		t.Fatalf("%v %+v", err, logs)
	}
	if logs, err := c.Logs(ctx, "b", 0, 10); err != nil || logs == nil || len(logs) != 0 {
		t.Fatalf("no log yet: %#v %v", logs, err)
	}
	if _, err := c.Logs(ctx, "nope", 0, 10); err == nil {
		t.Fatal("unknown job")
	}
}

func TestBadQueriesAreRefused(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer((&API{Jobs: f}).Handler())
	defer srv.Close()
	f.add(job.Job{ID: "a", State: job.Succeeded})
	for _, path := range []string{
		"/v1/jobs?state=bogus", "/v1/jobs?limit=-1", "/v1/jobs?limit=x", "/v1/jobs?limit=5000",
		"/v1/jobs/a/logs?after=-3", "/v1/jobs/a/logs?after=x", "/v1/jobs/a/logs?limit=0", "/v1/jobs/a/logs?limit=999999",
		"/v1/events?after=x",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s answered %d", path, resp.StatusCode)
		}
	}
	if resp, _ := http.Get(srv.URL + "/v1/events?job=ghost"); resp.StatusCode != 404 {
		t.Errorf("a watch of an unknown job answered %d", resp.StatusCode)
	}
}

func TestCancelAndRetry(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "run", Name: "n", State: job.Running})
	f.add(job.Job{ID: "done", Name: "n", State: job.Failed, Input: json.RawMessage(`{"a":1}`)})
	ctx := context.Background()
	if msg, err := c.Cancel(ctx, "run"); err != nil || !strings.Contains(msg, "run") || len(f.cancels) != 1 {
		t.Fatalf("%q %v", msg, err)
	}
	if _, err := c.Cancel(ctx, "done"); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("a conflict is explained, without the sentinel's own words: %v", err)
	}
	if _, err := c.Cancel(ctx, "ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	n, err := c.Retry(ctx, "done")
	if err != nil || n.RetryOf != "done" || n.State != job.Queued || n.Input != nil {
		t.Fatalf("%v %+v", err, n)
	}
	if _, err := c.Retry(ctx, "run"); err == nil || !strings.Contains(err.Error(), "only a finished job") {
		t.Fatalf("%v", err)
	}
}

func TestADaemonWithoutJobsSaysSo(t *testing.T) {
	p := sockPath(t)
	stop := start(t, p, fixed)
	defer stop()
	c := &Client{Socket: p}
	if _, err := c.Jobs(context.Background(), job.Filter{}); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("%v", err)
	}
	if err := c.Watch(context.Background(), "", 0, func(Message) error { return nil }); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("%v", err)
	}
}

func TestErrorsFromTheDaemonAreReadableWhenItIsNotThere(t *testing.T) {
	c := &Client{Socket: sockPath(t)}
	ctx := context.Background()
	if _, err := c.Jobs(ctx, job.Filter{}); err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Fatalf("%v", err)
	}
	if err := c.Watch(ctx, "", 0, func(Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Fatalf("%v", err)
	}
}

// collect runs Watch in the background and returns what it receives.
type watcher struct {
	msgs chan Message
	done chan error
	stop context.CancelFunc
}

func watch(t *testing.T, c *Client, jobID string, after int64) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{msgs: make(chan Message, 100), done: make(chan error, 1), stop: cancel}
	go func() {
		w.done <- c.Watch(ctx, jobID, after, func(m Message) error { w.msgs <- m; return nil })
	}()
	t.Cleanup(cancel)
	return w
}

func (w *watcher) next(t *testing.T) Message {
	t.Helper()
	select {
	case m := <-w.msgs:
		return m
	case err := <-w.done:
		t.Fatalf("the channel ended while waiting for a message: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	return Message{}
}

func waitSubscribers(t *testing.T, f *fakeJobs, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.subscribers() != n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d, want %d", f.subscribers(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheLiveChannelStartsWithASnapshotAndThenStreams(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "old", State: job.Succeeded})
	f.add(job.Job{ID: "q", Name: "n1", State: job.Queued, Input: json.RawMessage(`{"x":1}`)})
	f.add(job.Job{ID: "r", Name: "n2", State: job.Running})
	w := watch(t, c, "", 0)
	snap := w.next(t)
	if snap.Type != "snapshot" || len(snap.Jobs) != 2 || snap.Jobs[0].Input != nil {
		t.Fatalf("snapshot: %+v", snap)
	}
	waitSubscribers(t, f, 1)
	e := f.line("r", "hello")
	f.emit(Event{Type: "output", Job: job.Job{ID: "r", State: job.Running}, Entry: &e})
	f.emit(Event{Type: "finished", Job: job.Job{ID: "r", State: job.Succeeded}})
	if m := w.next(t); m.Type != "output" || m.Entry.Text != "hello" || m.Job.ID != "r" {
		t.Fatalf("%+v", m)
	}
	if m := w.next(t); m.Type != "finished" || m.Job.State != job.Succeeded {
		t.Fatalf("%+v", m)
	}
	w.stop()
	if err := <-w.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	waitSubscribers(t, f, 0)
}

func TestWatchingAJobReplaysItsLogThenFollowsAndEndsWhenItFinishes(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "r", Name: "n", State: job.Running})
	other := f.line("other", "not mine")
	f.line("r", "one")
	f.line("r", "two")
	f.line("r", "three")
	w := watch(t, c, "r", 1) // already saw line 1
	if m := w.next(t); m.Entry.Seq != 2 || m.Entry.Text != "two" {
		t.Fatalf("%+v", m)
	}
	if m := w.next(t); m.Entry.Seq != 3 {
		t.Fatalf("%+v", m)
	}
	waitSubscribers(t, f, 1)
	dup := job.Entry{Seq: 3, Stream: runner.Stdout, Text: "three"} // arrived both ways: must be shown once
	f.emit(Event{Type: "output", Job: job.Job{ID: "r"}, Entry: &dup})
	f.emit(Event{Type: "output", Job: job.Job{ID: "other"}, Entry: &other})
	fresh := f.line("r", "four")
	f.emit(Event{Type: "output", Job: job.Job{ID: "r"}, Entry: &fresh})
	f.emit(Event{Type: "finished", Job: job.Job{ID: "r", State: job.Failed}})
	if m := w.next(t); m.Entry == nil || m.Entry.Text != "four" {
		t.Fatalf("a duplicate or another job's line got through: %+v", m)
	}
	if m := w.next(t); m.Type != "finished" {
		t.Fatalf("%+v", m)
	}
	select {
	case err := <-w.done:
		if err != nil {
			t.Fatalf("a watched job that finishes ends the channel normally: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the channel stayed open")
	}
}

func TestWatchingAFinishedJobReplaysAndCloses(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "d", State: job.Succeeded})
	for i := 0; i < 700; i++ { // more than one page of replay
		f.line("d", "x")
	}
	w := watch(t, c, "d", 0)
	n := 0
	for {
		select {
		case m := <-w.msgs:
			if m.Type == "output" {
				n++
			}
			continue
		case err := <-w.done:
			if err != nil || n != 700 {
				t.Fatalf("err=%v replayed=%d", err, n)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("timeout")
		}
	}
}

func TestACallbackErrorEndsTheWatch(t *testing.T) {
	f, c := withJobs(t)
	f.add(job.Job{ID: "r", State: job.Running})
	boom := errors.New("enough")
	err := c.Watch(context.Background(), "", 0, func(Message) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	waitSubscribers(t, f, 0)
}

func TestTooManyLiveChannelsAreRefused(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer((&API{Jobs: f, MaxStreams: 1}).Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c1, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.CloseNow()
	waitSubscribers(t, f, 1)
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/events", nil)
	if err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("second channel: %v %v", err, resp)
	}
	c1.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c3, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/events", nil)
		if err == nil {
			c3.CloseNow()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was not released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReadOnlyAPIHasNoWriteEndpoints(t *testing.T) {
	f := newFake()
	f.add(job.Job{ID: "run", State: job.Running})
	srv := httptest.NewServer((&API{Jobs: f, ReadOnly: true}).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/jobs/run/cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 || len(f.cancels) != 0 {
		t.Fatalf("a read-only API cancelled something: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(srv.URL + "/v1/jobs"); resp.StatusCode != 200 {
		t.Fatalf("reading still works: %d", resp.StatusCode)
	}
}

func TestRequireToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	var denied int
	h := RequireToken("s3cret-token", ok, func(*http.Request) { denied++ })
	for name, tc := range map[string]struct {
		header string
		want   int
	}{
		"right":        {"Bearer s3cret-token", 204},
		"wrong":        {"Bearer s3cret-tokeN", 401},
		"no scheme":    {"s3cret-token", 401},
		"other scheme": {"Basic czNjcmV0LXRva2Vu", 401},
		"missing":      {"", 401},
		"empty bearer": {"Bearer ", 401},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d", w.Code)
			}
			if tc.want == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("a 401 must say how to authenticate")
			}
			if (tc.want == 401) != (denied == 1) {
				t.Fatalf("every refusal is reported, and only refusals: denied=%d", denied)
			}
			denied = 0
		})
	}
}

func TestOnlyTheListedWebOriginsMayOpenTheChannel(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer((&API{Jobs: f, OriginPatterns: []string{"example.com"}}).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/events"
	ctx := context.Background()
	hdr := func(origin string) *websocket.DialOptions {
		return &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}}
	}
	if c, _, err := websocket.Dial(ctx, url, hdr("https://example.com")); err != nil {
		t.Fatalf("the allowed origin: %v", err)
	} else {
		c.CloseNow()
	}
	if _, resp, err := websocket.Dial(ctx, url, hdr("https://evil.example")); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("another origin must be refused: %v %v", err, resp)
	}
}
