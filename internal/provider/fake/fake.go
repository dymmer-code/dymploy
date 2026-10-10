// Package fake is a minimal second provider. It exists to prove that nothing outside the provider
// packages depends on a particular host, and to give other packages' tests a provider to talk to.
package fake

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// Provider authenticates with a shared token in X-Fake-Token and reads a trivial JSON body.
type Provider struct {
	token string
	base  string // directory that hosts <owner>/<repo>.git, for the Cloner capability
}

// New returns a provider that accepts requests carrying token.
func New(token string) *Provider { return &Provider{token: token} }

// Name implements provider.Provider.
func (*Provider) Name() string { return "fake" }

type payload struct {
	Delivery string `json:"delivery"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Ref      string `json:"ref"`
	Commit   string `json:"commit"`
}

// ParseWebhook implements provider.Provider.
func (p *Provider) ParseWebhook(r *http.Request) (*provider.Event, error) {
	got := r.Header.Get("X-Fake-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.token)) != 1 {
		return nil, provider.ErrUnauthorized
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	var pl payload
	if err := json.Unmarshal(body, &pl); err != nil || pl.Owner == "" || pl.Repo == "" || pl.Ref == "" || pl.Commit == "" {
		return nil, fmt.Errorf("%w: need owner, repo, ref and commit", provider.ErrMalformed)
	}
	branch, tag := provider.SplitRef(pl.Ref)
	return &provider.Event{
		Provider: "fake", DeliveryID: pl.Delivery, Kind: provider.KindPush,
		Owner: pl.Owner, Repo: pl.Repo, FullName: pl.Owner + "/" + pl.Repo,
		Ref: pl.Ref, Branch: branch, Tag: tag, Commit: pl.Commit,
	}, nil
}

// WithBase returns a copy that can "clone" repositories stored under base as <owner>/<repo>.git.
func (p *Provider) WithBase(base string) *Provider {
	c := *p
	c.base = base
	return &c
}

// RepositoryURL implements provider.Cloner: a local path.
func (p *Provider) RepositoryURL(owner, repo string) string {
	return filepath.Join(p.base, owner, repo+".git")
}

// CloneCredentials implements provider.Cloner.
func (p *Provider) CloneCredentials(context.Context, *provider.Event) (provider.Credentials, error) {
	if p.base == "" {
		return provider.Credentials{}, errors.New("fake: no base directory configured")
	}
	return provider.Credentials{Username: "fake", Password: "fake-password"}, nil
}
