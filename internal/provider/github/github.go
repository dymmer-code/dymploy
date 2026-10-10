// Package github implements the webhook side of the GitHub provider: authenticating a delivery
// (X-Hub-Signature-256) and normalizing push events. It uses only the standard library on purpose:
// the code that decides whether a request is trusted should have no third-party dependencies.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// Provider is the GitHub provider.
type Provider struct{ secret []byte }

// New returns a provider that authenticates deliveries with the webhook secret configured in the
// GitHub App. An empty secret would accept anything, so it is an error.
func New(webhookSecret string) (*Provider, error) {
	if webhookSecret == "" {
		return nil, errors.New("github: the webhook secret must not be empty")
	}
	return &Provider{secret: []byte(webhookSecret)}, nil
}

// Name implements provider.Provider.
func (*Provider) Name() string { return "github" }

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		Name     string `json:"name"`
		CloneURL string `json:"clone_url"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// ParseWebhook implements provider.Provider. The signature is verified against the exact bytes
// received before anything else is looked at, so a forged request learns nothing.
func (p *Provider) ParseWebhook(r *http.Request) (*provider.Event, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if !p.validSignature(r.Header.Get("X-Hub-Signature-256"), body) {
		return nil, provider.ErrUnauthorized
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		return nil, fmt.Errorf("%w: %q", provider.ErrIgnored, r.Header.Get("X-GitHub-Event"))
	}
	var pl pushPayload
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrMalformed, err)
	}
	owner, repo := pl.Repository.Owner.Login, pl.Repository.Name
	if pl.Ref == "" || pl.After == "" || owner == "" || repo == "" {
		return nil, fmt.Errorf("%w: push without ref, commit or repository", provider.ErrMalformed)
	}
	branch, tag := provider.SplitRef(pl.Ref)
	return &provider.Event{
		Provider: "github", DeliveryID: r.Header.Get("X-GitHub-Delivery"), Kind: provider.KindPush,
		Owner: owner, Repo: repo, FullName: owner + "/" + repo,
		Ref: pl.Ref, Branch: branch, Tag: tag, Commit: pl.After, Deleted: pl.Deleted,
		InstallationID: pl.Installation.ID, CloneURL: pl.Repository.CloneURL,
	}, nil
}

// validSignature checks "sha256=<hex>" with a constant-time comparison.
func (p *Provider) validSignature(header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	m := hmac.New(sha256.New, p.secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}
