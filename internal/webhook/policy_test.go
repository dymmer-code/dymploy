package webhook

import (
	"testing"

	"github.com/dymmer-code/dymploy/internal/provider"
)

func ev(owner, repo, branch, tag string, inst int64) *provider.Event {
	return &provider.Event{Provider: "github", Owner: owner, Repo: repo, FullName: owner + "/" + repo, Branch: branch, Tag: tag, InstallationID: inst}
}

func TestEmptyPolicyDeniesEverything(t *testing.T) {
	if ok, _ := (Policy{}).Allow(ev("a", "b", "main", "", 1)); ok {
		t.Fatal("default deny: with no rules nothing may be built")
	}
}

func TestAllow(t *testing.T) {
	pol := Policy{Rules: []Rule{
		{Provider: "github", Owner: "acme", Repositories: []string{"*"}, Branches: []string{"main", "release/*"}, Installations: []int64{111}},
		{Provider: "github", Owner: "globex", Repositories: []string{"dymploy", "sho*"}, Branches: []string{"*"}, Tags: []string{"v*"}},
		{Provider: "fake", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}},
	}}
	cases := []struct {
		name string
		ev   *provider.Event
		want bool
	}{
		{"any repo of the owner on an allowed branch", ev("acme", "shop", "main", "", 111), true},
		{"glob branch", ev("acme", "shop", "release/1.x", "", 111), true},
		{"branch not listed", ev("acme", "shop", "feature/x", "", 111), false},
		{"installation not listed", ev("acme", "shop", "main", "", 999), false},
		{"installation missing", ev("acme", "shop", "main", "", 0), false},
		{"owner is case-insensitive", ev("AcMe", "shop", "main", "", 111), true},
		{"repo is exact or globbed", ev("globex", "dymploy", "anything/at/all", "", 7), true},
		{"repo glob", ev("globex", "shop", "main", "", 7), true},
		{"repo not listed", ev("globex", "other", "main", "", 7), false},
		{"tag allowed by pattern", ev("globex", "dymploy", "", "v1.2.3", 7), true},
		{"tag not matching", ev("globex", "dymploy", "", "nightly", 7), false},
		{"tags denied unless the rule lists them", ev("acme", "shop", "", "v1", 111), false},
		{"unknown owner", ev("evil", "shop", "main", "", 111), false},
		{"other provider does not borrow rules", ev("acme", "shop", "main", "", 111), true},
		{"fake provider rule", &provider.Event{Provider: "fake", Owner: "acme", Repo: "app", Branch: "main"}, true},
		{"provider mismatch", &provider.Event{Provider: "fake", Owner: "acme", Repo: "shop", Branch: "main", InstallationID: 111}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := pol.Allow(c.ev)
			if got != c.want {
				t.Fatalf("Allow = %v (%s), want %v", got, reason, c.want)
			}
			if !got && reason == "" {
				t.Fatal("a refusal must say why (for the log)")
			}
		})
	}
}

func TestEventWithoutBranchNorTagIsDenied(t *testing.T) {
	pol := Policy{Rules: []Rule{{Provider: "github", Owner: "a", Repositories: []string{"*"}, Branches: []string{"*"}, Tags: []string{"*"}}}}
	if ok, _ := pol.Allow(ev("a", "b", "", "", 0)); ok {
		t.Fatal("a ref that is neither a branch nor a tag (a pull request ref) must not build")
	}
}
