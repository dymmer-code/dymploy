package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// statusAPI plays the two endpoints PublishStatus uses and records what it is asked.
type statusAPI struct {
	mu         sync.Mutex
	tokenAsked map[string]any
	status     map[string]string
	statusPath string
	bearer     string
	tokenCode  int
	statusCode int
}

func (a *statusAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		a.tokenAsked = map[string]any{}
		json.Unmarshal(raw, &a.tokenAsked)
		if a.tokenCode != 0 {
			http.Error(w, `{"message":"no"}`, a.tokenCode)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_status", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case strings.Contains(r.URL.Path, "/statuses/"):
		a.bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a.statusPath = r.URL.Path
		a.status = map[string]string{}
		json.Unmarshal(raw, &a.status)
		if a.statusCode != 0 {
			http.Error(w, `{"message":"no"}`, a.statusCode)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func statusProvider(t *testing.T) (*Provider, *statusAPI) {
	t.Helper()
	a := &statusAPI{}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	app, err := NewApp(7, newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	app.APIBase = srv.URL
	p, _ := New("secret")
	return p.WithApp(app), a
}

var statusEvent = provider.Event{Provider: "github", Owner: "acme", Repo: "shop", Commit: sha1a, InstallationID: 99}

func TestPublishStatusShowsTheStateNextToTheCommit(t *testing.T) {
	p, a := statusProvider(t)
	ev := statusEvent
	if err := p.PublishStatus(context.Background(), &ev, provider.StateSuccess, "Run #2 succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if a.statusPath != "/repos/acme/shop/statuses/"+sha1a || a.status["state"] != "success" || a.status["context"] != "dymploy" ||
		a.status["description"] != "Run #2 succeeded" || a.bearer != "ghs_status" {
		t.Fatalf("%s %v %s", a.statusPath, a.status, a.bearer)
	}
	if _, has := a.status["target_url"]; has {
		t.Fatal("no link was given, none is sent")
	}
	perms, _ := a.tokenAsked["permissions"].(map[string]any)
	repos, _ := a.tokenAsked["repositories"].([]any)
	if len(perms) != 1 || perms["statuses"] != "write" || len(repos) != 1 || repos[0] != "shop" {
		t.Fatalf("the token is limited to one repository and to the status permission only: %v", a.tokenAsked)
	}
	if err := p.PublishStatus(context.Background(), &ev, provider.StateFailure, "x", "https://dymploy.example/runs/2"); err != nil || a.status["target_url"] != "https://dymploy.example/runs/2" || a.status["state"] != "failure" {
		t.Fatalf("%v %v", err, a.status)
	}
	for _, s := range []provider.State{provider.StatePending, provider.StateError} {
		if err := p.PublishStatus(context.Background(), &ev, s, "x", ""); err != nil || a.status["state"] != string(s) {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestPublishStatusDescriptionsAreCutToWhatGitHubAccepts(t *testing.T) {
	p, a := statusProvider(t)
	ev := statusEvent
	long := strings.Repeat("é", 300)
	if err := p.PublishStatus(context.Background(), &ev, provider.StatePending, long, ""); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(a.status["description"])); n != 140 || !strings.HasSuffix(a.status["description"], "…") {
		t.Fatalf("%d runes: %q", n, a.status["description"])
	}
}

func TestPublishStatusProblems(t *testing.T) {
	ctx := context.Background()
	p, a := statusProvider(t)
	ev := statusEvent
	a.statusCode = 403
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil || !strings.Contains(err.Error(), "refused the status") {
		t.Fatalf("%v", err)
	}
	// the installation has not accepted the permission: the token itself is refused
	p, a = statusProvider(t)
	a.tokenCode = 422
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil || !strings.Contains(err.Error(), "installation token") {
		t.Fatalf("%v", err)
	}
	p, _ = statusProvider(t)
	for name, bad := range map[string]provider.Event{
		"no installation": {Owner: "a", Repo: "b", Commit: sha1a},
		"bad commit":      {Owner: "a", Repo: "b", Commit: "main", InstallationID: 1},
		"bad owner":       {Owner: "a/b", Repo: "b", Commit: sha1a, InstallationID: 1},
		"bad repo":        {Owner: "a", Repo: "../x", Commit: sha1a, InstallationID: 1},
	} {
		e := bad
		if err := p.PublishStatus(ctx, &e, provider.StateSuccess, "x", ""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := p.PublishStatus(ctx, &ev, provider.State("bogus"), "x", ""); err == nil {
		t.Fatal("an unknown state")
	}
	bare, _ := New("secret")
	if err := bare.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil || !strings.Contains(err.Error(), "no app is configured") {
		t.Fatalf("%v", err)
	}
	p.app.APIBase = "http://127.0.0.1:1"
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil {
		t.Fatal("unreachable API")
	}
	// the token works but the status call cannot connect
	p, a = statusProvider(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/statuses/") {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		a.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p.app.APIBase = srv.URL
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil || !strings.Contains(err.Error(), "publishing the status") {
		t.Fatalf("%v", err)
	}
	p.app.APIBase = "http://bad host"
	p.app.cache = nil
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err == nil {
		t.Fatal("a malformed API address")
	}
}

func TestTheContentTokenAndTheStatusTokenAreNotMixedUp(t *testing.T) {
	p, a := statusProvider(t)
	ctx := context.Background()
	if _, _, err := p.app.InstallationToken(ctx, 99, "shop"); err != nil {
		t.Fatal(err)
	}
	if perms, _ := a.tokenAsked["permissions"].(map[string]any); perms["contents"] != "read" || len(perms) != 1 {
		t.Fatalf("cloning still gets read-only contents: %v", a.tokenAsked)
	}
	ev := statusEvent
	if err := p.PublishStatus(ctx, &ev, provider.StateSuccess, "x", ""); err != nil {
		t.Fatal(err)
	}
	if perms, _ := a.tokenAsked["permissions"].(map[string]any); perms["statuses"] != "write" || len(perms) != 1 {
		t.Fatalf("a status gets its own token, a cached one for contents is not reused: %v", a.tokenAsked)
	}
}
