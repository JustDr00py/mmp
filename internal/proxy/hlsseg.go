package proxy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	// segMaxBytes forces a cut so a stream without keyframe markers cannot
	// grow a segment without bound.
	segMaxBytes = 16 << 20
	pts33       = 1 << 33
)

// OutSegment is one finished HLS media segment.
type OutSegment struct {
	Seq           int64
	Duration      time.Duration
	Discontinuity bool // EXT-X-DISCONTINUITY precedes it (upstream switched)
	discSeq       int64
	Data          []byte
}

// HLSSegmenter cuts a live TS stream into HLS segments: each starts at a
// video keyframe (random_access_indicator) once Target has elapsed, opens
// with the current PAT and PMT so it decodes on its own, and is timed by
// PTS. Upstream switches (discontinuity_indicator) start a new segment
// flagged EXT-X-DISCONTINUITY. A sliding window is kept in memory.
//
// With a backlog, finished segments join the playlist no faster than real
// time: providers dump 20-40s of buffer on connect, and listing that at
// once slides the window past players, which then restart at the new edge.
// Held segments beyond backlog are dropped oldest first.
type HLSSegmenter struct {
	target  time.Duration
	window  int           // segments listed in the playlist
	keep    int           // segments kept for fetching (>= window)
	backlog time.Duration // most held back by pacing; 0: no pacing
	lead    time.Duration // how far releases may run ahead of real time
	clock   func() time.Time

	mu       sync.Mutex
	segs     []*OutSegment // released: listed and fetchable
	held     []*OutSegment // finished, waiting for their release time
	nextRel  time.Time     // when the next held segment is due
	nextSeq  int64
	discSeq  int64
	ended    bool
	changed  chan struct{} // closed and replaced whenever segs or ended change
	pending  []byte        // unaligned remainder between writes
	pat, pmt []byte        // latest single-packet PAT and PMT
	pmtPID   int
	videoPID int // -1: none (audio-only streams cut on time at PES starts)
	ptsPID   int // PID whose PTS times segments: video, else first audio
	cur      *segBuild
	// programChanged: the PAT moved the PMT, or the PMT moved the
	// elementary streams, mid-segment (a new upstream), so the next
	// segment must be marked discontinuous even if no packet carries
	// discontinuity_indicator.
	programChanged bool
	// sinceDisc: PIDs seen since the last discontinuity cut. The splicer
	// flags the first packet of every PID, so a flag is a new switch only
	// on a PID already seen since the last one.
	sinceDisc map[int]bool
	// pidPTS: furthest PTS per PID since the last discontinuity cut, to
	// catch a timestamp break on whichever PID shows it first.
	pidPTS map[int]int64
}

type segBuild struct {
	data      []byte
	startPTS  int64 // -1 unknown
	lastPTS   int64 // furthest-forward PTS (B-frames arrive out of order)
	startWall time.Time
	disc      bool
}

// NewHLSSegmenter cuts segments of about target, lists window of them and
// keeps a few more for clients still fetching older ones. A backlog > 0
// paces segments into the playlist, holding back at most that much.
func NewHLSSegmenter(target time.Duration, window int, backlog time.Duration) *HLSSegmenter {
	// Half a window ahead keeps the segment after a player's last one
	// listed while a burst is let through.
	return &HLSSegmenter{target: target, window: window, keep: window + 4,
		backlog: backlog, lead: target * time.Duration(window/2), clock: time.Now,
		pmtPID: -1, videoPID: -1, ptsPID: -1, changed: make(chan struct{}),
		sinceDisc: map[int]bool{}, pidPTS: map[int]int64{}}
}

// Write feeds stream bytes (any alignment) received at now.
func (s *HLSSegmenter) Write(p []byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := append(s.pending, p...)
	for len(buf) >= TSPacketSize {
		if buf[0] != tsSync {
			off, ok := findSync(buf)
			if !ok {
				buf = buf[max(0, len(buf)-(syncLock*TSPacketSize)):]
				break
			}
			buf = buf[off:]
			continue
		}
		s.packet(buf[:TSPacketSize], now)
		buf = buf[TSPacketSize:]
	}
	s.pending = append(s.pending[:0], buf...)
	s.release(now)
}

