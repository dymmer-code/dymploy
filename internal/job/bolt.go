package job

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	jobsBucket    = []byte("jobs")
	numbersBucket = []byte("numbers")
)

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
		if _, err := tx.CreateBucketIfNotExists(jobsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(numbersBucket)
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

// NextNumber implements Store. The counters live in their own bucket, so deleting old jobs never
// sends a name back to #1.
func (s *BoltStore) NextNumber(_ context.Context, name string) (int, error) {
	var n uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(numbersBucket)
		if v := b.Get([]byte(name)); len(v) == 8 {
			n = binary.BigEndian.Uint64(v)
		}
		n++
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], n)
		return b.Put([]byte(name), buf[:])
	})
	return int(n), err
}

// Delete implements Store.
func (s *BoltStore) Delete(_ context.Context, id ID) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(jobsBucket).Delete([]byte(id)) })
}
