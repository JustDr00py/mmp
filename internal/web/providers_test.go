package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source"
)

// fakeStore is an in-memory web.Store.
type fakeStore struct {
	Store     // unimplemented methods panic; channel pages are tested against Postgres
	mu        sync.Mutex
	nextID    int64
	providers map[int64]db.Provider
	streams   map[int64][]db.ProviderStream
	events    []db.Event
}

func newFakeStore() *fakeStore {
	return &fakeStore{nextID: 1, providers: map[int64]db.Provider{}, streams: map[int64][]db.ProviderStream{}}
}

func (f *fakeStore) ListProviders(context.Context) ([]db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Provider
	for _, p := range f.providers {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b db.Provider) int { return int(a.ID - b.ID) })
	return out, nil
}

func (f *fakeStore) GetProvider(_ context.Context, id int64) (db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.providers[id]
	if !ok {
		return db.Provider{}, pgx.ErrNoRows
	}
	return p, nil
}

func (f *fakeStore) CreateProvider(_ context.Context, a db.CreateProviderParams) (db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := db.Provider{ID: f.nextID, Name: a.Name, Type: a.Type, Url: a.Url, Username: a.Username, Password: a.Password,
		EpgUrl: a.EpgUrl, MaxConnections: a.MaxConnections, UserAgent: a.UserAgent,
		RefreshIntervalSecs: a.RefreshIntervalSecs, Priority: a.Priority, Enabled: a.Enabled}
	f.nextID++
	f.providers[p.ID] = p
	return p, nil
}

func (f *fakeStore) UpdateProvider(_ context.Context, a db.UpdateProviderParams) (db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.providers[a.ID]
	p.Name, p.Type, p.Url, p.Username, p.Password, p.EpgUrl = a.Name, a.Type, a.Url, a.Username, a.Password, a.EpgUrl
	p.MaxConnections, p.UserAgent, p.RefreshIntervalSecs, p.Priority, p.Enabled = a.MaxConnections, a.UserAgent, a.RefreshIntervalSecs, a.Priority, a.Enabled
	f.providers[a.ID] = p
	return p, nil
}

func (f *fakeStore) DeleteProvider(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.providers, id)
	return nil
}

func (f *fakeStore) SetProviderEnabled(_ context.Context, a db.SetProviderEnabledParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.providers[a.ID]
	p.Enabled = a.Enabled
	f.providers[a.ID] = p
	return nil
}

