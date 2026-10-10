// Package architecture holds tests that protect design rules from erosion.
package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/dymmer-code/dymploy"

// rules: importing an implementation (a provider, the container engine) is allowed only from the listed
// directories (relative to the module root). cmd/ is the composition root that chooses them.
var rules = map[string][]string{
	module + "/internal/provider/github":          {"cmd", "internal/provider/github"},
	module + "/internal/provider/fake":            {"internal/provider/fake"}, // other packages may use it in tests only
	module + "/internal/runner/docker":            {"cmd", "internal/runner/docker"},
	module + "/internal/runner/docker/dockertest": {"internal/runner/docker"}, // other packages may use it in tests only
}

// testDoubles may be imported from any _test.go file.
var testDoubles = map[string]bool{
	module + "/internal/provider/fake":            true,
	module + "/internal/runner/docker/dockertest": true,
}

// violations lists "file imports package" for every import that breaks a rule.
func violations(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))
		isTest := strings.HasSuffix(path, "_test.go")
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			allowed, restricted := rules[p]
			if !restricted {
				continue
			}
			if isTest && testDoubles[p] {
				continue
			}
			ok := false
			for _, a := range allowed {
				if dir == a || strings.HasPrefix(dir, a+"/") {
					ok = true
				}
			}
			if !ok {
				out = append(out, filepath.ToSlash(rel)+" imports "+p)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func TestNothingOutsideTheProvidersDependsOnAHost(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	v, err := violations(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) > 0 {
		t.Fatalf("provider isolation broken:\n  %s", strings.Join(v, "\n  "))
	}
}

func TestTheRuleItselfCatchesViolations(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gh := `package x; import _ "` + module + `/internal/provider/github"`
	fk := `package x; import _ "` + module + `/internal/provider/fake"`
	write("cmd/ok.go", gh)                      // composition root: allowed
	write("internal/provider/github/ok.go", gh) // itself: allowed
	write("internal/queue/bad.go", gh)          // a core package: forbidden
	write("internal/queue/bad_test.go", fk)     // fake in a test: allowed
	write("internal/queue/bad2.go", fk)         // fake in production code: forbidden
	dk := `package x; import _ "` + module + `/internal/runner/docker"`
	dt := `package x; import _ "` + module + `/internal/runner/docker/dockertest"`
	write("cmd/docker_ok.go", dk)                // composition root: allowed
	write("internal/runner/docker/ok.go", dk)    // itself: allowed
	write("internal/runner/docker/ok2.go", dt)   // its own double: allowed
	write("internal/queue/docker_bad.go", dk)    // a core package must not know Docker
	write("internal/job/docker_bad_test.go", dk) // not even in tests
	write("internal/job/double_ok_test.go", dt)  // the double in a test: allowed
	got, err := violations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/job/docker_bad_test.go imports " + module + "/internal/runner/docker",
		"internal/queue/bad.go imports " + module + "/internal/provider/github",
		"internal/queue/bad2.go imports " + module + "/internal/provider/fake",
		"internal/queue/docker_bad.go imports " + module + "/internal/runner/docker",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
