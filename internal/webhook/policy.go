// Package webhook receives repository events: it authenticates them through the provider, checks
// them against the daemon's allow-list, drops repeated deliveries and hands the rest to the queue.
package webhook

import (
	"fmt"
	"path"
	"strings"

	"github.com/dymmer-code/dymploy/internal/provider"
)

// Rule allows pushes of one provider to repositories of one owner. Everything a rule does not
// name is denied. Rules come from the daemon configuration, never from the repository.
type Rule struct {
	Provider      string
	Owner         string
	Repositories  []string // patterns (path.Match); "*" matches every repository of the owner
	Branches      []string // patterns for branch pushes; "*" matches every branch, including feature/x
	Tags          []string // patterns for tag pushes; empty means tags are not built
	Installations []int64  // if not empty, the event must come from one of these installations
}

// Policy is a list of rules; an event is allowed when at least one rule allows it.
type Policy struct{ Rules []Rule }

// Allow reports whether the event may start a job. When it may not, the second value says why
// (for the log; it is never sent to the caller).
func (p Policy) Allow(ev *provider.Event) (bool, string) {
	if len(p.Rules) == 0 {
		return false, "no rules configured (default deny)"
	}
	if ev.Branch == "" && ev.Tag == "" {
		return false, fmt.Sprintf("ref %q is neither a branch nor a tag", ev.Ref)
	}
	reason := "no rule for " + ev.Provider + " " + ev.Owner
	for _, r := range p.Rules {
		if r.Provider != ev.Provider || !strings.EqualFold(r.Owner, ev.Owner) {
			continue
		}
		switch {
		case !matchAny(r.Repositories, ev.Repo):
			reason = "repository " + ev.FullName + " not allowed"
		case len(r.Installations) > 0 && !hasInstallation(r.Installations, ev.InstallationID):
			reason = fmt.Sprintf("installation %d not allowed", ev.InstallationID)
		case ev.Branch != "" && !matchAny(r.Branches, ev.Branch):
			reason = "branch " + ev.Branch + " not allowed"
		case ev.Tag != "" && !matchAny(r.Tags, ev.Tag):
			reason = "tag " + ev.Tag + " not allowed"
		default:
			return true, ""
		}
	}
	return false, reason
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}

func hasInstallation(list []int64, id int64) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}
