package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/dymmer-code/dymploy/internal/provider"
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	refRE  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// installationID asks GitHub which installation of the App covers a repository.
func (a *App) installationID(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := a.jwt()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	code, err := a.getJSON(ctx, jwt, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/installation", &out)
	switch {
	case err != nil:
		return 0, err
	case code == http.StatusNotFound:
		return 0, fmt.Errorf("the GitHub App is not installed on %s/%s", owner, repo)
	case code != http.StatusOK || out.ID == 0:
		return 0, fmt.Errorf("GitHub could not tell which installation covers %s/%s: %s", owner, repo, http.StatusText(code))
	}
	return out.ID, nil
}

// getJSON does an authenticated GET on the API and decodes a 200 answer.
func (a *App) getJSON(ctx context.Context, bearer, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIBase+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("asking GitHub: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, errors.New("GitHub answered with something unreadable")
		}
	}
	return resp.StatusCode, nil
}

// ResolveRef implements provider.Resolver. A short name is tried as a branch and then as a tag; an
// annotated tag is resolved to the commit it points at.
func (p *Provider) ResolveRef(ctx context.Context, owner, repo, ref string) (*provider.Event, error) {
	if p.app == nil {
		return nil, errors.New("github: no app is configured, so references cannot be resolved")
	}
	if !nameRE.MatchString(owner) || !nameRE.MatchString(repo) || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return nil, fmt.Errorf("%q is not a repository of GitHub", owner+"/"+repo)
	}
	if !refRE.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return nil, fmt.Errorf("%q is not a branch or tag name", ref)
	}
	candidates := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		candidates = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	} else if branch, tag := provider.SplitRef(ref); branch == "" && tag == "" {
		return nil, fmt.Errorf("%q is neither a branch (refs/heads/…) nor a tag (refs/tags/…)", ref)
	}
	install, err := p.app.installationID(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	token, _, err := p.app.InstallationToken(ctx, install, repo)
	if err != nil {
		return nil, err
	}
	for _, full := range candidates {
		var out struct {
			SHA string `json:"sha"`
		}
		segs := strings.Split(full, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		code, err := p.app.getJSON(ctx, token, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/commits/"+strings.Join(segs, "/"), &out)
		switch {
		case err != nil:
			return nil, err
		case code == http.StatusNotFound || code == http.StatusUnprocessableEntity:
			continue // not a branch (or not a tag) of that name
		case code != http.StatusOK:
			return nil, fmt.Errorf("GitHub could not resolve %s: %s", full, http.StatusText(code))
		case !shaRE.MatchString(out.SHA):
			return nil, errors.New("GitHub answered with something that is not a commit")
		}
		branch, tag := provider.SplitRef(full)
		return &provider.Event{
			Provider: "github", Kind: provider.KindPush, Owner: owner, Repo: repo, FullName: owner + "/" + repo,
			Ref: full, Branch: branch, Tag: tag, Commit: out.SHA, InstallationID: install,
			CloneURL: p.RepositoryURL(owner, repo),
		}, nil
	}
	return nil, fmt.Errorf("%w: %s in %s/%s", provider.ErrRefNotFound, ref, owner, repo)
}
