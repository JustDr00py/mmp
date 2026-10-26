package netguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Cleanup(Disable)
	Enable([]netip.Prefix{netip.MustParsePrefix("192.168.50.0/24")})
	cases := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.29.0.10", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.100.1.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"::ffff:127.0.0.1", true},
		{"64:ff9b::a00:1", true},
		{"8.8.8.8", false},
		{"2606:4700::1111", false},
		{"192.168.50.7", false}, // allowlisted
	}
	for _, c := range cases {
		err := Check(netip.MustParseAddr(c.addr))
		if got := errors.Is(err, ErrBlocked); got != c.blocked {
			t.Errorf("Check(%s) blocked = %v, want %v (err %v)", c.addr, got, c.blocked, err)
		}
	}
}

func TestDisabledAllowsEverything(t *testing.T) {
	Disable()
	if err := Check(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("disabled guard blocked loopback: %v", err)
	}
}

func TestClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	t.Cleanup(Disable)

	Enable(nil)
	if _, err := Client().Get(srv.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Get loopback with guard on: err = %v, want ErrBlocked", err)
	}

	Enable([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	resp, err := Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get allowlisted loopback: %v", err)
	}
	resp.Body.Close()
}
