package proxy

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeTS encodes secs of h264+aac test video with one keyframe per second
// and B-frames (so video PTS arrives out of order, as on real channels).
func makeTS(t *testing.T, secs int) []byte {
	t.Helper()
	return makeTSAt(t, secs, 0)
}

// makeTSAt is makeTS with timestamps starting about at, as on a live
// channel whose clock has been running for hours.
func makeTSAt(t *testing.T, secs int, at time.Duration) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine",
		"-t", strconv.Itoa(secs), "-c:v", "libx264", "-preset", "ultrafast", "-x264-params", "bframes=3", "-g", "25", "-c:a", "aac",
		"-output_ts_offset", strconv.FormatFloat(at.Seconds(), 'f', 3, 64),
		"-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// feed writes data in odd-sized chunks, as a ring reader would deliver it.
func feed(s *HLSSegmenter, data []byte) {
	now := time.Now()
	for off := 0; off < len(data); off += 7000 {
		s.Write(data[off:min(off+7000, len(data))], now)
	}
}

var extinf = regexp.MustCompile(`#EXTINF:([0-9.]+),`)

func TestHLSSegmenterCutsOnKeyframes(t *testing.T) {
	data := makeTS(t, 9)
	s := NewHLSSegmenter(2*time.Second, 3)
	feed(s, data)
	// 9s of 1s GOPs at a 2s target: complete 2s segments; the tail stays
	// open until the next keyframe.
	if !s.Wait(context.Background(), 4) {
		t.Fatal("fewer than 4 segments")
	}
	pl := s.Playlist(func(seq int64) string { return strconv.FormatInt(seq, 10) + ".ts" })
	for _, want := range []string{"#EXTM3U\n", "#EXT-X-TARGETDURATION:2\n", "#EXT-X-MEDIA-SEQUENCE:1\n", "#EXT-X-DISCONTINUITY-SEQUENCE:0\n", "\n3.ts\n"} {
		if !strings.Contains(pl, want) {
			t.Errorf("playlist lacks %q:\n%s", want, pl)
		}
	}
	if strings.Contains(pl, "0.ts") || strings.Contains(pl, "#EXT-X-DISCONTINUITY\n") || strings.Contains(pl, "ENDLIST") {
		t.Errorf("window/discontinuity wrong:\n%s", pl)
	}
	for _, m := range extinf.FindAllStringSubmatch(pl, -1) {
		if d, _ := strconv.ParseFloat(m[1], 64); d < 1.9 || d > 2.1 {
			t.Errorf("segment of %vs, want 2s by PTS", d)
		}
	}
	// Every segment starts with PAT+PMT and decodes on its own.
	ffprobe, err := exec.LookPath("ffprobe")
	for seq := int64(0); seq < 4; seq++ {
		seg, ok := s.Segment(seq)
		if !ok {
			t.Fatalf("segment %d missing (kept window too small)", seq)
		}
		if PID(seg.Data[:TSPacketSize]) != 0 || len(seg.Data)%TSPacketSize != 0 {
			t.Fatalf("segment %d does not start with a PAT / is unaligned", seq)
		}
		if err == nil {
			cmd := exec.Command(ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "frame=key_frame", "-of", "csv=p=0", "-i", "pipe:0")
			cmd.Stdin = bytes.NewReader(seg.Data)
			out, perr := cmd.Output()
			if perr != nil || !strings.HasPrefix(string(out), "1") {
				t.Errorf("segment %d: first frame not a keyframe (%q, %v)", seq, firstLine(out), perr)
			}
		}
	}
	s.End()
	if !strings.HasSuffix(s.Playlist(func(int64) string { return "x" }), "#EXT-X-ENDLIST\n") {
		t.Error("ended playlist lacks ENDLIST")
	}
}

// TestHLSSegmenterLateClock: a channel whose clock is hours in (every
// real one) is cut on keyframes too. A PID's first PTS used to be compared
// with 0 and taken for a timestamp break, cutting a discontinuity at every
// switch between the audio and video PIDs.
func TestHLSSegmenterLateClock(t *testing.T) {
	s := NewHLSSegmenter(2*time.Second, 3)
	feed(s, makeTSAt(t, 9, 10*time.Hour))
	if n := len(s.segs); n < 3 || n > 5 {
		t.Fatalf("%d segments from 9s at a 2s target, want 3-5", n)
	}
	for _, seg := range s.segs {
		if seg.Discontinuity || seg.Duration < 1900*time.Millisecond || seg.Duration > 2100*time.Millisecond {
			t.Errorf("segment %d: %v, discontinuity %v; want 2s, none", seg.Seq, seg.Duration, seg.Discontinuity)
		}
	}

	// A real, unflagged break (the clock moves 10h on) is still caught once.
	feed(s, makeTSAt(t, 5, 20*time.Hour))
	var disc int
	for _, seg := range s.segs {
		if seg.Discontinuity {
			disc++
		}
	}
	if disc != 1 {
		t.Errorf("%d discontinuities after one timestamp break, want 1", disc)
	}
}

