package webhook

import (
	"testing"
	"time"
)

func TestClaimIsOnce(t *testing.T) {
	d := NewDedupe(time.Hour, 100)
	if !d.Claim("github", "d1") {
		t.Fatal("first claim must succeed")
	}
	if d.Claim("github", "d1") {
		t.Fatal("the same delivery must not be claimed twice")
	}
	if !d.Claim("fake", "d1") {
		t.Fatal("the delivery id is scoped by provider")
	}
}

func TestReleaseLetsARetryThrough(t *testing.T) {
	d := NewDedupe(time.Hour, 100)
	d.Claim("github", "d1")
	d.Release("github", "d1")
	if !d.Claim("github", "d1") {
		t.Fatal("after a failed enqueue the provider's retry must be accepted")
	}
}

func TestClaimsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	d := NewDedupe(time.Minute, 100)
	d.now = func() time.Time { return now }
	d.Claim("github", "d1")
	now = now.Add(2 * time.Minute)
	if !d.Claim("github", "d1") {
		t.Fatal("old claims must expire")
	}
}

func TestSizeIsBounded(t *testing.T) {
	d := NewDedupe(time.Hour, 3)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		d.Claim("github", id)
	}
	if n := d.Len(); n > 3 {
		t.Fatalf("memory must be bounded, holds %d", n)
	}
	if d.Claim("github", "e") {
		t.Fatal("the most recent claims must be remembered")
	}
}

func TestEmptyDeliveryIDIsNeverDeduplicated(t *testing.T) {
	d := NewDedupe(time.Hour, 10)
	if !d.Claim("github", "") || !d.Claim("github", "") {
		t.Fatal("without an id there is nothing to compare")
	}
}
