package proxy

import (
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestParseHLSMedia(t *testing.T) {
	base, _ := url.Parse("http://cdn.example/live/u/p/chan.m3u8?tok=1")
	pl := "\ufeff#EXTM3U\r\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:41\n" +
		"#EXT-X-KEY:METHOD=NONE\n" +
		"#EXTINF:6.006,\nseg41.ts\n" +
		"#EXT-X-DISCONTINUITY\n#EXTINF:5.5,title, with comma\n/abs/seg42.ts?x=1\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-10-13T12:00:00Z\n#EXTINF:6\nhttp://other.example/seg43.ts\n" +
		"#EXT-X-ENDLIST\n"
	m, vs, err := parseHLS([]byte(pl), base)
	if err != nil || vs != nil || m == nil {
		t.Fatalf("parse = %+v %v %v", m, vs, err)
	}
	if m.TargetDuration != 6*time.Second || !m.Ended || len(m.Segments) != 3 {
		t.Fatalf("media = %+v", m)
	}
	want := []hlsSegment{
		{41, "http://cdn.example/live/u/p/seg41.ts", 6006 * time.Millisecond, false},
		{42, "http://cdn.example/abs/seg42.ts?x=1", 5500 * time.Millisecond, true},
		{43, "http://other.example/seg43.ts", 6 * time.Second, false},
	}
	for i, w := range want {
		if m.Segments[i] != w {
			t.Errorf("seg %d = %+v, want %+v", i, m.Segments[i], w)
		}
	}
}

func TestParseHLSMaster(t *testing.T) {
	base, _ := url.Parse("http://cdn.example/master.m3u8")
	pl := "#EXTM3U\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=640x360` + "\nlow/index.m3u8\n" +
		`#EXT-X-STREAM-INF:CODECS="avc1.640028,mp4a.40.2",BANDWIDTH=5000000` + "\nhi/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000\nmid/index.m3u8\n"
	m, vs, err := parseHLS([]byte(pl), base)
	if err != nil || m != nil || len(vs) != 3 {
		t.Fatalf("parse = %v %+v %v", m, vs, err)
	}
	if b := bestVariant(vs); b.URI != "http://cdn.example/hi/index.m3u8" || b.Bandwidth != 5000000 {
		t.Errorf("best = %+v", b)
	}
}

func TestParseHLSRejects(t *testing.T) {
	base, _ := url.Parse("http://x/")
	for name, pl := range map[string]string{
		"aes":       "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:1,\na.ts\n",
		"fmp4":      "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1,\na.m4s\n",
		"byterange": "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:100@0\na.ts\n",
	} {
		if _, _, err := parseHLS([]byte(pl), base); !errors.Is(err, ErrHLSUnsupported) {
			t.Errorf("%s: err = %v, want ErrHLSUnsupported", name, err)
		}
	}
	for name, pl := range map[string]string{
		"not m3u":    "<html>",
		"bad extinf": "#EXTM3U\n#EXTINF:abc,\na.ts\n",
	} {
		if _, _, err := parseHLS([]byte(pl), base); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHLSAttrs(t *testing.T) {
	got := hlsAttrs(`BANDWIDTH=1,CODECS="a,b",URI="x=y",method=NONE`)
	if got["BANDWIDTH"] != "1" || got["CODECS"] != "a,b" || got["URI"] != "x=y" || got["METHOD"] != "NONE" {
		t.Errorf("attrs = %v", got)
	}
}

func TestIsHLSURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://p/live/u/p/1.m3u8":         true,
		"http://p/live/u/p/1.M3U8?token=a": true,
		"http://p/live/u/p/1.ts":           false,
		"http://p/get?f=x.m3u8":            false,
		"::bad":                            false,
	} {
		if IsHLSURL(raw) != want {
			t.Errorf("IsHLSURL(%q) = %v", raw, !want)
		}
	}
}
