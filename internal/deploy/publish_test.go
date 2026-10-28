package deploy_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/deploy/deploytest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

type entry struct {
	name, body string
	typeflag   byte
}

func archive(t *testing.T, entries []entry) (string, artifact.Record) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "books.tar.gz")
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typeflag}
		if e.typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if e.typeflag == tar.TypeDir || e.typeflag == tar.TypeSymlink {
			h.Size = 0
			h.Linkname = e.body
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	out.Close()
	raw, _ := os.ReadFile(file)
	sum := sha256.Sum256(raw)
	return file, artifact.Record{Name: "books", File: "books.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))}
}

func bucketTarget() deploy.Target {
	return deploy.Target{Name: "garage-books", Kind: deploy.KindS3, Endpoint: "http://s3", Bucket: "altenwald-books", CredentialsFile: "/x",
		Allow: webhook.Policy{Rules: []webhook.Rule{{Provider: "github", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}}}}}
}

type pub struct {
	d     *deploy.Deployer
	store *deploytest.FakeStore
	logs  []string
}

func newPub() *pub {
	p := &pub{store: &deploytest.FakeStore{}}
	p.d = &deploy.Deployer{Remote: &deploytest.Fake{}, Store: p.store, Targets: deploy.NewTargets()}
	return p
}

func (p *pub) run(t *testing.T, entries []entry, deployYAML string) (int, error) {
	t.Helper()
	file, rec := archive(t, entries)
	rep := runner.NewLineReporter(func(s runner.Stream, line string) { p.logs = append(p.logs, line) })
	defer rep.Close()
	return p.d.Run(context.Background(), deploy.Request{
		Target: bucketTarget(), Deploy: definition(t, deployYAML), Event: ev, JobID: "job/2", Record: rec, File: file, Log: rep,
	})
}

const books = "deploy:\n  target: garage-books\n  path: libros/\n"

func TestPublishPutsEveryFileUnderThePath(t *testing.T) {
	p := newPub()
	code, err := p.run(t, []entry{
		{name: "rust.pdf", body: "PDF"}, {name: "rust.epub", body: "EPUB"},
		{name: "docs", typeflag: tar.TypeDir}, {name: "docs/notas.txt", body: "n"},
		{name: "link", typeflag: tar.TypeSymlink, body: "rust.pdf"},
	}, books)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, strings.Join(p.logs, "\n"))
	}
	got := p.store.Objects
	if len(got) != 3 || string(got["libros/rust.pdf"].Body) != "PDF" || got["libros/rust.pdf"].ContentType != "application/pdf" ||
		got["libros/rust.epub"].ContentType != "application/epub+zip" || string(got["libros/docs/notas.txt"].Body) != "n" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(strings.Join(p.logs, "\n"), "skipped link") || !strings.Contains(strings.Join(p.logs, "\n"), "3 put, 0 unchanged") {
		t.Fatalf("log:\n%s", strings.Join(p.logs, "\n"))
	}
}

func TestPublishLeavesAloneWhatIsAlreadyThere(t *testing.T) {
	p := newPub()
	entries := []entry{{name: "a.pdf", body: "one"}}
	if code, err := p.run(t, entries, books); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	p.store.Puts = nil
	if code, err := p.run(t, entries, books); err != nil || code != 0 || len(p.store.Puts) != 0 {
		t.Fatalf("code=%d err=%v puts=%v", code, err, p.store.Puts)
	}
}

func TestPublishWithoutPathPutsAtTheRoot(t *testing.T) {
	p := newPub()
	if code, err := p.run(t, []entry{{name: "a.pdf", body: "x"}}, "deploy:\n  target: garage-books\n"); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if _, ok := p.store.Objects["a.pdf"]; !ok {
		t.Fatalf("%v", p.store.Objects)
	}
}

