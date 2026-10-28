// Package s3store is the deploy.Store that puts files in a bucket of an S3 server (Garage, MinIO, S3).
//
// Requests are path-style and signed with AWS Signature Version 4 by this package itself. The key comes from the
// target's credentials file, which must not be readable by other users; neither it nor the signatures are ever
// written in an error or a log. dymploy never deletes anything in a bucket: it puts files, and reads them back.
package s3store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/secret"
)

// DefaultRegion is what a target signs for when it does not say.
const DefaultRegion = "us-east-1"

// emptySHA256 is the SHA-256 of an empty body, the payload hash of requests that have none.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Store implements deploy.Store.
type Store struct {
	// Client is used for the requests; nil means one that connects in 15 s and follows no redirect.
	Client *http.Client
	// Now returns the time to sign with (tests); nil means time.Now.
	Now func() time.Time
}

type credentials struct{ id, secret string }

// readCredentials parses the NAME=value lines of the file.
func readCredentials(path string) (credentials, error) {
	text, err := secret.ReadFile(path)
	if err != nil {
		return credentials{}, fmt.Errorf("credentials_file: %w", err)
	}
	var c credentials
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "S3_ACCESS_KEY_ID":
			c.id = strings.TrimSpace(value)
		case "S3_SECRET_ACCESS_KEY":
			c.secret = strings.TrimSpace(value)
		}
	}
	if c.id == "" || c.secret == "" {
		return credentials{}, fmt.Errorf("credentials_file %s needs the lines S3_ACCESS_KEY_ID=... and S3_SECRET_ACCESS_KEY=...", path)
	}
	return c, nil
}

// Check implements deploy.Store: the credentials file must exist, be private and have both values.
func (s *Store) Check(t deploy.Target) error {
	_, err := readCredentials(t.CredentialsFile)
	return err
}

// Open implements deploy.Store. Nothing is sent yet: the first request is the first Put.
func (s *Store) Open(_ context.Context, t deploy.Target) (deploy.Bucket, error) {
	c, err := readCredentials(t.CredentialsFile)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(t.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	region := t.Region
	if region == "" {
		region = DefaultRegion
	}
	client := s.Client
	if client == nil {
		client = &http.Client{
			Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext, ResponseHeaderTimeout: 2 * time.Minute},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	return &bucket{client: client, base: base, name: t.Bucket, region: region, cred: c, now: now, cache: t.CacheControl}, nil
}

type bucket struct {
	client *http.Client
	base   *url.URL
	name   string
	region string
	cred   credentials
	now    func() time.Time
	// cache is the Cache-Control every object is stored with ('' stores none).
	cache string
}

// escape encodes one path segment the way Signature Version 4 wants: everything but the unreserved characters.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = escape(p)
	}
	return strings.Join(parts, "/")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// authorization is the Authorization header of Signature Version 4 for a request with no query: the headers
// (lowercase names, including host, x-amz-date and x-amz-content-sha256) are all signed.
func authorization(c credentials, region, method, canonicalURI string, headers map[string]string, payloadSHA string, when time.Time) string {
	stamp, day := when.UTC().Format("20060102T150405Z"), when.UTC().Format("20060102")
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, k := range names {
		canonical.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{method, canonicalURI, "", canonical.String(), signed, payloadSHA}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.id, scope, signed, sig)
}

// request builds a signed request for the object. extra are the headers to send and sign (lowercase names).
func (b *bucket) request(ctx context.Context, method, key, payloadSHA string, extra map[string]string, body io.Reader, size int64) (*http.Request, error) {
	encoded := "/" + escape(b.name) + "/" + escapeKey(key)
	u := *b.base
	u.Path = "/" + b.name + "/" + key
	u.RawPath = encoded
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	when := b.now().UTC()
	stamp := when.Format("20060102T150405Z")

	headers := map[string]string{"host": req.URL.Host, "x-amz-content-sha256": payloadSHA, "x-amz-date": stamp}
	for k, v := range extra {
		headers[k] = v
	}
	auth := authorization(b.cred, b.region, method, encoded, headers, payloadSHA, when)
	for name, v := range headers {
		if name != "host" {
			req.Header.Set(name, v)
		}
	}
	req.Header.Set("Authorization", auth)
	return req, nil
}

// do sends the request and returns the response; the error never carries the request (so no signature).
func (b *bucket) do(req *http.Request) (*http.Response, error) {
	resp, err := b.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s %s: %w", req.Method, b.name, err)
	}
	return resp, nil
}

func status(op string, resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := ""
	if code := between(string(msg), "<Code>", "</Code>"); code != "" {
		detail = " (" + code + ")"
	}
	return fmt.Errorf("%s: the server answered %s%s", op, strconv.Itoa(resp.StatusCode), detail)
}

func between(s, a, z string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, z)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// Put implements deploy.Bucket.
func (b *bucket) Put(ctx context.Context, key, contentType, sha string, size int64, body io.ReadSeeker) (bool, error) {
	head, err := b.request(ctx, http.MethodHead, key, emptySHA256, nil, nil, 0)
	if err != nil {
		return false, err
	}
	resp, err := b.do(head)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	// Same content is left alone, unless the object lacks the Cache-Control this target now asks for.
	if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Amz-Meta-Sha256") == sha && (b.cache == "" || resp.Header.Get("Cache-Control") == b.cache) {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return false, fmt.Errorf("HEAD %s: the server answered %d", key, resp.StatusCode)
	}

	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	hdr := map[string]string{"content-type": contentType, "x-amz-meta-sha256": sha}
	if b.cache != "" {
		hdr["cache-control"] = b.cache
	}
	put, err := b.request(ctx, http.MethodPut, key, sha, hdr, body, size)
	if err != nil {
		return false, err
	}
	resp, err = b.do(put)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, status("PUT "+key, resp)
	}
	return true, nil
}

// Sum implements deploy.Bucket.
func (b *bucket) Sum(ctx context.Context, key string) (string, error) {
	get, err := b.request(ctx, http.MethodGet, key, emptySHA256, nil, nil, 0)
	if err != nil {
		return "", err
	}
	resp, err := b.do(get)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", status("GET "+key, resp)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", fmt.Errorf("reading %s back: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
