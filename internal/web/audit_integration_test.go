//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/audit"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/events"
)

func TestAuditLog(t *testing.T) {
	b, q := pgUI(t, func(d *Deps, pool *pgxpool.Pool) {
		qq := db.New(pool)
		d.Audit = &audit.Recorder{Store: qq, Namer: audit.StoreNamer(qq)}
	})
	ctx := context.Background()

	// A failed sign-in from another browser.
	other := newBrowser(t, b.base)
	if resp, _ := other.post("/admin/login", url.Values{"username": {"boss"}, "password": {"wrong"}}, b.base); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", resp.StatusCode)
	}

	// Changes: create, rename and delete a group; a refused change.
	if resp, _ := b.post("/admin/groups", url.Values{"name": {"Sports"}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create group = %d", resp.StatusCode)
	}
	groups, _ := q.ListChannelGroups(ctx)
	gid := strconv.FormatInt(groups[0].ID, 10)
	b.post("/admin/groups/"+gid, url.Values{"name": {"Sport"}}, b.base)
	b.post("/admin/groups/"+gid+"/delete", nil, b.base)
	if resp, _ := b.post("/admin/groups", url.Values{"name": {""}}, b.base); resp.StatusCode < 400 {
		t.Fatalf("empty group name = %d, want refused", resp.StatusCode)
	}

	_, body := b.get("/admin/audit")
	for _, want := range []string{
		"Someone tried to sign in as <strong>boss</strong>",
		"<strong>boss</strong> signed in",
		"<strong>boss</strong> created channel group",
		"<strong>boss</strong> updated channel group “Sport”",
		"<strong>boss</strong> deleted channel group “Sport”", // named before it was deleted
		"/admin/groups/" + gid + "/delete",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
	if n := strings.Count(body, "<li>"); n != 5 {
		t.Errorf("audit log has %d entries, want 5 (the refused change is not one):\n%s", n, body)
	}
	if _, body := b.get("/admin/audit?type=" + url.QueryEscape("channel group")); strings.Count(body, "<li>") != 3 {
		t.Error("type filter")
	}
	if _, body := b.get("/admin/audit?admin=nobody"); !strings.Contains(body, "Nothing recorded that matches") {
		t.Error("admin filter")
	}

	// Pruned with the rest of the history.
	if n, err := events.Prune(ctx, q, time.Now().AddDate(0, 0, 31)); err != nil || n != 5 {
		t.Fatalf("Prune 31 days on = %d, %v; want the 5 entries", n, err)
	}
}
