package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Build caches live at <CacheDir>/<provider>/<owner>/<repo>[/@<scope>]/<name>. Next to them, in a directory a
// cache name cannot have (names are letters, digits and . _ -), a small file per cache records when it was
// last used: it is outside what the build mounts, so a build cannot hide its cache from the clean-up.
const useDir = "@use"

// CacheScope says who shares a cache: every build of the repository, or only the builds of the same branch.
const (
	ScopeRepository = "repository"
	ScopeBranch     = "branch"
)

// cacheStore remembers which caches a build is using, so the clean-up never removes one under a build.
type cacheStore struct {
	mu    sync.Mutex
	inUse map[string]int
}

// scopeDir is where a build's caches go below the repository's directory. With the "branch" scope a build
// only sees what builds of its own branch (or of tags, or of nothing in particular) left: a branch that the
// policy trusts less than another cannot plant something in the cache the other one builds from.
func scopeDir(scope string, branch, tag string) string {
	if scope != ScopeBranch {
		return ""
	}
	switch {
	case branch != "":
		if s := safe(branch); s != "" {
			return "@branch-" + s
		}
	case tag != "":
		return "@tags"
	}
	return "@other"
}

// acquire creates the caches of a build (or finds them), marks them used now and in use, and returns a
// function that releases them.
func (s *cacheStore) acquire(base string, names []string) (dirs []string, release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse == nil {
		s.inUse = map[string]int{}
	}
	now := time.Now()
	for _, name := range names {
		host := filepath.Join(base, name)
		if err := os.MkdirAll(host, 0o700); err != nil {
			return nil, nil, fmt.Errorf("creating cache %s: %w", name, err)
		}
		if err := touchUse(base, name, now); err != nil {
			return nil, nil, fmt.Errorf("recording the use of cache %s: %w", name, err)
		}
		dirs = append(dirs, host)
	}
	for _, d := range dirs {
		s.inUse[d]++
	}
	return dirs, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		for i, d := range dirs {
			if s.inUse[d]--; s.inUse[d] <= 0 {
				delete(s.inUse, d)
			}
			_ = touchUse(base, names[i], now) // a long build counts as used until it ends
		}
	}, nil
}

func touchUse(base, name string, t time.Time) error {
	dir := filepath.Join(base, useDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return os.Chtimes(p, t, t)
}

// CacheReport says what a clean-up did.
type CacheReport struct {
	Expired   int   // removed because nobody used them for long enough
	OverLimit int   // removed because they grew past the limit (the next build starts from nothing)
	Freed     int64 // bytes known to have been freed (caches removed for age are not measured)
}

// CleanCaches removes the caches not used for olderThan (0 keeps them for ever) and those larger than
// maxBytes (0 means no limit). A cache a build is using is never touched. Nothing is cleaned in a way
// that a running build could notice: both the decision and the removal happen while no build can start
// using a cache.
func (r *Runner) CleanCaches(olderThan time.Duration, maxBytes int64) (CacheReport, error) {
	var rep CacheReport
	var errs []error
	root := r.cfg.CacheDir
	var uses []string // the @use directories found
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() && d.Name() == useDir {
			uses = append(uses, p)
			return filepath.SkipDir
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, u := range uses {
		base := filepath.Dir(u)
		entries, err := os.ReadDir(u)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			cache := filepath.Join(base, e.Name())
			r.caches.mu.Lock()
			switch {
			case r.caches.inUse[cache] > 0:
				// a build is using it
			case !exists(cache):
				_ = os.Remove(filepath.Join(u, e.Name()))
			case olderThan > 0 && info.ModTime().Before(cutoff):
				if err := os.RemoveAll(cache); err != nil {
					errs = append(errs, err)
				} else {
					rep.Expired++
					_ = os.Remove(filepath.Join(u, e.Name()))
				}
			case maxBytes > 0:
				if size := sizeAbove(cache, maxBytes); size > maxBytes {
					if err := os.RemoveAll(cache); err != nil {
						errs = append(errs, err)
					} else {
						rep.OverLimit++
						rep.Freed += size
					}
				}
			}
			r.caches.mu.Unlock()
		}
		pruneEmpty(base, root)
	}
	return rep, errors.Join(errs...)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// sizeAbove returns the size of dir's files, or as soon as it is known to be above limit, a value above it.
func sizeAbove(dir string, limit int64) int64 {
	var total int64
	errStop := errors.New("enough")
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		if total > limit {
			return errStop
		}
		return nil
	})
	return total
}

// pruneEmpty removes the directories above a repository's caches that have become empty, up to root.
func pruneEmpty(dir, root string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if u := filepath.Join(dir, useDir); exists(u) {
			if entries, _ := os.ReadDir(u); len(entries) == 0 {
				_ = os.Remove(u)
			}
		}
		if os.Remove(dir) != nil { // not empty (or gone): stop
			return
		}
		dir = filepath.Dir(dir)
	}
}
