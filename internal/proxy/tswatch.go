package proxy

import (
	"bytes"
	"sync"
	"time"
)

const (
	tsSync  = 0x47
	nullPID = 0x1FFF
	// syncLock is how many sync bytes, one packet apart, must line up
	// before the watcher trusts an alignment.
	syncLock = 3
	// minCCPackets is the fewest packets in a window before the CC error
	// rate is judged, so a single early error is not a 100% rate.
	minCCPackets = 100
)

// Verdict is the watcher's health judgement of an upstream.
type Verdict int

// Anything but Healthy should trigger failover.
const (
	Healthy    Verdict = iota
	Stalled            // no aligned packets for StallTimeout (dead, or garbage only)
	LowBitrate         // window bitrate below LowBitrateKbps
	CCBurst            // continuity error rate above MaxCCErrorRate
)

func (v Verdict) String() string {
	switch v {
	case Healthy:
		return "healthy"
	case Stalled:
		return "stalled"
	case LowBitrate:
		return "low_bitrate"
	case CCBurst:
		return "cc_burst"
	}
	return "unknown"
}

// Thresholds configure Check. Zero LowBitrateKbps or MaxCCErrorRate
// disables that check.
type Thresholds struct {
	StallTimeout   time.Duration
	LowBitrateKbps int
	MaxCCErrorRate float64
	Window         time.Duration // bitrate/CC window, whole seconds; also the startup grace for those checks
}

// Stats are cumulative counters since the watcher was created.
type Stats struct {
	Packets      uint64 `json:"packets"`       // aligned packets emitted
	DroppedBytes uint64 `json:"dropped_bytes"` // garbage discarded while (re)syncing
	SyncLosses   uint64 `json:"sync_losses"`   // times an established lock was lost
	CCErrors     uint64 `json:"cc_errors"`     // continuity counter discontinuities
	TEIErrors    uint64 `json:"tei_errors"`    // packets flagged transport_error_indicator
}

type bucket struct {
	sec      int64
	bytes    uint64
	packets  uint64
	ccErrors uint64
}

// Watcher aligns a raw upstream byte stream to TS packets and tracks its
// health. Feed and Check may be called from different goroutines.
type Watcher struct {
	th Thresholds

	mu         sync.Mutex
	pending    []byte
	synced     bool
	cc         [8192]int8 // last continuity counter per PID; -1 = unseen
	stats      Stats
	buckets    []bucket // one per second of the window, indexed by unix second
	start      time.Time
	lastPacket time.Time
}

// NewWatcher starts a watcher at now; the stall timer runs from now.
func NewWatcher(th Thresholds, now time.Time) *Watcher {
	th.Window = max(th.Window.Truncate(time.Second), time.Second)
	w := &Watcher{
		th:         th,
		buckets:    make([]bucket, int(th.Window/time.Second)),
		start:      now,
		lastPacket: now,
	}
	for i := range w.cc {
		w.cc[i] = -1
	}
	return w
}

// Feed consumes raw upstream bytes received at now and appends every
// complete, aligned packet to dst, returning it. Garbage is dropped and
// partial packets are held for the next call. The newest packet is held
// back until the following sync byte confirms it is whole.
func (w *Watcher) Feed(now time.Time, dst, p []byte) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	buf := w.pending
	for {
		if !w.synced {
			off, ok := findSync(buf)
			if !ok {
				// Keep only a tail that could still begin a lock once more
				// bytes arrive, starting at a sync byte.
				drop := max(len(buf)-((syncLock-1)*TSPacketSize), 0)
				if i := bytes.IndexByte(buf[drop:], tsSync); i >= 0 {
					drop += i
				} else {
					drop = len(buf)
				}
				w.stats.DroppedBytes += uint64(drop)
				buf = buf[drop:]
				break
			}
			w.stats.DroppedBytes += uint64(off)
			buf = buf[off:]
			w.synced = true
		}
		// Release a packet only once the next one's sync byte is visible:
		// one packet of latency, but a truncated packet is caught here
		// instead of being glued to the start of its successor.
		if len(buf) <= TSPacketSize {
			break
		}
		if buf[0] != tsSync {
			w.synced = false
			w.stats.SyncLosses++
			continue
		}
		if buf[TSPacketSize] != tsSync {
			// Either this packet is truncated (the stream resumes inside
			// it) or it is whole and junk follows. Look for the resume
			// point within the packet's span to tell them apart.
			need := TSPacketSize + (syncLock-1)*TSPacketSize + 1
			if len(buf) < need {
				break // wait for enough lookahead
			}
			w.stats.SyncLosses++
			if off, ok := findSync(buf[1:need]); ok && off+1 < TSPacketSize {
				w.stats.DroppedBytes += uint64(off + 1) // truncated: drop it
				buf = buf[off+1:]
				continue // still synced, at the resume point
			}
			w.synced = false // whole packet, then junk: keep the packet
		}
		pkt := buf[:TSPacketSize]
		w.account(now, pkt)
		dst = append(dst, pkt...)
		buf = buf[TSPacketSize:]
	}
	w.pending = append(w.pending[:0], buf...) // compact: bounded memory
	return dst
}

