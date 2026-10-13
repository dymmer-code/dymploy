package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
)

func cacheRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	return &Runner{cfg: Config{CacheDir: dir}}, dir
}

func buildOf(r *Runner, scope, branch, tag string, names ...string) *build {
	m := &manifest.Manifest{}
	for _, n := range names {
		m.Build.Cache = append(m.Build.Cache, manifest.Cache{Name: n, Path: "/cache/" + n})
	}
	return &build{r: r, m: m, cacheScope: scope, ev: provider.Event{Provider: "github", Owner: "acme", Repo: "shop", Branch: branch, Tag: tag}}
}

func writeFile(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func age(t *testing.T, base, name string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	if err := os.Chtimes(filepath.Join(base, useDir, name), old, old); err != nil {
		t.Fatal(err)
	}
}

func TestACacheIsCreatedMarkedAndGivenToTheBuild(t *testing.T) {
	r, root := cacheRunner(t)
	b := buildOf(r, "", "main", "", "deps")
	mounts, release, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	base := filepath.Join(root, "github", "acme", "shop")
	if len(mounts) != 1 || mounts[0].host != filepath.Join(base, "deps") || mounts[0].container != "/cache/deps" {
		t.Fatalf("%+v", mounts)
	}
	if _, err := os.Stat(filepath.Join(base, useDir, "deps")); err != nil {
		t.Fatalf("the use is recorded outside the mounted directory: %v", err)
	}
}

func TestOldCachesAreRemovedAndFreshOnesKept(t *testing.T) {
	r, root := cacheRunner(t)
	base := filepath.Join(root, "github", "acme", "shop")
	for _, n := range []string{"old", "fresh"} {
		_, release, err := buildOf(r, "", "main", "", n).cacheMounts()
		if err != nil {
			t.Fatal(err)
		}
		release()
		writeFile(t, filepath.Join(base, n, "x"), 10)
	}
	age(t, base, "old", 40*24*time.Hour)
	rep, err := r.CleanCaches(30*24*time.Hour, 0)
	if err != nil || rep.Expired != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if exists(filepath.Join(base, "old")) || !exists(filepath.Join(base, "fresh", "x")) {
		t.Fatal("the old one goes, the fresh one stays")
	}
	if exists(filepath.Join(base, useDir, "old")) {
		t.Fatal("its record goes with it")
	}
}

func TestACacheInUseIsNeverTouchedAndEmptyDirectoriesDisappear(t *testing.T) {
	r, root := cacheRunner(t)
	base := filepath.Join(root, "github", "acme", "shop")
	_, release, err := buildOf(r, "", "main", "", "busy").cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "busy", "x"), 10)
	age(t, base, "busy", 90*24*time.Hour)
	if rep, _ := r.CleanCaches(time.Hour, 1); rep.Expired+rep.OverLimit != 0 || !exists(filepath.Join(base, "busy", "x")) {
		t.Fatal("a build is using it")
	}
	release()
	age(t, base, "busy", 90*24*time.Hour)
	if rep, _ := r.CleanCaches(time.Hour, 0); rep.Expired != 1 {
		t.Fatalf("%+v", rep)
	}
	if exists(filepath.Join(root, "github")) {
		t.Fatal("nothing is left of an unused repository, directories included")
	}
}

func TestACacheOverTheLimitStartsFromNothing(t *testing.T) {
	r, root := cacheRunner(t)
	base := filepath.Join(root, "github", "acme", "shop")
	_, release, err := buildOf(r, "", "main", "", "big", "small").cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	writeFile(t, filepath.Join(base, "big", "sub", "a"), 600)
	writeFile(t, filepath.Join(base, "big", "b"), 600)
	writeFile(t, filepath.Join(base, "small", "a"), 100)
	rep, err := r.CleanCaches(0, 1000)
	if err != nil || rep.OverLimit != 1 || rep.Freed < 1000 {
		t.Fatalf("%+v %v", rep, err)
	}
	if exists(filepath.Join(base, "big")) || !exists(filepath.Join(base, "small", "a")) {
		t.Fatal("only the one over the limit goes")
	}
	// the next build finds it empty and records its use again
	if _, rel, err := buildOf(r, "", "main", "", "big").cacheMounts(); err != nil || !exists(filepath.Join(base, "big")) {
		t.Fatalf("%v", err)
	} else {
		rel()
	}
}

func TestBranchScopeKeepsBranchesApart(t *testing.T) {
	r, root := cacheRunner(t)
	main, rel1, _ := buildOf(r, ScopeBranch, "main", "", "deps").cacheMounts()
	feat, rel2, _ := buildOf(r, ScopeBranch, "feature/x", "", "deps").cacheMounts()
	tag, rel3, _ := buildOf(r, ScopeBranch, "", "v1", "deps").cacheMounts()
	shared, rel4, _ := buildOf(r, ScopeRepository, "feature/x", "", "deps").cacheMounts()
	for _, rel := range []func(){rel1, rel2, rel3, rel4} {
		defer rel()
	}
	paths := map[string]bool{main[0].host: true, feat[0].host: true, tag[0].host: true, shared[0].host: true}
	if len(paths) != 4 {
		t.Fatalf("each scope needs its own directory: %v", paths)
	}
	if !strings.Contains(main[0].host, "@branch-main") || !strings.Contains(tag[0].host, "@tags") || strings.Contains(shared[0].host, "@") {
		t.Fatalf("%v", paths)
	}
	if !strings.HasPrefix(feat[0].host, filepath.Join(root, "github", "acme", "shop")) || strings.Contains(feat[0].host, "..") {
		t.Fatalf("a branch name cannot climb out of the cache directory: %s", feat[0].host)
	}
}

func TestACacheScopeMustBeKnown(t *testing.T) {
	if _, err := ParseParams([]byte(`{"cache_scope":"global"}`)); err == nil {
		t.Fatal("an unknown scope must be refused")
	}
	if p, err := ParseParams([]byte(`{"cache_scope":"branch"}`)); err != nil || p.CacheScope != ScopeBranch {
		t.Fatalf("%v", err)
	}
	if p, err := ParseParams(nil); err != nil || p.CacheScope != ScopeRepository {
		t.Fatalf("the default shares per repository: %v", err)
	}
}

func TestCleaningWithoutCachesIsNotAnError(t *testing.T) {
	r := &Runner{cfg: Config{CacheDir: filepath.Join(t.TempDir(), "missing")}}
	if rep, err := r.CleanCaches(time.Hour, 1); err != nil || rep.Expired != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}
