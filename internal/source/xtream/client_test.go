package xtream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakePanel is a minimal Xtream panel used as an httptest backend.
type fakePanel struct {
	ts       *httptest.Server
	gotUA    string
	requests int
}

func newFakePanel(t *testing.T) *fakePanel {
	t.Helper()
	fp := &fakePanel{}
	mux := http.NewServeMux()
	mux.HandleFunc("/player_api.php", func(w http.ResponseWriter, r *http.Request) {
		fp.requests++
		fp.gotUA = r.Header.Get("User-Agent")
		q := r.URL.Query()
		if q.Get("username") != "alice" || q.Get("password") != "pw123" {
			// Real panels answer 401 with a JSON body.
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"user_info": map[string]any{"auth": 0}})
			return
		}
		switch q.Get("action") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_info": map[string]any{
					"username": "alice", "auth": 1, "status": "Active",
					"exp_date": "1798761600", "is_trial": "0",
					"max_connections": "2", "active_cons": "0",
				},
				"server_info": map[string]any{
					"url": "panel.example.tv", "port": "8080", "https_port": "8443",
					"server_protocol": "http", "timezone": "Europe/Berlin",
				},
			})
		case "get_live_categories":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"category_name": "US Entertainment", "category_id": "1", "parent_id": 0},
				{"category_name": "News", "category_id": "2", "parent_id": 0},
			})
		case "get_live_streams":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"num": 1, "name": "ABC US", "stream_type": "live", "stream_id": 42,
					"stream_icon": "http://l/abc.png", "epg_channel_id": "USABC", "added": "1610000000",
					"category_id": "1", "custom_sid": "", "tv_format": "hd"},
				{"num": 2, "name": "News 24", "stream_type": "live", "stream_id": 43,
					"stream_icon": "", "epg_channel_id": "", "added": "1610000001",
					"category_id": "2", "custom_sid": "", "tv_format": "sd"},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	fp.ts = httptest.NewServer(mux)
	t.Cleanup(fp.ts.Close)
	return fp
}

func TestLogin(t *testing.T) {
	fp := newFakePanel(t)
	c, err := New(fp.ts.URL, "alice", "pw123", "MMP/1.0")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := c.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if auth.UserInfo.Username != "alice" || auth.UserInfo.Auth != 1 {
		t.Errorf("auth = %+v", auth.UserInfo)
	}
	if auth.ServerInfo.URL != "panel.example.tv" || auth.ServerInfo.Port != "8080" {
		t.Errorf("server_info = %+v", auth.ServerInfo)
	}
	if fp.gotUA != "MMP/1.0" {
		t.Errorf("user agent = %q", fp.gotUA)
	}
}

// Some panels block Go's default User-Agent, so an unset one must not
// fall through to it.
func TestDefaultUserAgent(t *testing.T) {
	fp := newFakePanel(t)
	c, err := New(fp.ts.URL, "alice", "pw123", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if fp.gotUA != DefaultUserAgent {
		t.Errorf("user agent = %q, want %q", fp.gotUA, DefaultUserAgent)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	fp := newFakePanel(t)
	c, _ := New(fp.ts.URL, "alice", "wrong", "")
	if _, err := c.Login(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestLoginAuthZero(t *testing.T) {
	// Some panels return 200 with auth:0 instead of 401.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"user_info": map[string]any{"auth": 0}})
	}))
	defer ts.Close()
	c, _ := New(ts.URL, "alice", "pw123", "")
	if _, err := c.Login(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestLiveCategoriesAndStreams(t *testing.T) {
	fp := newFakePanel(t)
	c, _ := New(fp.ts.URL, "alice", "pw123", "")
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	cats, err := c.LiveCategories(context.Background())
	if err != nil {
		t.Fatalf("LiveCategories: %v", err)
	}
	if len(cats) != 2 || cats[0].CategoryName != "US Entertainment" || cats[0].CategoryID != "1" {
		t.Errorf("cats = %+v", cats)
	}
	streams, err := c.LiveStreams(context.Background())
	if err != nil {
		t.Fatalf("LiveStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %+v", streams)
	}
	want := Stream{Num: 1, Name: "ABC US", StreamType: "live", StreamID: 42,
		StreamIcon: "http://l/abc.png", EPGChannel: "USABC", Added: "1610000000",
		CategoryID: "1", TVFormat: "hd"}
	if streams[0] != want {
		t.Errorf("stream 0 = %+v, want %+v", streams[0], want)
	}
}

func TestURLs(t *testing.T) {
	c, err := New("http://panel.example.tv:8080/", "user name", "p@ss/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.apiURL("get_live_streams"),
		"http://panel.example.tv:8080/player_api.php?action=get_live_streams&password=p%40ss%2Fw&username=user+name"; got != want {
		t.Errorf("apiURL = %q, want %q", got, want)
	}
	// Path semantics: '/' must be escaped within a segment, '@' is legal.
	if got, want := c.StreamURL(42), "http://panel.example.tv:8080/live/user%20name/p@ss%2Fw/42.ts"; got != want {
		t.Errorf("StreamURL = %q, want %q", got, want)
	}
	if got, want := c.XMLTVURL(), "http://panel.example.tv:8080/xmltv.php?username=user+name&password=p%40ss%2Fw"; got != want {
		t.Errorf("XMLTVURL = %q, want %q", got, want)
	}
}

func TestNewRejectsBadBase(t *testing.T) {
	if _, err := New("not a url", "u", "p", ""); err == nil {
		t.Error("want error for invalid base URL")
	}
}

func TestRequestContextCancel(t *testing.T) {
	fp := newFakePanel(t)
	c, _ := New(fp.ts.URL, "alice", "pw123", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := c.LiveStreams(ctx); err == nil {
		t.Error("want error when context deadline exceeded")
	}
}

// TestRequestErrorsHideCredentials: a transport failure (here: nothing
// listening) must not surface the player_api URL, which carries the
// password.
func TestRequestErrorsHideCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused from now on
	c, err := New(base, "someuser", "s3cretPass", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Login(context.Background())
	if err == nil {
		t.Fatal("login against a closed port succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, "s3cretPass") || strings.Contains(msg, "someuser") || strings.Contains(msg, "player_api") {
		t.Errorf("error leaks the request URL: %v", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error lost its cause: %v", err)
	}
}
