package output

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

const testBase = "http://mmp.example:8080"

type fakeUsers map[string]db.User

func (f fakeUsers) GetUserByUsername(_ context.Context, n string) (db.User, error) {
	if u, ok := f[n]; ok {
		return u, nil
	}
	return db.User{}, pgx.ErrNoRows
}

func (f fakeUsers) GetUserByStreamToken(_ context.Context, tok string) (db.User, error) {
	for _, u := range f {
		if u.StreamToken == tok {
			return u, nil
		}
	}
	return db.User{}, pgx.ErrNoRows
}

type fakeStore struct {
	byUser   map[int64][]db.ListUserChannelsRow
	all      []db.ListEnabledChannelsRow
	guide    map[string][]db.ListGuideProgrammesRow // by lower-cased tvg-id
	nostream map[int64]bool                         // channels without streams
	fetchs   []db.ListGuideProgrammesParams
}

func (f *fakeStore) GetChannelAccess(_ context.Context, arg db.GetChannelAccessParams) (db.GetChannelAccessRow, error) {
	var allowed bool
	if arg.IsAdmin {
		for _, c := range f.all {
			allowed = allowed || c.ID == arg.ChannelID
		}
	} else {
		for _, c := range f.byUser[arg.UserID] {
			allowed = allowed || c.ID == arg.ChannelID
		}
	}
	row := db.GetChannelAccessRow{Allowed: allowed, Streams: 1}
	if f.nostream[arg.ChannelID] {
		row.Streams = 0
	}
	return row, nil
}

func (f *fakeStore) ListGuideProgrammes(_ context.Context, arg db.ListGuideProgrammesParams) ([]db.ListGuideProgrammesRow, error) {
	f.fetchs = append(f.fetchs, arg)
	var out []db.ListGuideProgrammesRow
	for _, k := range arg.TvgKeys {
		out = append(out, f.guide[k]...)
	}
	return out, nil
}

func (f *fakeStore) ListUserChannels(_ context.Context, id int64) ([]db.ListUserChannelsRow, error) {
	return f.byUser[id], nil
}

func (f *fakeStore) ListEnabledChannels(context.Context) ([]db.ListEnabledChannelsRow, error) {
	return f.all, nil
}

func i32(v int32) *int32 { return &v }

