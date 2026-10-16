package output

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/auth"
)

// newLimitedHandlers builds the test handlers with a 3-attempt lockout.
func newLimitedHandlers(t *testing.T) (*Handlers, http.Handler) {
	t.Helper()
	h, r := newTestHandlers(t)
	h.Limit = auth.NewLimiter(3, 5*time.Minute)
	return h, r
}

func TestGetPHPLockout(t *testing.T) {
	_, h := newLimitedHandlers(t)
	for range 3 {
		if code, _, _ := get(t, h, "/get.php?username=alice&password=bad"); code != http.StatusUnauthorized {
			t.Fatalf("attempt = %d, want 401", code)
		}
	}
	// The correct password is refused while locked, with a Retry-After.
	code, hdr, body := get(t, h, "/get.php?username=alice&password=alice-pw1")
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" {
		t.Fatalf("locked get.php = %d Retry-After=%q", code, hdr.Get("Retry-After"))
	}
	if strings.Contains(body, "alice-pw1") || strings.Contains(body, "EXTINF") {
		t.Fatalf("locked response leaked content: %q", body)
	}
	// A different account from the same client is refused too (IP key).
	if code, _, _ := get(t, h, "/get.php?username=root&password=root-pw1"); code != http.StatusTooManyRequests {
		t.Fatalf("other account from locked IP = %d", code)
	}
	// Token auth is not throttled: the bearer path still works.
	if code, _, _ := get(t, h, "/playlist/tok-alice.m3u"); code != 200 {
		t.Fatalf("token playlist during lockout = %d", code)
	}
}

func TestPlayerAPILockoutKeepsXtreamShape(t *testing.T) {
	_, h := newLimitedHandlers(t)
	for range 3 {
		code, _, body := get(t, h, "/player_api.php?username=alice&password=bad")
		if code != 200 || !strings.Contains(body, `"auth":0`) {
			t.Fatalf("attempt = %d %q", code, body)
		}
	}
	// Locked: still a 200 with auth 0 — players must not see an HTTP error.
	code, _, body := get(t, h, "/player_api.php?username=alice&password=alice-pw1")
	if code != 200 || !strings.Contains(body, `"auth":0`) || strings.Contains(body, `"auth":1`) {
		t.Fatalf("locked player_api = %d %q", code, body)
	}
}

func TestLiveLockout(t *testing.T) {
	_, h := newLimitedHandlers(t)
	for range 3 {
		if code, _, _ := get(t, h, "/live/alice/bad/10.ts"); code != http.StatusUnauthorized {
			t.Fatalf("attempt = %d, want 401", code)
		}
	}
	if code, hdr, _ := get(t, h, "/live/alice/alice-pw1/10.ts"); code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" {
		t.Fatalf("locked live = %d Retry-After=%q", code, hdr.Get("Retry-After"))
	}
}

func TestNoLimiterMeansNoLimiting(t *testing.T) {
	_, h := newTestHandlers(t) // Limit is nil
	for range 20 {
		if code, _, _ := get(t, h, "/get.php?username=alice&password=bad"); code != http.StatusUnauthorized {
			t.Fatalf("attempt = %d, want 401", code)
		}
	}
	if code, _, _ := get(t, h, "/get.php?username=alice&password=alice-pw1"); code != 200 {
		t.Fatalf("correct password after failures = %d, want 200", code)
	}
}