func TestPublishFailureIsExit8AndSaysNothingIsUndone(t *testing.T) {
	p := newPub()
	p.store.PutErr = map[string]error{"libros/b.pdf": errors.New("PUT libros/b.pdf: the server answered 403 (AccessDenied)")}
	code, err := p.run(t, []entry{{name: "a.pdf", body: "1"}, {name: "b.pdf", body: "2"}, {name: "c.pdf", body: "3"}}, books)
	if err != nil || code != deploy.ExitPublish {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, ok := p.store.Objects["libros/c.pdf"]; ok {
		t.Fatal("it must stop at the first failure")
	}
	log := strings.Join(p.logs, "\n")
	if !strings.Contains(log, "403") || !strings.Contains(log, "nothing was undone") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestPublishChecksWhatItPut(t *testing.T) {
	p := newPub()
	p.store.Corrupt = true
	code, err := p.run(t, []entry{{name: "a.pdf", body: "1"}}, books)
	if err != nil || code != deploy.ExitPublish || !strings.Contains(strings.Join(p.logs, "\n"), "could not verify") {
		t.Fatalf("code=%d err=%v\n%s", code, err, strings.Join(p.logs, "\n"))
	}
}

func TestPublishRefusesUnsafeNamesInTheArchive(t *testing.T) {
	p := newPub()
	code, err := p.run(t, []entry{{name: "../x.pdf", body: "1"}}, books)
	if err != nil || code != deploy.ExitPublish || len(p.store.Objects) != 0 {
		t.Fatalf("code=%d err=%v %v", code, err, p.store.Objects)
	}
}

func TestPublishCannotReachTheBucket(t *testing.T) {
	p := newPub()
	p.store.OpenErr = errors.New("credentials_file: nope")
	_, err := p.run(t, []entry{{name: "a.pdf", body: "1"}}, books)
	if err == nil || !strings.Contains(err.Error(), "garage-books") {
		t.Fatalf("%v", err)
	}
}

func TestAuthorizeDeployKnowsTheKindOfTheTarget(t *testing.T) {
	p := newPub()
	p.d.Targets.Set(map[string]deploy.Target{"garage-books": bucketTarget(), "production": target()})
	ok := provider.Event{Provider: "github", Owner: "acme", Repo: "app", Branch: "main"}

	if _, err := p.d.AuthorizeDeploy(definition(t, books), ok); err != nil {
		t.Fatalf("a bucket takes target and path: %v", err)
	}
	for name, yaml := range map[string]string{
		"steps":   "deploy:\n  target: garage-books\n  steps: [{run: x}]\n",
		"check":   "deploy:\n  target: garage-books\n  check: {run: x, attempts: 1, interval: 1s, timeout: 1s}\n",
		"secrets": "deploy:\n  target: garage-books\n  secrets: true\n",
	} {
		if _, err := p.d.AuthorizeDeploy(definition(t, yaml), ok); err == nil || !strings.Contains(err.Error(), "is a bucket") {
			t.Errorf("%s on a bucket must be refused: %v", name, err)
		}
	}
	if _, err := p.d.AuthorizeDeploy(definition(t, "deploy:\n  target: production\n  path: x/\n  steps: [{run: x}]\n"), ok); err == nil || !strings.Contains(err.Error(), "deploy.path") {
		t.Errorf("a path on a machine must be refused: %v", err)
	}
	if _, err := p.d.AuthorizeDeploy(definition(t, "deploy:\n  target: production\n"), ok); err == nil || !strings.Contains(err.Error(), "deploy.steps") {
		t.Errorf("a machine needs steps: %v", err)
	}
	p.d.Store = nil
	if _, err := p.d.AuthorizeDeploy(definition(t, books), ok); err == nil {
		t.Error("a server with no store cannot deploy to a bucket")
	}
}

func TestPublishOfAnEmptyArtifactDoesNothingAndSucceeds(t *testing.T) {
	p := newPub()
	code, err := p.run(t, nil, books)
	if err != nil || code != 0 || len(p.store.Objects) != 0 {
		t.Fatalf("code=%d err=%v %v", code, err, p.store.Objects)
	}
	if !strings.Contains(strings.Join(p.logs, "\n"), "0 put, 0 unchanged") {
		t.Fatalf("log:\n%s", strings.Join(p.logs, "\n"))
	}
}

func TestPublishPutsThePagesAfterEverythingElse(t *testing.T) {
	p := newPub()
	code, err := p.run(t, []entry{
		{name: "index.html", body: "<a href=a.css>"}, {name: "a.css", body: "body{}"},
		{name: "docs", typeflag: tar.TypeDir}, {name: "docs/page.HTM", body: "<p>"}, {name: "img.png", body: "PNG"},
		{name: "link", typeflag: tar.TypeSymlink, body: "a.css"},
	}, "deploy:\n  target: garage-books\n")
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, strings.Join(p.logs, "\n"))
	}
	if got := strings.Join(p.store.Puts, " "); got != "a.css img.png index.html docs/page.HTM" {
		t.Fatalf("order of the puts: %s", got)
	}
	if n := strings.Count(strings.Join(p.logs, "\n"), "skipped link"); n != 1 {
		t.Fatalf("what is skipped is said once, not %d times", n)
	}
}
