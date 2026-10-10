package artifact

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

type entry struct {
	Type byte
	Body string
	Link string
	Mode int64
}

func read(t *testing.T, file string) map[string]entry {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]entry{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s carries an owner: %d:%d %q %q", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		out[h.Name] = entry{h.Typeflag, string(b), h.Linkname, h.Mode}
	}
}

func TestPackStoresEachPathUnderItsBaseNameWithTheRightDigest(t *testing.T) {
	root, dest := t.TempDir(), filepath.Join(t.TempDir(), "out")
	write(t, root, "_build/prod/rel/app/bin/app", "#!/bin/sh\n", 0o755)
	write(t, root, "_build/prod/rel/app/lib/a.beam", "beam", 0o644)
	write(t, root, "priv/static/x.css", "css", 0o644)
	res, err := Pack(root, Spec{Name: "app", Paths: []string{"_build/prod/rel/app", "priv/static/x.css"}}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(dest, "app.tar.gz") {
		t.Fatalf("path %s", res.Path)
	}
	got := read(t, res.Path)
	if got["app/bin/app"].Body != "#!/bin/sh\n" || got["app/bin/app"].Mode != 0o755 || got["app/lib/a.beam"].Body != "beam" || got["x.css"].Body != "css" {
		t.Fatalf("contents: %+v", got)
	}
	if got["app/"].Type != tar.TypeDir {
		t.Fatalf("directory entry missing: %+v", got)
	}
	data, _ := os.ReadFile(res.Path)
	sum := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(sum[:]) || res.Size != int64(len(data)) || res.Files != len(got) {
		t.Fatalf("result %+v does not describe the file (%d bytes, %d entries)", res, len(data), len(got))
	}
	if fi, _ := os.Stat(res.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 1 {
		t.Fatalf("leftovers in the destination: %v", ents)
	}
}

func TestPackIsReproducible(t *testing.T) {
	root := t.TempDir()
	write(t, root, "d/b", "2", 0o644)
	write(t, root, "d/a", "1", 0o644)
	a, err := Pack(root, Spec{Name: "x", Paths: []string{"d"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Pack(root, Spec{Name: "x", Paths: []string{"d"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatal("same tree, different archive")
	}
}

func TestExcludeByNameAnywhereAndByArchivePath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/keep.txt", "k", 0o644)
	write(t, root, "app/a.log", "l", 0o644)
	write(t, root, "app/sub/b.log", "l", 0o644)
	write(t, root, "app/tmp/big", "x", 0o644)
	write(t, root, "app/sub/tmp/y", "y", 0o644)
	res, err := Pack(root, Spec{Name: "x", Paths: []string{"app"}, Exclude: []string{"*.log", "app/tmp"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, res.Path)
	for _, gone := range []string{"app/a.log", "app/sub/b.log", "app/tmp/", "app/tmp/big"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s should be excluded", gone)
		}
	}
	for _, kept := range []string{"app/keep.txt", "app/sub/tmp/y"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("%s should be kept (a pattern with / only matches the archive path)", kept)
		}
	}
}

func TestSymlinksStayInsideWhatIsPacked(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/bin/real", "r", 0o755)
	if err := os.Symlink("real", filepath.Join(root, "app/bin/alias")); err != nil {
		t.Fatal(err)
	}
	res, err := Pack(root, Spec{Name: "x", Paths: []string{"app"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if e := read(t, res.Path)["app/bin/alias"]; e.Type != tar.TypeSymlink || e.Link != "real" {
		t.Fatalf("link not preserved: %+v", e)
	}
}

func TestRefusals(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret", "s", 0o644)
	cases := map[string]struct {
		setup func(root string)
		spec  Spec
		want  string
	}{
		"absolute link": {func(r string) { write(t, r, "app/f", "f", 0o644); os.Symlink("/etc/passwd", filepath.Join(r, "app/l")) },
			Spec{Name: "x", Paths: []string{"app"}}, "absolute path"},
		"link out of the packed path": {func(r string) {
			write(t, r, "other", "o", 0o644)
			write(t, r, "app/f", "f", 0o644)
			os.Symlink("../other", filepath.Join(r, "app/l"))
		},
			Spec{Name: "x", Paths: []string{"app"}}, "outside what is being packed"},
		"path through a link out of the build": {func(r string) { os.Symlink(outside, filepath.Join(r, "esc")) },
			Spec{Name: "x", Paths: []string{"esc/secret"}}, "leads outside the build"},
		"path that is itself a link out": {func(r string) { os.Symlink(filepath.Join(outside, "secret"), filepath.Join(r, "s")) },
			Spec{Name: "x", Paths: []string{"s"}}, "absolute path"},
		"missing path":        {func(r string) {}, Spec{Name: "x", Paths: []string{"nope"}}, "was not produced by the build"},
		"parent escape":       {func(r string) {}, Spec{Name: "x", Paths: []string{"../x"}}, "must stay inside"},
		"everything excluded": {func(r string) { write(t, r, "a.log", "l", 0o644) }, Spec{Name: "x", Paths: []string{"a.log"}, Exclude: []string{"*.log"}}, "empty"},
		"too big":             {func(r string) { write(t, r, "a", "0123456789", 0o644) }, Spec{Name: "x", Paths: []string{"a"}, MaxBytes: 5}, "larger than the limit"},
		"bad name":            {func(r string) { write(t, r, "a", "a", 0o644) }, Spec{Name: "../x", Paths: []string{"a"}}, "invalid artifact name"},
		"bad pattern":         {func(r string) { write(t, r, "a", "a", 0o644) }, Spec{Name: "x", Paths: []string{"a"}, Exclude: []string{"["}}, "invalid exclude"},
		"no paths":            {func(r string) {}, Spec{Name: "x"}, "no paths"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root, dest := t.TempDir(), filepath.Join(t.TempDir(), "o")
			c.setup(root)
			_, err := Pack(root, c.spec, dest)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
			if ents, _ := os.ReadDir(dest); len(ents) != 0 {
				t.Fatalf("a failed pack left files behind: %v", ents)
			}
		})
	}
}

func TestSpecialFilesAreRefused(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/f", "f", 0o644)
	if err := mkfifo(filepath.Join(root, "app/pipe")); err != nil {
		t.Skip("cannot create a fifo here")
	}
	_, err := Pack(root, Spec{Name: "x", Paths: []string{"app"}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("got %v", err)
	}
}

func TestCleanRemovesOnlyOldJobDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"old", "new"} {
		write(t, dir, n+"/app.tar.gz", "x", 0o600)
	}
	write(t, dir, "loose-file", "x", 0o600)
	long := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old"), long, long); err != nil {
		t.Fatal(err)
	}
	n, err := Clean(dir, 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Fatal("the old directory is still there")
	}
	for _, keep := range []string{"new/app.tar.gz", "loose-file"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("%s must stay: %v", keep, err)
		}
	}
}

func TestCleanToleratesAMissingDirectoryAndReportsOtherFailures(t *testing.T) {
	if n, err := Clean(filepath.Join(t.TempDir(), "none"), time.Hour); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o600)
	if _, err := Clean(file, time.Hour); err == nil {
		t.Fatal("a file is not a directory of artifacts")
	}
	dir := t.TempDir()
	write(t, dir, "locked/inner/x", "x", 0o600)
	long := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, "locked"), long, long)
	os.Chmod(filepath.Join(dir, "locked", "inner"), 0o500)
	defer os.Chmod(filepath.Join(dir, "locked", "inner"), 0o700)
	if os.Getuid() != 0 {
		if n, err := Clean(dir, time.Hour); err == nil || n != 0 {
			t.Fatalf("removed %d, err %v: a directory that cannot be removed is an error", n, err)
		}
	}
}
