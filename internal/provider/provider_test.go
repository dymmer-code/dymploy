package provider

import (
	"errors"
	"testing"
)

func TestErrorsAreDistinct(t *testing.T) {
	all := []error{ErrUnauthorized, ErrIgnored, ErrMalformed}
	for i, a := range all {
		for j, b := range all {
			if (i == j) != errors.Is(a, b) {
				t.Fatalf("errors %d and %d must be distinguishable", i, j)
			}
		}
	}
}

func TestBranchFromRef(t *testing.T) {
	cases := map[string][2]string{
		"refs/heads/main":        {"main", ""},
		"refs/heads/release/1.x": {"release/1.x", ""},
		"refs/tags/v1.0.0":       {"", "v1.0.0"},
		"refs/pull/12/head":      {"", ""},
		"main":                   {"", ""},
		"":                       {"", ""},
	}
	for ref, want := range cases {
		b, tag := SplitRef(ref)
		if b != want[0] || tag != want[1] {
			t.Errorf("%q → (%q,%q), want %v", ref, b, tag, want)
		}
	}
}
