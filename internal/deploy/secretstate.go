package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type SecretStatus struct {
	Target     string    `json:"target"`
	State      string    `json:"state"`
	DeployedAt time.Time `json:"deployed_at"`
	CheckedAt  time.Time `json:"checked_at,omitzero"`
}
type fingerprint struct {
	Sum string    `json:"hmac"`
	At  time.Time `json:"deployed_at"`
}
type SecretState struct {
	mu      sync.Mutex
	dir     string
	key     []byte
	targets *Targets
	source  SecretFetcher
	records map[string]fingerprint
	cached  map[string]SecretStatus
	busy    map[string]bool
}

func NewSecretState(dir string, targets *Targets, source SecretFetcher) (*SecretState, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyfile := filepath.Join(dir, "key")
	key, err := os.ReadFile(keyfile)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyfile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		ce := f.Close()
		if err == nil {
			err = ce
		}
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(keyfile)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("secret state key must be private and 32 bytes")
	}
	s := &SecretState{dir: dir, key: key, targets: targets, source: source, records: map[string]fingerprint{}, cached: map[string]SecretStatus{}, busy: map[string]bool{}}
	raw, err := os.ReadFile(filepath.Join(dir, "fingerprints.json"))
	if err == nil {
		err = json.Unmarshal(raw, &s.records)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if s.records == nil {
		s.records = map[string]fingerprint{}
	}
	return s, nil
}
func (s *SecretState) sum(name string, data []byte) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}
func (s *SecretState) Record(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]fingerprint{}
	for k, v := range s.records {
		next[k] = v
	}
	next[name] = fingerprint{s.sum(name, data), time.Now().UTC()}
	if err := s.save(next); err != nil {
		return err
	}
	delete(s.cached, name)
	return nil
}
func (s *SecretState) Forget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[name]; !ok {
		return nil
	}
	next := map[string]fingerprint{}
	for k, v := range s.records {
		if k != name {
			next[k] = v
		}
	}
	if err := s.save(next); err != nil {
		return err
	}
	delete(s.cached, name)
	return nil
}
func (s *SecretState) save(next map[string]fingerprint) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".fingerprints-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.dir, "fingerprints.json"))
	}
	if err != nil {
		return err
	}
	s.records = next
	return nil
}

// Check compares opaque bytes with the last successful delivery. Concurrent deployments win: a
// check of an old baseline never overwrites the new baseline or caches a misleading result.
func (s *SecretState) Check(ctx context.Context, name string) SecretStatus {
	s.mu.Lock()
	rec, ok := s.records[name]
	s.mu.Unlock()
	out := SecretStatus{Target: name, State: "unavailable", DeployedAt: rec.At, CheckedAt: time.Now().UTC()}
	t, exists := s.targets.Get(name)
	if ok && exists && t.Secrets != nil && s.source != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if data, err := s.source.Fetch(ctx, *t.Secrets); err == nil {
			out.State = "changed"
			if hmac.Equal([]byte(s.sum(name, data)), []byte(rec.Sum)) {
				out.State = "current"
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records[name] == rec {
		s.cached[name] = out
	}
	return out
}

// Snapshot is nonblocking. It refreshes stale comparisons in the background, at most once every
// five minutes per target, and only for destinations with a recorded successful secrets delivery.
func (s *SecretState) Snapshot() []SecretStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SecretStatus
	for name, rec := range s.records {
		target, ok := s.targets.Get(name)
		if !ok || target.Secrets == nil {
			continue
		}
		status, ok := s.cached[name]
		if !ok {
			status = SecretStatus{Target: name, State: "checking", DeployedAt: rec.At}
		}
		out = append(out, status)
		if !s.busy[name] && (!ok || time.Since(status.CheckedAt) > 5*time.Minute) {
			s.busy[name] = true
			go func() { s.Check(context.Background(), name); s.mu.Lock(); delete(s.busy, name); s.mu.Unlock() }()
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}
