package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RealIP sets r.RemoteAddr to the client behind a trusted reverse proxy.
// Only a request whose connection comes from a trusted proxy is rewritten,
// so a client cannot pick its own address with a forged header. The client
// is the rightmost X-Forwarded-For entry that is not itself a trusted proxy
// (each proxy appends the address it saw, so entries left of that are the
// client's own claims); without X-Forwarded-For, X-Real-IP. Everything
// downstream (logs, login lockout, the dashboard) reads r.RemoteAddr.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		if len(trusted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, port, err := net.SplitHostPort(r.RemoteAddr)
			peer, perr := netip.ParseAddr(host)
			if err == nil && perr == nil && isTrusted(peer.Unmap()) {
				if client, ok := forwardedClient(r.Header, isTrusted); ok {
					r.RemoteAddr = net.JoinHostPort(client.String(), port)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedClient picks the client address from the forwarding headers.
func forwardedClient(h http.Header, isTrusted func(netip.Addr) bool) (netip.Addr, bool) {
	var hops []netip.Addr
	for _, v := range h.Values("X-Forwarded-For") {
		for _, part := range strings.Split(v, ",") {
			a, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return netip.Addr{}, false // a garbled chain is not trusted at all
			}
			hops = append(hops, a.Unmap())
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !isTrusted(hops[i]) {
			return hops[i], true
		}
	}
	if len(hops) > 0 {
		return hops[0], true // every hop is a trusted proxy: the first one is the client
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(h.Get("X-Real-IP"))); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}
