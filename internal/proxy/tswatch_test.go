package proxy

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

// tsPkt builds a payload-only TS packet for pid with continuity counter cc.
func tsPkt(pid uint16, cc uint8) []byte {
	b := make([]byte, TSPacketSize)
	b[0] = tsSync
	b[1] = byte(pid>>8) & 0x1F
	b[2] = byte(pid)
	b[3] = 0x10 | cc&0x0F // afc=01 payload only
	for i := 4; i < len(b); i++ {
		b[i] = byte(i)
		if b[i] == tsSync {
			b[i] = 0
		}
	}
	return b
}

// tsStream returns n consecutive packets on pid with correct counters.
func tsStream(pid uint16, n int) []byte {
	var out []byte
	for i := range n {
		out = append(out, tsPkt(pid, uint8(i))...)
	}
	return out
}

// garbage is n bytes containing no sync byte.
func garbage(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
		if b[i] == tsSync {
			b[i] = 0x48
		}
	}
	return b
}

func defaultTh() Thresholds {
	return Thresholds{StallTimeout: 5 * time.Second, LowBitrateKbps: 20, MaxCCErrorRate: 0.05, Window: 5 * time.Second}
}

// feedChunks feeds data in fixed-size chunks and returns the output.
func feedChunks(w *Watcher, data []byte, chunk int) []byte {
	var out []byte
	for i := 0; i < len(data); i += chunk {
		out = w.Feed(t0, out, data[i:min(i+chunk, len(data))])
	}
	return out
}

func TestWatcherCleanStreamAnyChunking(t *testing.T) {
	clean := tsStream(0x100, 50)
	for _, chunk := range []int{1, 7, 187, 188, 189, 1000, len(clean)} {
		w := NewWatcher(defaultTh(), t0)
		out := feedChunks(w, clean, chunk)
		// The last packet is held until the next sync byte confirms it.
		if !bytes.Equal(out, clean[:49*TSPacketSize]) {
			t.Errorf("chunk %d: output differs (%d vs %d bytes)", chunk, len(out), 49*TSPacketSize)
		}
		out = w.Feed(t0, out, tsPkt(0x100, 50)[:1])
		if !bytes.Equal(out, clean) {
			t.Errorf("chunk %d: held packet not released (%d bytes)", chunk, len(out))
		}
		if s := w.Stats(); s.Packets != 50 || s.DroppedBytes != 0 || s.SyncLosses != 0 || s.CCErrors != 0 {
			t.Errorf("chunk %d: stats %+v", chunk, s)
		}
	}
}

func TestWatcherResync(t *testing.T) {
	// Leading junk (with a lone false sync byte), 10 packets, a 50-byte
	// splice of junk, 10 packets, a truncated packet, then 5 packets.
	falseSync := garbage(100)
	falseSync[40] = tsSync // no partner 188 bytes later: must not lock
	var in []byte
	in = append(in, falseSync...)
	in = append(in, tsStream(0x100, 10)...)
	in = append(in, garbage(50)...)
	in = append(in, tsStream(0x101, 10)...)
	in = append(in, tsPkt(0x102, 0)[:90]...)
	in = append(in, tsStream(0x103, 5)...)
	in = append(in, tsSync) // releases the held-back final packet

	for _, chunk := range []int{1, 13, 188, 4096} {
		w := NewWatcher(defaultTh(), t0)
		out := feedChunks(w, in, chunk)
		if len(out)%TSPacketSize != 0 {
			t.Fatalf("chunk %d: unaligned output %d", chunk, len(out))
		}
		got := map[uint16]int{}
		for i := 0; i < len(out); i += TSPacketSize {
			if out[i] != tsSync {
				t.Fatalf("chunk %d: output packet %d not synced", chunk, i/TSPacketSize)
			}
			got[PID(out[i:])]++
		}
		for pid, n := range map[uint16]int{0x100: 10, 0x101: 10, 0x103: 5} {
			if got[pid] != n {
				t.Errorf("chunk %d: pid %#x got %d packets, want %d (all %v)", chunk, pid, got[pid], n, got)
			}
		}
		if got[0x102] != 0 {
			t.Errorf("chunk %d: truncated packet leaked", chunk)
		}
		s := w.Stats()
		if s.SyncLosses != 2 {
			t.Errorf("chunk %d: sync losses = %d, want 2", chunk, s.SyncLosses)
		}
		if s.DroppedBytes != 100+50+90 {
			t.Errorf("chunk %d: dropped = %d, want %d", chunk, s.DroppedBytes, 100+50+90)
		}
	}
}

