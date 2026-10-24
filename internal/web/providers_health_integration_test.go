//go:build integration

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestProviderNotAnswering: a provider whose last check failed is flagged
// on the providers page and the dashboard, until it answers again.
func TestProviderNotAnswering(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "TREX01", Type: "xtream", Url: "http://line.example.xyz/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const msg = "Server name line.example.xyz does not exist (DNS)."
	if err := q.MarkProviderFailing(ctx, db.MarkProviderFailingParams{ID: p.ID, Error: msg}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/providers", "/admin/"} {
		if _, body := b.get(path); !strings.Contains(body, msg) {
			t.Errorf("%s does not show the failure:\n%s", path, body)
		}
	}
	if _, body := b.get("/admin/providers"); !strings.Contains(body, "Not answering") {
		t.Error("providers page has no Not answering badge")
	}

	if err := q.MarkProviderHealthy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/providers", "/admin/"} {
		if _, body := b.get(path); strings.Contains(body, msg) || strings.Contains(body, "not answering") {
			t.Errorf("%s still shows the failure after recovery", path)
		}
	}
}
