//go:build integration

package web

import (
	"context"
	"strconv"
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

// TestProvidersAttention: the summary counts enabled providers and their
// connection limits, the Needs attention card and ?status=attention list
// only the failing provider, and its sub-pages share tabs.
func TestProvidersAttention(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	mk := func(name string, conns int32, enabled bool) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + name + ".example/",
			MaxConnections: conns, RefreshIntervalSecs: 3600, Enabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := mk("Failing", 2, true)
	mk("Fine", 3, true)
	mk("Off", 5, false)
	if err := q.MarkProviderFailing(ctx, db.MarkProviderFailingParams{ID: bad.ID, Error: "connection refused"}); err != nil {
		t.Fatal(err)
	}

	_, body := b.get("/admin/providers")
	for _, want := range []string{`2<span class="of"> / 3</span></span><span class="label">providers enabled`,
		`0<span class="of"> / 5</span></span><span class="label">connections in use`,
		`href="/admin/providers?status=attention"`, "Needs attention", "Check settings"} {
		if !strings.Contains(body, want) {
			t.Errorf("providers page lacks %q", want)
		}
	}

	_, body = b.get("/admin/providers?status=attention")
	if !strings.Contains(body, ">Failing</a>") || strings.Contains(body, ">Fine</a>") || strings.Contains(body, ">Off</a>") {
		t.Errorf("status=attention does not list only the failing provider:\n%s", body)
	}
	if !strings.Contains(body, `<option value="attention" selected>`) {
		t.Error("the attention status is not selected")
	}

	id := strconv.FormatInt(bad.ID, 10)
	for page, current := range map[string]string{"streams": "Streams", "groups": "Groups", "vod-categories": "Movies &amp; series", "edit": "Settings"} {
		_, body := b.get("/admin/providers/" + id + "/" + page)
		if want := `href="/admin/providers/` + id + `/` + page + `" aria-current="page">` + current + `</a>`; !strings.Contains(body, want) {
			t.Errorf("%s page lacks its current tab %q", page, want)
		}
	}
}
