package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hlsTestPkts = 20                     // packets per segment
	hlsTestDur  = 100 * time.Millisecond // segment duration
	hlsTestPID  = 0x100
)

// fakeHLS is a live HLS origin: a sliding window of 5 segments that grows
// by one every hlsTestDur. Each segment is a PAT plus hlsTestPkts-1 packets
// on hlsTestPID carrying the segment number; the first of those has an
// adaptation field. Continuity counters run on across segments.
type fakeHLS struct {
	t0       time.Time
	start    int64 // media sequence of the first segment ever
	mu       sync.Mutex
	discAt   int64 // segment preceded by EXT-X-DISCONTINUITY (CC restarts there)
	missing  int64 // segment answering 404
	failFrom int64 // playlist answers 500 once this segment is newest (0 = never)
	ended    int64 // if > 0, a finished playlist of this many segments
	hits     map[string]int
	srv      *httptest.Server
}

func newFakeHLS(t *testing.T, tune func(*fakeHLS)) *fakeHLS {
	t.Helper()
	f := &fakeHLS{t0: time.Now(), start: 1000, discAt: -1, missing: -1, hits: map[string]int{}}
	if tune != nil {
		tune(f)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		f.hit(r)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100000\nlow/live.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS=\"avc1,mp4a\"\nhi/live.m3u8\n")
	})
	mux.HandleFunc("/redirect.m3u8", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hi/live.m3u8", http.StatusFound)
	})
	for _, dir := range []string{"/", "/hi/", "/low/", "/live/user/secretpw/"} {
		mux.HandleFunc(dir+"live.m3u8", f.playlist)
		mux.HandleFunc(dir+"seg/", f.segment)
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHLS) hit(r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	f.mu.Unlock()
}

func (f *fakeHLS) hitCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.hits {
		if strings.HasPrefix(p, prefix) {
			n += c
		}
	}
	return n
}

func (f *fakeHLS) newest() int64 {
	return f.start + int64(time.Since(f.t0)/hlsTestDur) + 4
}