func (f *fakeStore) ProviderStreamCounts(context.Context) ([]db.ProviderStreamCountsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.ProviderStreamCountsRow
	for id, ss := range f.streams {
		c := db.ProviderStreamCountsRow{ProviderID: id, Total: int64(len(ss))}
		for _, s := range ss {
			if !s.Disabled {
				c.Active++
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) ListProviderStreams(_ context.Context, a db.ListProviderStreamsParams) ([]db.ProviderStream, error) {
	return f.page(a.ProviderID, "", a.Limit, a.Offset), nil
}

func (f *fakeStore) SearchProviderStreams(_ context.Context, a db.SearchProviderStreamsParams) ([]db.ProviderStream, error) {
	return f.page(a.ProviderID, a.Name, a.Limit, a.Offset), nil
}

// page filters by a %...% ILIKE pattern (as the UI builds it) and pages.
func (f *fakeStore) page(id int64, like string, limit, offset int32) []db.ProviderStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	needle := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(like, "%"), "%"))
	needle = strings.NewReplacer(`\%`, "%", `\_`, "_", `\\`, `\`).Replace(needle)
	var out []db.ProviderStream
	for _, s := range f.streams[id] {
		if strings.Contains(strings.ToLower(s.Name), needle) {
			out = append(out, s)
		}
	}
	if int(offset) >= len(out) {
		return nil
	}
	return out[offset:min(int(offset+limit), len(out))]
}

func (f *fakeStore) ListRecentEvents(context.Context, db.ListRecentEventsParams) ([]db.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events, nil
}

func (f *fakeStore) ChannelNamesByIDs(_ context.Context, ids []int64) ([]db.ChannelNamesByIDsRow, error) {
	var out []db.ChannelNamesByIDsRow
	for _, id := range ids {
		out = append(out, db.ChannelNamesByIDsRow{ID: id, Name: fmt.Sprintf("Channel %d", id)})
	}
	return out, nil
}

func (f *fakeStore) GetStreamsByIDs(_ context.Context, ids []int64) ([]db.GetStreamsByIDsRow, error) {
	var out []db.GetStreamsByIDsRow
	for _, id := range ids {
		out = append(out, db.GetStreamsByIDsRow{ID: id, Name: fmt.Sprintf("Stream %d", id), ProviderName: "Acme"})
	}
	return out, nil
}

type fakeRefresher struct {
	mu             sync.Mutex
	calls          []int64
	forced         []bool
	err            error
	refuseUnforced bool // answer like the disable threshold unless forced
}

func (f *fakeRefresher) RefreshEPG(_ context.Context, p db.Provider) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p.ID)
	if f.err != nil {
		return 0, f.err
	}
	return 4321, nil
}

func (f *fakeRefresher) RefreshProvider(_ context.Context, p db.Provider, force bool) (source.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p.ID)
	f.forced = append(f.forced, force)
	if f.err != nil {
		return source.Result{}, f.err
	}
	if f.refuseUnforced && !force {
		return source.Result{}, fmt.Errorf("refresh provider %d: %w", p.ID, &source.DisableRefusedError{ProviderID: int(p.ID), WouldDisable: 900, Active: 1000, MaxPct: 80})
	}
	return source.Result{Seen: 1234, Upserted: 1234, Disabled: 5}, nil
}

// signedIn returns a browser with an admin session.
func signedIn(t *testing.T) (*browser, *UI) {
	t.Helper()
	srv, _, ui := newUIWith(t)
	b := newBrowser(t, srv.URL)
	if resp, _ := b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	return b, ui
}

func (b *browser) htmx(method, path string, form url.Values, target string) (*http.Response, string) {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.base)
	req.Header.Set("HX-Request", "true")
	if target != "" {
		req.Header.Set("HX-Target", target)
	}
	return b.do(req)
}

var goodForm = url.Values{
	"name": {"Acme IPTV"}, "type": {"xtream"}, "url": {"http://panel.example:8080"},
	"username": {"acmeuser"}, "password": {"acme-secret-pw"}, "max_connections": {"2"},
	"refresh_minutes": {"120"}, "priority": {"1"}, "enabled": {"on"},
}

func TestProviderCRUD(t *testing.T) {
	b, ui := signedIn(t)
	store := ui.Store.(*fakeStore)

	resp, body := b.get("/admin/providers")
	if resp.StatusCode != 200 || !strings.Contains(body, "No providers yet.") || !strings.Contains(body, `aria-current="page">Providers`) {
		t.Fatalf("empty list = %d\n%s", resp.StatusCode, body)
	}
	if resp, body := b.get("/admin/providers/new"); resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("new form = %d", resp.StatusCode)
	}

	// Invalid input re-renders the form with the error and the values
	// typed, but never the password.
	bad := url.Values{}
	for k, v := range goodForm {
		bad[k] = v
	}
	bad.Set("url", "panel.example")
	resp, body = b.post("/admin/providers/new", bad, b.base)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "URL must be an absolute http(s) URL.") ||
		!strings.Contains(body, `value="Acme IPTV"`) || strings.Contains(body, "acme-secret-pw") {
		t.Fatalf("invalid create = %d\n%s", resp.StatusCode, body)
	}
	bad.Set("url", "http://panel.example")
	bad.Set("max_connections", "lots")
	if _, body := b.post("/admin/providers/new", bad, b.base); !strings.Contains(body, "Max connections must be a whole number.") {
		t.Errorf("non-numeric field not reported:\n%s", body)
	}

	resp, _ = b.post("/admin/providers/new", goodForm, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/providers?msg=created" {
		t.Fatalf("create = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	p := store.providers[1]
	if p.Name != "Acme IPTV" || p.Password != "acme-secret-pw" || p.MaxConnections != 2 || p.RefreshIntervalSecs != 7200 || !p.Enabled {
		t.Fatalf("stored = %+v", p)
	}
	_, body = b.get("/admin/providers?msg=created")
	if !strings.Contains(body, "Provider added.") || !strings.Contains(body, "Acme IPTV") || !strings.Contains(body, "1 / 2") {
		t.Errorf("list after create:\n%s", body)
	}
	if _, body := b.get("/admin/providers?msg=<script>"); strings.Contains(body, "<script>") {
		t.Error("msg reflected")
	}

	// Edit: the stored password is never sent to the browser; blank keeps it.
	_, body = b.get("/admin/providers/1/edit")
	if strings.Contains(body, "acme-secret-pw") || !strings.Contains(body, "Leave blank to keep") {
		t.Errorf("edit form leaks or lacks the password hint:\n%s", body)
	}
	edit := url.Values{}
	for k, v := range goodForm {
		edit[k] = v
	}
	edit.Set("name", "Acme Renamed")
	edit.Set("password", "")
	edit.Del("enabled")
	if resp, _ := b.post("/admin/providers/1/edit", edit, b.base); resp.Header.Get("Location") != "/admin/providers?msg=saved" {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	if p := store.providers[1]; p.Name != "Acme Renamed" || p.Password != "acme-secret-pw" || p.Enabled {
		t.Errorf("after update = %+v", p)
	}
	edit.Set("password", "new-secret")
	b.post("/admin/providers/1/edit", edit, b.base)
	if store.providers[1].Password != "new-secret" {
		t.Error("new password not stored")
	}

	// Toggle via htmx returns just the row.
	resp, body = b.htmx(http.MethodPost, "/admin/providers/1/toggle", nil, "provider-1")
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<tr id="provider-1">`) || !strings.Contains(body, "Enabled") || strings.Contains(body, "<html") {
		t.Errorf("toggle fragment = %d\n%s", resp.StatusCode, body)
	}
	if !store.providers[1].Enabled {
		t.Error("toggle did not enable")
	}

	// Delete via htmx: a full-page redirect.
	resp, _ = b.htmx(http.MethodPost, "/admin/providers/1/delete", nil, "")
	if resp.Header.Get("HX-Redirect") != "/admin/providers?msg=deleted" || len(store.providers) != 0 {
		t.Errorf("delete = %d %q, providers %d", resp.StatusCode, resp.Header.Get("HX-Redirect"), len(store.providers))
	}
	for _, path := range []string{"/admin/providers/1/edit", "/admin/providers/1/streams", "/admin/providers/x/edit"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestProviderRefresh(t *testing.T) {
	b, ui := signedIn(t)
	b.post("/admin/providers/new", goodForm, b.base)
	refresher := ui.Refresh.(*fakeRefresher)

	resp, body := b.htmx(http.MethodPost, "/admin/providers/1/refresh", nil, "provider-1")
	if resp.StatusCode != 200 || !strings.Contains(body, "1234 streams, 5 gone") || strings.Contains(body, "<html") ||
		!strings.HasPrefix(strings.TrimSpace(body), `<tr id="provider-1">`) {
		t.Errorf("refresh = %d\n%s", resp.StatusCode, body)
	}
	refresher.err = errors.New("refresh provider 1: xtream: request: Get: dial tcp: connection refused")
	_, body = b.htmx(http.MethodPost, "/admin/providers/1/refresh", nil, "provider-1")
	if !strings.Contains(body, "connection refused") || !strings.Contains(body, "errmsg") {
		t.Errorf("refresh error not shown:\n%s", body)
	}
	if len(refresher.calls) != 2 {
		t.Errorf("refresh calls = %v", refresher.calls)
	}

	// Refused by the disable threshold: the numbers and a confirmed
	// "Disable anyway" that re-posts with force=1.
	refresher.err, refresher.refuseUnforced = nil, true
	_, body = b.htmx(http.MethodPost, "/admin/providers/1/refresh", nil, "provider-1")
	if !strings.Contains(body, "would disable 900 of 1000 active streams (90%)") || !strings.Contains(body, `name="force" value="1"`) ||
		!strings.Contains(body, "Disable anyway") {
		t.Fatalf("refused row:\n%s", body)
	}
	_, body = b.htmx(http.MethodPost, "/admin/providers/1/refresh", url.Values{"force": {"1"}}, "provider-1")
	if !strings.Contains(body, "1234 streams, 5 gone") || refresher.forced[len(refresher.forced)-1] != true {
		t.Errorf("forced refresh:\n%s", body)
	}
}

func TestStreamsBrowser(t *testing.T) {
	b, ui := signedIn(t)
	b.post("/admin/providers/new", goodForm, b.base)
	store := ui.Store.(*fakeStore)
	until := time.Now().Add(time.Hour)
	for i := range 120 {
		s := db.ProviderStream{ID: int64(i + 1), ProviderID: 1, Name: fmt.Sprintf("Channel %03d", i), HealthScore: 1,
			StreamUrl: fmt.Sprintf("http://panel.example:8080/live/acmeuser/acme-secret-pw/%d.ts", i)}
		if i == 7 {
			s.Name, s.CooldownUntil, s.LastError, s.HealthScore = "News 100% HD", &until, "upstream status 503", 0.5
		}
		store.streams[1] = append(store.streams[1], s)
	}

	resp, body := b.get("/admin/providers/1/streams")
	if resp.StatusCode != 200 || strings.Count(body, "<tr") != streamsPerPage+1 || !strings.Contains(body, "Next →") || strings.Contains(body, "← Previous") {
		t.Fatalf("page 1 = %d, %d rows", resp.StatusCode, strings.Count(body, "<tr"))
	}
	if strings.Contains(body, "acme-secret-pw") || strings.Contains(body, "acmeuser") {
		t.Error("stream URLs shown with credentials")
	}
	if !strings.Contains(body, "cooling down") || !strings.Contains(body, "50%") {
		t.Error("cooldown/health not shown")
	}
	_, body = b.get("/admin/providers/1/streams?offset=100")
	if strings.Count(body, "<tr") != 21 || strings.Contains(body, "Next →") || !strings.Contains(body, "101–120") {
		t.Errorf("last page: %d rows", strings.Count(body, "<tr"))
	}

	// Live search (htmx) returns just the table; % is literal.
	resp, body = b.htmx(http.MethodGet, "/admin/providers/1/streams?q=100%25", nil, "streams")
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<div id="streams">`) || strings.Count(body, "<tr") != 2 || !strings.Contains(body, "News 100% HD") {
		t.Errorf("search fragment:\n%s", body)
	}
	if _, body := b.get("/admin/providers/1/streams?q=zzz"); !strings.Contains(body, "No streams match") {
		t.Error("empty search result not explained")
	}
}

func TestProviderPagesRequireAdmin(t *testing.T) {
	srv, store := newUI(t)
	b := newBrowser(t, srv.URL)
	for _, path := range []string{"/admin/providers", "/admin/providers/new", "/admin/providers/1/streams"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s signed out = %d", path, resp.StatusCode)
		}
	}
	if resp, _ := b.post("/admin/providers/new", goodForm, srv.URL); resp.StatusCode != http.StatusSeeOther || len(store.sessions) != 0 {
		t.Errorf("POST signed out = %d", resp.StatusCode)
	}
}

// TestProviderFormDefaultRefresh: the form is pre-filled with
// refresh.interval and a blank interval falls back to it.
func TestProviderFormDefaultRefresh(t *testing.T) {
	b, ui := signedIn(t)
	ui.DefaultRefresh = 90 * time.Minute
	if _, body := b.get("/admin/providers/new"); !strings.Contains(body, `name="refresh_minutes" type="number" min="1" value="90"`) {
		t.Errorf("new form not pre-filled with 90 minutes:\n%s", body)
	}
	f := url.Values{}
	for k, v := range goodForm {
		f[k] = v
	}
	f.Set("refresh_minutes", "")
	b.post("/admin/providers/new", f, b.base)
	if p := ui.Store.(*fakeStore).providers[1]; p.RefreshIntervalSecs != 5400 {
		t.Errorf("blank interval stored %ds, want 5400", p.RefreshIntervalSecs)
	}
}
