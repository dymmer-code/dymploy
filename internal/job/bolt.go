package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var jobsBucket = []byte("jobs")

// BoltStore is a Store persisted in a single bbolt file (pure Go, no cgo, one writer process).
type BoltStore struct{ db *bolt.DB }

// OpenBolt opens or creates the database at path (mode 0600). A second process opening the same file
// fails after a short wait instead of corrupting it: only one daemon may own the state.
func OpenBolt(path string) (*BoltStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating the state directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening the job database %s (is another dymploy using it?): %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(jobsBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &BoltStore{db: db}, nil
}

// Close releases the database.
func (s *BoltStore) Close() error { return s.db.Close() }

// Put implements Store.
func (s *BoltStore) Put(_ context.Context, j Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(jobsBucket).Put([]byte(j.ID), raw) })
}

// Get implements Store.
func (s *BoltStore) Get(_ context.Context, id ID) (Job, error) {
	var j Job
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(jobsBucket).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &j)
	})
	return j, err
}

// List implements Store.
func (s *BoltStore) List(_ context.Context, f Filter) ([]Job, error) {
	var out []Job
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(jobsBucket).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() { // ids sort by creation: newest first
			var j Job
			if err := json.Unmarshal(v, &j); err != nil {
				return fmt.Errorf("job %s: %w", bytes.TrimSpace(k), err)
			}
			if f.Matches(j) {
				out = append(out, j)
				if f.Limit > 0 && len(out) == f.Limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// Delete implements Store.
func (s *BoltStore) Delete(_ context.Context, id ID) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(jobsBucket).Delete([]byte(id)) })
}