func (f *fakeHLS) playlist(w http.ResponseWriter, r *http.Request) {
	f.hit(r)
	first, last := f.newest()-4, f.newest()
	if f.ended > 0 {
		first, last = f.start, f.start+f.ended-1
	}
	if f.failFrom > 0 && last >= f.failFrom {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for k := first; k <= last; k++ {
		if k == f.discAt {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg/%d.ts\n", hlsTestDur.Seconds(), k)
	}
	if f.ended > 0 {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	io.WriteString(w, b.String())
}

func (f *fakeHLS) segment(w http.ResponseWriter, r *http.Request) {
	f.hit(r)
	k, err := strconv.ParseInt(strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".ts"), 10, 64)
	if err != nil || k == f.missing {
		http.NotFound(w, r)
		return
	}
	w.Write(fakeSegment(k, f.discAt))
}

// fakeSegment builds segment k. Counters restart at discAt, as an encoder
// restart would.
func fakeSegment(k, discAt int64) []byte {
	base := k
	if discAt >= 0 && k >= discAt {
		base = k - discAt + 7 // a different, restarted counter phase
	}
	var out []byte
	pat := make([]byte, TSPacketSize)
	pat[0], pat[1], pat[2], pat[3] = 0x47, 0x40, 0, 0x10|byte(base)&0x0F
	out = append(out, pat...)
	for j := range hlsTestPkts - 1 {
		p := make([]byte, TSPacketSize)
		cc := byte(int(base)*(hlsTestPkts-1)+j) & 0x0F
		p[0], p[1], p[2] = 0x47, byte(hlsTestPID>>8), byte(hlsTestPID&0xFF)
		payload := p[4:]
		if j == 0 {
			p[3] = 0x30 | cc // adaptation field + payload
			p[4], p[5] = 1, 0
			payload = p[6:]
		} else {
			p[3] = 0x10 | cc
		}
		binary.BigEndian.PutUint64(payload, uint64(k))
		out = append(out, p...)
	}
	return out
}

// hlsPacket is one decoded test packet.
type hlsPacket struct {
	seg  int64
	cc   byte
	disc bool
}

// readHLS reads until n hlsTestPID packets arrive (or the stream ends),
// checking alignment.
func readHLS(t *testing.T, r io.Reader, n int) ([]hlsPacket, error) {
	t.Helper()
	var out []hlsPacket
	p := make([]byte, TSPacketSize)
	for len(out) < n {
		if _, err := io.ReadFull(r, p); err != nil {
			return out, err
		}
		if p[0] != 0x47 {
			t.Fatalf("unaligned output after %d packets", len(out))
		}
		if PID(p) != hlsTestPID {
			continue
		}
		hp := hlsPacket{cc: p[3] & 0x0F}
		payload := p[4:]
		if p[3]&0x20 != 0 {
			hp.disc = p[5]&0x80 != 0
			payload = p[6:]
		}
		hp.seg = int64(binary.BigEndian.Uint64(payload))
		out = append(out, hp)
	}
	return out, nil
}

// checkCC fails on a counter jump on hlsTestPID that is not flagged with
// discontinuity_indicator.
func checkCC(t *testing.T, pkts []hlsPacket) {
	t.Helper()
	for i := 1; i < len(pkts); i++ {
		if !pkts[i].disc && pkts[i].cc != (pkts[i-1].cc+1)&0x0F {
			t.Fatalf("unflagged CC jump %d->%d entering segment %d", pkts[i-1].cc, pkts[i].cc, pkts[i].seg)
		}
	}
}

func openHLS(t *testing.T, rawURL string) io.ReadCloser {
	t.Helper()
	rc, err := NewHLSOpener(time.Second).Open(context.Background(), Candidate{URL: rawURL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

func TestHLSFollowsLivePlaylist(t *testing.T) {
	f := newFakeHLS(t, nil)
	newestAtOpen := f.newest()
	rc := openHLS(t, f.srv.URL+"/live.m3u8")
	start := time.Now()
	pkts, err := readHLS(t, rc, 8*(hlsTestPkts-1))
	if err != nil {
		t.Fatal(err)
	}
	if first := pkts[0].seg; first < newestAtOpen-hlsLiveEdge || first > newestAtOpen {
		t.Errorf("joined at segment %d, newest was %d", first, newestAtOpen)
	}
	for i := 1; i < len(pkts); i++ {
		if d := pkts[i].seg - pkts[i-1].seg; d != 0 && d != 1 {
			t.Fatalf("segments out of order: %d then %d", pkts[i-1].seg, pkts[i].seg)
		}
	}
	checkCC(t, pkts)
	// 8 segments of 100ms, the first sent at once: paced, not a burst.
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Errorf("8 segments arrived in %v: not paced", el)
	}
}

func TestHLSDiscontinuityAndMissingSegment(t *testing.T) {
	f := newFakeHLS(t, func(f *fakeHLS) {
		f.discAt = f.start + 9 // EXT-X-DISCONTINUITY with a counter restart
		f.missing = f.start + 12
	})
	rc := openHLS(t, f.srv.URL+"/live.m3u8")
	pkts, err := readHLS(t, rc, 12*(hlsTestPkts-1))
	if err != nil {
		t.Fatal(err)
	}
	checkCC(t, pkts) // both boundaries carry discontinuity_indicator
	var flagged []int64
	for _, p := range pkts {
		if p.disc {
			flagged = append(flagged, p.seg)
		}
		if p.seg == f.missing {
			t.Fatal("missing segment delivered")
		}
	}
	if fmt.Sprint(flagged) != fmt.Sprint([]int64{f.discAt, f.missing + 1}) {
		t.Errorf("flagged segments = %v, want [%d %d]", flagged, f.discAt, f.missing+1)
	}
}

func TestHLSMasterAndRedirect(t *testing.T) {
	f := newFakeHLS(t, nil)
	for _, path := range []string{"/master.m3u8", "/redirect.m3u8"} {
		rc := openHLS(t, f.srv.URL+path)
		if _, err := readHLS(t, rc, hlsTestPkts); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		rc.Close()
	}
	if f.hitCount("/low/") != 0 || f.hitCount("/hi/seg/") == 0 {
		t.Errorf("hits = %v: want the high-bandwidth variant, segments resolved against it", f.hits)
	}
}

func TestHLSEndlistEndsStream(t *testing.T) {
	f := newFakeHLS(t, func(f *fakeHLS) { f.ended = 3 })
	rc := openHLS(t, f.srv.URL+"/live.m3u8")
	pkts, err := readHLS(t, rc, 1000)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	if len(pkts) != 3*(hlsTestPkts-1) || pkts[0].seg != f.start || pkts[len(pkts)-1].seg != f.start+2 {
		t.Errorf("got %d packets, segments %d..%d", len(pkts), pkts[0].seg, pkts[len(pkts)-1].seg)
	}
}

func TestHLSPlaylistFailureEndsStream(t *testing.T) {
	f := newFakeHLS(t, nil)
	f.failFrom = f.newest() + 3
	rc := openHLS(t, f.srv.URL+"/live.m3u8")
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, rc); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v, want the playlist failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the playlist kept failing")
	}
}

func TestHLSOpenErrors(t *testing.T) {
	f := newFakeHLS(t, nil)
	enc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:1,\na.ts\n")
	}))
	defer enc.Close()

	_, err := NewHLSOpener(time.Second).Open(context.Background(), Candidate{URL: f.srv.URL + "/live/user/secretpw/nope.m3u8"})
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusNotFound {
		t.Errorf("err = %v, want StatusError 404", err)
	}
	if err != nil && strings.Contains(err.Error(), "secretpw") {
		t.Errorf("error leaks credentials: %v", err)
	}
	if _, err := NewHLSOpener(time.Second).Open(context.Background(), Candidate{URL: enc.URL + "/x.m3u8"}); !errors.Is(err, ErrHLSUnsupported) {
		t.Errorf("encrypted: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHLSOpener(time.Second).Open(ctx, Candidate{URL: f.srv.URL + "/live.m3u8"}); err == nil {
		t.Error("cancelled ctx: opened")
	}
}

