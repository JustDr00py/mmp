package output

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source/xtream"
)

var update = flag.Bool("update", false, "rewrite golden files")

func newXtreamHandlers(t *testing.T) http.Handler {
	t.Helper()
	store, h := newGuideHandlers(t)
	store.all = append(store.all, db.ListEnabledChannelsRow{
		ID: 14, Number: i32(300), Name: "Docs & <More>", GroupID: 2, GroupName: "Docs", GroupSort: 1,
		CreatedAt: time.Unix(1700000900, 0),
	})
	return h
}

// golden compares body with testdata/xtream/<name>.json (-update rewrites).
func golden(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join("testdata", "xtream", name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(body), "", "  "); err != nil {
			t.Fatalf("%s: invalid json: %v\n%s", name, err, body)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	var w, g bytes.Buffer
	if err := json.Compact(&w, want); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&g, []byte(body)); err != nil {
		t.Fatalf("%s: invalid json: %v\n%s", name, err, body)
	}
	if w.String() != g.String() {
		t.Errorf("%s mismatch\n got: %s\nwant: %s", name, g.String(), w.String())
	}
}

func TestPlayerAPIGolden(t *testing.T) {
	h := newXtreamHandlers(t)
	const alice = "/player_api.php?username=alice&password=alice-pw1"
	const root = "/player_api.php?username=root&password=root-pw1"
	for _, tc := range []struct{ name, path string }{
		{"login", alice},
		{"login_bad", "/player_api.php?username=alice&password=nope-pw1"},
		{"live_categories_admin", root + "&action=get_live_categories"},
		{"live_categories_customer", alice + "&action=get_live_categories"},
		{"live_streams_admin", root + "&action=get_live_streams"},
		{"live_streams_category", root + "&action=get_live_streams&category_id=0"},
		{"short_epg", alice + "&action=get_short_epg&stream_id=10"},
		{"short_epg_limit", alice + "&action=get_short_epg&stream_id=10&limit=1"},
		{"simple_data_table", alice + "&action=get_simple_data_table&stream_id=10"},
		{"short_epg_forbidden", alice + "&action=get_short_epg&stream_id=12"},
		{"short_epg_no_tvg", root + "&action=get_short_epg&stream_id=11"},
		{"vod_streams", alice + "&action=get_vod_streams"},
		{"series", alice + "&action=get_series"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, hdr, body := get(t, h, tc.path)
			if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
				t.Fatalf("%s = %d %s", tc.path, code, hdr.Get("Content-Type"))
			}
			golden(t, tc.name, body)
		})
	}
}

// TestPlayerAPIPost: some players POST the credentials as a form.
func TestPlayerAPIPost(t *testing.T) {
	h := newXtreamHandlers(t)
	form := url.Values{"username": {"alice"}, "password": {"alice-pw1"}, "action": {"get_live_streams"}}
	req := httptest.NewRequest(http.MethodPost, "/player_api.php", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var streams []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &streams); err != nil || len(streams) != 1 {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
}

// TestPlayerAPIDecodesAsPanel: MMP's own Xtream ingest client (written
// against real panels) decodes MMP's answers, i.e. the types line up.
func TestPlayerAPIDecodesAsPanel(t *testing.T) {
	srv := httptest.NewServer(newXtreamHandlers(t))
	defer srv.Close()
	c, err := xtream.New(srv.URL, "root", "root-pw1", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	login, err := c.Login(ctx)
	if err != nil || login.UserInfo.Auth != 1 || login.ServerInfo.Port != "8080" || login.ServerInfo.URL != "mmp.example" {
		t.Fatalf("login = %+v, %v", login, err)
	}
	cats, err := c.LiveCategories(ctx)
	if err != nil || len(cats) != 3 || cats[0].CategoryName != "Docs" || cats[2].CategoryID != "0" {
		t.Fatalf("categories = %+v, %v", cats, err)
	}
	streams, err := c.LiveStreams(ctx)
	if err != nil || len(streams) != 5 {
		t.Fatalf("streams = %+v, %v", streams, err)
	}
	if s := streams[0]; s.StreamID != 10 || s.Num != 101 || s.EPGChannel != "news.us" || s.CategoryID != "1" {
		t.Errorf("stream[0] = %+v", s)
	}
	// Bad credentials surface as the client's auth failure.
	bad, _ := xtream.New(srv.URL, "root", "wrong-pw1", "")
	if _, err := bad.Login(ctx); err == nil {
		t.Error("bad login accepted")
	}
}
