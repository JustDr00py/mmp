package source

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysadmin/mmp/internal/db"
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