func (s *HLSSegmenter) packet(pkt []byte, now time.Time) {
	pid := int(PID(pkt))
	pusi := pkt[1]&0x40 != 0
	afc := pkt[3] >> 4 & 0x3
	var rai, disc bool
	payload := pkt[4:]
	if afc&0x2 != 0 { // adaptation field
		alen := int(pkt[4])
		if alen > 0 && alen <= 183 {
			rai = pkt[5]&0x40 != 0
			disc = pkt[5]&0x80 != 0
		}
		if 5+alen > TSPacketSize {
			return
		}
		payload = pkt[5+alen:]
	}
	if afc&0x1 == 0 {
		payload = nil
	}

	switch {
	case pid == 0 && pusi:
		if prog := parsePAT(payload); prog >= 0 {
			s.pat = append(s.pat[:0], pkt...)
			if prog != s.pmtPID {
				s.pmtPID, s.pmt, s.videoPID, s.ptsPID = prog, nil, -1, -1
				s.programChanged = s.cur != nil
			}
		}
	case pid == s.pmtPID && pusi:
		if video, first, ok := parsePMT(payload); ok {
			ptsPID := video
			if video < 0 {
				ptsPID = first
			}
			// Same PMT PID, new elementary PIDs: a new upstream whose
			// flagged packets are all first-seen PIDs, so nothing else
			// would mark it.
			if s.pmt != nil && s.cur != nil && (video != s.videoPID || ptsPID != s.ptsPID) {
				s.programChanged = true
			}
			s.pmt = append(s.pmt[:0], pkt...)
			s.videoPID, s.ptsPID = video, ptsPID
		}
	}
	if s.pat == nil || s.pmt == nil {
		return // cannot start a decodable segment yet
	}

	// Any PES start's PTS can reveal an unflagged timestamp break; only the
	// timing PID's PTS measures segment length.
	anyPTS := int64(-1)
	if pusi && pid != 0 && pid != s.pmtPID {
		anyPTS = parsePTS(payload)
	}
	pts := int64(-1)
	if pid == s.ptsPID {
		pts = anyPTS
	}
	// A PID's first PTS since the last break has nothing to jump from.
	jump := false
	if last, ok := s.pidPTS[pid]; ok && anyPTS >= 0 {
		jump = ptsJump(last, anyPTS)
	}
	keyStart := (s.videoPID >= 0 && pid == s.videoPID && pusi && rai) ||
		(s.videoPID < 0 && pid == s.ptsPID && pusi)

	if s.cur == nil {
		if !keyStart {
			return // the first segment starts on a keyframe
		}
		s.begin(now, pts, disc)
	} else {
		cur := s.cur
		switch {
		case (disc && s.sinceDisc[pid]) || s.programChanged || jump:
			s.cut(now, -1) // pts is on the next upstream's clock
			s.begin(now, pts, true)
		case keyStart && s.elapsed(now, pts) >= s.target,
			pusi && pid == s.ptsPID && s.elapsed(now, pts) >= 3*s.target, // no keyframe markers
			len(cur.data) >= segMaxBytes:
			s.cut(now, pts)
			s.begin(now, pts, false)
		}
	}
	if pts >= 0 {
		if s.cur.startPTS < 0 {
			s.cur.startPTS = pts
		}
		if _, fwd := ptsDelta(s.cur.lastPTS, pts); fwd || s.cur.lastPTS < 0 {
			s.cur.lastPTS = pts
		}
	}
	if anyPTS >= 0 {
		if last, ok := s.pidPTS[pid]; !ok || last < 0 {
			s.pidPTS[pid] = anyPTS
		} else if _, fwd := ptsDelta(last, anyPTS); fwd {
			s.pidPTS[pid] = anyPTS
		}
	}
	s.cur.data = append(s.cur.data, pkt...)
	s.sinceDisc[pid] = true
}

