// Package provider is the boundary between dymploy and a repository host (GitHub, GitLab, …).
// Everything that is specific to a host lives in a subpackage; the rest of dymploy only sees the
// types declared here.
package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Kind is the type of a normalized event.
type Kind string

// KindPush is a push to a branch or a tag.
const KindPush Kind = "push"

// Event is what dymploy understands of a webhook, whatever its origin. It is a plain comparable
// value on purpose: it is copied through queues and compared in tests.
type Event struct {
	Provider       string // name of the provider that produced it ("github")
	DeliveryID     string // unique per delivery; used to drop repeated deliveries
	Kind           Kind
	Owner          string // organization or user that owns the repository
	Repo           string // repository name, without the owner
	FullName       string // Owner/Repo
	Ref            string // full reference as the host sent it
	Branch         string // set when the ref is a branch
	Tag            string // set when the ref is a tag
	Commit         string // the commit to build
	Deleted        bool   // the branch or tag was deleted (there is nothing to build)
	InstallationID int64  // host-specific installation of the app, 0 if the host has none
	CloneURL       string
}

// Errors a provider returns from ParseWebhook. The receiver maps them to responses.
var (
	// ErrUnauthorized: the request is not proven to come from the host (bad or missing signature).
	ErrUnauthorized = errors.New("webhook not authenticated")
	// ErrIgnored: authentic, but not an event dymploy acts on (ping, issues, …).
	ErrIgnored = errors.New("webhook event ignored")
	// ErrMalformed: authentic, but the content is not what the event type promises.
	ErrMalformed = errors.New("webhook payload malformed")
)

// Provider authenticates and decodes the webhooks of one repository host.
type Provider interface {
	// Name is the short identifier used in routes (/hooks/<name>) and in the configuration.
	Name() string
	// ParseWebhook proves the request is authentic BEFORE looking at its content, then normalizes it.
	ParseWebhook(r *http.Request) (*Event, error)
}

// Credentials give read access to a repository for a limited time.
type Credentials struct {
	Username  string
	Password  string
	ExpiresAt time.Time
}

// Cloner is implemented by providers that can say where a repository lives and issue read-only
// credentials for it.
type Cloner interface {
	// RepositoryURL is where owner/repo is cloned from. The provider builds it; it is never taken from
	// the webhook payload, so a payload cannot point a build at another host.
	RepositoryURL(owner, repo string) string
	// CloneCredentials returns credentials that can only read the event's repository, for a limited time.
	CloneCredentials(ctx context.Context, ev *Event) (Credentials, error)
}

// State of a job as reported back to the host.
type State string

// Reportable states.
const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	// StateError: the run did not reach a verdict (it was stopped, or dymploy itself failed).
	StateError State = "error"
)

// ErrRefNotFound is returned by a Resolver when the branch or tag does not exist.
var ErrRefNotFound = errors.New("no such branch or tag")

// Resolver is implemented by providers that can turn a branch or tag name into the commit it points
// at, which is what starting a job by hand needs. ref is a short name (a branch is tried before a tag)
// or a full reference (refs/heads/x, refs/tags/x). The event it returns is what a push of that commit
// would have been, except that it has no delivery id.
type Resolver interface {
	ResolveRef(ctx context.Context, owner, repo, ref string) (*Event, error)
}

// StatusPublisher is implemented by providers that can show a job's state on the commit.
type StatusPublisher interface {
	PublishStatus(ctx context.Context, ev *Event, state State, description, targetURL string) error
}

// HookManager is implemented by providers that can create or update a repository's webhook.
type HookManager interface {
	EnsureWebhook(ctx context.Context, owner, repo, url, secret string) error
}

// SplitRef splits a git reference into (branch, tag); anything else yields two empty strings.
func SplitRef(ref string) (branch, tag string) {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/"), ""
	case strings.HasPrefix(ref, "refs/tags/"):
		return "", strings.TrimPrefix(ref, "refs/tags/")
	}
	return "", ""
}
