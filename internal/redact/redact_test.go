package redact

import (
	"strings"
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
		"http://a@" + secret + ".example.com/", // host contains it but is not a credential — acceptable
	}
	for i, in := range inputs {
		out := URL(in)
		if i < 3 && strings.Contains(out, secret) {
			t.Errorf("input %d: secret survived: %q", i, out)
		}
	}
}
