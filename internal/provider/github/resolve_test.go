package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
)

const sha1a = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
const sha1b = "73876f4861cd3d187a4682290ab75c9dccadbc56"

// api plays the three endpoints ResolveRef uses, and records what it was asked.
type api struct {
	t         *testing.T
	mu        sync.Mutex
	paths     []string
	refs      map[string]string // "refs/heads/main" → sha
	installed bool
	tokenSeen string
	status    map[string]int // path prefix → forced status
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.paths = append(a.paths, r.Method+" "+r.URL.EscapedPath())
	a.mu.Unlock()
	for prefix, code := range a.status {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, `{"message":"forced"}`, code)
			return
		}
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation"):
		if !a.installed {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 4242})
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_tok", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case strings.Contains(r.URL.Path, "/commits/"):
		a.tokenSeen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ref := r.URL.Path[strings.Index(r.URL.Path, "/commits/")+len("/commits/"):]
		if sha, ok := a.refs[ref]; ok {
			json.NewEncoder(w).Encode(map[string]any{"sha": sha})
			return
		}
		http.Error(w, `{"message":"No commit found"}`, 422)
	default:
		http.NotFound(w, r)
	}
}

func resolver(t *testing.T) (*Provider, *api) {
	t.Helper()
	a := &api{t: t, installed: true, refs: map[string]string{"refs/heads/main": sha1a, "refs/tags/v1.0": sha1b, "refs/heads/release/2": sha1a, "refs/tags/main": sha1b}}
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

func TestResolveRefFindsBranchesAndTags(t *testing.T) {
	p, a := resolver(t)
	ctx := context.Background()
	ev, err := p.ResolveRef(ctx, "acme", "app", "main")
	if err != nil || ev.Commit != sha1a || ev.Branch != "main" || ev.Tag != "" || ev.Ref != "refs/heads/main" ||
		ev.InstallationID != 4242 || ev.FullName != "acme/app" || ev.Provider != "github" || ev.Kind != provider.KindPush || ev.DeliveryID != "" {
		t.Fatalf("%v %+v", err, ev)
	}
	if a.tokenSeen != "ghs_tok" {
		t.Fatal("the commit is asked for with the installation token, not with the App's own")
	}
	ev, err = p.ResolveRef(ctx, "acme", "app", "v1.0")
	if err != nil || ev.Commit != sha1b || ev.Tag != "v1.0" || ev.Branch != "" || ev.Ref != "refs/tags/v1.0" {
		t.Fatalf("a tag: %v %+v", err, ev)
	}
	if ev, err = p.ResolveRef(ctx, "acme", "app", "release/2"); err != nil || ev.Branch != "release/2" {
		t.Fatalf("a branch with a slash: %v %+v", err, ev)
	}
	// a name that is both: the branch wins, which the full reference can override
	if ev, _ = p.ResolveRef(ctx, "acme", "app", "main"); ev.Commit != sha1a {
		t.Fatalf("branch first: %+v", ev)
	}
	if ev, err = p.ResolveRef(ctx, "acme", "app", "refs/tags/main"); err != nil || ev.Commit != sha1b || ev.Tag != "main" {
		t.Fatalf("full reference: %v %+v", err, ev)
	}
}

func TestResolveRefErrors(t *testing.T) {
	p, a := resolver(t)
	ctx := context.Background()
	if _, err := p.ResolveRef(ctx, "acme", "app", "nope"); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := p.ResolveRef(ctx, "acme", "app", "refs/heads/nope"); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := p.ResolveRef(ctx, "acme", "app", "refs/pull/1/head"); err == nil || !strings.Contains(err.Error(), "neither a branch") {
		t.Fatalf("%v", err)
	}
	a.installed = false
	if _, err := p.ResolveRef(ctx, "acme", "app", "main"); err == nil || !strings.Contains(err.Error(), "not installed on acme/app") {
		t.Fatalf("%v", err)
	}
	for prefix, want := range map[string]string{"/repos/acme/app/installation": "could not tell", "/app/installations": "refused the installation token", "/repos/acme/app/commits": "could not resolve"} {
		p, a := resolver(t) // a fresh provider: installation tokens are cached
		a.status = map[string]int{prefix: 500}
		if _, err := p.ResolveRef(ctx, "acme", "app", "main"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", prefix, err)
		}
	}
	p, a = resolver(t)
	a.refs["refs/heads/main"] = "not-a-sha"
	if _, err := p.ResolveRef(ctx, "acme", "app", "main"); err == nil || !strings.Contains(err.Error(), "not a commit") {
		t.Fatalf("%v", err)
	}
}

func TestResolveRefRefusesWhatCouldBeSmuggledIntoTheRequest(t *testing.T) {
	p, a := resolver(t)
	for _, tc := range []struct{ owner, repo, ref string }{
		{"acme/evil", "app", "main"}, {"acme", "app/../x", "main"}, {"..", "app", "main"}, {"acme", ".", "main"}, {"", "app", "main"},
		{"acme", "app", ""}, {"acme", "app", "../../x"}, {"acme", "app", "-flag"}, {"acme", "app", "a//b"}, {"acme", "app", "a/"},
		{"acme", "app", "a b"}, {"acme", "app", "a?b=c"}, {"acme", "app", "a#b"}, {"acme", "app", "a%2fb"},
	} {
		if _, err := p.ResolveRef(context.Background(), tc.owner, tc.repo, tc.ref); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
	if len(a.paths) != 0 {
		t.Fatalf("nothing may reach GitHub for these: %v", a.paths)
	}
}

func TestResolveRefNeedsAnApp(t *testing.T) {
	p, _ := New("secret")
	if _, err := p.ResolveRef(context.Background(), "a", "b", "main"); err == nil || !strings.Contains(err.Error(), "no app is configured") {
		t.Fatalf("%v", err)
	}
	var _ provider.Resolver = p
}

func TestResolveRefAnUnusableAnswer(t *testing.T) {
	p, a := resolver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/installation") {
			w.Write([]byte("<html>not json"))
			return
		}
		a.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p.app.APIBase = srv.URL
	if _, err := p.ResolveRef(context.Background(), "acme", "app", "main"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("%v", err)
	}
	p.app.APIBase = "http://127.0.0.1:1"
	if _, err := p.ResolveRef(context.Background(), "acme", "app", "main"); err == nil {
		t.Fatal("unreachable API")
	}
	if _, err := p.app.getJSON(context.Background(), "x", "/p", nil); err == nil {
		t.Fatal("unreachable API")
	}
}

func TestResolveRefWithAKeyThatCannotSign(t *testing.T) {
	p, _ := resolver(t)
	p.app.Key = &rsa.PrivateKey{} // no usable key material: signing fails
	if _, err := p.ResolveRef(context.Background(), "acme", "app", "main"); err == nil || !strings.Contains(err.Error(), "signing") {
		t.Fatalf("%v", err)
	}
}

func TestResolveRefReportsBrokenPlumbing(t *testing.T) {
	ctx := context.Background()
	// an API address that is not a URL
	p, _ := resolver(t)
	p.app.APIBase = "http://bad host"
	if _, err := p.ResolveRef(ctx, "acme", "app", "main"); err == nil {
		t.Fatal("a malformed API address")
	}
	// the connection breaks while the commit is being asked for
	p, a := resolver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/") {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		a.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p.app.APIBase = srv.URL
	if _, err := p.ResolveRef(ctx, "acme", "app", "main"); err == nil {
		t.Fatal("a broken connection is an error, not 'no such branch'")
	}
}
