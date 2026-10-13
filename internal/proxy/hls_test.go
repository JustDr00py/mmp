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
		{41, "http://cdn.example/live/u/p/seg41.ts", 6006 * time.Millisecond, false, nil},
		{42, "http://cdn.example/abs/seg42.ts?x=1", 5500 * time.Millisecond, true, nil},
		{43, "http://other.example/seg43.ts", 6 * time.Second, false, nil},
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
		`#EXT-X-STREAM-INF:CODECS="avc1.640028,mp4a.40.2",BANDWIDTH=5000000,RESOLUTION=1920x1080` + "\nhi/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nmid/index.m3u8\n"
	m, vs, err := parseHLS([]byte(pl), base)
	if err != nil || m != nil || len(vs) != 3 {
		t.Fatalf("parse = %v %+v %v", m, vs, err)
	}
	if b := bestVariant(vs, VariantLimit{}); b.URI != "http://cdn.example/hi/index.m3u8" || b.Bandwidth != 5000000 || b.Height != 1080 {
		t.Errorf("best = %+v", b)
	}
	for _, tc := range []struct {
		lim  VariantLimit
		want string
	}{
		{VariantLimit{MaxHeight: 720}, "mid"},
		{VariantLimit{MaxBandwidth: 1_000_000}, "low"},
		{VariantLimit{MaxBandwidth: 6_000_000, MaxHeight: 1080}, "hi"},
		{VariantLimit{MaxBandwidth: 2_000_000, MaxHeight: 360}, "low"},
		{VariantLimit{MaxBandwidth: 1000}, "low"}, // nothing fits: the lowest
	} {
		if b := bestVariant(vs, tc.lim); b.URI != "http://cdn.example/"+tc.want+"/index.m3u8" {
			t.Errorf("%+v: best = %s, want %s", tc.lim, b.URI, tc.want)
		}
	}
}

func TestParseHLSKeys(t *testing.T) {
	base, _ := url.Parse("http://cdn.example/live/chan.m3u8")
	pl := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:1,\nclear.ts\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="keys/1?t=x"` + "\n#EXTINF:1,\na.ts\n#EXTINF:1,\nb.ts\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="/k2",IV=0x000102030405060708090A0B0C0D0E0F,KEYFORMAT="identity"` + "\n#EXTINF:1,\nc.ts\n" +
		"#EXT-X-KEY:METHOD=NONE\n#EXTINF:1,\nd.ts\n"
	m, _, err := parseHLS([]byte(pl), base)
	if err != nil {
		t.Fatal(err)
	}
	segs := m.Segments
	if len(segs) != 5 || segs[0].Key != nil || segs[4].Key != nil {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[1].Key != segs[2].Key || segs[1].Key.URI != "http://cdn.example/live/keys/1?t=x" || segs[1].Key.IV != nil {
		t.Errorf("key 1 = %+v / %+v", segs[1].Key, segs[2].Key)
	}
	if k := segs[3].Key; k.URI != "http://cdn.example/k2" || len(k.IV) != 16 || k.IV[1] != 1 || k.IV[15] != 15 {
		t.Errorf("key 2 = %+v", k)
	}
}

func TestParseHLSRejects(t *testing.T) {
	base, _ := url.Parse("http://x/")
	for name, pl := range map[string]string{
		"sample-aes": "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:1,\na.ts\n",
		"keyformat":  "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:1,\na.ts\n",
		"fmp4":       "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1,\na.m4s\n",
		"byterange":  "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:100@0\na.ts\n",
	} {
		if _, _, err := parseHLS([]byte(pl), base); !errors.Is(err, ErrHLSUnsupported) {
			t.Errorf("%s: err = %v, want ErrHLSUnsupported", name, err)
		}
	}
	for name, pl := range map[string]string{
		"not m3u":    "<html>",
		"bad extinf": "#EXTM3U\n#EXTINF:abc,\na.ts\n",
		"key no uri": "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128\n#EXTINF:1,\na.ts\n",
		"key bad iv": "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",IV=0x1234\n#EXTINF:1,\na.ts\n",
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
