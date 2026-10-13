// Package proxy is the streaming core: one shared upstream per channel,
// fanned out to many viewers through a ring buffer, with failover.
package proxy

import (
	"context"
	"io"
	"sync"
)

// TSPacketSize is the MPEG-TS packet size. Ring positions stay aligned to it
// as long as the writer only writes whole packets.
const TSPacketSize = 188

// Ring is a single-writer, many-reader byte ring. The writer never blocks on
// readers: a reader that falls more than the ring's size behind loses the
// overwritten bytes and skips ahead to just behind the live edge.
//
// Positions are absolute byte offsets since the ring was created, so a
// reader's cursor is valid across wrap-arounds.
type Ring struct {
	mu     sync.RWMutex
	buf    []byte
	head   uint64        // absolute position of the next byte to be written
	closed bool          // no more writes; readers drain then get io.EOF
	notify chan struct{} // closed and replaced on every Write and on Close
}

// NewRing allocates a ring of size bytes, rounded down to whole TS packets
// (minimum one packet).
func NewRing(size int) *Ring {
	size -= size % TSPacketSize
	if size < TSPacketSize {
		size = TSPacketSize
	}
	return &Ring{buf: make([]byte, size), notify: make(chan struct{})}
}

// Size is the ring capacity in bytes.
func (r *Ring) Size() int { return len(r.buf) }

// Head is the absolute position of the live edge.
func (r *Ring) Head() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.head
}

// Write appends p and wakes waiting readers. It never blocks on readers.
// If p is larger than the ring only its tail is kept. Writing to a closed
// ring returns io.ErrClosedPipe.
func (r *Ring) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	n := len(p)
	size := uint64(len(r.buf))
	data := p
	start := r.head
	if uint64(n) > size {
		data = p[n-int(size):]
		start = r.head + uint64(n) - size
	}
	off := int(start % size)
	c := copy(r.buf[off:], data)
	copy(r.buf, data[c:])
	r.head += uint64(n)
	ch := r.notify
	r.notify = make(chan struct{})
	r.mu.Unlock()
	close(ch)
	return n, nil
}

// Close stops the ring. Readers receive the remaining buffered bytes, then
// io.EOF. Close is idempotent.
func (r *Ring) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	ch := r.notify
	r.mu.Unlock()
	close(ch)
}

// rejoinBacklog is how far behind the edge a lapped reader resumes: a
// quarter of the ring, whole packets, never before the stream start.
// Callers hold r.mu.
func (r *Ring) rejoinBacklog() uint64 {
	back := uint64(len(r.buf) / 4)
	back -= back % TSPacketSize
	return min(back, r.head)
}

// NewReader attaches a reader at the live edge, backed off by up to backlog
// bytes of already-buffered data (rounded down to whole TS packets) so a
// player can start decoding sooner. backlog 0 joins exactly at the edge.
func (r *Ring) NewReader(backlog int) *Reader {
	r.mu.RLock()
	defer r.mu.RUnlock()
	back := uint64(max(backlog, 0))
	back -= back % TSPacketSize
	back = min(back, r.head, uint64(len(r.buf)))
	return &Reader{ring: r, pos: r.head - back}
}

// Reader is one viewer's cursor into a Ring. A Reader must not be used from
// more than one goroutine at a time; different Readers are independent.
type Reader struct {
	ring    *Ring
	pos     uint64
	skipped uint64
}

// Pos is the reader's absolute position.
func (rd *Reader) Pos() uint64 { return rd.pos }

// Skipped is the total number of bytes this reader lost to overruns.
func (rd *Reader) Skipped() uint64 { return rd.skipped }

// Read copies available bytes into p, blocking until data arrives, ctx is
// done, or the ring is closed and drained (io.EOF). If the reader has been
// lapped, it first jumps to just behind the live edge and counts the lost
// bytes.
func (rd *Reader) Read(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r := rd.ring
	for {
		r.mu.RLock()
		size := uint64(len(r.buf))
		if r.head-rd.pos > size {
			// Overrun: the bytes at our cursor were overwritten. Rejoin a
			// quarter-ring behind the live edge: not the oldest byte (the
			// writer overwrites it next) and not the edge itself (the viewer
			// would get nothing until the next write).
			newPos := r.head - r.rejoinBacklog()
			rd.skipped += newPos - rd.pos
			rd.pos = newPos
		}
		if avail := r.head - rd.pos; avail > 0 {
			n := int(min(avail, uint64(len(p))))
			off := int(rd.pos % size)
			c := copy(p[:n], r.buf[off:])
			copy(p[c:n], r.buf)
			r.mu.RUnlock()
			rd.pos += uint64(n)
			return n, nil
		}
		if r.closed {
			r.mu.RUnlock()
			return 0, io.EOF
		}
		wait := r.notify
		r.mu.RUnlock()

		select {
		case <-wait:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}
