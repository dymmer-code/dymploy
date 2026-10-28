package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"
)

// extraTypes are the types that the system's table may not know and that books and sites need.
var extraTypes = map[string]string{
	".epub":  "application/epub+zip",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
	".json":  "application/json",
	".pdf":   "application/pdf",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// publish puts every file of the artifact in the bucket of an s3 target, under the definition's path, and reads
// each one back to check it. It returns the job's exit code like Run does: an error only when it could not do
// its work (the bucket cannot be reached) or ctx ended.
//
// Files keep the names they have in the archive (where each path of `artifact.paths` is stored under its base
// name), so `out/book.pdf` is `<path>book.pdf`. Directories are walked; anything that is not a regular file is
// skipped and said. Nothing is ever deleted in the bucket, and a file that is already there with the same
// SHA-256 is left alone.
func (d *Deployer) publish(ctx context.Context, r Request, ended func(error) (int, error)) (int, error) {
	log, t := r.Log, r.Target
	if d.Store == nil {
		return 0, fmt.Errorf("this server cannot put files in buckets (target %s)", t.Name)
	}
	prefix := strings.Trim(r.Deploy.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	log.System(fmt.Sprintf("publishing to bucket %s of target %s under %q", t.Bucket, t.Name, prefix))
	b, err := d.Store.Open(ctx, t)
	if err != nil {
		return ended(fmt.Errorf("opening target %s: %w", t.Name, err))
	}

	put, same := 0, 0
	// walk reads the artifact once and handles its pages (.html and .htm) or everything else. Pages go in a
	// second pass: a page published before the files it refers to would be broken until they arrive.
	walk := func(pages bool) (code int, stop bool, err error) {
		f, err := os.Open(r.File)
		if err != nil {
			return 0, true, fmt.Errorf("opening the artifact: %w", err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, true, fmt.Errorf("reading the artifact: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return 0, false, nil
			}
			if err != nil {
				c, e := ended(fmt.Errorf("reading the artifact: %w", err))
				return c, true, e
			}
			name := path.Clean(strings.TrimPrefix(h.Name, "./"))
			switch {
			case h.Typeflag == tar.TypeDir:
				continue
			case h.Typeflag != tar.TypeReg:
				if !pages {
					log.System(fmt.Sprintf("skipped %s: not a regular file", name))
				}
				continue
			case name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../"):
				log.System(fmt.Sprintf("the artifact has an unsafe name %q: nothing more was published", h.Name))
				return ExitPublish, true, nil
			}
			if ext := strings.ToLower(path.Ext(name)); (ext == ".html" || ext == ".htm") != pages {
				continue
			}
			key := prefix + name
			sum, size, tmp, err := spool(tr)
			if err != nil {
				c, e := ended(fmt.Errorf("reading %s from the artifact: %w", name, err))
				return c, true, e
			}
			written, perr := b.Put(ctx, key, contentType(name), sum, size, tmp)
			tmp.Close()
			os.Remove(tmp.Name())
			if perr != nil {
				if ctx.Err() != nil {
					c, e := ended(perr)
					return c, true, e
				}
				log.System(fmt.Sprintf("could not put %s: %v", key, perr))
				d.noPublishUndo(r, put)
				return ExitPublish, true, nil
			}
			if !written {
				same++
				log.System(fmt.Sprintf("unchanged %s (sha256 %s)", key, sum[:12]))
				continue
			}
			got, serr := b.Sum(ctx, key)
			if serr != nil && ctx.Err() != nil {
				c, e := ended(serr)
				return c, true, e
			}
			if serr != nil || got != sum {
				why := "SHA-256 differs"
				if serr != nil {
					why = serr.Error()
				}
				log.System(fmt.Sprintf("could not verify %s after putting it: %s", key, why))
				d.noPublishUndo(r, put)
				return ExitPublish, true, nil
			}
			put++
			log.System(fmt.Sprintf("put %s (%d bytes, sha256 %s, verified)", key, size, sum[:12]))
		}
	}
	for _, pages := range []bool{false, true} {
		if code, stop, err := walk(pages); stop {
			return code, err
		}
	}
	log.System(fmt.Sprintf("published to %s: %d put, %d unchanged", t.Name, put, same))
	return 0, nil
}

func (d *Deployer) noPublishUndo(r Request, put int) {
	r.Log.System(fmt.Sprintf("nothing was undone: the %d file(s) already put stay in the bucket of %s", put, r.Target.Name))
}

// spool copies one entry to a private temporary file and returns its SHA-256 and size, with the file ready to be read.
func spool(r io.Reader) (sum string, size int64, f *os.File, err error) {
	f, err = os.CreateTemp("", "dymploy-publish-*")
	if err != nil {
		return "", 0, nil, err
	}
	fail := func(err error) (string, int64, *os.File, error) {
		f.Close()
		os.Remove(f.Name())
		return "", 0, nil, err
	}
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return fail(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, f, nil
}
