//go:build integration

package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/events"
)

func TestUserActivity(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Username: "alice", PasswordHash: "x", Role: "customer",
		MaxConnections: 1, Enabled: true, StreamToken: "tok-alice"})
	if err != nil {
		t.Fatal(err)
	}
	page := "/admin/users/" + strconv.FormatInt(u.ID, 10)
	if _, body := b.get(page); !strings.Contains(body, "Not seen yet") || !strings.Contains(body, "Nothing watched yet") {
		t.Fatalf("empty activity:\n%s", body)
	}

	news, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News 24"})
	gone, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Gone"})
	now := time.Now()
	touch := func(network, ip string, at time.Time) {
		t.Helper()
		if err := q.TouchUserNetwork(ctx, db.TouchUserNetworkParams{UserID: u.ID, Network: network, Ip: ip, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	touch("203.0.113.7", "203.0.113.7", now.Add(-48*time.Hour))
	touch("203.0.113.7", "203.0.113.7", now.Add(-time.Minute)) // same network again: last seen moves
	touch("198.51.100.4", "198.51.100.4", now.Add(-time.Hour))
	touch("2001:db8:1:2::/64", "last 2001:db8:1:2::9", now.Add(-2*time.Hour))
	touch("192.0.2.1", "192.0.2.1", now.AddDate(0, 0, -40)) // old: pruned below
	if err := q.TouchUserNetwork(ctx, db.TouchUserNetworkParams{UserID: 999999, Network: "x", Ip: "x", At: now}); err != nil {
		t.Fatalf("unknown user: %v (should be skipped quietly)", err)
	}
	watch := func(ch int64, start time.Time, d time.Duration, out string) {
		t.Helper()
		if err := q.InsertViewingSession(ctx, db.InsertViewingSessionParams{UserID: u.ID, ChannelID: ch, Ip: "203.0.113.7",
			Output: out, StartedAt: start, EndedAt: start.Add(d)}); err != nil {
			t.Fatal(err)
		}
	}
	watch(news.ID, now.Add(-30*time.Minute), 25*time.Minute, "ts")
	watch(gone.ID, now.Add(-3*time.Hour), 90*time.Second, "hls")
	watch(news.ID, now.AddDate(0, 0, -40), time.Minute, "ts") // old
	if err := q.DeleteChannel(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-time.Minute), now.AddDate(0, 0, -40)} {
		if err := q.InsertAuthFailure(ctx, db.InsertAuthFailureParams{Ts: at, Username: "alice", Ip: "198.51.100.66"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.InsertAuthFailure(ctx, db.InsertAuthFailureParams{Ts: now, Username: "nobody", Ip: "198.51.100.66"}); err != nil {
		t.Fatal(err)
	}

	_, body := b.get(page)
	for _, want := range []string{
		"Last seen 1m ago from <span class=\"mono\">203.0.113.7</span>",
		"Used from 3 different networks in the last 24 hours", // 203.0.113.7, 198.51.100.4, the IPv6 /64
		"2001:db8:1:2::/64", "last 2001:db8:1:2::9",
		">News 24</a>", "25m00s", "deleted channel", "HLS",
		"198.51.100.66", "1 in the last 24 hours",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("user page lacks %q", want)
		}
	}
	if _, body := b.get("/admin/users"); !strings.Contains(body, "1m ago <span class=\"sub mono\">203.0.113.7</span>") {
		t.Error("users list lacks last seen")
	}

	// Pruning drops the 40-day-old network, session and failure.
	if n, err := events.Prune(ctx, q, now); err != nil || n != 3 {
		t.Fatalf("Prune = %d, %v; want 3 rows", n, err)
	}
	if nets, _ := q.UserNetworks(ctx, db.UserNetworksParams{UserID: u.ID, Limit: 10}); len(nets) != 3 {
		t.Errorf("networks after pruning = %d, want 3", len(nets))
	}
	if ws, _ := q.UserViewingSessions(ctx, db.UserViewingSessionsParams{UserID: u.ID, Limit: 10}); len(ws) != 2 {
		t.Errorf("sessions after pruning = %d, want 2", len(ws))
	}

	// Deleting the user deletes their activity.
	if err := q.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if ws, _ := q.UserViewingSessions(ctx, db.UserViewingSessionsParams{UserID: u.ID, Limit: 10}); len(ws) != 0 {
		t.Errorf("sessions outlive their user: %d", len(ws))
	}
}
