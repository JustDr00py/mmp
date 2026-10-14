//go:build integration

package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
)

func TestStreamProfilesAPI(t *testing.T) {
	env := newAPITestEnv(t)
	h := apiServer(env)
	ctx := context.Background()
	q := env.Store.(*db.Queries)

	// The migration's default profile is listed.
	code, _, list := call(t, h, http.MethodGet, "/api/v1/stream-profiles", nil)
	if code != 200 || len(list) != 1 || list[0].(map[string]any)["name"] != "default" {
		t.Fatalf("list = %d %v", code, list)
	}

	for name, body := range map[string]map[string]any{
		"no name":        {"mode": "passthrough"},
		"bad mode":       {"name": "x", "mode": "transcode"},
		"ffmpeg no args": {"name": "x", "mode": "ffmpeg"},
		"ffmpeg -i":      {"name": "x", "mode": "ffmpeg", "ffmpeg_args": "-i http://evil/ -c copy"},
		"bad quoting":    {"name": "x", "mode": "ffmpeg", "ffmpeg_args": `-vf "scale`},
	} {
		if code, obj, _ := call(t, h, http.MethodPost, "/api/v1/stream-profiles", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, code, obj)
		}
	}

	code, p, _ := call(t, h, http.MethodPost, "/api/v1/stream-profiles",
		map[string]any{"name": "remux", "mode": "ffmpeg", "ffmpeg_args": "-c copy", "user_agent": "ProfileUA/2"})
	if code != http.StatusCreated || p["mode"] != "ffmpeg" || p["ffmpeg_args"] != "-c copy" {
		t.Fatalf("create = %d %v", code, p)
	}
	pid := int64(p["id"].(float64))
	path := "/api/v1/stream-profiles/" + strconv.FormatInt(pid, 10)
	if code, _, _ := call(t, h, http.MethodPost, "/api/v1/stream-profiles", map[string]any{"name": "remux"}); code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", code)
	}
	if code, got, _ := call(t, h, http.MethodGet, path, nil); code != 200 || got["name"] != "remux" {
		t.Errorf("get = %d %v", code, got)
	}
	if code, _, _ := call(t, h, http.MethodGet, "/api/v1/stream-profiles/999999", nil); code != http.StatusNotFound {
		t.Errorf("get missing = %d", code)
	}

	// A channel on the profile: its candidates carry the ffmpeg stage and
	// the profile's user agent over the provider's.
	prov, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, UserAgent: "ProviderUA/1"})
	sid, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: "1", Name: "News", StreamUrl: "http://p/1.ts"})
	code, c, _ := call(t, h, http.MethodPost, "/api/v1/channels", map[string]any{"name": "News", "stream_profile_id": pid})
	if code != http.StatusCreated {
		t.Fatalf("create channel = %d %v", code, c)
	}
	chID := int64(c["id"].(float64))
	if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: chID, ProviderStreamID: sid, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	cands, err := proxy.DBSource(q)(ctx, chID)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %v %v", cands, err)
	}
	if c := cands[0]; !c.FFmpeg || c.FFmpegArgs != "-c copy" || c.UserAgent != "ProfileUA/2" || c.Health != 1 {
		t.Errorf("candidate = %+v", c)
	}
	if code, obj, _ := call(t, h, http.MethodPost, "/api/v1/channels", map[string]any{"name": "X", "stream_profile_id": 999999}); code != http.StatusBadRequest {
		t.Errorf("unknown profile on channel = %d %v, want 400", code, obj)
	}

	// Update to passthrough without a UA: the provider's applies again.
	if code, got, _ := call(t, h, http.MethodPut, path, map[string]any{"name": "remux", "mode": "passthrough"}); code != 200 || got["mode"] != "passthrough" {
		t.Fatalf("update = %d %v", code, got)
	}
	cands, _ = proxy.DBSource(q)(ctx, chID)
	if c := cands[0]; c.FFmpeg || c.UserAgent != "ProviderUA/1" {
		t.Errorf("after update: %+v", c)
	}

	// Delete: the channel keeps working, without a profile.
	if code, _, _ := call(t, h, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, _, _ := call(t, h, http.MethodDelete, path, nil); code != http.StatusNotFound {
		t.Errorf("delete again = %d", code)
	}
	ch, _ := q.GetChannel(ctx, chID)
	if ch.StreamProfileID != nil {
		t.Errorf("channel still references deleted profile %d", *ch.StreamProfileID)
	}
}

// TestCopyProfilePlaysEndToEnd is the 6.2 AC: a channel on a "-c copy"
// profile plays through real ffmpeg, and ffmpeg is gone after teardown.
func TestCopyProfilePlaysEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	fixture, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-c:a", "aac", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		per := 2 * time.Second * 18800 / time.Duration(len(fixture)) // real time
		for {
			for off := 0; off < len(fixture); off += 18800 {
				if _, err := w.Write(fixture[off:min(off+18800, len(fixture))]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(per):
				}
			}
		}
	}))
	t.Cleanup(up.Close)

	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	mgr := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: proxy.DBSource(q), Opener: proxy.NewProfileOpener(proxy.NewProtocolOpener(2*time.Second, proxy.VariantLimit{}), ""),
		Capacity: proxy.NewCapacity(), Cooldowns: proxy.NewCooldowns(time.Second, 10*time.Second),
		RingSize: 4 << 20, IdleGrace: 200 * time.Millisecond, MaxRetryWindow: 5 * time.Second,
		Thresholds: proxy.Thresholds{StallTimeout: 3 * time.Second, Window: 5 * time.Second},
	})
	t.Cleanup(mgr.Shutdown)
	env.Streams = mgr
	srv := httptest.NewServer(apiServer(env))
	t.Cleanup(srv.Close)

	prof, err := q.CreateStreamProfile(ctx, db.CreateStreamProfileParams{Name: "remux", Mode: "ffmpeg", FfmpegArgs: "-c copy"})
	if err != nil {
		t.Fatal(err)
	}
	prov, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/", RefreshIntervalSecs: 3600, Enabled: true})
	sid, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: "1", Name: "News", StreamUrl: up.URL + "/live/u/secret/1.ts"})
	ch, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News", StreamProfileID: &prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: sid, Priority: 1}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/api/v1/channels/" + strconv.FormatInt(ch.ID, 10) + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 188*600)
	_, err = io.ReadFull(resp.Body, buf)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %d %v", resp.StatusCode, err)
	}
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", "-i", "pipe:0")
		cmd.Stdin = bytes.NewReader(buf)
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), "h264") || !strings.Contains(string(out), "aac") {
			t.Errorf("ffprobe = %q %v", out, err)
		}
	}

	// After the idle grace the session ends: no ffmpeg child is left,
	// running or zombie.
	deadline := time.Now().Add(5 * time.Second)
	for {
		kids := ffmpegChildren(t)
		if kids == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ffmpeg children remain after teardown:\n%s", kids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ffmpegChildren lists this process's ffmpeg children (any state).
func ffmpegChildren(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command("ps", "-o", "pid=,stat=,comm=", "--ppid", strconv.Itoa(os.Getpid())).Output()
	var kids []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "ffmpeg") {
			kids = append(kids, l)
		}
	}
	return strings.Join(kids, "\n")
}
