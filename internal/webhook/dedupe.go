package webhook

import (
	"sync"
	"time"
)

// Dedupe remembers recent delivery ids so that a repeated or retried delivery does not start a
// second job. It is bounded in time and in size.
type Dedupe struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	seen  map[string]time.Time
	order []string // insertion order, oldest first
	now   func() time.Time
}

// NewDedupe remembers at most max ids, each for at most ttl.
func NewDedupe(ttl time.Duration, max int) *Dedupe {
	return &Dedupe{ttl: ttl, max: max, seen: map[string]time.Time{}, now: time.Now}
}

// Claim returns true the first time it sees the (provider, id) pair and false afterwards. An empty
// id is never deduplicated: without it there is nothing to compare.
func (d *Dedupe) Claim(provider, id string) bool {
	if id == "" {
		return true
	}
	key := provider + "\x00" + id
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire()
	if _, dup := d.seen[key]; dup {
		return false
	}
	d.seen[key] = d.now()
	d.order = append(d.order, key)
	for len(d.order) > d.max {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// Release forgets a claim. It is used when the job could not be queued, so that the provider's
// retry is not mistaken for a duplicate.
func (d *Dedupe) Release(provider, id string) {
	if id == "" {
		return
	}
	key := provider + "\x00" + id
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// Len is the number of remembered ids.
func (d *Dedupe) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

func (d *Dedupe) expire() {
	cutoff := d.now().Add(-d.ttl)
	for len(d.order) > 0 {
		k := d.order[0]
		if at, ok := d.seen[k]; ok && at.After(cutoff) {
			return
		}
		delete(d.seen, k)
		d.order = d.order[1:]
	}
}