// TestHLSSegmenterSwitchTiming: the segment a switch closes is timed by its
// own PTS, not the next upstream's (whose clock may be a little ahead and
// made it look ~30s long), and the target duration only reflects listed
// segments. A long EXT-X-TARGETDURATION makes players reload the playlist
// so rarely that their lease idles out.
func TestHLSSegmenterSwitchTiming(t *testing.T) {
	for _, offset := range []time.Duration{40 * time.Second, -30 * time.Second} {
		s := NewHLSSegmenter(2*time.Second, 3)
		next := bytes.Clone(makeTSAt(t, 9, 10*time.Hour+offset))
		markDiscontinuity(next) // as the session marks a spliced upstream
		feed(s, makeTSAt(t, 9, 10*time.Hour))
		feed(s, next)
		for _, seg := range s.segs {
			if seg.Duration > 2100*time.Millisecond {
				t.Errorf("offset %v: segment %d lasts %v, want <= 2s", offset, seg.Seq, seg.Duration)
			}
		}
		if pl := s.Playlist(func(int64) string { return "x.ts" }); !strings.Contains(pl, "#EXT-X-TARGETDURATION:2\n") {
			t.Errorf("offset %v: playlist:\n%s", offset, pl)
		}
	}

	// A long segment stops counting once it leaves the window.
	s := NewHLSSegmenter(2*time.Second, 3)
	s.segs = []*OutSegment{{Seq: 0, Duration: 9 * time.Second}}
	for i := int64(1); i <= 3; i++ {
		s.segs = append(s.segs, &OutSegment{Seq: i, Duration: 2 * time.Second})
	}
	if pl := s.Playlist(func(int64) string { return "x.ts" }); !strings.Contains(pl, "#EXT-X-TARGETDURATION:2\n") {
		t.Errorf("target duration kept an unlisted segment:\n%s", pl)
	}
}

func firstLine(b []byte) string { l, _, _ := strings.Cut(string(b), "\n"); return l }

// TestHLSSegmenterDiscontinuity: an upstream switch (the splicer flags
// discontinuity_indicator) starts a segment marked EXT-X-DISCONTINUITY,
// and DISCONTINUITY-SEQUENCE counts it once it slides out of the window.
func TestHLSSegmenterDiscontinuity(t *testing.T) {
	a := makeTS(t, 5)
	b := bytes.Clone(a)
	markDiscontinuity(b) // as the session marks a spliced upstream
	s := NewHLSSegmenter(2*time.Second, 3)
	feed(s, a)
	before := len(s.segs)
	feed(s, b)
	var disc []int64
	for _, seg := range s.segs {
		if seg.Discontinuity {
			disc = append(disc, seg.Seq)
		}
	}
	// Segment `before` is the first stream's open tail, closed by the switch.
	if len(disc) != 1 || disc[0] != int64(before)+1 {
		t.Fatalf("discontinuous segments %v, want exactly [%d] (the first after the switch)", disc, before+1)
	}
	uri := func(seq int64) string { return strconv.FormatInt(seq, 10) + ".ts" }
	if pl := s.Playlist(uri); !strings.Contains(pl, "#EXT-X-DISCONTINUITY\n#EXTINF") {
		t.Errorf("playlist lacks the discontinuity:\n%s", pl)
	}
	feed(s, a[TSPacketSize*100:]) // more segments: the marked one slides out
	feed(s, a)
	pl := s.Playlist(uri)
	if strings.Contains(pl, "\n"+strconv.FormatInt(disc[0], 10)+".ts\n") {
		t.Fatalf("window did not slide past the discontinuity:\n%s", pl)
	}
	// The flagged switch plus the unflagged PTS restarts of the two feeds
	// above: each starts a discontinuity; those out of the window are counted.
	m := regexp.MustCompile(`#EXT-X-DISCONTINUITY-SEQUENCE:(\d+)`).FindStringSubmatch(pl)
	listed := strings.Count(pl, "#EXT-X-DISCONTINUITY\n")
	if m == nil {
		t.Fatalf("no DISCONTINUITY-SEQUENCE:\n%s", pl)
	}
	if n, _ := strconv.Atoi(m[1]); n < 1 || int64(n+listed) != s.discSeq {
		t.Errorf("DISCONTINUITY-SEQUENCE %d + %d listed != %d total:\n%s", n, listed, s.discSeq, pl)
	}
}

func TestHLSSegmenterWaitsForProgram(t *testing.T) {
	data := makeTS(t, 3)
	s := NewHLSSegmenter(time.Second, 3)
	// Garbage and a mid-stream start (no PAT yet) are skipped.
	s.Write(bytes.Repeat([]byte{0xAB}, 5000), time.Now())
	feed(s, data[TSPacketSize*7+50:])
	if !s.Wait(context.Background(), 1) {
		t.Fatal("no segment after joining mid-stream")
	}
	seg, _ := s.Segment(s.segs[0].Seq)
	if PID(seg.Data[:TSPacketSize]) != 0 {
		t.Error("first segment does not start with the PAT")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.Wait(ctx, 100) {
		t.Error("Wait reported 100 segments")
	}
}

func TestPTSDelta(t *testing.T) {
	for _, tc := range []struct {
		a, b int64
		want time.Duration
		ok   bool
	}{
		{0, 90000, time.Second, true},
		{pts33 - 45000, 45000, time.Second, true}, // wrap
		{-1, 5, 0, false},
		{90000, 0, 0, false}, // backwards
		{0, 61 * 90000, 0, false},
	} {
		if d, ok := ptsDelta(tc.a, tc.b); d != tc.want || ok != tc.ok {
			t.Errorf("ptsDelta(%d,%d) = %v %v", tc.a, tc.b, d, ok)
		}
	}
	for _, tc := range []struct {
		last, pts int64
		jump      bool
	}{
		{90000, 93600, false},       // next frame
		{90000, 82800, false},       // B-frame 80ms behind
		{10 * 90000, 0, true},       // restart
		{0, 2 * 3600 * 90000, true}, // far ahead
		{-1, 5, false},
	} {
		if got := ptsJump(tc.last, tc.pts); got != tc.jump {
			t.Errorf("ptsJump(%d,%d) = %v", tc.last, tc.pts, got)
		}
	}
}
