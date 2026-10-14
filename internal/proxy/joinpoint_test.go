package proxy

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func tagPkt(tag byte) []byte {
	p := make([]byte, TSPacketSize)
	p[0], p[4] = 0x47, tag
	return p
}

func TestRingNewReaderAtKeyframe(t *testing.T) {
	r := NewRing(100 * TSPacketSize)
	for i := range 10 {
		r.Write(tagPkt(byte(i)))
	}
	// No join point yet: plain backlog behaviour.
	if rd := r.NewReaderAtKeyframe(2 * TSPacketSize); rd.Pos() != 8*TSPacketSize || len(rd.prefix) != 0 {
		t.Fatalf("fallback reader at %d", rd.Pos())
	}
	prefix := append(tagPkt('P'), tagPkt('M')...)
	r.SetJoinPoint(4*TSPacketSize, prefix)
	rd := r.NewReaderAtKeyframe(TSPacketSize)
	buf := make([]byte, 20*TSPacketSize)
	n, _ := rd.Read(context.Background(), buf)
	if n != len(prefix) || !bytes.Equal(buf[:n], prefix) {
		t.Fatalf("first read = %d bytes, want the %d-byte prefix", n, len(prefix))
	}
	n, _ = rd.Read(context.Background(), buf)
	if n != 6*TSPacketSize || buf[4] != 4 {
		t.Fatalf("data starts with packet %d (%d bytes), want packet 4", buf[4], n)
	}

	// A join point more than half a ring behind the edge is too old.
	for i := range 50 {
		r.Write(tagPkt(byte(i)))
	}
	if rd := r.NewReaderAtKeyframe(TSPacketSize); len(rd.prefix) != 0 || rd.Pos() != r.Head()-TSPacketSize {
		t.Errorf("stale join point used (pos %d, head %d)", rd.Pos(), r.Head())
	}
}

func TestRingOverrunRejoinsAtKeyframe(t *testing.T) {
	r := NewRing(10 * TSPacketSize)
	rd := r.NewReader(0)
	for i := range 30 { // lap the reader
		r.Write(tagPkt(byte(i)))
	}
	r.SetJoinPoint(r.Head()-3*TSPacketSize, tagPkt('P'))
	buf := make([]byte, 10*TSPacketSize)
	n, _ := rd.Read(context.Background(), buf)
	if n != TSPacketSize || buf[4] != 'P' {
		t.Fatalf("lapped reader did not get the prefix first")
	}
	n, _ = rd.Read(context.Background(), buf)
	if buf[4] != 27 || n != 3*TSPacketSize || rd.Skipped() == 0 {
		t.Errorf("lapped reader resumed at packet %d (%d bytes)", buf[4], n)
	}
}

func TestKeyTrackerFindsKeyframes(t *testing.T) {
	data := makeTS(t, 3) // one keyframe per second, B-frames
	k := newKeyTracker()
	found := 0
	for off := 0; off < len(data); off += 100 * TSPacketSize {
		chunk := data[off:min(off+100*TSPacketSize, len(data))]
		idx, prefix := k.scan(chunk)
		if idx < 0 {
			continue
		}
		found++
		if len(prefix) != 2*TSPacketSize || PID(prefix) != 0 {
			t.Fatalf("prefix is not PAT+PMT")
		}
		if ffprobe, err := exec.LookPath("ffprobe"); err == nil && found == 2 {
			// Decoding from prefix + keyframe onwards starts on a keyframe.
			stream := append(bytes.Clone(prefix), data[off+idx*TSPacketSize:]...)
			cmd := exec.Command(ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "frame=key_frame", "-of", "csv=p=0", "-i", "pipe:0")
			cmd.Stdin = bytes.NewReader(stream)
			out, err := cmd.Output()
			if err != nil || !strings.HasPrefix(string(out), "1") {
				t.Errorf("first frame from the join point is not a keyframe: %q %v", firstLine(out), err)
			}
		}
	}
	if found < 2 {
		t.Errorf("keyframes found in %d chunks, want several", found)
	}
}

// pacedOpener serves data in real time (dur long), looping.
func pacedOpener(data []byte, dur time.Duration) Opener {
	return openerFunc(func(ctx context.Context, _ Candidate) (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			const chunk = 50 * TSPacketSize
			per := dur * chunk / time.Duration(len(data))
			for {
				for off := 0; off < len(data); off += chunk {
					if _, err := pw.Write(data[off:min(off+chunk, len(data))]); err != nil {
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(per):
					}
				}
			}
		}()
		return pr, nil
	})
}

// TestLateJoinerStartsAtKeyframe is the 8.4c AC: a viewer joining a
// running channel decodes from its first byte without errors.
func TestLateJoinerStartsAtKeyframe(t *testing.T) {
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	data := makeTS(t, 20)
	m := newTestManager(t, map[int64][]Candidate{1: {{StreamID: 1, ProviderID: 1}}}, func(cfg *ManagerConfig) {
		cfg.Opener = pacedOpener(data, 20*time.Second)
		cfg.RingSize = 8 << 20
		cfg.JoinBacklog = 50 * TSPacketSize // well under one GOP: the old join landed mid-GOP
		cfg.Thresholds.StallTimeout = 3 * time.Second
		cfg.Thresholds.LowBitrateKbps = 0
	})
	first, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { // keep the first viewer draining
		buf := make([]byte, 64*1024)
		for {
			if _, err := first.Read(ctx, buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(2500 * time.Millisecond) // mid-GOP for 1s GOPs, most of the time

	late, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	var got []byte
	buf := make([]byte, 64*1024)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(got) < 600*1024; {
		n, err := late.Read(ctx, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if PID(got) != 0 {
		t.Fatalf("late joiner does not start with a PAT (pid %#x)", PID(got))
	}
	probe := exec.Command(ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "frame=key_frame", "-of", "csv=p=0", "-i", "pipe:0")
	probe.Stdin = bytes.NewReader(got)
	out, err := probe.Output()
	if err != nil || !strings.HasPrefix(string(out), "1") {
		t.Errorf("late joiner's first video frame is not a keyframe: %q %v", firstLine(out), err)
	}
	dec := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0", "-f", "null", "-")
	dec.Stdin = bytes.NewReader(got)
	if msg, _ := dec.CombinedOutput(); strings.Contains(string(msg), "non-existing PPS") || strings.Contains(string(msg), "no frame") {
		t.Errorf("late joiner hits join-time decode errors:\n%s", msg)
	}
}
