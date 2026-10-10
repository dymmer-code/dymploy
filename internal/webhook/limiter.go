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
