//go:build integration

package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
)

var shownPassword = regexp.MustCompile(`id="cred-pass" class="mono" value="([^"]+)"`)

func TestUserPages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	sports, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Sports"})
	news, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "News"})

	_, body := b.get("/admin/users")
	if !strings.Contains(body, ">boss</a>") {
		t.Fatalf("list lacks the admin:\n%s", body)
	}
	_, body = b.get("/admin/users/new")
	if !strings.Contains(body, "> Sports</label>") || !strings.Contains(body, "leave blank to generate one") {
		t.Errorf("new form:\n%s", body)
	}

	// Create with a generated password: shown once, straight from the POST.
	resp, body := b.post("/admin/users/new", url.Values{"username": {"alice"}, "role": {"customer"}, "max_connections": {"2"},
		"expires": {"2030-06-30"}, "enabled": {"on"}, "bouquet": {strconv.FormatInt(sports.ID, 10)}}, b.base)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create = %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	m := shownPassword.FindStringSubmatch(body)
	if m == nil || len(m[1]) != 16 || !strings.Contains(body, "shown only now") {
		t.Fatalf("generated password not shown:\n%s", body)
	}
	pw := m[1]
	users, _ := q.ListUsers(ctx)
	var alice db.User
	for _, u := range users {
		if u.Username == "alice" {
			alice = u
		}
	}
	if !auth.CheckPassword(alice.PasswordHash, pw) || alice.MaxConnections != 2 || alice.ExpiresAt == nil ||
		!alice.ExpiresAt.Equal(time.Date(2030, 6, 30, 23, 59, 59, 0, time.UTC)) || alice.StreamToken == "" {
		t.Fatalf("stored = %+v", alice)
	}
	if ids, _ := q.ListUserBouquetIDs(ctx, alice.ID); len(ids) != 1 || ids[0] != sports.ID {
		t.Errorf("bouquets = %v", ids)
	}
	unescaped := html.UnescapeString(body)
	for _, want := range []string{
		`value="http://mmp.example:8080"`,
		`value="http://mmp.example:8080/playlist/` + alice.StreamToken + `.m3u"`,
		`value="http://mmp.example:8080/epg/` + alice.StreamToken + `.xml"`,
		"http://mmp.example:8080/get.php?output=ts&password=" + pw + "&type=m3u_plus&username=alice",
	} {
		if !strings.Contains(unescaped, want) {
			t.Errorf("credentials lack %s", want)
		}
	}
	base := "/admin/users/" + strconv.FormatInt(alice.ID, 10)

	// Later visits never show the password.
	_, body = b.get(base)
	if strings.Contains(body, pw) || strings.Contains(body, `id="cred-pass"`) || !strings.Contains(body, "Reset password") {
		t.Error("password visible after creation")
	}

	// Duplicate username and a bad password are form errors.
	if resp, body := b.post("/admin/users/new", url.Values{"username": {"alice"}, "role": {"customer"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "username is taken") {
		t.Errorf("duplicate = %d", resp.StatusCode)
	}
	if _, body := b.post("/admin/users/new", url.Values{"username": {"bob"}, "role": {"customer"}, "password": {"a/b c"}}, b.base); !strings.Contains(body, "Password must be") && !strings.Contains(body, "Password may not") {
		t.Errorf("bad password not reported")
	}

	// Edit: limits, expiry cleared, bouquets swapped, disabled.
	resp, _ = b.post(base, url.Values{"username": {"alice"}, "role": {"customer"}, "max_connections": {"0"}, "expires": {""},
		"bouquet": {strconv.FormatInt(news.ID, 10)}}, b.base)
	if resp.Header.Get("Location") != base+"?msg=saved" {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	alice, _ = q.GetUser(ctx, alice.ID)
	if alice.MaxConnections != 0 || alice.ExpiresAt != nil || alice.Enabled || !auth.CheckPassword(alice.PasswordHash, pw) {
		t.Errorf("after update = %+v", alice)
	}
	if ids, _ := q.ListUserBouquetIDs(ctx, alice.ID); len(ids) != 1 || ids[0] != news.ID {
		t.Errorf("bouquets after update = %v", ids)
	}
	_, body = b.get("/admin/users")
	if !strings.Contains(body, "0 / ∞") || !strings.Contains(body, "disabled</span>") {
		t.Errorf("list does not reflect the edit")
	}

	// Reset password: a new one, shown once; the old one stops working.
	resp, body = b.post(base+"/password", url.Values{}, b.base)
	m = shownPassword.FindStringSubmatch(body)
	if resp.StatusCode != 200 || m == nil || m[1] == pw {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	alice, _ = q.GetUser(ctx, alice.ID)
	if auth.CheckPassword(alice.PasswordHash, pw) || !auth.CheckPassword(alice.PasswordHash, m[1]) {
		t.Error("reset did not replace the password")
	}

	// Rotate token.
	old := alice.StreamToken
	resp, _ = b.post(base+"/token", url.Values{}, b.base)
	alice, _ = q.GetUser(ctx, alice.ID)
	if resp.Header.Get("Location") != base+"?msg=token" || alice.StreamToken == old {
		t.Errorf("rotate = %d, token changed %v", resp.StatusCode, alice.StreamToken != old)
	}

	// The last enabled admin can be neither demoted nor deleted.
	boss := "/admin/users/1"
	if resp, body := b.post(boss, url.Values{"username": {"boss"}, "role": {"customer"}, "enabled": {"on"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "last enabled admin") {
		t.Errorf("demote last admin = %d", resp.StatusCode)
	}
	if resp, body := b.post(boss+"/delete", url.Values{}, b.base); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "cannot be deleted") {
		t.Errorf("delete last admin = %d", resp.StatusCode)
	}
	if resp, _ := b.post(base+"/delete", url.Values{}, b.base); resp.Header.Get("Location") != "/admin/users?msg=deleted" {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := b.get(base); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted user = %d", resp.StatusCode)
	}
}
