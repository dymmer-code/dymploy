package webhook

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of whoever made the request. Behind a reverse proxy the connection comes
// from the proxy, and the real address is in X-Forwarded-For, but that header is only believed when the
// connection really comes from a proxy the administrator listed: anybody else could write anything in it.
// The list is read from the right, skipping the proxies, so what a client put at the left of the header
// cannot pass for its address.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	remote = remote.Unmap()
	if !contains(trusted, remote) {
		return remote.String()
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return remote.String() // a header we cannot read: fall back to the proxy rather than guess
		}
		if a = a.Unmap(); !contains(trusted, a) {
			return a.String()
		}
	}
	return remote.String()
}

func contains(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
