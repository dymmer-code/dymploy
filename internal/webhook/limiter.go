package webhook

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter used to blunt floods before any cryptography is done.
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
	now    func() time.Time
}

// NewLimiter allows limit requests per window; limit 0 disables it.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, now: time.Now}
}

// Allow reports whether one more request fits in the current window.
func (l *Limiter) Allow() bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if l.start.IsZero() || t.Sub(l.start) >= l.window {
		l.start, l.count = t, 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

// IPLimiter limits requests per client address, so that one address cannot use up what everybody else
// shares. It keeps a bounded table: when it is full, expired windows go first and, if that is not
// enough, the new address is refused rather than letting memory grow.
type IPLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	max     int
	entries map[string]*ipWindow
	now     func() time.Time
}

type ipWindow struct {
	start time.Time
	count int
}

// NewIPLimiter allows limit requests per window from each address; limit 0 disables it.
func NewIPLimiter(limit int, window time.Duration) *IPLimiter {
	return &IPLimiter{limit: limit, window: window, max: 10000, entries: map[string]*ipWindow{}, now: time.Now}
}

// Allow reports whether one more request from ip fits in its current window.
func (l *IPLimiter) Allow(ip string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	w, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= l.max {
			for k, e := range l.entries {
				if t.Sub(e.start) >= l.window {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= l.max {
				return false
			}
		}
		w = &ipWindow{start: t}
		l.entries[ip] = w
	}
	if t.Sub(w.start) >= l.window {
		w.start, w.count = t, 0
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}
