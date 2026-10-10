package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
)

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeAPI plays api.github.com for the installation-token endpoint and checks what it receives.
type fakeAPI struct {
	t        *testing.T
	pub      *rsa.PublicKey
	calls    atomic.Int32
	status   int
	expires  time.Time
	lastPath string
	lastBody map[string]any
	lastJWT  string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.lastPath = r.URL.Path
	auth := r.Header.Get("Authorization")
	f.lastJWT = strings.TrimPrefix(auth, "Bearer ")
	if !strings.HasPrefix(auth, "Bearer ") || !f.validJWT(f.lastJWT) {
		http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	f.lastBody = map[string]any{}
	_ = json.Unmarshal(raw, &f.lastBody)
	if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/vnd.github+json" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if f.status != 0 {
		http.Error(w, `{"message":"nope"}`, f.status)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installation_token", "expires_at": f.expires.UTC().Format(time.RFC3339)})
}

func (f *fakeAPI) validJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig) == nil
}

func claims(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	return c
}

func setup(t *testing.T) (*App, *fakeAPI, *time.Time) {
	t.Helper()
	key := newKey(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{t: t, pub: &key.PublicKey, expires: now.Add(time.Hour)}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	app := &App{ID: 424242, Key: key, APIBase: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}
	return app, api, &now
}

func TestInstallationTokenIsScopedToOneRepositoryAndReadOnly(t *testing.T) {
	app, api, _ := setup(t)
	tok, exp, err := app.InstallationToken(context.Background(), 777, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_installation_token" || exp.IsZero() {
		t.Fatalf("%q %v", tok, exp)
	}
	if api.lastPath != "/app/installations/777/access_tokens" {
		t.Fatalf("path %q", api.lastPath)
	}
	repos, _ := api.lastBody["repositories"].([]any)
	perms, _ := api.lastBody["permissions"].(map[string]any)
	if len(repos) != 1 || repos[0] != "shop" || len(perms) != 1 || perms["contents"] != "read" {
		t.Fatalf("a build only needs to read ONE repository's contents; the token must be limited to that: %v", api.lastBody)
	}
}

func TestTheJWTIsShortLivedAndSigned(t *testing.T) {
	app, api, now := setup(t)
	if _, _, err := app.InstallationToken(context.Background(), 1, "r"); err != nil {
		t.Fatal(err)
	}
	c := claims(t, api.lastJWT)
	iat, exp := int64(c["iat"].(float64)), int64(c["exp"].(float64))
	if c["iss"] != "424242" {
		t.Fatalf("iss %v", c["iss"])
	}
	if exp-now.Unix() > 600 || exp <= now.Unix() || iat > now.Unix() {
		t.Fatalf("GitHub rejects JWTs valid for more than 10 minutes; iat=%d exp=%d now=%d", iat, exp, now.Unix())
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(strings.Split(api.lastJWT, ".")[0])
	if !strings.Contains(string(hdr), `"RS256"`) {
		t.Fatalf("header %s", hdr)
	}
}

func TestTokensAreReusedUntilTheyAreAboutToExpire(t *testing.T) {
	app, api, now := setup(t)
	for i := 0; i < 3; i++ {
		if _, _, err := app.InstallationToken(context.Background(), 1, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if api.calls.Load() != 1 {
		t.Fatalf("a valid token must be reused, API called %d times", api.calls.Load())
	}
	if _, _, err := app.InstallationToken(context.Background(), 1, "other"); err != nil || api.calls.Load() != 2 {
		t.Fatalf("a token is scoped to a repository: another repository needs its own (%d calls, %v)", api.calls.Load(), err)
	}
	*now = now.Add(56 * time.Minute) // 4 minutes left: too close
	api.expires = now.Add(time.Hour)
	if _, _, err := app.InstallationToken(context.Background(), 1, "r"); err != nil || api.calls.Load() != 3 {
		t.Fatalf("a token about to expire must be renewed (%d calls, %v)", api.calls.Load(), err)
	}
}

func TestAPIErrorsAreReportedWithoutLeakingCredentials(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500} {
		app, api, _ := setup(t)
		api.status = status
		_, _, err := app.InstallationToken(context.Background(), 1, "r")
		if err == nil || !strings.Contains(err.Error(), http.StatusText(status)) {
			t.Fatalf("%d: %v", status, err)
		}
		if strings.Contains(err.Error(), api.lastJWT) || strings.Contains(err.Error(), "ghs_") {
			t.Fatalf("the error must not carry credentials: %v", err)
		}
	}
}

func TestACancelledContextStopsTheRequest(t *testing.T) {
	app, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := app.InstallationToken(ctx, 1, "r"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestPrivateKeyParsing(t *testing.T) {
	k := newKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der8, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8})
	for name, in := range map[string][]byte{"pkcs1 (what GitHub issues)": pkcs1, "pkcs8": pkcs8} {
		got, err := ParsePrivateKey(in)
		if err != nil || !got.Equal(k) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]string{"not pem": "hello", "empty": "", "wrong block": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"} {
		if _, err := ParsePrivateKey([]byte(in)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestProviderAsCloner(t *testing.T) {
	app, _, _ := setup(t)
	p, _ := New("secret")
	var _ provider.Cloner = p.WithApp(app) // compile-time: the GitHub provider offers the capability
	c := p.WithApp(app)

	if got := c.RepositoryURL("acme", "shop"); got != "https://github.com/acme/shop.git" {
		t.Fatal(got)
	}
	creds, err := c.CloneCredentials(context.Background(), &provider.Event{Owner: "acme", Repo: "shop", InstallationID: 5})
	if err != nil || creds.Username != "x-access-token" || creds.Password != "ghs_installation_token" || creds.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", creds, err)
	}
}

func TestClonerNeedsAnAppAndAnInstallation(t *testing.T) {
	p, _ := New("secret")
	if _, err := p.CloneCredentials(context.Background(), &provider.Event{Repo: "r", InstallationID: 1}); err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("without a configured App the provider cannot clone private repositories: %v", err)
	}
	app, _, _ := setup(t)
	if _, err := p.WithApp(app).CloneCredentials(context.Background(), &provider.Event{Repo: "r"}); err == nil || !strings.Contains(err.Error(), "installation") {
		t.Fatalf("an event without installation cannot be authenticated: %v", err)
	}
}

func TestWithAppDoesNotChangeTheOriginalProvider(t *testing.T) {
	p, _ := New("secret")
	app, _, _ := setup(t)
	_ = p.WithApp(app)
	if _, err := p.CloneCredentials(context.Background(), &provider.Event{Repo: "r", InstallationID: 1}); err == nil {
		t.Fatal("WithApp must return a new provider")
	}
}

func TestAnAppNeedsAnIDAndAKey(t *testing.T) {
	if _, err := NewApp(0, newKey(t)); err == nil {
		t.Fatal("app id")
	}
	if _, err := NewApp(1, nil); err == nil {
		t.Fatal("key")
	}
	a, err := NewApp(1, newKey(t))
	if err != nil || a.APIBase != "https://api.github.com" {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestOnlyRSAKeysAreAccepted(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ec)
	_, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err == nil || !strings.Contains(err.Error(), "RSA") {
		t.Fatalf("GitHub Apps sign with RSA: %v", err)
	}
}

func TestAMalformedAPIBaseIsReported(t *testing.T) {
	app, _, _ := setup(t)
	app.APIBase = "://bad"
	if _, _, err := app.InstallationToken(context.Background(), 1, "r"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAnUnreachableAPIIsReportedWithoutTheEndpoint(t *testing.T) {
	app, _, _ := setup(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	app.APIBase = srv.URL
	srv.Close()
	_, _, err := app.InstallationToken(context.Background(), 1, "r")
	if err == nil || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("%v", err)
	}
}

func TestAnUnusableAnswerIsAnError(t *testing.T) {
	for name, body := range map[string]string{"not json": "<html>", "no token": `{"expires_at":"2030-01-01T00:00:00Z"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(body))
		}))
		app, _, _ := setup(t)
		app.APIBase, app.HTTP = srv.URL, srv.Client()
		if _, _, err := app.InstallationToken(context.Background(), 1, "r"); err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Fatalf("%s: %v", name, err)
		}
		srv.Close()
	}
}

func TestCloneCredentialsPropagatesAPIErrors(t *testing.T) {
	app, api, _ := setup(t)
	api.status = 404
	p, _ := New("s")
	if _, err := p.WithApp(app).CloneCredentials(context.Background(), &provider.Event{Repo: "r", InstallationID: 9}); err == nil {
		t.Fatal("the failure to obtain a token must reach the caller")
	}
}
