package deploytest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

// Object is what a FakeStore keeps for a key.
type Object struct {
	Body        []byte
	ContentType string
	SHA256      string
}

// FakeStore is a deploy.Store whose bucket lives in memory.
type FakeStore struct {
	// OpenErr and CheckErr make Open and Check fail; PutErr makes the Put of that key fail; Corrupt makes the
	// read-back return something else, as a damaged write would.
	OpenErr, CheckErr error
	PutErr            map[string]error
	Corrupt           bool

	mu      sync.Mutex
	Opened  []deploy.Target
	Objects map[string]Object
	Puts    []string
}

// Check implements deploy.Store.
func (f *FakeStore) Check(deploy.Target) error { return f.CheckErr }

// Open implements deploy.Store.
func (f *FakeStore) Open(_ context.Context, t deploy.Target) (deploy.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OpenErr != nil {
		return nil, f.OpenErr
	}
	f.Opened = append(f.Opened, t)
	if f.Objects == nil {
		f.Objects = map[string]Object{}
	}
	return bucket{f}, nil
}

type bucket struct{ f *FakeStore }

func (b bucket) Put(_ context.Context, key, contentType, sha string, size int64, body io.ReadSeeker) (bool, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	if err := b.f.PutErr[key]; err != nil {
		return false, err
	}
	if o, ok := b.f.Objects[key]; ok && o.SHA256 == sha {
		return false, nil
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return false, err
	}
	if int64(len(data)) != size {
		return false, errors.New("size differs")
	}
	b.f.Objects[key] = Object{Body: data, ContentType: contentType, SHA256: sha}
	b.f.Puts = append(b.f.Puts, key)
	return true, nil
}

func (b bucket) Sum(_ context.Context, key string) (string, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	o, ok := b.f.Objects[key]
	if !ok {
		return "", errors.New("not found")
	}
	if b.f.Corrupt {
		return "00", nil
	}
	h := sha256.Sum256(o.Body)
	return hex.EncodeToString(h[:]), nil
}
