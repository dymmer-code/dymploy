package fake

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/provider"
)

func TestFakeProviderSpeaksTheCommonContract(t *testing.T) {
	var _ provider.Provider = New("tok") // compile-time proof that the interface is enough

	body := `{"delivery":"d1","owner":"acme","repo":"app","ref":"refs/heads/main","commit":"abc123"}`
	r := httptest.NewRequest(http.MethodPost, "/hooks/fake", strings.NewReader(body))
	r.Header.Set("X-Fake-Token", "tok")
	ev, err := New("tok").ParseWebhook(r)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Provider != "fake" || ev.Owner != "acme" || ev.Repo != "app" || ev.FullName != "acme/app" ||
		ev.Branch != "main" || ev.Commit != "abc123" || ev.DeliveryID != "d1" || ev.Kind != provider.KindPush {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestFakeProviderRejectsABadToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/hooks/fake", strings.NewReader(`{}`))
	r.Header.Set("X-Fake-Token", "nope")
	if _, err := New("tok").ParseWebhook(r); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestFakeProviderRejectsIncompletePayloads(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"owner":"a","repo":"b","ref":"refs/heads/main"}`} {
		r := httptest.NewRequest(http.MethodPost, "/hooks/fake", strings.NewReader(body))
		r.Header.Set("X-Fake-Token", "tok")
		if _, err := New("tok").ParseWebhook(r); !errors.Is(err, provider.ErrMalformed) {
			t.Fatalf("%q: got %v", body, err)
		}
	}
}

func TestFakeName(t *testing.T) {
	if New("x").Name() != "fake" {
		t.Fatal("name")
	}
}

func TestFakeProviderAsCloner(t *testing.T) {
	var _ provider.Cloner = New("tok").WithBase("/srv/git")
	p := New("tok").WithBase("/srv/git")
	if got := p.RepositoryURL("acme", "app"); got != "/srv/git/acme/app.git" {
		t.Fatal(got)
	}
	creds, err := p.CloneCredentials(context.Background(), &provider.Event{Owner: "acme", Repo: "app"})
	if err != nil || creds.Username == "" || creds.Password == "" {
		t.Fatalf("%+v %v", creds, err)
	}
	if _, err := New("tok").CloneCredentials(context.Background(), &provider.Event{}); err == nil {
		t.Fatal("without a base directory the fake cannot clone")
	}
}

func TestFakeResolvesReferences(t *testing.T) {
	p := New("t").WithRefs(map[string]string{"acme/app:refs/heads/main": "aaaa", "acme/app:refs/tags/v1": "bbbb"})
	ev, err := p.ResolveRef(context.Background(), "acme", "app", "main")
	if err != nil || ev.Commit != "aaaa" || ev.Branch != "main" || ev.Ref != "refs/heads/main" || ev.FullName != "acme/app" {
		t.Fatalf("%v %+v", err, ev)
	}
	if ev, err = p.ResolveRef(context.Background(), "acme", "app", "v1"); err != nil || ev.Tag != "v1" || ev.Commit != "bbbb" {
		t.Fatalf("%v %+v", err, ev)
	}
	if ev, err = p.ResolveRef(context.Background(), "acme", "app", "refs/tags/v1"); err != nil || ev.Commit != "bbbb" {
		t.Fatalf("%v %+v", err, ev)
	}
	if _, err = p.ResolveRef(context.Background(), "acme", "app", "nope"); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("%v", err)
	}
}
