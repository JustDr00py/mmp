package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source"
)

// fakeStore is an in-memory Store. The embedded *db.Queries satisfies the
// channel/bouquet methods (they panic if a provider-focused test calls them).
type fakeStore struct {
	*db.Queries
	providers        map[int64]db.Provider
	streams          map[int64][]db.ProviderStream // providerID -> streams
	nextID           int64
	lastCreateParams db.CreateProviderParams
	lastUpdateParams db.UpdateProviderParams
}

func newFakeStore() *fakeStore {
	return &fakeStore{providers: map[int64]db.Provider{}, streams: map[int64][]db.ProviderStream{}}
}

func (f *fakeStore) CreateProvider(_ context.Context, arg db.CreateProviderParams) (db.Provider, error) {
	f.nextID++
	f.lastCreateParams = arg
	p := db.Provider{
		ID: f.nextID, Name: arg.Name, Type: arg.Type, Url: arg.Url,
		Username: arg.Username, Password: arg.Password, EpgUrl: arg.EpgUrl,
		MaxConnections: arg.MaxConnections, UserAgent: arg.UserAgent,
		RefreshIntervalSecs: arg.RefreshIntervalSecs, Priority: arg.Priority, Enabled: arg.Enabled,
	}
	f.providers[p.ID] = p
	return p, nil
}

func (f *fakeStore) GetProvider(_ context.Context, id int64) (db.Provider, error) {
	p, ok := f.providers[id]
	if !ok {
		return db.Provider{}, pgx.ErrNoRows
	}
	return p, nil
}

func (f *fakeStore) ListProviders(_ context.Context) ([]db.Provider, error) {
	out := make([]db.Provider, 0, len(f.providers))
	for _, p := range f.providers {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) UpdateProvider(_ context.Context, arg db.UpdateProviderParams) (db.Provider, error) {
	p, ok := f.providers[arg.ID]
	if !ok {
		return db.Provider{}, pgx.ErrNoRows
	}
	p.Name, p.Type, p.Url = arg.Name, arg.Type, arg.Url
	p.Username, p.Password, p.EpgUrl = arg.Username, arg.Password, arg.EpgUrl
	p.MaxConnections, p.UserAgent = arg.MaxConnections, arg.UserAgent
	p.RefreshIntervalSecs, p.Priority, p.Enabled = arg.RefreshIntervalSecs, arg.Priority, arg.Enabled
	f.providers[arg.ID] = p
	f.lastUpdateParams = arg
	return p, nil
}

func (f *fakeStore) DeleteProvider(_ context.Context, id int64) error {
	delete(f.providers, id)
	return nil
}

func (f *fakeStore) SetProviderEnabled(_ context.Context, _ db.SetProviderEnabledParams) error {
	return nil
}

func (f *fakeStore) ListProviderStreams(_ context.Context, arg db.ListProviderStreamsParams) ([]db.ProviderStream, error) {
	return f.streams[arg.ProviderID], nil
}

func (f *fakeStore) SearchProviderStreams(_ context.Context, arg db.SearchProviderStreamsParams) ([]db.ProviderStream, error) {
	var out []db.ProviderStream
	for _, s := range f.streams[arg.ProviderID] {
		if strings.Contains(strings.ToLower(s.Name), strings.ToLower(strings.Trim(arg.Name, "%"))) {
			out = append(out, s)
		}
	}
	return out, nil
}

// fakeRefresher captures refresh calls.
type fakeRefresher struct {
	got    []int64
	result source.Result
	err    error
}

func (f *fakeRefresher) RefreshProvider(_ context.Context, p db.Provider) (source.Result, error) {
	f.got = append(f.got, p.ID)
	return f.result, f.err
}

func newTestServer(t *testing.T, store Store, ref Refresher) http.Handler {
	t.Helper()
	env := &Env{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store, Refresh: ref}
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { Mount(r, env) })
	return r
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (*httptest.ResponseRecorder, any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("want JSON object, got %T %v", v, v)
	}
	return m
}

