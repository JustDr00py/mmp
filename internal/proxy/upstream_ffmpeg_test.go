package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseFFmpegArgs(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "[]",
		"-c copy":                           "[-c copy]",
		"  -c:v libx264\t-preset veryfast ": "[-c:v libx264 -preset veryfast]",
		`-vf "scale=1280:-2, fps=25" -metadata title='a "b"'`: `[-vf scale=1280:-2, fps=25 -metadata title=a "b"]`,
		`-metadata a\ b=c -metadata ''`: `[-metadata a b=c -metadata ]`,
		// Full transcode chains stay within the allowlist.
		"-c:v libx264 -preset veryfast -b:v 3M -maxrate 3M -bufsize 6M -c:a aac -b:a 128k -vf scale=-2:480 -g 50 -threads 2": "[-c:v libx264 -preset veryfast -b:v 3M -maxrate 3M -bufsize 6M -c:a aac -b:a 128k -vf scale=-2:480 -g 50 -threads 2]",
		"-map 0:v:0 -map 0:a:0 -c copy":                      "[-map 0:v:0 -map 0:a:0 -c copy]",
		"-t 30 -mpegts_flags +initial_discontinuity -c copy": "[-t 30 -mpegts_flags +initial_discontinuity -c copy]",
		"-an -vn -sn -dn -c copy":                            "[-an -vn -sn -dn -c copy]",
	} {
		got, err := ParseFFmpegArgs(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%q = %q, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{
		// Output files and URLs: the attack this validation exists for.
		`-c copy /tmp/out.ts`, `out.ts -c copy`, `-c copy pipe:1`, `pipe:1`,
		`-c copy http://evil/x`, `-c copy udp://239.0.0.1:5000`, `-c copy /dev/stderr`,
		// Options MMP owns.
		`-i http://x/y.ts -c copy`, `-f mpegts -c copy`, `-c copy -f flv`,
		// Unknown or dangerous options.
		`-report`, `-c copy -y /tmp/out.ts`, `-filter_complex lavfi -c copy`,
		`-progress /tmp/p.txt -c copy`, `-c copy -report`, `-hide_banner`,
		// Malformed.
		`-vf "open`, `-x 'open`, `trailing\`, `-c copy -vf`, `-c`,
	} {
		if _, err := ParseFFmpegArgs(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

var (
	fixtureOnce sync.Once
	fixtureTS   []byte
	fixtureErr  error
)

// tsFixture is 2s of h264+aac MPEG-TS made by ffmpeg; tests that need it
// skip when ffmpeg is not installed.
func tsFixture(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	fixtureOnce.Do(func() {
		fixtureTS, fixtureErr = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440",
			"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-c:a", "aac",
			"-f", "mpegts", "pipe:1").Output()
	})
	if fixtureErr != nil || len(fixtureTS) == 0 {
		t.Fatalf("make fixture: %v", fixtureErr)
	}
	return fixtureTS
}

// fakeTSSource serves the fixture in real time, looping (or once), and
// counts open streams.
type fakeTSSource struct {
	data  []byte
	once  bool
	mu    sync.Mutex
	open  int
	opens int
}

func (f *fakeTSSource) Open(ctx context.Context, _ Candidate) (io.ReadCloser, error) {
	f.mu.Lock()
	f.open++
	f.opens++
	f.mu.Unlock()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer pw.Close()
		const chunk = 32 * TSPacketSize
		per := 2 * time.Second * chunk / time.Duration(len(f.data)) // the fixture is 2s long
		for {
			for off := 0; off < len(f.data); off += chunk {
				if _, err := pw.Write(f.data[off:min(off+chunk, len(f.data))]); err != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(per):
				}
			}
			if f.once {
				return
			}
		}
	}()
	return &closeHook{ReadCloser: pr, close: func() {
		cancel()
		f.mu.Lock()
		f.open--
		f.mu.Unlock()
	}}, nil
}

func (f *fakeTSSource) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

type closeHook struct {
	io.ReadCloser
	once  sync.Once
	close func()
}

func (c *closeHook) Close() error {
	c.once.Do(c.close)
	return c.ReadCloser.Close()
}

// checkReaped fails if pid is still running or left as a zombie.
func checkReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			state := "?"
			if i := bytes.LastIndexByte(stat, ')'); i > 0 && i+2 < len(stat) {
				state = string(stat[i+2])
			}
			t.Fatalf("ffmpeg pid %d still present (state %s)", pid, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ffmpegPID(t *testing.T, rc io.ReadCloser) int {
	t.Helper()
	s, ok := rc.(*ffmpegStream)
	if !ok {
		t.Fatalf("stream is %T", rc)
	}
	return s.cmd.Process.Pid
}

// readTS reads at least n bytes and checks they are aligned TS.
func readTS(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 0; i < len(buf); i += TSPacketSize {
		if buf[i] != 0x47 {
			t.Fatalf("unaligned TS at %d", i)
		}
	}
	return buf
}

func TestFFmpegCopyProfilePlays(t *testing.T) {
	src := &fakeTSSource{data: tsFixture(t)}
	o := &FFmpegOpener{Upstream: src}
	rc, err := o.Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"})
	if err != nil {
		t.Fatal(err)
	}
	pid := ffmpegPID(t, rc)
	out := readTS(t, rc, 400*TSPacketSize)
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", "-i", "pipe:0")
		cmd.Stdin = bytes.NewReader(out)
		got, err := cmd.Output()
		if err != nil || !strings.Contains(string(got), "h264") || !strings.Contains(string(got), "aac") {
			t.Errorf("ffprobe = %q, %v; want h264 and aac", got, err)
		}
	}
	rc.Close()
	checkReaped(t, pid)
	if n := src.openCount(); n != 0 {
		t.Errorf("%d upstreams still open after Close", n)
	}
	if _, err := rc.Read(make([]byte, 188)); err == nil {
		t.Error("read after Close succeeded")
	}
}

func TestFFmpegUpstreamEndEndsStream(t *testing.T) {
	src := &fakeTSSource{data: tsFixture(t), once: true}
	rc, err := (&FFmpegOpener{Upstream: src}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	pid := ffmpegPID(t, rc)
	n, err := io.Copy(io.Discard, rc)
	if err != nil || n < int64(len(src.data))/2 {
		t.Errorf("copied %d bytes (fixture %d), err %v; want a clean EOF", n, len(src.data), err)
	}
	checkReaped(t, pid)
}

func TestFFmpegFailureIsReported(t *testing.T) {
	src := &fakeTSSource{data: tsFixture(t)}
	rc, err := (&FFmpegOpener{Upstream: src}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c:v no_such_codec"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	pid := ffmpegPID(t, rc)
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, rc); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ffmpeg: exit status") || !strings.Contains(err.Error(), "no_such_codec") {
			t.Errorf("err = %v, want ffmpeg's exit status and stderr", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end when ffmpeg failed")
	}
	checkReaped(t, pid)
	if n := src.openCount(); n != 0 {
		t.Errorf("%d upstreams still open", n)
	}
}

func TestFFmpegOpenErrors(t *testing.T) {
	src := &fakeTSSource{data: []byte{0x47}}
	_, err := (&FFmpegOpener{Path: "/nonexistent/ffmpeg", Upstream: src}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"})
	if err == nil || !strings.Contains(err.Error(), "start ffmpeg") {
		t.Errorf("missing binary: err = %v", err)
	}
	if n := src.openCount(); n != 0 {
		t.Errorf("missing binary left %d upstreams open", n)
	}
	if _, err := (&FFmpegOpener{Upstream: src}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-i x"}); err == nil || src.opens != 1 {
		t.Errorf("bad args: err = %v, opens = %d (want no upstream opened)", err, src.opens)
	}
	upErr := &StatusError{Code: 503}
	failing := openerFunc(func(context.Context, Candidate) (io.ReadCloser, error) { return nil, upErr })
	if _, err := (&FFmpegOpener{Upstream: failing}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"}); !errors.Is(err, upErr) {
		t.Errorf("upstream failure: err = %v, want it passed through", err)
	}
}

func TestProfileOpenerRoutes(t *testing.T) {
	var direct, viaFFmpeg int
	p := &ProfileOpener{
		Direct: openerFunc(func(context.Context, Candidate) (io.ReadCloser, error) { direct++; return io.NopCloser(nil), nil }),
		FFmpeg: openerFunc(func(context.Context, Candidate) (io.ReadCloser, error) { viaFFmpeg++; return io.NopCloser(nil), nil }),
	}
	p.Open(context.Background(), Candidate{})
	p.Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"})
	if direct != 1 || viaFFmpeg != 1 {
		t.Errorf("direct %d, ffmpeg %d", direct, viaFFmpeg)
	}
}

// TestFFmpegSessionTeardown: two viewers share one ffmpeg; when both leave,
// the session ends after the idle grace and the process is reaped.
func TestFFmpegSessionTeardown(t *testing.T) {
	src := &fakeTSSource{data: tsFixture(t)}
	var mu sync.Mutex
	var pids []int
	ff := &FFmpegOpener{Upstream: src}
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 1, ProviderID: 1, FFmpeg: true, FFmpegArgs: "-c copy"}},
	}, func(cfg *ManagerConfig) {
		cfg.Thresholds.StallTimeout = 3 * time.Second // ffmpeg needs a moment to start
		cfg.Opener = &ProfileOpener{Direct: src, FFmpeg: openerFunc(func(ctx context.Context, c Candidate) (io.ReadCloser, error) {
			rc, err := ff.Open(ctx, c)
			if err == nil {
				mu.Lock()
				pids = append(pids, rc.(*ffmpegStream).cmd.Process.Pid)
				mu.Unlock()
			}
			return rc, err
		})}
	})
	var viewers []*Viewer
	for range 2 {
		v, err := m.Attach(1)
		if err != nil {
			t.Fatal(err)
		}
		viewers = append(viewers, v)
	}
	for _, v := range viewers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		got := 0
		buf := make([]byte, 64*1024)
		for got < 100*TSPacketSize {
			n, err := v.Read(ctx, buf)
			if err != nil {
				t.Fatalf("viewer read: %v", err)
			}
			got += n
		}
		cancel()
		v.Close()
	}
	mu.Lock()
	started := append([]int(nil), pids...)
	mu.Unlock()
	if len(started) != 1 {
		t.Fatalf("ffmpeg started %d times, want once for both viewers", len(started))
	}
	checkReaped(t, started[0])
	// The upstream is closed just after ffmpeg is reaped.
	for deadline := time.Now().Add(2 * time.Second); src.openCount() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d upstreams open after teardown", src.openCount())
		}
	}
}

// TestFFmpegCloseWithStalledUpstream: Close must not hang on a feeder
// blocked reading an upstream that sends nothing, nor on a reader blocked
// on ffmpeg's output.
func TestFFmpegCloseWithStalledUpstream(t *testing.T) {
	tsFixture(t) // skip without ffmpeg
	pr, pw := io.Pipe()
	defer pw.Close()
	stalled := openerFunc(func(context.Context, Candidate) (io.ReadCloser, error) { return pr, nil })
	rc, err := (&FFmpegOpener{Upstream: stalled}).Open(context.Background(), Candidate{FFmpeg: true, FFmpegArgs: "-c copy"})
	if err != nil {
		t.Fatal(err)
	}
	pid := ffmpegPID(t, rc)
	readDone := make(chan struct{})
	go func() { rc.Read(make([]byte, 188)); close(readDone) }()
	time.Sleep(100 * time.Millisecond)
	closed := make(chan struct{})
	go func() { rc.Close(); close(closed) }()
	for name, ch := range map[string]chan struct{}{"Close": closed, "blocked Read": readDone} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hung", name)
		}
	}
	checkReaped(t, pid)
}
