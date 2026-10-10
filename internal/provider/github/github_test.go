package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/provider"
)

const secret = "s3cr3t-shared-with-github"

const pushBody = `{
 "ref": "refs/heads/main",
 "after": "1111111111111111111111111111111111111111",
 "deleted": false,
 "repository": {"name": "shop", "full_name": "acme/shop", "clone_url": "https://github.com/acme/shop.git", "owner": {"login": "acme"}},
 "installation": {"id": 4242}
}`

func sign(body, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func req(event, body, signature string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	if signature != "" {
		r.Header.Set("X-Hub-Signature-256", signature)
	}
	return r
}

func newProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestName(t *testing.T) {
	if newProvider(t).Name() != "github" {
		t.Fatal("the provider name is part of the webhook route and of the allow-list")
	}
}

func TestPushIsNormalized(t *testing.T) {
	ev, err := newProvider(t).ParseWebhook(req("push", pushBody, sign(pushBody, secret)))
	if err != nil {
		t.Fatal(err)
	}
	want := provider.Event{
		Provider: "github", DeliveryID: "delivery-1", Kind: provider.KindPush,
		Owner: "acme", Repo: "shop", FullName: "acme/shop",
		Ref: "refs/heads/main", Branch: "main", Commit: "1111111111111111111111111111111111111111",
		InstallationID: 4242, CloneURL: "https://github.com/acme/shop.git",
	}
	if *ev != want {
		t.Fatalf("got  %+v\nwant %+v", *ev, want)
	}
}

func TestTagPush(t *testing.T) {
	body := strings.Replace(pushBody, "refs/heads/main", "refs/tags/v1.2.0", 1)
	ev, err := newProvider(t).ParseWebhook(req("push", body, sign(body, secret)))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Branch != "" || ev.Tag != "v1.2.0" {
		t.Fatalf("branch=%q tag=%q", ev.Branch, ev.Tag)
	}
}

func TestDeletedBranchIsFlagged(t *testing.T) {
	body := strings.Replace(pushBody, `"deleted": false`, `"deleted": true`, 1)
	ev, err := newProvider(t).ParseWebhook(req("push", body, sign(body, secret)))
	if err != nil || !ev.Deleted {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
}

func TestSignatureFailures(t *testing.T) {
	p := newProvider(t)
	cases := map[string]string{
		"missing":          "",
		"wrong key":        sign(pushBody, "another-secret"),
		"wrong body":       sign(pushBody+" ", secret),
		"no prefix":        strings.TrimPrefix(sign(pushBody, secret), "sha256="),
		"sha1 prefix":      "sha1=" + strings.TrimPrefix(sign(pushBody, secret), "sha256="),
		"not hex":          "sha256=zzzz",
		"empty digest":     "sha256=",
		"uppercase prefix": "SHA256=" + strings.TrimPrefix(sign(pushBody, secret), "sha256="),
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseWebhook(req("push", pushBody, sig))
			if !errors.Is(err, provider.ErrUnauthorized) {
				t.Fatalf("must be unauthorized, got %v", err)
			}
		})
	}
}

func TestSignatureIsCheckedBeforeAnythingElse(t *testing.T) {
	// a forged request must not learn whether its event type or body would have been accepted
	_, err := newProvider(t).ParseWebhook(req("deployment", "not json", "sha256=00"))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestMalformedBodyWithValidSignature(t *testing.T) {
	_, err := newProvider(t).ParseWebhook(req("push", "{nope", sign("{nope", secret)))
	if !errors.Is(err, provider.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestIncompletePushIsMalformed(t *testing.T) {
	for _, body := range []string{`{}`, `{"ref":"refs/heads/main"}`, `{"ref":"refs/heads/main","after":"abc","repository":{"name":"x"}}`} {
		_, err := newProvider(t).ParseWebhook(req("push", body, sign(body, secret)))
		if !errors.Is(err, provider.ErrMalformed) {
			t.Fatalf("%s: got %v", body, err)
		}
	}
}

func TestOtherEventsAreIgnored(t *testing.T) {
	for _, event := range []string{"ping", "issues", "pull_request", "deployment", ""} {
		_, err := newProvider(t).ParseWebhook(req(event, "{}", sign("{}", secret)))
		if !errors.Is(err, provider.ErrIgnored) {
			t.Fatalf("event %q: got %v", event, err)
		}
	}
}

func TestNewRejectsAnEmptySecret(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("a provider without secret would accept anything")
	}
}
