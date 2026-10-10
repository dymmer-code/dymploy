// Package artifact packs the result of a build into the tar.gz that travels to the deploy.
//
// What goes in is decided by the manifest (paths and exclusions) but the files come from a build that
// ran arbitrary commands, so the packer treats the tree as untrusted: it never follows a link out of
// what it packs, refuses special files and bounds the total size.
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Spec says what to pack.
type Spec struct {
	// Name of the archive without extension: the file is <Name>.tar.gz.
	Name string
	// Paths are relative to the root of the build. Each one is stored under its base name.
	Paths []string
	// Exclude are patterns (path.Match syntax). One without "/" matches the name of a file or directory
	// at any depth; one with "/" matches the path inside the archive (for example "app/log/*").
	// An excluded directory is left out with everything below it.
	Exclude []string
	// MaxBytes bounds the uncompressed size of the contents; 0 means DefaultMaxBytes.
	MaxBytes int64
}

// DefaultMaxBytes bounds an artifact when the spec does not.
const DefaultMaxBytes int64 = 4 << 30

// Result describes the archive written.
type Result struct {
	Path   string // absolute path of <Name>.tar.gz
	SHA256 string // hex digest of the archive file
	Size   int64  // size of the archive file
	Files  int    // entries stored (files, directories and links)
}

// Pack writes <spec.Name>.tar.gz into destDir (created 0700 if needed; the file is 0600) and returns
// what it wrote. root is the build's working directory. Nothing is left behind on failure.
func Pack(root string, spec Spec, destDir string) (Result, error) {
	if spec.Name == "" || strings.ContainsAny(spec.Name, "/\\") || spec.Name == "." || spec.Name == ".." {
		return Result{}, fmt.Errorf("invalid artifact name %q", spec.Name)
	}
	if len(spec.Paths) == 0 {
		return Result{}, errors.New("the artifact has no paths")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Result{}, fmt.Errorf("build directory: %w", err)
	}
	for _, e := range spec.Exclude {
		if _, err := path.Match(e, ""); err != nil {
			return Result{}, fmt.Errorf("invalid exclude pattern %q", e)
		}
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return Result{}, err
	}
	tmp, err := os.CreateTemp(destDir, ".pack-*")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	defer tmp.Close()

	p := &packer{root: realRoot, spec: spec, limit: spec.MaxBytes}
	if p.limit <= 0 {
		p.limit = DefaultMaxBytes
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, h)}
	gz, _ := gzip.NewWriterLevel(cw, gzip.BestCompression)
	p.tw = tar.NewWriter(gz)
	if err := p.packAll(); err != nil {
		return Result{}, err
	}
	if p.files == 0 {
		return Result{}, errors.New("the artifact is empty: the paths exist but everything in them was excluded")
	}
	if err := p.tw.Close(); err != nil {
		return Result{}, err
	}
	if err := gz.Close(); err != nil {
		return Result{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	final := filepath.Join(destDir, spec.Name+".tar.gz")
	if err := os.Rename(tmp.Name(), final); err != nil {
		return Result{}, err
	}
	abs, err := filepath.Abs(final)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: abs, SHA256: hex.EncodeToString(h.Sum(nil)), Size: cw.n, Files: p.files}, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

type packer struct {
	root  string
	spec  Spec
	tw    *tar.Writer
	limit int64
	bytes int64
	files int
}

func (p *packer) packAll() error {
	for _, rel := range p.spec.Paths {
		clean := path.Clean(rel)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("artifact path %q must stay inside the build", rel)
		}
		src := filepath.Join(p.root, filepath.FromSlash(clean))
		// Resolve the parents: a link in the middle of the path must not lead out of the build.
		parent, err := filepath.EvalSymlinks(filepath.Dir(src))
		if err != nil {
			return fmt.Errorf("artifact path %q was not produced by the build", rel)
		}
		if !within(p.root, parent) {
			return fmt.Errorf("artifact path %q leads outside the build", rel)
		}
		src = filepath.Join(parent, filepath.Base(src))
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("artifact path %q was not produced by the build", rel)
		}
		if err := p.walk(src, path.Base(clean), src); err != nil {
			return err
		}
	}
	return nil
}

func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func (p *packer) excluded(archivePath string) bool {
	for _, e := range p.spec.Exclude {
		target := path.Base(archivePath)
		if strings.Contains(e, "/") {
			target = archivePath
		}
		if ok, _ := path.Match(e, target); ok {
			return true
		}
	}
	return false
}

// walk stores src (a path in the build) as name in the archive; top is the packed path's own location,
// which links must not escape.
func (p *packer) walk(src, name, top string) error {
	if p.excluded(name) {
		return nil
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime().Truncate(1e9), Format: tar.FormatPAX}
	switch mode := fi.Mode(); {
	case mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
		if err := p.tw.WriteHeader(hdr); err != nil {
			return err
		}
		p.files++
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if err := p.walk(filepath.Join(src, e.Name()), name+"/"+e.Name(), top); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		p.bytes += fi.Size()
		if p.bytes > p.limit {
			return fmt.Errorf("the artifact is larger than the limit (%d bytes uncompressed)", p.limit)
		}
		hdr.Typeflag = tar.TypeReg
		hdr.Size = fi.Size()
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := p.tw.WriteHeader(hdr); err != nil {
			return err
		}
		// The file may grow while it is read: never write more than the header promised.
		if _, err := io.CopyN(p.tw, f, fi.Size()); err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		p.files++
		return nil
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("%s is a link to the absolute path %s: links in an artifact must be relative and stay inside it", name, target)
		}
		resolved := filepath.Join(filepath.Dir(src), target)
		if !within(top, resolved) && resolved != top {
			return fmt.Errorf("%s is a link to %s, which is outside what is being packed", name, target)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = filepath.ToSlash(target)
		hdr.Mode = 0o777
		if err := p.tw.WriteHeader(hdr); err != nil {
			return err
		}
		p.files++
		return nil
	default:
		return fmt.Errorf("%s is a special file (device, socket or pipe) and cannot be part of an artifact", name)
	}
}

// Clean removes the per-job directories of dir that were last modified more than olderThan ago and
// returns how many it removed. A missing dir is not an error.
func Clean(dir string, olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var errs []error
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !e.IsDir() || fi.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
