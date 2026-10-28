package docker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dymmer-code/dymploy/internal/runner"
)

type cachePointer struct {
	Generation string `json:"generation"`
	Key        string `json:"key"`
}

func pointer(base, name string) (cachePointer, error) {
	var p cachePointer
	raw, err := os.ReadFile(filepath.Join(base, "@current", name))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if len(p.Generation) != 32 {
		return p, errors.New("invalid cache generation")
	}
	if _, err := hex.DecodeString(p.Generation); err != nil {
		return p, err
	}
	return p, nil
}
func generationPath(base, name, generation string) string {
	if generation == "" {
		return filepath.Join(base, name)
	}
	return filepath.Join(base, "@generations", name, generation)
}
func marker(name, gen string) string {
	if gen == "" {
		return name
	}
	return name + "@" + gen
}
func splitMarker(m string) (string, string) { name, gen, _ := strings.Cut(m, "@"); return name, gen }
func newGeneration(base, name, key string) (cachePointer, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return cachePointer{}, err
	}
	p := cachePointer{hex.EncodeToString(b), key}
	if err := os.MkdirAll(generationPath(base, name, p.Generation), 0700); err != nil {
		return p, err
	}
	dir := filepath.Join(base, "@current")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return p, err
	}
	raw, _ := json.Marshal(p)
	f, err := os.CreateTemp(dir, ".new-")
	if err != nil {
		return p, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		return p, err
	}
	return p, touchUse(base, marker(name, p.Generation), time.Now())
}

func cacheRequest(q runner.CacheRequest, invalidating bool) error {
	if q.Repository != "" {
		parts := strings.Split(q.Repository, "/")
		if len(parts) != 3 {
			return errors.New("use provider/owner/repository")
		}
		for _, s := range parts {
			if s == "" || safe(s) != s {
				return errors.New("invalid repository name")
			}
		}
	} else if invalidating {
		return errors.New("invalidation requires one repository")
	}
	if q.Name != "" && (safe(q.Name) != q.Name || strings.Contains(q.Name, "@")) {
		return errors.New("invalid cache name")
	}
	if q.Scope != "" && q.Scope != "repository" && q.Scope != "@tags" && q.Scope != "@other" {
		if !strings.HasPrefix(q.Scope, "@branch-") || safe(strings.TrimPrefix(q.Scope, "@branch-")) != strings.TrimPrefix(q.Scope, "@branch-") {
			return errors.New("invalid scope: use repository or the scope shown by cache list")
		}
	}
	if invalidating && (q.Name == "") == (!q.All) {
		return errors.New("choose exactly one of --name or --all")
	}
	return nil
}

// cacheUses scans only structural directories, never user-controlled cache contents or symlinks.
func cacheUses(root string) ([]string, error) {
	var out []string
	var visit func(string, int) error
	visit = func(dir string, depth int) error {
		es, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range es {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if depth < 3 {
				if safe(e.Name()) != e.Name() {
					continue
				}
				if err := visit(p, depth+1); err != nil {
					return err
				}
			} else if e.Name() == useDir {
				out = append(out, p)
			} else if depth == 3 && (strings.HasPrefix(e.Name(), "@branch-") || e.Name() == "@tags" || e.Name() == "@other") {
				if err := visit(p, 4); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := visit(root, 0)
	return out, err
}
func (r *Runner) listCaches(q runner.CacheRequest) ([]runner.CacheEntry, error) {
	uses, err := cacheUses(r.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	var out []runner.CacheEntry
	for _, u := range uses {
		base := filepath.Dir(u)
		rel, _ := filepath.Rel(r.cfg.CacheDir, base)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		repo := strings.Join(parts[:3], "/")
		scope := "repository"
		if len(parts) > 3 {
			scope = parts[3]
		}
		if q.Repository != "" && repo != q.Repository || q.Scope != "" && scope != q.Scope {
			continue
		}
		es, err := os.ReadDir(u)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if !e.Type().IsRegular() {
				continue
			}
			name, gen := splitMarker(e.Name())
			if q.Name != "" && name != q.Name {
				continue
			}
			if safe(name) != name || name == "" {
				continue
			}
			if gen != "" {
				if len(gen) != 32 {
					continue
				}
				if _, err := hex.DecodeString(gen); err != nil {
					continue
				}
			}
			p, err := pointer(base, name)
			if err != nil {
				return nil, err
			}
			dir := generationPath(base, name, gen)
			if !exists(dir) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return nil, err
			}
			key := ""
			if p.Generation == gen {
				key = p.Key
			}
			out = append(out, runner.CacheEntry{Repository: repo, Name: name, Scope: scope, Generation: gen, Key: key, Bytes: sizeAbove(dir, math.MaxInt64), LastUsed: info.ModTime(), InUse: r.caches.inUse[dir] > 0 || cacheLeased(base, e.Name()), Retired: p.Generation != gen})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Repository+a.Scope+a.Name+a.Generation < b.Repository+b.Scope+b.Name+b.Generation
	})
	return out, nil
}
func (r *Runner) ListCaches(q runner.CacheRequest) ([]runner.CacheEntry, error) {
	if err := cacheRequest(q, false); err != nil {
		return nil, err
	}
	r.caches.mu.Lock()
	defer r.caches.mu.Unlock()
	return r.listCaches(q)
}
func (r *Runner) InvalidateCaches(q runner.CacheRequest) (runner.CacheInvalidation, error) {
	var out runner.CacheInvalidation
	if err := cacheRequest(q, true); err != nil {
		return out, err
	}
	r.caches.mu.Lock()
	items, err := r.listCaches(q)
	if err == nil {
		for _, item := range items {
			if item.Retired {
				continue
			}
			base := filepath.Join(r.cfg.CacheDir, filepath.FromSlash(item.Repository))
			if item.Scope != "repository" {
				base = filepath.Join(base, item.Scope)
			}
			if _, err = newGeneration(base, item.Name, item.Key); err != nil {
				break
			}
			out.Invalidated++
		}
	}
	r.caches.mu.Unlock()
	if err != nil {
		return out, err
	}
	if out.Invalidated == 0 {
		return out, fmt.Errorf("no matching caches")
	}
	rep, err := r.cleanCaches(0, 0, q)
	out.Freed = rep.Freed
	return out, err
}

// Leases live outside mounted directories. A crash leaves them pinned rather than risking removal
// under an orphaned container; an operator can retire stale leases after stopping that container.
func cacheLeased(base, mark string) bool {
	es, err := os.ReadDir(filepath.Join(base, "@leases", mark))
	return (err != nil && !os.IsNotExist(err)) || len(es) > 0
}
