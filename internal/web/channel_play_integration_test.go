//go:build integration

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/health"
)

// TestChannelPlaySuggestion: a source that keeps falling behind on a
// channel gets a banner and a list badge, and applying the suggestion moves
// its streams below the others.
func TestChannelPlaySuggestion(t *testing.T) {
	b, q := pgUI(t)
	ids := seedStreams(t, q)
	ctx := context.Background()
	c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "KTLA"})
	if err != nil {
		t.Fatal(err)
	}
	list := []string{"BBC One HD@A", "Only A@A", "BBC One HD@B"}
	for i, s := range list {
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: ids[s], Priority: int32(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	base := "/admin/channels/" + strconv.FormatInt(c.ID, 10)

	_, body := b.get(base)
	if strings.Contains(body, "Play health") {
		t.Fatalf("banner with no play history:\n%s", body)
	}

	rows, _ := q.ListChannelStreamSources(ctx, []int64{c.ID})
	provA := rows[0].ProviderID
	now := time.Now().UTC().Truncate(time.Minute)
	for _, o := range []struct {
		stream  int64
		outcome string
		n       int32
	}{
		{ids["BBC One HD@A"], health.OutcomeFellBehind, 3},
		{ids["BBC One HD@A"], health.OutcomePlayed10m, 2},
		{ids["BBC One HD@B"], health.OutcomePlayed10m, 6},
	} {
		prov := provA
		if o.stream == ids["BBC One HD@B"] {
			prov = rows[2].ProviderID
		}
		if err := q.UpsertOutcomeBucket(ctx, db.UpsertOutcomeBucketParams{BucketStart: now, ProviderID: prov, StreamID: o.stream,
			Outcome: o.outcome, Count: o.n}); err != nil {
			t.Fatal(err)
		}
	}

	_, body = b.get(base)
	src := "provider #" + strconv.FormatInt(provA, 10)
	if !strings.Contains(body, src+" fell behind real time 3× in 0.3 h watched") || !strings.Contains(body, "Apply suggested order") {
		t.Fatalf("no suggestion banner:\n%s", body)
	}
	_, body = b.get("/admin/channels")
	if !strings.Contains(body, "reorder suggested") {
		t.Errorf("channels list lacks the badge:\n%s", body)
	}

	resp, body := b.htmx(http.MethodPost, base+"/streams/play-order", nil, "stream-list")
	if resp.StatusCode != 200 {
		t.Fatalf("apply = %d\n%s", resp.StatusCode, body)
	}
	want := []int64{ids["BBC One HD@B"], ids["BBC One HD@A"], ids["Only A@A"]}
	if got := order(t, q, c.ID); !eq(got, want) {
		t.Fatalf("order after apply = %v, want %v", got, want)
	}
	if !strings.Contains(body, "already below the others") || strings.Contains(body, "Apply suggested order") {
		t.Errorf("after apply, want the record without a new suggestion:\n%s", body)
	}
	_, body = b.get("/admin/channels")
	if strings.Contains(body, "reorder suggested") {
		t.Errorf("badge stays after the suggestion was applied")
	}
}
