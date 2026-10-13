package webhook

import (
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func req(remote string, xff ...string) *http.Request {
	r, _ := http.NewRequest("POST", "/hooks/github", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

var maple = []netip.Prefix{netip.MustParsePrefix("192.168.1.40/32")}

func TestTheClientIsTheConnectionUnlessItComesFromATrustedProxy(t *testing.T) {
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"direct", req("203.0.113.9:5555"), "203.0.113.9"},
		{"direct, header ignored", req("203.0.113.9:5555", "198.51.100.1"), "203.0.113.9"},
		{"through the proxy", req("192.168.1.40:41000", "198.51.100.7"), "198.51.100.7"},
		{"a forged first entry does not pass", req("192.168.1.40:41000", "10.0.0.1, 198.51.100.7"), "198.51.100.7"},
		{"a chain of proxies", req("192.168.1.40:41000", "198.51.100.7, 192.168.1.40"), "198.51.100.7"},
		{"two header lines", req("192.168.1.40:41000", "10.9.9.9", "198.51.100.7"), "198.51.100.7"},
		{"unreadable header falls back to the proxy", req("192.168.1.40:41000", "not-an-address"), "192.168.1.40"},
		{"no header", req("192.168.1.40:41000"), "192.168.1.40"},
		{"ipv6", req("[2001:db8::1]:443"), "2001:db8::1"},
		{"ipv4 in ipv6 form", req("[::ffff:192.168.1.40]:41000", "198.51.100.7"), "198.51.100.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClientIP(c.r, maple); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestEachAddressHasItsOwnBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewIPLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if !l.Allow("203.0.113.9") {
			t.Fatal("within the limit")
		}
	}
	if l.Allow("203.0.113.9") {
		t.Fatal("the third from the same address is over")
	}
	if !l.Allow("198.51.100.7") {
		t.Fatal("another address is not affected")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("203.0.113.9") {
		t.Fatal("the window passed")
	}
	if !NewIPLimiter(0, time.Minute).Allow("x") || !(*IPLimiter)(nil).Allow("x") {
		t.Fatal("0 disables it, and no limiter means no limit")
	}
}

func TestTheTableOfAddressesIsBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewIPLimiter(1, time.Minute)
	l.max = 3
	l.now = func() time.Time { return now }
	for _, ip := range []string{"a", "b", "c"} {
		if !l.Allow(ip) {
			t.Fatal(ip)
		}
	}
	if l.Allow("d") {
		t.Fatal("a full table refuses new addresses instead of growing")
	}
	now = now.Add(2 * time.Minute)
	if !l.Allow("d") {
		t.Fatal("expired windows make room")
	}
}
