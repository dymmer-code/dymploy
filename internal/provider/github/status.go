package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// StatusContext is the name under which dymploy's verdict appears next to a commit.
const StatusContext = "dymploy"

// maxDescription is the longest description GitHub accepts for a status.
const maxDescription = 140

// PublishStatus implements provider.StatusPublisher: it shows the state of a run next to the commit, the
// green tick or the red cross. It uses a token limited to this one repository and to the commit-status
// permission, which the App must have been granted (and the installation must have accepted).
func (p *Provider) PublishStatus(ctx context.Context, ev *provider.Event, state provider.State, description, targetURL string) error {
	if p.app == nil {
		return errors.New("github: no app is configured, so statuses cannot be published")
	}
	if ev.InstallationID == 0 || !shaRE.MatchString(ev.Commit) || !nameRE.MatchString(ev.Owner) || !nameRE.MatchString(ev.Repo) {
		return errors.New("github: the event does not identify a commit of an installed repository")
	}
	switch state {
	case provider.StatePending, provider.StateSuccess, provider.StateFailure, provider.StateError:
	default:
		return fmt.Errorf("github: %q is not a state GitHub knows", state)
	}
	token, _, err := p.app.installationToken(ctx, ev.InstallationID, ev.Repo, "statuses")
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(description) > maxDescription {
		description = string([]rune(description)[:maxDescription-1]) + "…"
	}
	payload := map[string]string{"state": string(state), "context": StatusContext, "description": strings.TrimSpace(description)}
	if targetURL != "" {
		payload["target_url"] = targetURL
	}
	body, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", p.app.APIBase, url.PathEscape(ev.Owner), url.PathEscape(ev.Repo), ev.Commit)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.app.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("publishing the status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("GitHub refused the status: %s", http.StatusText(resp.StatusCode))
	}
	return nil
}

var _ provider.StatusPublisher = (*Provider)(nil)
