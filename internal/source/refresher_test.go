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