func TestWatcherGarbageOnlyBoundedAndStalls(t *testing.T) {
	w := NewWatcher(defaultTh(), t0)
	for i := range 1000 {
		g := garbage(4096)
		g[i%4096] = tsSync // sprinkle lone false syncs
		if out := w.Feed(t0.Add(time.Duration(i)*10*time.Millisecond), nil, g); len(out) != 0 {
			t.Fatalf("garbage produced %d bytes", len(out))
		}
	}
	if len(w.pending) > syncLock*TSPacketSize {
		t.Errorf("pending grew to %d bytes", len(w.pending))
	}
	// Bytes flowed for 10s but no packets: that is a stall.
	if v := w.Check(t0.Add(10 * time.Second)); v != Stalled {
		t.Errorf("verdict = %v, want stalled", v)
	}
}

func TestWatcherContinuity(t *testing.T) {
	withAF := func(pid uint16, cc uint8, afc byte, disc bool) []byte {
		b := tsPkt(pid, cc)
		b[3] = afc<<4 | cc&0x0F
		b[4] = 1 // adaptation_field_length
		b[5] = 0
		if disc {
			b[5] = 0x80
		}
		return b
	}
	cat := func(ps ...[]byte) []byte { return bytes.Join(ps, nil) }

	tests := []struct {
		name    string
		in      []byte
		wantCC  uint64
		wantTEI uint64
	}{
		{"clean wrap past 15", tsStream(0x100, 40), 0, 0},
		{"lost packet", cat(tsPkt(0x100, 0), tsPkt(0x100, 1), tsPkt(0x100, 3), tsPkt(0x100, 4)), 1, 0},
		{"duplicate allowed", cat(tsPkt(0x100, 0), tsPkt(0x100, 1), tsPkt(0x100, 1), tsPkt(0x100, 2)), 0, 0},
		{"independent pids", cat(tsPkt(0x100, 0), tsPkt(0x200, 9), tsPkt(0x100, 1), tsPkt(0x200, 10)), 0, 0},
		{"adaptation-only does not advance", cat(tsPkt(0x100, 4), withAF(0x100, 4, 0x2, false), tsPkt(0x100, 5)), 0, 0},
		{"discontinuity flag resets", cat(tsPkt(0x100, 4), withAF(0x100, 11, 0x3, true), tsPkt(0x100, 12)), 0, 0},
		{"jump without flag", cat(tsPkt(0x100, 4), withAF(0x100, 11, 0x3, false), tsPkt(0x100, 12)), 1, 0},
		{"null pid ignored", cat(tsPkt(nullPID, 0), tsPkt(nullPID, 9), tsPkt(nullPID, 2)), 0, 0},
		{"tei counted", func() []byte {
			b := tsStream(0x100, 3)
			b[TSPacketSize+1] |= 0x80
			return b
		}(), 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWatcher(defaultTh(), t0)
			w.Feed(t0, nil, tt.in)
			s := w.Stats()
			if s.CCErrors != tt.wantCC || s.TEIErrors != tt.wantTEI {
				t.Errorf("cc=%d tei=%d, want cc=%d tei=%d", s.CCErrors, s.TEIErrors, tt.wantCC, tt.wantTEI)
			}
		})
	}
}

