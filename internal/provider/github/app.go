package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// DefaultAPIBase is GitHub's REST API.
const DefaultAPIBase = "https://api.github.com"

// renewBefore is how long before expiry a cached installation token is replaced.
const renewBefore = 5 * time.Minute

// App is a GitHub App identity: it signs a short-lived JWT with its private key and exchanges it
// for installation tokens. Like the webhook verification, it uses only the standard library.
type App struct {
	ID      int64
	Key     *rsa.PrivateKey
	APIBase string
	HTTP    *http.Client
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// NewApp returns an App that talks to the public GitHub API.
func NewApp(id int64, key *rsa.PrivateKey) (*App, error) {
	if id <= 0 {
		return nil, errors.New("github app: the app id is required")
	}
	if key == nil {
		return nil, errors.New("github app: the private key is required")
	}
	return &App{ID: id, Key: key, APIBase: DefaultAPIBase, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}, nil
}

// ParsePrivateKey reads an RSA private key in PEM, PKCS#1 (what GitHub issues) or PKCS#8.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the PEM block is not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not RSA")
	}
	return rk, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt returns a token that identifies the App for at most 9 minutes (GitHub accepts up to 10).
func (a *App) jwt() (string, error) {
	now := a.Now()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // tolerate clock skew
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	signing := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the app token: %w", err)
	}
	return signing + "." + b64(sig), nil
}

// InstallationToken returns a token for the installation that can only read the contents of one
// repository. Tokens are cached until shortly before they expire.
func (a *App) InstallationToken(ctx context.Context, installationID int64, repo string) (string, time.Time, error) {
	key := fmt.Sprintf("%d/%s", installationID, repo)
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && c.expires.Sub(a.Now()) > renewBefore {
		a.mu.Unlock()
		return c.token, c.expires, nil
	}
	a.mu.Unlock()

	jwt, err := a.jwt()
	if err != nil {
		return "", time.Time{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"repositories": []string{repo},
		"permissions":  map[string]string{"contents": "read"},
	})
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.APIBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL error would repeat the endpoint; the cause is enough
		}
		return "", time.Time{}, fmt.Errorf("asking GitHub for an installation token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("GitHub refused the installation token: %s", http.StatusText(resp.StatusCode))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", time.Time{}, errors.New("GitHub answered with an unusable installation token")
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cachedToken{}
	}
	a.cache[key] = cachedToken{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, out.ExpiresAt, nil
}

// WithApp returns a copy of the provider that can also clone private repositories through app.
func (p *Provider) WithApp(app *App) *Provider {
	c := *p
	c.app = app
	return &c
}

// RepositoryURL implements provider.Cloner.
func (*Provider) RepositoryURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo + ".git"
}

// CloneCredentials implements provider.Cloner: an installation token limited to the event's
// repository, usable as a password with the user name x-access-token.
func (p *Provider) CloneCredentials(ctx context.Context, ev *provider.Event) (provider.Credentials, error) {
	if p.app == nil {
		return provider.Credentials{}, errors.New("github: no app is configured, so private repositories cannot be cloned")
	}
	if ev.InstallationID == 0 {
		return provider.Credentials{}, errors.New("github: the event has no installation to authenticate with")
	}
	tok, exp, err := p.app.InstallationToken(ctx, ev.InstallationID, ev.Repo)
	if err != nil {
		return provider.Credentials{}, err
	}
	return provider.Credentials{Username: "x-access-token", Password: tok, ExpiresAt: exp}, nil
}
