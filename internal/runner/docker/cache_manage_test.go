package docker

import (
	"github.com/dymmer-code/dymploy/internal/runner"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidationKeepsActiveGenerationAndPersistsPointer(t *testing.T) {
	r, root := cacheRunner(t)
	b := buildOf(r, "", "main", "", "plt")
	first, release, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(first[0].host, "old"), 10)
	if _, err := r.InvalidateCaches(runner.CacheRequest{Repository: "github/acme/shop", Name: "plt"}); err != nil {
		t.Fatal(err)
	}
	second, release2, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	if first[0].host == second[0].host || !exists(filepath.Join(first[0].host, "old")) {
		t.Fatal("active cache was replaced")
	}
	if exists(filepath.Join(second[0].host, "old")) {
		t.Fatal("new generation is not empty")
	}
	release2()
	release()
	r.CleanCaches(0, 0)
	if exists(first[0].host) {
		t.Fatal("retired generation was not collected")
	}
	restarted := &Runner{cfg: Config{CacheDir: root}}
	third, release3, err := buildOf(restarted, "", "main", "", "plt").cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	defer release3()
	if third[0].host != second[0].host {
		t.Fatal("lost generation after restart")
	}
	entries, err := restarted.ListCaches(runner.CacheRequest{Repository: "github/acme/shop"})
	if err != nil || len(entries) != 1 || !entries[0].InUse {
		t.Fatalf("%+v %v", entries, err)
	}
}
func TestCacheKeyAndScopeIsolation(t *testing.T) {
	r, _ := cacheRunner(t)
	b := buildOf(r, ScopeBranch, "main", "", "deps")
	b.m.Build.Cache[0].Key = "otp29"
	a, ra, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	ra()
	os.WriteFile(filepath.Join(a[0].host, "keep"), []byte("x"), 0600)
	b.m.Build.Cache[0].Key = "otp30"
	c, rc, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	defer rc()
	if a[0].host == c[0].host {
		t.Fatal("incompatible cache reused")
	}
	for _, repo := range []string{"../acme/shop", "github/acme/../shop", "github/acme", "github/acme/shop/extra"} {
		if _, err := r.InvalidateCaches(runner.CacheRequest{Repository: repo, All: true}); err == nil {
			t.Fatal(repo)
		}
	}
}

func TestRestartCannotCollectAnOrphanedActiveGeneration(t *testing.T) {
	r, root := cacheRunner(t)
	mounts, release, err := buildOf(r, "", "main", "", "plt").cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	restarted := &Runner{cfg: Config{CacheDir: root}}
	if _, err := restarted.InvalidateCaches(runner.CacheRequest{Repository: "github/acme/shop", Name: "plt"}); err != nil {
		t.Fatal(err)
	}
	if !exists(mounts[0].host) {
		t.Fatal("lost a cache still mounted by a previous daemon's build")
	}
	entries, err := restarted.ListCaches(runner.CacheRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Retired && e.InUse {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", entries)
	}
}

func TestCacheSelectionAndCorruptMetadata(t *testing.T) {
	r, root := cacheRunner(t)
	for _, branch := range []string{"main", "feature"} {
		for _, name := range []string{"plt", "deps"} {
			_, rel, err := buildOf(r, ScopeBranch, branch, "", name).cacheMounts()
			if err != nil {
				t.Fatal(err)
			}
			rel()
		}
	}
	q := runner.CacheRequest{Repository: "github/acme/shop", Scope: "@branch-main", All: true}
	out, err := r.InvalidateCaches(q)
	if err != nil || out.Invalidated != 2 {
		t.Fatal(out, err)
	}
	entries, err := r.ListCaches(runner.CacheRequest{Repository: q.Repository, Scope: "@branch-feature", Name: "deps"})
	if err != nil || len(entries) != 1 || entries[0].Generation != "" {
		t.Fatal(entries, err)
	}
	if _, err := r.InvalidateCaches(runner.CacheRequest{Repository: q.Repository, Name: "missing"}); err == nil {
		t.Fatal("missing cache silently accepted")
	}
	for _, q := range []runner.CacheRequest{{Name: "../bad"}, {Scope: "../bad"}, {Scope: "@branch-../../bad"}, {Repository: "bad"}} {
		if _, err := r.ListCaches(q); err == nil {
			t.Fatal(q)
		}
	}
	for _, q := range []runner.CacheRequest{{}, {Repository: "github/acme/shop"}, {Repository: "github/acme/shop", Name: "plt", All: true}} {
		if _, err := r.InvalidateCaches(q); err == nil {
			t.Fatal(q)
		}
	}
	ptr := filepath.Join(root, "github", "acme", "shop", "@branch-main", "@current", "plt")
	for _, raw := range []string{"bad", `{"generation":"../../escape"}`, `{"generation":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`} {
		os.WriteFile(ptr, []byte(raw), 0600)
		if _, err := r.ListCaches(runner.CacheRequest{}); err == nil {
			t.Fatal("accepted malformed pointer")
		}
	}
}

func TestInvalidationDoesNotCollectAnotherRepository(t *testing.T) {
	r, _ := cacheRunner(t)
	other := buildOf(r, "", "main", "", "plt")
	other.ev.Repo = "other"
	other.m.Build.Cache[0].Key = "old"
	old, release, err := other.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	other.m.Build.Cache[0].Key = "new"
	_, release, err = other.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, release, err = buildOf(r, "", "main", "", "plt").cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := r.InvalidateCaches(runner.CacheRequest{Repository: "github/acme/shop", Name: "plt"}); err != nil {
		t.Fatal(err)
	}
	if !exists(old[0].host) {
		t.Fatal("invalidating shop collected another repository's retired cache")
	}
}
