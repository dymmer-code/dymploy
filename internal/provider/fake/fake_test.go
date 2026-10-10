package fake

import (
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