// feedRate feeds pktsPerSec packets each second for secs seconds, skipping
// a counter value every dropEvery packets (0 = never) to inject CC errors.
// It returns the time just after the last second.
func feedRate(w *Watcher, start time.Time, secs, pktsPerSec, dropEvery int) time.Time {
	cc := uint8(0)
	n := 0
	for s := range secs {
		at := start.Add(time.Duration(s) * time.Second)
		for range pktsPerSec {
			n++
			if dropEvery > 0 && n%dropEvery == 0 {
				cc++
			}
			w.Feed(at, nil, tsPkt(0x100, cc))
			cc++
		}
	}
	return start.Add(time.Duration(secs) * time.Second)
}

func TestWatcherBitrateAndVerdicts(t *testing.T) {
	th := defaultTh()

	// 1000 pkt/s = 1504 kbps: healthy.
	w := NewWatcher(th, t0)
	end := feedRate(w, t0, 6, 1000, 0)
	if kbps := w.BitrateKbps(end.Add(-time.Millisecond)); kbps < 1400 || kbps > 1600 {
		t.Errorf("bitrate = %.0f kbps, want ~1504", kbps)
	}
	if v := w.Check(end.Add(-time.Millisecond)); v != Healthy {
		t.Errorf("healthy stream verdict = %v", v)
	}

	// 10 pkt/s = 15 kbps < 20: low bitrate, but only after the window.
	w = NewWatcher(th, t0)
	feedRate(w, t0, 3, 10, 0)
	if v := w.Check(t0.Add(2*time.Second + 500*time.Millisecond)); v != Healthy {
		t.Errorf("within startup grace verdict = %v, want healthy", v)
	}
	end = feedRate(w, t0.Add(3*time.Second), 3, 10, 0)
	if v := w.Check(end.Add(-time.Millisecond)); v != LowBitrate {
		t.Errorf("slow stream verdict = %v, want low_bitrate", v)
	}

	// 10% CC loss > 5%: burst.
	w = NewWatcher(th, t0)
	end = feedRate(w, t0, 6, 500, 10)
	if v := w.Check(end.Add(-time.Millisecond)); v != CCBurst {
		t.Errorf("lossy stream verdict = %v (stats %+v), want cc_burst", v, w.Stats())
	}
	// Errors age out of the window once the stream is clean again. (The
	// clean run restarts cc at 0: that single jump stays under 5%.)
	end = feedRate(w, end, 6, 500, 0)
	if v := w.Check(end.Add(-time.Millisecond)); v != Healthy {
		t.Errorf("recovered stream verdict = %v, want healthy", v)
	}

	// Data stops: stall after StallTimeout, not before.
	if v := w.Check(end.Add(3 * time.Second)); v == Stalled {
		t.Errorf("stalled too early")
	}
	if v := w.Check(end.Add(5 * time.Second)); v != Stalled {
		t.Errorf("verdict after silence = %v, want stalled", v)
	}

	// Disabled checks never fire.
	w = NewWatcher(Thresholds{StallTimeout: 5 * time.Second, Window: 5 * time.Second}, t0)
	end = feedRate(w, t0, 6, 1, 2)
	if v := w.Check(end.Add(-time.Millisecond)); v != Healthy {
		t.Errorf("disabled checks verdict = %v", v)
	}
}

// TestWatcherConcurrentFeedCheck: the session feeds from its read loop
// while a ticker checks health.
func TestWatcherConcurrentFeedCheck(t *testing.T) {
	w := NewWatcher(defaultTh(), t0)
	data := tsStream(0x100, 2000)
	var wg sync.WaitGroup
	wg.Go(func() {
		var out []byte
		for i := 0; i < len(data); i += 1000 {
			out = w.Feed(t0, out[:0], data[i:min(i+1000, len(data))])
		}
	})
	wg.Go(func() {
		for i := range 2000 {
			_ = w.Check(t0.Add(time.Duration(i) * time.Millisecond))
			_ = w.BitrateKbps(t0)
			_ = w.Stats()
		}
	})
	wg.Wait()
	if s := w.Stats(); s.Packets != 1999 || s.CCErrors != 0 { // last packet held back
		t.Errorf("stats = %+v", s)
	}
}
