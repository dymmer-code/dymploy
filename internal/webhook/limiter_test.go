package webhook

import (
	"testing"
	"time"
)

func TestLimiterAllowsUpToTheLimitPerWindow(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("request %d must pass", i)
		}
	}
	if l.Allow() {
		t.Fatal("the 4th request in the window must be refused")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow() {
		t.Fatal("a new window starts after the old one")
	}
}

func TestLimiterWithZeroLimitIsDisabled(t *testing.T) {
	l := NewLimiter(0, time.Minute)
	for i := 0; i < 1000; i++ {
		if !l.Allow() {
			t.Fatal("limit 0 means unlimited")
		}
	}
}