func (s *HLSSegmenter) begin(now time.Time, pts int64, disc bool) {
	b := &segBuild{startPTS: pts, lastPTS: pts, startWall: now, disc: disc}
	b.data = append(append(make([]byte, 0, 1<<20), s.pat...), s.pmt...)
	s.cur = b
	s.programChanged = false
	if disc {
		clear(s.sinceDisc)
		clear(s.pidPTS)
	}
}

// elapsed is the current segment's length so far: by PTS when known,
// else by arrival time.
func (s *HLSSegmenter) elapsed(now time.Time, pts int64) time.Duration {
	if d, ok := ptsDelta(s.cur.startPTS, pts); ok {
		return d
	}
	if d, ok := ptsDelta(s.cur.startPTS, s.cur.lastPTS); ok {
		return d
	}
	return now.Sub(s.cur.startWall)
}

// cut finishes the current segment; nextPTS starts the next one.
func (s *HLSSegmenter) cut(now time.Time, nextPTS int64) {
	b := s.cur
	s.cur = nil
	d, ok := ptsDelta(b.startPTS, nextPTS)
	if !ok { // a discontinuity: nextPTS is -1, time it by its own PTS
		d, ok = ptsDelta(b.startPTS, b.lastPTS)
	}
	if !ok || d <= 0 {
		d = now.Sub(b.startWall)
	}
	if d <= 0 {
		d = time.Millisecond
	}
	s.held = append(s.held, &OutSegment{Duration: d, Discontinuity: b.disc, Data: b.data})
	s.release(now) // only what is not due counts against the backlog
	if s.backlog <= 0 {
		return
	}
	var total time.Duration
	for _, seg := range s.held {
		total += seg.Duration
	}
	dropped := false
	for total > s.backlog && len(s.held) > 1 {
		total -= s.held[0].Duration
		s.held[0] = nil
		s.held = s.held[1:]
		dropped = true
	}
	if dropped { // players skip the gap rather than stall on it
		s.held[0].Discontinuity = true
	}
}

// release lists held segments that are due by now (all of them without
// pacing), numbering them as they join.
func (s *HLSSegmenter) release(now time.Time) {
	n := 0
	for len(s.held) > 0 {
		seg := s.held[0]
		if s.backlog > 0 && !s.ended {
			if s.nextRel.Before(now) { // caught up: real time restarts here
				s.nextRel = now
			}
			if ahead := s.nextRel.Sub(now); ahead > 0 && ahead >= s.lead {
				break
			}
			s.nextRel = s.nextRel.Add(seg.Duration)
		}
		s.held[0] = nil
		s.held = s.held[1:]
		if seg.Discontinuity {
			s.discSeq++
		}
		seg.Seq, seg.discSeq = s.nextSeq, s.discSeq
		s.nextSeq++
		s.segs = append(s.segs, seg)
		n++
	}
	if len(s.segs) > s.keep {
		s.segs = append(s.segs[:0], s.segs[len(s.segs)-s.keep:]...)
	}
	if n > 0 {
		s.notify()
	}
}

// due is how long until the next held segment may be released, or -1.
func (s *HLSSegmenter) due(now time.Time) time.Duration {
	if len(s.held) == 0 {
		return -1
	}
	return max(s.nextRel.Sub(now)-s.lead, time.Millisecond)
}

func (s *HLSSegmenter) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// End marks the stream finished: held segments are listed at once and the
// playlist gets EXT-X-ENDLIST.
func (s *HLSSegmenter) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	s.release(s.clock())
	s.notify()
}

// Wait blocks until at least n segments exist, the stream ended, or ctx
// ends; it reports whether n segments are available.
func (s *HLSSegmenter) Wait(ctx context.Context, n int) bool {
	for {
		s.mu.Lock()
		now := s.clock()
		s.release(now)
		have, ended, ch, due := len(s.segs), s.ended, s.changed, s.due(now)
		s.mu.Unlock()
		if have >= n {
			return true
		}
		if ended {
			return false
		}
		var tick <-chan time.Time
		var t *time.Timer
		if due >= 0 { // a held segment comes due without a write
			t = time.NewTimer(due)
			tick = t.C
		}
		select {
		case <-ctx.Done():
		case <-ch:
		case <-tick:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return false
		}
	}
}

