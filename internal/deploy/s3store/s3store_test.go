package s3store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

// The example "GET Object" of the Signature Version 4 documentation of AWS (header authentication).
func TestSignatureMatchesTheDocumentedExample(t *testing.T) {
	c := credentials{id: "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	headers := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": emptySHA256,
		"x-amz-date":           "20130524T000000Z",
	}
	got := authorization(c, "us-east-1", "GET", "/test.txt", headers, emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

type object struct {
	body []byte
	meta string
	typ  string
}

// fakeS3 keeps objects in memory and records the requests it saw.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]object
	seen    []string
	auth    []string
	failPut bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.EscapedPath())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		o, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Meta-Sha256", o.meta)
		if r.Method == http.MethodGet {
			w.Write(o.body)
		}
	case http.MethodPut:
		if f.failPut {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.objects[r.URL.Path] = object{body: body, meta: r.Header.Get("X-Amz-Meta-Sha256"), typ: r.Header.Get("Content-Type")}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setup(t *testing.T) (*fakeS3, deploy.Bucket) {
	t.Helper()
	f := &fakeS3{objects: map[string]object{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cred := filepath.Join(t.TempDir(), "s3.env")
	if err := os.WriteFile(cred, []byte("# key\nS3_ACCESS_KEY_ID=AKTEST\nS3_SECRET_ACCESS_KEY=the-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := (&Store{}).Open(context.Background(), deploy.Target{Kind: deploy.KindS3, Endpoint: srv.URL, Bucket: "books", Region: "garage", CredentialsFile: cred})
	if err != nil {
		t.Fatal(err)
	}
	return f, b
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestPutStoresAndReadsBackAndSkipsWhatIsThere(t *testing.T) {
	f, b := setup(t)
	ctx := context.Background()
	data := "%PDF-1.7 hello"

	written, err := b.Put(ctx, "libros/mi libro.pdf", "application/pdf", sum(data), int64(len(data)), strings.NewReader(data))
	if err != nil || !written {
		t.Fatalf("written=%v err=%v", written, err)
	}
	o := f.objects["/books/libros/mi libro.pdf"]
	if string(o.body) != data || o.typ != "application/pdf" || o.meta != sum(data) {
		t.Fatalf("stored %+v", o)
	}
	if !strings.Contains(strings.Join(f.seen, " "), "PUT /books/libros/mi%20libro.pdf") {
		t.Fatalf("the key was not encoded on the wire: %v", f.seen)
	}
	if got, err := b.Sum(ctx, "libros/mi libro.pdf"); err != nil || got != sum(data) {
		t.Fatalf("sum %q %v", got, err)
	}

	f.seen = nil
	written, err = b.Put(ctx, "libros/mi libro.pdf", "application/pdf", sum(data), int64(len(data)), strings.NewReader(data))
	if err != nil || written {
		t.Fatalf("an identical file must be left alone: written=%v err=%v", written, err)
	}
	if len(f.seen) != 1 || !strings.HasPrefix(f.seen[0], "HEAD") {
		t.Fatalf("only a HEAD expected: %v", f.seen)
	}

	written, err = b.Put(ctx, "libros/mi libro.pdf", "application/pdf", sum("new"), 3, strings.NewReader("new"))
	if err != nil || !written || string(f.objects["/books/libros/mi libro.pdf"].body) != "new" {
		t.Fatalf("a different file must replace it: written=%v err=%v", written, err)
	}
}

func TestEveryRequestIsSignedWithTheKeyAndNothingLeaks(t *testing.T) {
	f, b := setup(t)
	_, err := b.Put(context.Background(), "a.txt", "text/plain", sum("x"), 1, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range f.auth {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKTEST/") || !strings.Contains(a, "/garage/s3/aws4_request") ||
			!strings.Contains(a, "x-amz-content-sha256") || strings.Contains(a, "the-secret") {
			t.Fatalf("authorization %q", a)
		}
	}
}

func TestAnErrorSaysWhatTheServerAnsweredAndNothingMore(t *testing.T) {
	f, b := setup(t)
	f.failPut = true
	_, err := b.Put(context.Background(), "a.txt", "text/plain", sum("x"), 1, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "AccessDenied") ||
		strings.Contains(err.Error(), "AKTEST") || strings.Contains(err.Error(), "the-secret") {
		t.Fatalf("%v", err)
	}
}

func TestCredentialsMustBePrivateAndComplete(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s := &Store{}
	if err := s.Check(deploy.Target{CredentialsFile: write("ok", "S3_ACCESS_KEY_ID=a\nS3_SECRET_ACCESS_KEY=b\n", 0o600)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(deploy.Target{CredentialsFile: write("open", "S3_ACCESS_KEY_ID=a\nS3_SECRET_ACCESS_KEY=b\n", 0o644)}); err == nil {
		t.Fatal("a file readable by others must be refused")
	}
	if err := s.Check(deploy.Target{CredentialsFile: write("half", "S3_ACCESS_KEY_ID=a\n", 0o600)}); err == nil {
		t.Fatal("a missing secret must be refused")
	}
	if err := s.Check(deploy.Target{CredentialsFile: filepath.Join(dir, "nope")}); err == nil {
		t.Fatal("a missing file must be refused")
	}
}