func TestProviderCRUD(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, &fakeRefresher{})

	// Create.
	rec, raw := doJSON(t, h, http.MethodPost, "/api/v1/providers", map[string]any{
		"name": "Panel A", "type": "xtream", "url": "http://panel.a:8080",
		"username": "alice", "password": "secret", "max_connections": 3,
	})
	body := asMap(t, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %v", rec.Code, body)
	}
	if body["password"] != nil {
		t.Errorf("password must never be serialized, got %v", body["password"])
	}
	if body["username"] != "alice" || body["max_connections"] != float64(3) {
		t.Errorf("create body = %v", body)
	}

	// List.
	rec, rawList := doJSON(t, h, http.MethodGet, "/api/v1/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	list, ok := rawList.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}

	// Get.
	rec, raw = doJSON(t, h, http.MethodGet, "/api/v1/providers/1", nil)
	got := asMap(t, raw)
	if rec.Code != http.StatusOK || got["name"] != "Panel A" {
		t.Fatalf("get = %d %v", rec.Code, got)
	}
	if _, has := got["password"]; has {
		t.Error("password leaked in GET")
	}

	// Update: empty password keeps the old one.
	rec, _ = doJSON(t, h, http.MethodPut, "/api/v1/providers/1", map[string]any{
		"name": "Panel A2", "type": "xtream", "url": "http://panel.a:8080",
		"username": "alice", "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d", rec.Code)
	}
	if store.lastUpdateParams.Password != "secret" {
		t.Errorf("password = %q, want kept secret", store.lastUpdateParams.Password)
	}
	if store.providers[1].Name != "Panel A2" || store.providers[1].Enabled {
		t.Errorf("update not applied: %+v", store.providers[1])
	}

	// Delete.
	rec, _ = doJSON(t, h, http.MethodDelete, "/api/v1/providers/1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/providers/1", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
}

func TestProviderValidation(t *testing.T) {
	h := newTestServer(t, newFakeStore(), &fakeRefresher{})
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing name", map[string]any{"type": "m3u", "url": "http://x"}, 400},
		{"bad type", map[string]any{"name": "x", "type": "stalker", "url": "http://x"}, 400},
		{"bad url", map[string]any{"name": "x", "type": "m3u", "url": "ftp://nope"}, 400},
		{"negative conns", map[string]any{"name": "x", "type": "m3u", "url": "http://x", "max_connections": -1}, 400},
		{"ok minimal", map[string]any{"name": "x", "type": "m3u", "url": "http://x/y.m3u"}, 201},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := doJSON(t, h, http.MethodPost, "/api/v1/providers", c.body)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%v)", rec.Code, c.want, body)
			}
		})
	}
}

func TestRefreshNow(t *testing.T) {
	store := newFakeStore()
	store.providers[1] = db.Provider{ID: 1, Name: "P", Type: "m3u", Url: "http://x/"}
	ref := &fakeRefresher{result: source.Result{Seen: 5, Upserted: 5}}
	h := newTestServer(t, store, ref)

	rec, raw := doJSON(t, h, http.MethodPost, "/api/v1/providers/1/refresh", nil)
	body := asMap(t, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %v", rec.Code, body)
	}
	if len(ref.got) != 1 || ref.got[0] != 1 {
		t.Errorf("refresher got %v", ref.got)
	}
	if body["seen"] != float64(5) {
		t.Errorf("body = %v", body)
	}

	// Refresh failure surfaces as 502 with a message.
	ref.err = errors.New("upstream down")
	rec, raw = doJSON(t, h, http.MethodPost, "/api/v1/providers/1/refresh", nil)
	body = asMap(t, raw)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh err = %d %v", rec.Code, body)
	}
	if !strings.Contains(body["error"].(string), "upstream down") {
		t.Errorf("error = %v", body)
	}
}

func TestListStreams(t *testing.T) {
	store := newFakeStore()
	store.providers[1] = db.Provider{ID: 1, Name: "P", Type: "m3u"}
	store.streams[1] = []db.ProviderStream{
		{ID: 10, ExternalID: "u1", Name: "ABC HD", GroupTitle: "US"},
		{ID: 11, ExternalID: "u2", Name: "NBC", GroupTitle: "US"},
	}
	h := newTestServer(t, store, &fakeRefresher{})

	rec, body := doJSON(t, h, http.MethodGet, "/api/v1/providers/1/streams", nil)
	all, ok := body.([]any)
	if rec.Code != http.StatusOK || !ok || len(all) != 2 {
		t.Fatalf("streams = %d %v", rec.Code, body)
	}

	rec, body = doJSON(t, h, http.MethodGet, "/api/v1/providers/1/streams?q=abc", nil)
	matched, ok := body.([]any)
	if rec.Code != http.StatusOK || !ok || len(matched) != 1 {
		t.Errorf("search = %d %v", rec.Code, body)
	}
}