// Segment returns segment seq if it is still kept.
func (s *HLSSegmenter) Segment(seq int64) (*OutSegment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seg := range s.segs {
		if seg.Seq == seq {
			return seg, true
		}
	}
	return nil, false
}

// Playlist renders the live media playlist; uri maps a sequence number to
// the segment's URI.
func (s *HLSSegmenter) Playlist(uri func(seq int64) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release(s.clock()) // held segments come due during upstream stalls too
	list := s.segs[max(0, len(s.segs)-s.window):]
	// The listed segments' longest, so one long segment ages out with it.
	longest := s.target
	for _, seg := range list {
		longest = max(longest, seg.Duration)
	}
	target := int(math.Ceil(longest.Seconds()))
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n", target)
	if len(list) > 0 {
		first := list[0]
		discSeq := first.discSeq
		if first.Discontinuity {
			discSeq--
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", first.Seq, discSeq)
	}
	for _, seg := range list {
		if seg.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", seg.Duration.Seconds(), uri(seg.Seq))
	}
	if s.ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// psiSection returns the section bytes of a PSI payload starting a section.
func psiSection(payload []byte) []byte {
	if len(payload) < 1 {
		return nil
	}
	ptr := int(payload[0])
	if 1+ptr+3 > len(payload) {
		return nil
	}
	sec := payload[1+ptr:]
	n := int(sec[1]&0x0F)<<8 | int(sec[2])
	if 3+n > len(sec) || n < 9 {
		return nil // multi-packet sections are not supported
	}
	return sec[:3+n]
}

// parsePAT returns the first program's PMT PID, or -1.
func parsePAT(payload []byte) int {
	sec := psiSection(payload)
	if sec == nil || sec[0] != 0x00 {
		return -1
	}
	for i := 8; i+4 <= len(sec)-4; i += 4 { // entries up to the CRC
		prog := int(sec[i])<<8 | int(sec[i+1])
		if prog != 0 {
			return int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
		}
	}
	return -1
}

// parsePMT returns the video PID (-1 if none) and the first elementary PID.
func parsePMT(payload []byte) (video, first int, ok bool) {
	sec := psiSection(payload)
	if sec == nil || sec[0] != 0x02 || len(sec) < 12 {
		return 0, 0, false
	}
	video, first = -1, -1
	i := 12 + (int(sec[10]&0x0F)<<8 | int(sec[11]))
	for i+5 <= len(sec)-4 {
		st := sec[i]
		pid := int(sec[i+1]&0x1F)<<8 | int(sec[i+2])
		if first < 0 {
			first = pid
		}
		switch st {
		case 0x01, 0x02, 0x10, 0x1B, 0x24, 0x42, 0xEA:
			if video < 0 {
				video = pid
			}
		}
		i += 5 + (int(sec[i+3]&0x0F)<<8 | int(sec[i+4]))
	}
	return video, first, first >= 0
}

// parsePTS reads the PTS of a PES packet start, or -1.
func parsePTS(p []byte) int64 {
	if len(p) < 14 || p[0] != 0 || p[1] != 0 || p[2] != 1 || p[7]&0x80 == 0 {
		return -1
	}
	b := p[9:14]
	return int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

// ptsJump reports a timestamp break (an unflagged upstream restart): more
// than a minute ahead of the segment's furthest PTS, or more than 2s behind
// it. Smaller backward steps are B-frame reordering.
func ptsJump(last, pts int64) bool {
	if last < 0 || pts < 0 {
		return false
	}
	if _, ok := ptsDelta(last, pts); ok {
		return false
	}
	back, _ := ptsDelta(pts, last)
	return back == 0 || back > 2*time.Second
}

// ptsDelta is b-a in 90kHz ticks as a duration, allowing one wrap; it
// fails for unknown or implausible (negative, > 1 min) deltas.
func ptsDelta(a, b int64) (time.Duration, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	d := (b - a + pts33) % pts33
	if d > 60*90000 {
		return 0, false
	}
	return time.Duration(d) * time.Second / 90000, true
}
