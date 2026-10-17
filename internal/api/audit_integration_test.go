//go:build integration

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/audit"
	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

// TestAPIAudit: API sign-ins and changes land in the audit log as the
// admin who made them, marked as API.
func TestAPIAudit(t *testing.T) {
	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	env.Auth = auth.NewAuthenticator(q, nil, time.Hour, false)
	env.Audit = &audit.Recorder{Store: q, Namer: audit.StoreNamer(q)}
	if err := auth.EnsureAdmin(ctx, q, slog.New(slog.DiscardHandler), "root", "root-pw1"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apiServer(env))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(http.MethodPost, "/api/v1/auth/login", `{"username":"root","password":"nope"}`); code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", code)
	}
	if code := post(http.MethodPost, "/api/v1/auth/login", `{"username":"root","password":"root-pw1"}`); code != 200 {
		t.Fatalf("login = %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/channel-groups", strings.NewReader(`{"name":"Kids"}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var g struct{ ID int64 }
	_ = json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if code := post(http.MethodDelete, "/api/v1/channel-groups/"+strconv.FormatInt(g.ID, 10), ""); code >= 300 {
		t.Fatalf("delete group = %d", code)
	}

	rows, err := q.ListAudit(ctx, db.ListAuditParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		got = append(got, r.Username+" "+r.Action+" "+r.TargetType+" "+r.TargetName+" "+r.Via)
	}
	want := []string{
		"root failed to sign in   api",
		"root signed in   api",
		"root created channel group  api",
		"root deleted channel group Kids api",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
