package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// pkt builds a TS-sized test packet carrying seq; every payload byte is
// derived from seq so torn copies are detectable.
func pkt(seq uint64) []byte {
	b := make([]byte, TSPacketSize)
	b[0] = 0x47
	binary.BigEndian.PutUint64(b[1:9], seq)
	for i := 9; i < len(b); i++ {
		b[i] = byte(seq) ^ byte(i)
	}
	return b
}

// checkPkt validates a packet and returns its seq.
func checkPkt(b []byte) (uint64, error) {
	if len(b) != TSPacketSize || b[0] != 0x47 {
		return 0, fmt.Errorf("bad packet header/len %d", len(b))
	}
	seq := binary.BigEndian.Uint64(b[1:9])
	for i := 9; i < len(b); i++ {
		if b[i] != byte(seq)^byte(i) {
			return seq, fmt.Errorf("torn packet seq=%d at byte %d", seq, i)
		}
	}
	return seq, nil
}

// readPackets reads whole packets until want are read or the reader ends.
func readPackets(ctx context.Context, rd *Reader, want int) ([]uint64, error) {
	var seqs []uint64
	buf := make([]byte, 7*TSPacketSize) // odd multiple: exercises wrap splits
	var pending []byte
	for len(seqs) < want {
		n, err := rd.Read(ctx, buf)
		pending = append(pending, buf[:n]...)
		for len(pending) >= TSPacketSize {
			seq, perr := checkPkt(pending[:TSPacketSize])
			if perr != nil {
				return seqs, perr
			}
			seqs = append(seqs, seq)
			pending = pending[TSPacketSize:]
		}
		if err != nil {
			return seqs, err
		}
	}
	return seqs, nil
}

// readThrough reads packets until it has read the one numbered last.
func readThrough(ctx context.Context, rd *Reader, last uint64) ([]uint64, error) {
	var seqs []uint64
	for len(seqs) == 0 || seqs[len(seqs)-1] < last {
		got, err := readPackets(ctx, rd, 1)
		seqs = append(seqs, got...)
		if err != nil {
			return seqs, err
		}
	}
	return seqs, nil
}

func TestRingBasicOrderAndWrap(t *testing.T) {
	r := NewRing(10 * TSPacketSize)
	rd := r.NewReader(0)
	ctx := context.Background()
	for round := uint64(0); round < 5; round++ { // 5 laps of an 8-packet batch
		for i := range uint64(8) {
			if _, err := r.Write(pkt(round*8 + i)); err != nil {
				t.Fatal(err)
			}
		}
		seqs, err := readPackets(ctx, rd, 8)
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range seqs {
			if s != round*8+uint64(i) {
				t.Fatalf("round %d: got seq %d at %d", round, s, i)
			}
		}
	}
	if rd.Skipped() != 0 {
		t.Errorf("skipped = %d, want 0", rd.Skipped())
	}
}

func TestRingSizeRoundsToPackets(t *testing.T) {
	if got := NewRing(1000).Size(); got != 5*TSPacketSize {
		t.Errorf("size = %d", got)
	}
	if got := NewRing(1).Size(); got != TSPacketSize {
		t.Errorf("min size = %d", got)
	}
}

func TestRingLiveEdgeJoinAndBacklog(t *testing.T) {
	r := NewRing(4 * TSPacketSize)
	for i := range uint64(10) {
		r.Write(pkt(i))
	}
	edge := r.NewReader(0)
	if edge.Pos() != r.Head() {
		t.Fatalf("edge reader pos %d != head %d", edge.Pos(), r.Head())
	}
	// Backlog of 2.5 packets rounds down to 2 packets.
	back := r.NewReader(2*TSPacketSize + 90)
	// Backlog beyond capacity clamps to the ring size (4 packets: seq 6..9).
	huge := r.NewReader(1 << 20)

	r.Write(pkt(10)) // overwrites seq 6: `huge` is now lapped
	ctx := context.Background()
	if seqs, err := readPackets(ctx, edge, 1); err != nil || seqs[0] != 10 {
		t.Errorf("edge reader got %v %v, want [10]", seqs, err)
	}
	if seqs, err := readPackets(ctx, back, 3); err != nil || seqs[0] != 8 || seqs[2] != 10 {
		t.Errorf("backlog reader got %v %v, want [8 9 10]", seqs, err)
	}
	r.Write(pkt(11))
	seqs, err := readPackets(ctx, huge, 1)
	if err != nil {
		t.Fatal(err)
	}
	if huge.Skipped() == 0 || seqs[0] != 11 {
		t.Errorf("lapped reader got %v skipped=%d, want rejoin 1 packet back at [11]", seqs, huge.Skipped())
	}
}