// findSync returns the first offset where syncLock sync bytes line up one
// packet apart.
func findSync(b []byte) (int, bool) {
	span := (syncLock-1)*TSPacketSize + 1
	for i := 0; i+span <= len(b); i++ {
		if b[i] != tsSync {
			continue
		}
		ok := true
		for k := 1; k < syncLock && ok; k++ {
			ok = b[i+k*TSPacketSize] == tsSync
		}
		if ok {
			return i, true
		}
	}
	return 0, false
}

// account updates counters for one aligned packet. Callers hold w.mu.
func (w *Watcher) account(now time.Time, pkt []byte) {
	w.stats.Packets++
	w.lastPacket = now
	b := w.bucket(now)
	b.bytes += TSPacketSize
	b.packets++

	if pkt[1]&0x80 != 0 {
		w.stats.TEIErrors++
	}
	pid := PID(pkt)
	if pid == nullPID {
		return
	}
	afc := (pkt[3] >> 4) & 0x3
	cc := int8(pkt[3] & 0x0F)
	if afc&0x2 != 0 && pkt[4] > 0 && pkt[5]&0x80 != 0 {
		// discontinuity_indicator: the counter may jump legitimately.
		w.cc[pid] = cc
		return
	}
	if afc&0x1 == 0 {
		return // no payload: the counter does not advance
	}
	last := w.cc[pid]
	w.cc[pid] = cc
	// First sight, the next value, or a single duplicate are all valid.
	if last < 0 || cc == (last+1)&0x0F || cc == last {
		return
	}
	w.stats.CCErrors++
	b.ccErrors++
}

// bucket returns the window bucket for now, recycling a stale slot.
func (w *Watcher) bucket(now time.Time) *bucket {
	sec := now.Unix()
	b := &w.buckets[int(sec%int64(len(w.buckets)))]
	if b.sec != sec {
		*b = bucket{sec: sec}
	}
	return b
}

// window sums the buckets inside the window ending at now. Callers hold w.mu.
func (w *Watcher) window(now time.Time) (bytes, packets, ccErrors uint64) {
	nowSec := now.Unix()
	n := int64(len(w.buckets))
	for _, b := range w.buckets {
		if b.sec > nowSec-n && b.sec <= nowSec {
			bytes += b.bytes
			packets += b.packets
			ccErrors += b.ccErrors
		}
	}
	return bytes, packets, ccErrors
}

// Stats returns a snapshot of the cumulative counters.
func (w *Watcher) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// BitrateKbps is the average bitrate over the window ending at now (or
// since start, if younger than the window).
func (w *Watcher) BitrateKbps(now time.Time) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bitrate(now)
}

func (w *Watcher) bitrate(now time.Time) float64 {
	bytes, _, _ := w.window(now)
	span := min(w.th.Window, now.Sub(w.start))
	if span <= 0 {
		return 0
	}
	return float64(bytes*8) / 1000 / span.Seconds()
}

// Check judges upstream health at now. The stall check applies from the
// start; bitrate and CC checks wait until a full window has elapsed.
func (w *Watcher) Check(now time.Time) Verdict {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.th.StallTimeout > 0 && now.Sub(w.lastPacket) >= w.th.StallTimeout {
		return Stalled
	}
	if now.Sub(w.start) < w.th.Window {
		return Healthy
	}
	_, packets, ccErrors := w.window(now)
	if w.th.MaxCCErrorRate > 0 && packets >= minCCPackets &&
		float64(ccErrors)/float64(packets) > w.th.MaxCCErrorRate {
		return CCBurst
	}
	if w.th.LowBitrateKbps > 0 && w.bitrate(now) < float64(w.th.LowBitrateKbps) {
		return LowBitrate
	}
	return Healthy
}

// PID returns a TS packet's 13-bit packet identifier.
func PID(pkt []byte) uint16 {
	return uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
}
