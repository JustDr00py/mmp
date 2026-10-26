// Package netguard keeps upstream fetches off the local network. Providers
// choose the URLs MMP fetches (playlist entries, redirects, HLS variants,
// segments and keys), so without a guard a hostile provider could point MMP
// at 127.0.0.1, the LAN or a cloud metadata service and have the answer
// streamed back to a viewer (SSRF).
//
// The check runs on the address actually dialed, after DNS resolution, so
// redirects and DNS rebinding are covered too. It is off until Enable is
// called, so tests talking to httptest servers on loopback keep working.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrBlocked is returned for a dial to a private or local address that no
// allowlisted prefix covers.
var ErrBlocked = errors.New("upstream address is private or local")

// policy is the active allowlist; nil means the guard is off.
var policy atomic.Pointer[[]netip.Prefix]

// Enable turns the guard on. Addresses inside allow may still be dialed
// (e.g. a provider hosted on the LAN, or an HTTP proxy).
func Enable(allow []netip.Prefix) {
	a := append([]netip.Prefix(nil), allow...)
	policy.Store(&a)
}

// Disable turns the guard off.
func Disable() { policy.Store(nil) }

// special ranges that IsPrivate, IsLoopback and friends do not cover.
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT (also Tailscale)
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: embeds an IPv4 address
	netip.MustParsePrefix("2002::/16"),     // 6to4: embeds an IPv4 address
}

// Local reports whether a is loopback, private, link-local or otherwise
// not a public unicast address.
func Local(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return true // loopback, link-local, multicast, unspecified, RFC 1918, ULA
	}
	for _, p := range special {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Check returns ErrBlocked if the guard is on and a may not be dialed.
func Check(a netip.Addr) error {
	allow := policy.Load()
	if allow == nil {
		return nil
	}
	a = a.Unmap()
	if !Local(a) {
		return nil
	}
	for _, p := range *allow {
		if p.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s (allow it with upstream.allow_cidrs)", ErrBlocked, a)
}

// control vets each resolved address just before connecting.
func control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address %q", ErrBlocked, address)
	}
	return Check(ap.Addr())
}

// Transport returns a fresh clone of http.DefaultTransport that dials
// through the guard, for callers that tune their own transport.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: control}).DialContext
	return t
}

// shared pools connections for every Client.
var shared = Transport()

// Client returns an http.Client with no overall timeout that dials through
// the guard. All such clients share one connection pool.
func Client() *http.Client {
	return &http.Client{Transport: shared}
}