// TestRingSlowReaderSkipsAhead: a stalled reader never blocks the writer,
// and when it resumes it skips to a quarter-ring behind the live edge and
// reads valid, contiguous data from there.
func TestRingSlowReaderSkipsAhead(t *testing.T) {
	r := NewRing(16 * TSPacketSize)
	slow := r.NewReader(0)
	fast := r.NewReader(0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const total = 2000
	var fastSeqs []uint64
	var fastErr error
	var wg sync.WaitGroup
	// Read through the last packet rather than a count: a fast reader the
	// scheduler stalls can be lapped and skip some, and must not then wait
	// for packets that never come.
	wg.Go(func() { fastSeqs, fastErr = readThrough(ctx, fast, total-1) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range uint64(total) {
			r.Write(pkt(i))
			if i%8 == 0 {
				time.Sleep(50 * time.Microsecond) // pace so the fast reader keeps up
			}
		}
	}()
	select {
	case <-done: // writer finished while `slow` never read: not blocked
	case <-ctx.Done():
		t.Fatal("writer blocked by a stalled reader")
	}
	wg.Wait()
	if fastErr != nil {
		t.Fatalf("fast reader: %v", fastErr)
	}
	if fast.Skipped() == 0 { // a keeping-up reader sees every packet in order
		for i, s := range fastSeqs {
			if s != uint64(i) {
				t.Fatalf("fast reader gap at %d: seq %d", i, s)
			}
		}
	} else {
		t.Logf("fast reader skipped %d bytes (scheduler hiccup)", fast.Skipped())
	}

	r.Write(pkt(total))
	r.Close()
	seqs, err := readPackets(ctx, slow, 1<<30)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("slow reader err = %v", err)
	}
	if slow.Skipped() == 0 || slow.Skipped()%TSPacketSize != 0 {
		t.Errorf("slow skipped = %d, want >0 and packet-aligned", slow.Skipped())
	}
	// 16-packet ring: rejoin 4 packets behind the edge -> seqs 1997..2000.
	if len(seqs) != 4 || seqs[0] != total-3 || seqs[3] != total {
		t.Errorf("slow reader after skip got %v, want [%d..%d]", seqs, total-3, total)
	}
}

// TestRingConcurrentReaders: one writer, readers of mixed speed. Every
// reader sees only valid packets in increasing order, gaps only where it
// recorded a skip, and drains to the final packet after Close.
func TestRingConcurrentReaders(t *testing.T) {
	r := NewRing(64 * TSPacketSize)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const total = 20000
	delays := []time.Duration{0, 0, 0, time.Microsecond, 20 * time.Microsecond, 200 * time.Microsecond, time.Millisecond, 0}
	type result struct {
		seqs    []uint64
		skipped uint64
		err     error
	}
	results := make([]result, len(delays))
	var wg sync.WaitGroup
	for i, d := range delays {
		rd := r.NewReader(0)
		wg.Go(func() {
			buf := make([]byte, 3*TSPacketSize)
			var pending []byte
			res := &results[i]
			for {
				n, err := rd.Read(ctx, buf)
				pending = append(pending, buf[:n]...)
				for len(pending) >= TSPacketSize {
					seq, perr := checkPkt(pending[:TSPacketSize])
					if perr != nil {
						res.err = perr
						return
					}
					res.seqs = append(res.seqs, seq)
					pending = pending[TSPacketSize:]
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						res.err = err
					}
					res.skipped = rd.Skipped()
					return
				}
				if d > 0 {
					time.Sleep(d)
				}
			}
		})
	}
	for i := range uint64(total) {
		r.Write(pkt(i))
		if i%256 == 0 {
			time.Sleep(100 * time.Microsecond)
		}
	}
	r.Close()
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Errorf("reader %d: %v", i, res.err)
			continue
		}
		if len(res.seqs) == 0 {
			t.Errorf("reader %d got nothing", i)
			continue
		}
		gaps := 0
		for j := 1; j < len(res.seqs); j++ {
			switch {
			case res.seqs[j] <= res.seqs[j-1]:
				t.Errorf("reader %d: seq went backwards %d -> %d", i, res.seqs[j-1], res.seqs[j])
			case res.seqs[j] != res.seqs[j-1]+1:
				gaps++
			}
		}
		if gaps > 0 && res.skipped == 0 {
			t.Errorf("reader %d: %d gaps but no skip recorded", i, gaps)
		}
		if last := res.seqs[len(res.seqs)-1]; last != total-1 {
			t.Errorf("reader %d did not drain to the end: last %d", i, last)
		}
	}
}

func TestRingReadCancelAndClose(t *testing.T) {
	r := NewRing(4 * TSPacketSize)
	rd := r.NewReader(0)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := rd.Read(ctx, make([]byte, TSPacketSize))
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return on cancel")
	}

	// A blocked reader wakes on Close with io.EOF.
	go func() {
		_, err := rd.Read(context.Background(), make([]byte, TSPacketSize))
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond)
	r.Close()
	r.Close() // idempotent
	select {
	case err := <-errc:
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return on Close")
	}
	if _, err := r.Write(pkt(0)); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("write after close = %v", err)
	}
}

func TestRingOversizedWriteKeepsTail(t *testing.T) {
	var big []byte
	for i := range uint64(10) {
		big = append(big, pkt(i)...)
	}

	// A reader at 0 is lapped by 10 packets in a 4-packet ring: it rejoins
	// one packet (a quarter ring) behind the edge.
	r := NewRing(4 * TSPacketSize)
	rd := r.NewReader(0)
	r.Write(big)
	r.Close()
	seqs, err := readPackets(context.Background(), rd, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(seqs) != 1 || seqs[0] != 9 || rd.Skipped() != 9*TSPacketSize {
		t.Errorf("seqs=%v skipped=%d", seqs, rd.Skipped())
	}

	// A backlog reader sees the last 4 packets intact.
	r2 := NewRing(4 * TSPacketSize)
	r2.Write(big)
	r2.Close()
	seqs, _ = readPackets(context.Background(), r2.NewReader(1<<20), 100)
	if len(seqs) != 4 || seqs[0] != 6 || seqs[3] != 9 {
		t.Errorf("tail = %v, want [6 7 8 9]", seqs)
	}
}
