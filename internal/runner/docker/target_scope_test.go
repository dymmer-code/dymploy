package docker

import "testing"

func TestApplicationTargetScope(t *testing.T) {
	for _, tc := range []struct {
		scope, targets []string
		denied         bool
	}{
		{nil, []string{"any"}, false}, {[]string{}, []string{"rowan"}, true}, {[]string{"rowan"}, []string{"rowan"}, false}, {[]string{"rowan"}, []string{"rowan", "other"}, true},
	} {
		p := Params{AllowedTargets: tc.scope}
		if (p.authorizeTargets(tc.targets) != nil) != tc.denied {
			t.Fatalf("scope %v targets %v", tc.scope, tc.targets)
		}
	}
}
