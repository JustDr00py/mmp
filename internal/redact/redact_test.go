package redact

import (
	"errors"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Xtream query credentials.
		{
			name: "xtream get.php",
			in:   "http://panel.example.tv:8080/get.php?username=alice&password=s3cret&type=m3u_plus&output=ts",
			want: "http://panel.example.tv:8080/get.php?username=" + Mask + "&password=" + Mask + "&type=m3u_plus&output=ts",
		},
		{
			name: "xtream player_api mixed case",
			in:   "http://panel.example.tv/player_api.php?USERNAME=alice&Password=s3cret&action=get_live_streams",
			want: "http://panel.example.tv/player_api.php?USERNAME=" + Mask + "&Password=" + Mask + "&action=get_live_streams",
		},
		// MMP stream-token paths (relative, as the request logger sees them).
		{
			name: "token playlist",
			in:   "/playlist/AbC-123_xyz.m3u",
			want: "/playlist/" + Mask + ".m3u",
		},
		{
			name: "token epg",
			in:   "/epg/AbC-123_xyz.xml",
			want: "/epg/" + Mask + ".xml",
		},
		{
			name: "token stream",
			in:   "/t/AbC-123_xyz/42.ts",
			want: "/t/" + Mask + "/42.ts",
		},
		{
			name: "hls playlist key",
			in:   "/hls/kx9-L3ase_Key/index.m3u8",
			want: "/hls/" + Mask + "/index.m3u8",
		},
		{
			name: "hls segment key",
			in:   "/hls/kx9-L3ase_Key/17.ts",
			want: "/hls/" + Mask + "/17.ts",
		},
		{
			name: "non-token path untouched",
			in:   "/api/v1/channels/42/stream",
			want: "/api/v1/channels/42/stream",
		},
		// Xtream path credentials.
		{
			name: "xtream live path",
			in:   "http://panel.example.tv:25461/live/alice/s3cret/42.ts",
			want: "http://panel.example.tv:25461/live/" + Mask + "/" + Mask + "/42.ts",
		},
		{
			name: "xtream movie path",
			in:   "http://panel.example.tv/movie/alice/s3cret/99.mkv",
			want: "http://panel.example.tv/movie/" + Mask + "/" + Mask + "/99.mkv",
		},
		// M3U basic-auth style.
		{
			name: "userinfo host",
			in:   "http://alice:s3cret@provider.tv/stream/channel1.m3u8",
			want: "http://" + userMask + ":" + userMask + "@provider.tv/stream/channel1.m3u8",
		},
		{
			name: "userinfo user only",
			in:   "http://alice@provider.tv/stream.m3u8",
			want: "http://" + userMask + "@provider.tv/stream.m3u8",
		},
		// Postgres DSN.
		{
			name: "postgres dsn",
			in:   "postgres://mmp:hunter2@db.internal:5432/mmp?sslmode=disable",
			want: "postgres://" + userMask + ":" + userMask + "@db.internal:5432/mmp?sslmode=disable",
		},
		// No credentials: untouched.
		{
			name: "clean url stays",
			in:   "http://provider.tv/stream/42.ts",
			want: "http://provider.tv/stream/42.ts",
		},
		{
			name: "query without secrets stays",
			in:   "http://provider.tv/epg?day=2",
			want: "http://provider.tv/epg?day=2",
		},
		// Odd inputs.
		{name: "empty", in: "", want: ""},
		{
			name: "request uri (relative)",
			in:   "/get.php?username=alice&password=s3cret&type=m3u_plus",
			want: "/get.php?username=" + Mask + "&password=" + Mask + "&type=m3u_plus",
		},
		{
			name: "relative xtream path",
			in:   "/live/alice/s3cret/42.ts?token=abc",
			want: "/live/" + Mask + "/" + Mask + "/42.ts?token=" + Mask,
		},
		{
			name: "garbage with embedded userinfo still scrubbed",
			in:   "not a url http://alice:s3cret@host/x trailing",
			want: "not a url http://" + userMask + "@host/x trailing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URL(tt.in); got != tt.want {
				t.Errorf("URL(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// Anything that looks like a secret must not survive redaction, regardless
// of URL shape (fuzz-ish property test over known shapes).
func TestNoSecretsSurvive(t *testing.T) {
	secret := "s3cret"
	inputs := []string{
		"http://h/get.php?username=a&password=" + secret,
		"http://h/live/a/" + secret + "/1.ts",
		"http://a:" + secret + "@h/",
		"/playlist/" + secret + ".m3u",
		"/epg/" + secret + ".xml",
		"/t/" + secret + "/7.ts",
		"/live/a/" + secret + "/1.ts",
		"/hls/" + secret + "/index.m3u8",
		"http://a@" + secret + ".example.com/", // host contains it but is not a credential — acceptable
	}
	for i, in := range inputs {
		out := URL(in)
		if i < len(inputs)-1 && strings.Contains(out, secret) {
			t.Errorf("input %d: secret survived: %q", i, out)
		}
	}
}

func TestErr(t *testing.T) {
	_, err := http.Get("http://127.0.0.1:1/get.php?username=u&password=hunter2")
	if err == nil {
		t.Skip("port 1 answered")
	}
	got := Err(err)
	if strings.Contains(got.Error(), "hunter2") || !strings.HasPrefix(got.Error(), "Get: ") {
		t.Errorf("Err = %v", got)
	}
	if !errors.Is(got, syscall.ECONNREFUSED) {
		t.Errorf("cause lost: %v", got)
	}
	plain := errors.New("x")
	if Err(plain) != plain {
		t.Error("non-URL error changed")
	}
}

func TestURLUnparseable(t *testing.T) {
	for _, in := range []string{
		"http://someuser:s3cretPass@bad host/get.php?username=someuser&password=s3cretPass&type=m3u",
		"http://bad host/live/someuser/s3cretPass/1.ts",
		"http://bad host/playlist/s3cretPass.m3u",
		"%zz?token=s3cretPass",
	} {
		if got := URL(in); strings.Contains(got, "s3cretPass") {
			t.Errorf("URL(%q) = %q", in, got)
		}
	}
}

func TestText(t *testing.T) {
	// pgx parse errors quote the DSN verbatim.
	in := `cannot parse "postgres://mmp:hunter2@db.internal:5432/mmp?sslmode=disable": failed to parse as URL (parse "postgres://mmp:hunter2@db.internal:5432/mmp?sslmode=disable": first path segment in URL cannot contain colon)`
	out := Text(in)
	if strings.Contains(out, "hunter2") || strings.Contains(out, "mmp:hunter2") {
		t.Errorf("DSN password survived: %q", out)
	}
	if !strings.Contains(out, "cannot parse") || !strings.Contains(out, "failed to parse as URL") {
		t.Errorf("cause text lost: %q", out)
	}
	// Credential-bearing URLs inside other error prose.
	if out := Text(`Get "http://u:p@host/x?token=t0ps3cret": dial refused`); strings.Contains(out, "p@") || strings.Contains(out, "t0ps3cret") {
		t.Errorf("error prose leaked: %q", out)
	}
	if got, want := Text("plain error text"), "plain error text"; got != want {
		t.Errorf("plain text changed: %q", got)
	}
}