func TestHLSCloseStopsFetching(t *testing.T) {
	f := newFakeHLS(t, nil)
	rc := openHLS(t, f.srv.URL+"/live.m3u8")
	if _, err := readHLS(t, rc, hlsTestPkts); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { rc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung")
	}
	// A request cancelled by Close may still land at the server; let it,
	// then nothing new may follow (Close waited for the poller to exit).
	time.Sleep(50 * time.Millisecond)
	before := f.hitCount("/")
	time.Sleep(4 * hlsTestDur)
	if after := f.hitCount("/"); after != before {
		t.Errorf("requests after Close: %d -> %d", before, after)
	}
	if _, err := rc.Read(make([]byte, 10)); err == nil {
		t.Error("read after Close succeeded")
	}
}

// TestHLSThroughManager: an HLS candidate behind the protocol opener feeds
// a shared session whose stall watchdog (300ms) is far below the playlist
// reload interval it would see without pacing.
func TestHLSThroughManager(t *testing.T) {
	f := newFakeHLS(t, nil)
	var opens atomic.Int32
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 1, ProviderID: 1, URL: f.srv.URL + "/live.m3u8"}},
	}, func(cfg *ManagerConfig) {
		cfg.Opener = openerFunc(func(ctx context.Context, c Candidate) (io.ReadCloser, error) {
			opens.Add(1)
			return NewProtocolOpener(time.Second).Open(ctx, c)
		})
	})
	v, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := v.Read(context.Background(), buf)
			if _, werr := pw.Write(buf[:n]); werr != nil || err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	defer pr.Close()
	// ~1.5s of stream, several reload cycles, on one upstream open.
	pkts, err := readHLS(t, pr, 15*(hlsTestPkts-1))
	if err != nil {
		t.Fatal(err)
	}
	if pkts[len(pkts)-1].seg-pkts[0].seg < 10 {
		t.Errorf("segments %d..%d", pkts[0].seg, pkts[len(pkts)-1].seg)
	}
	if n := opens.Load(); n != 1 {
		t.Errorf("upstream opened %d times: the watchdog tripped on HLS bursts", n)
	}
}

type openerFunc func(ctx context.Context, c Candidate) (io.ReadCloser, error)

func (f openerFunc) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) { return f(ctx, c) }