func newTestHandlers(t *testing.T) (*Handlers, http.Handler) {
	t.Helper()
	auth.BcryptCost = bcrypt.MinCost
	hash := func(pw string) string {
		h, err := auth.HashPassword(pw)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	past := time.Now().Add(-time.Hour)
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	users := fakeUsers{
		"alice": {ID: 1, Username: "alice", PasswordHash: hash("alice-pw1"), Role: auth.RoleCustomer, Enabled: true, StreamToken: "tok-alice", MaxConnections: 1,
			CreatedAt: time.Unix(1700000000, 0), ExpiresAt: &future},
		"root": {ID: 2, Username: "root", PasswordHash: hash("root-pw1"), Role: auth.RoleAdmin, Enabled: true, StreamToken: "tok-root"},
		"old":  {ID: 3, Username: "old", PasswordHash: hash("old-pw1"), Role: auth.RoleCustomer, Enabled: true, StreamToken: "tok-old", ExpiresAt: &past},
		"off":  {ID: 4, Username: "off", PasswordHash: hash("off-pw1"), Role: auth.RoleCustomer, Enabled: false, StreamToken: "tok-off"},
	}
	added := time.Unix(1700000500, 0)
	news := db.ListUserChannelsRow{ID: 10, Number: i32(101), Name: `News "24"`, TvgID: "news.us", LogoUrl: "http://logo/n.png", GroupID: 1, GroupName: "News", CreatedAt: added}
	store := &fakeStore{
		byUser: map[int64][]db.ListUserChannelsRow{1: {news}},
		all: []db.ListEnabledChannelsRow{
			{ID: 10, Number: i32(101), Name: `News "24"`, TvgID: "news.us", LogoUrl: "http://logo/n.png", GroupID: 1, GroupName: "News", GroupSort: 5, CreatedAt: added},
			{ID: 11, Name: "Evil\n#EXTINF:-1,Injected\nhttp://attacker/x.ts", GroupName: "Sports"},
		},
	}
	h := &Handlers{
		Access: NewAccess(auth.NewCustomers(users, time.Minute)), Store: store,
		BaseURL: testBase, Log: slog.New(slog.DiscardHandler),
	}
	r := chi.NewRouter()
	h.Mount(r)
	return h, r
}

func get(t *testing.T, h http.Handler, path string) (int, http.Header, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, rec.Header(), string(b)
}

// streamLines returns the non-comment playlist lines (the URLs).
func streamLines(body string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestGetPHPCustomerPlaylist(t *testing.T) {
	_, h := newTestHandlers(t)
	code, hdr, body := get(t, h, "/get.php?username=alice&password=alice-pw1&type=m3u_plus&output=ts")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "audio/x-mpegurl") {
		t.Fatalf("get.php = %d %s", code, hdr.Get("Content-Type"))
	}
	want := `#EXTM3U x-tvg-url="` + testBase + `/xmltv.php?username=alice&password=alice-pw1" url-tvg="` + testBase + `/xmltv.php?username=alice&password=alice-pw1"
#EXTINF:-1 tvg-id="news.us" tvg-name="News '24'" tvg-logo="http://logo/n.png" tvg-chno="101" group-title="News",News "24"
` + testBase + `/live/alice/alice-pw1/10.ts
`
	if body != want {
		t.Errorf("playlist mismatch\n got: %q\nwant: %q", body, want)
	}
}

func TestGetPHPPlainM3UAndAdmin(t *testing.T) {
	_, h := newTestHandlers(t)
	_, _, body := get(t, h, "/get.php?username=alice&password=alice-pw1&type=m3u")
	if !strings.HasPrefix(body, "#EXTM3U\n#EXTINF:-1,News \"24\"\n") || strings.Contains(body, "tvg-") {
		t.Errorf("plain m3u = %q", body)
	}

	// Admins see every enabled channel; the hostile name cannot inject a line.
	_, _, body = get(t, h, "/get.php?username=root&password=root-pw1")
	urls := streamLines(body)
	if len(urls) != 2 {
		t.Fatalf("admin playlist urls = %v\n%s", urls, body)
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, testBase+"/live/root/root-pw1/") {
			t.Errorf("url not on MMP / injected: %q", u)
		}
	}
	entries := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "#EXTINF") {
			entries++
		}
	}
	if entries != 2 {
		t.Errorf("injection produced %d entries, want 2:\n%s", entries, body)
	}
}

func TestTokenPlaylistHasNoPassword(t *testing.T) {
	_, h := newTestHandlers(t)
	code, _, body := get(t, h, "/playlist/tok-alice.m3u")
	if code != 200 {
		t.Fatalf("token playlist = %d", code)
	}
	if strings.Contains(body, "alice-pw1") {
		t.Error("token playlist leaks the password")
	}
	if !strings.Contains(body, `x-tvg-url="`+testBase+`/epg/tok-alice.xml"`) {
		t.Errorf("epg url missing: %s", body)
	}
	if urls := streamLines(body); len(urls) != 1 || urls[0] != testBase+"/t/tok-alice/10.ts" {
		t.Errorf("token urls = %v", urls)
	}
}

func TestPlaylistAuthFailures(t *testing.T) {
	_, h := newTestHandlers(t)
	for _, p := range []string{
		"/get.php",
		"/get.php?username=alice&password=wrong-pw1",
		"/get.php?username=ghost&password=alice-pw1",
		"/get.php?username=old&password=old-pw1", // expired
		"/get.php?username=off&password=off-pw1", // disabled
		"/playlist/nope.m3u",
		"/playlist/tok-old.m3u",
		"/playlist/tok-off.m3u",
	} {
		if code, _, body := get(t, h, p); code != http.StatusUnauthorized || strings.Contains(body, "EXTM3U") {
			t.Errorf("%s = %d", p, code)
		}
	}
}

func TestCustomerWithoutBouquetsGetsEmptyPlaylist(t *testing.T) {
	hs, h := newTestHandlers(t)
	hs.Store.(*fakeStore).byUser = map[int64][]db.ListUserChannelsRow{}
	_, _, body := get(t, h, "/get.php?username=alice&password=alice-pw1")
	if len(streamLines(body)) != 0 || !strings.HasPrefix(body, "#EXTM3U") {
		t.Errorf("no-bouquet playlist = %q", body)
	}
}
