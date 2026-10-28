package job

import (
	"encoding/binary"
	"encoding/json"
	"errors"

	bolt "go.etcd.io/bbolt"
)

var eventBucket = []byte("events")
var sinkBucket = []byte("event-sinks")

type Transition struct {
	Sequence uint64 `json:"sequence"`
	Job      Job    `json:"job"`
}

func eventKey(n uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], n); return b[:] }

// EventSinks enables transactional state-change recording and keeps durable delivery cursors.
// Removing a sink retires its pending deliveries; a new sink starts at the retained backlog.
func (s *BoltStore) EventSinks(names []string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		events, err := tx.CreateBucketIfNotExists(eventBucket)
		if err != nil {
			return err
		}
		_ = events
		sinks, err := tx.CreateBucketIfNotExists(sinkBucket)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, n := range names {
			if n == "" {
				return errors.New("empty event sink")
			}
			keep[n] = true
			if sinks.Get([]byte(n)) == nil {
				if err := sinks.Put([]byte(n), eventKey(0)); err != nil {
					return err
				}
			}
		}
		c := sinks.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if !keep[string(k)] {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return pruneEvents(tx)
	})
}

func recordTransition(tx *bolt.Tx, previous []byte, j Job) error {
	sinks := tx.Bucket(sinkBucket)
	if sinks == nil || sinks.Stats().KeyN == 0 {
		return nil
	}
	var old Job
	if len(previous) > 0 {
		if err := json.Unmarshal(previous, &old); err != nil {
			return err
		}
		if old.State == j.State {
			return nil
		}
	}
	b := tx.Bucket(eventBucket)
	n, err := b.NextSequence()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(Transition{Sequence: n, Job: j})
	if err != nil {
		return err
	}
	return b.Put(eventKey(n), raw)
}

func (s *BoltStore) NextEvent(sink string) (out Transition, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(sinkBucket)
		if b == nil || len(b.Get([]byte(sink))) != 8 {
			return errors.New("unknown event sink")
		}
		n := binary.BigEndian.Uint64(b.Get([]byte(sink)))
		c := tx.Bucket(eventBucket).Cursor()
		k, v := c.Seek(eventKey(n + 1))
		if k == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(v, &out)
	})
	return
}
func (s *BoltStore) AckEvent(sink string, n uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(sinkBucket)
		if b == nil || len(b.Get([]byte(sink))) != 8 {
			return errors.New("unknown event sink")
		}
		if n < binary.BigEndian.Uint64(b.Get([]byte(sink))) {
			return nil
		}
		if err := b.Put([]byte(sink), eventKey(n)); err != nil {
			return err
		}
		return pruneEvents(tx)
	})
}
func pruneEvents(tx *bolt.Tx) error {
	b, s := tx.Bucket(eventBucket), tx.Bucket(sinkBucket)
	if b == nil || s == nil {
		return nil
	}
	min := ^uint64(0)
	if err := s.ForEach(func(_, v []byte) error {
		n := binary.BigEndian.Uint64(v)
		if n < min {
			min = n
		}
		return nil
	}); err != nil {
		return err
	}
	c := b.Cursor()
	for k, _ := c.First(); k != nil && binary.BigEndian.Uint64(k) <= min; k, _ = c.Next() {
		if err := c.Delete(); err != nil {
			return err
		}
	}
	return nil
}
