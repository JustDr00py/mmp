//go:build integration

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestProviderHealthOnPage: a provider's recent outcomes score it on the
// providers page, and its backbone gets a row in the backbone health table.
func TestProviderHealthOnPage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "STRONG1", Type: "m3u", Url: "http://strong.example/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: "Strong8K"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertOutcomeBucket(ctx, db.UpsertOutcomeBucketParams{BucketStart: time.Now().UTC(), ProviderID: p.ID,
		Backbone: "Strong8K", Outcome: "open_ok", Count: 20}); err != nil {
		t.Fatal(err)
	}

	_, body := b.get("/admin/providers")
	for _, want := range []string{"Backbone health", "Strong8K", `pill ok">100</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("providers page lacks %q", want)
		}
	}
}
