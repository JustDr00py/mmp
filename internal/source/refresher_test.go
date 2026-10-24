package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source/xtream"
)

// TestFetchErrorsHideCredentials: transport failures must not echo the
// playlist URL's credentials (they reach logs and the admin UI).
func TestFetchErrorsHideCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused
	r := NewRefresher(slog.New(slog.DiscardHandler), nil, 80)
	for name, raw := range map[string]string{
		"refused": base + "/get.php?username=someuser&password=s3cretPass&type=m3u_plus",
		"invalid": "http://someuser:s3cretPass@bad host/get.php?password=s3cretPass",
	} {
		_, err := r.fetchM3U(context.Background(), db.Provider{ID: 1, Type: "m3u", Url: raw})
		if err == nil {
			t.Fatalf("%s: fetch succeeded", name)
		}
		if strings.Contains(err.Error(), "s3cretPass") {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
	}
}

// TestDescribe: provider errors read as the problem an admin fixes.
func TestDescribe(t *testing.T) {
	dns := fmt.Errorf("login: xtream: request: %w", &net.DNSError{Err: "no such host", Name: "line.example.xyz", IsNotFound: true})
	for _, c := range []struct {
		err  error
		want string
	}{
		{dns, "Server name line.example.xyz does not exist (DNS)"},
		{fmt.Errorf("login: %w", xtream.ErrUnauthorized), "rejected the login"},
		{ErrNoStreams, "no streams"},
		{errors.New("xtream: status 502"), "xtream: status 502"},
	} {
		if got := Describe(c.err); !strings.Contains(got, c.want) {
			t.Errorf("Describe(%v) = %q, want %q in it", c.err, got, c.want)
		}
	}
}

// TestHostPacer: a second request to one server waits out the gap; other
// servers and URLs without a host don't wait.
func TestHostPacer(t *testing.T) {
	ctx := context.Background()
	h := hostPacer{gap: 200 * time.Millisecond}
	start := time.Now()
	for _, u := range []string{"http://a.example/x", "http://b.example:8080/y", "not a url", ""} {
		if err := h.wait(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("distinct hosts waited %v", d)
	}
	if err := h.wait(ctx, "http://A.example:25461/z"); err != nil { // same server, other port and case
		t.Fatal(err)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("same host waited only %v, want the 200ms gap", d)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.wait(cctx, "http://a.example/x"); err == nil {
		t.Error("wait ignored a cancelled context")
	}
}
