package proxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JustDr00py/mmp/internal/netguard"
	"github.com/JustDr00py/mmp/internal/redact"
)

const (
	// hlsLiveEdge is how many segments behind the newest one a stream
	// starts: enough buffer to ride out a slow segment fetch.
	hlsLiveEdge = 3
	// hlsQueue bounds fetched-but-unsent segments.
	hlsQueue = 8
	// hlsMaxErrors consecutive playlist or segment failures end the stream.
	hlsMaxErrors = 3
	// Size caps keep a hostile upstream from exhausting memory.
	hlsMaxPlaylist = 4 << 20
	hlsMaxSegment  = 64 << 20
	// hlsPaceChunk is the write unit when pacing a segment out.
	hlsPaceChunk = 64 * TSPacketSize
	// hlsKeyCache bounds remembered AES keys; rotating keys would otherwise
	// accumulate for the life of the stream.
	hlsKeyCache = 16
)

// HLSOpener turns a live HLS (.m3u8) upstream into one continuous MPEG-TS
// byte stream: it follows the playlist, fetches new segments in order and
// paces them out in real time, so the session's stall watchdog sees a
// steady stream rather than a burst every target duration.
type HLSOpener struct {
	Client *http.Client
	// ConnectTimeout bounds each playlist fetch, and is the floor for a
	// segment fetch (which also gets two target durations). 0 = 10s.
	ConnectTimeout time.Duration
	// Variants caps the variant chosen from a master playlist.
	Variants VariantLimit
}

// NewHLSOpener builds an HLS opener.
func NewHLSOpener(connectTimeout time.Duration, lim VariantLimit) *HLSOpener {
	return &HLSOpener{Client: netguard.Client(), ConnectTimeout: connectTimeout, Variants: lim}
}

// Open fetches the playlist (resolving a master playlist to its
// highest-bandwidth variant within o.Variants) before returning, so an unreachable or broken
// upstream fails here and the session moves on. Errors carry redacted URLs.
func (o *HLSOpener) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) {
	safe := redact.URL(c.URL)
	playlistURL := c.URL
	media, final, err := o.playlist(ctx, c, playlistURL)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", safe, err)
	}
	if media == nil { // master: final holds the chosen variant
		playlistURL = final
		if media, _, err = o.playlist(ctx, c, playlistURL); err != nil {
			return nil, fmt.Errorf("open %s: variant: %w", safe, err)
		}
		if media == nil {
			return nil, fmt.Errorf("open %s: variant is a master playlist", safe)
		}
	}

	sctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &hlsStream{pr: pr, cancel: cancel}
	queue := make(chan hlsSegmentData, hlsQueue)
	f := &hlsFetcher{o: o, c: c, url: playlistURL, safe: safe, queue: queue}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		defer close(queue)
		if err := f.run(sctx, media); err != nil {
			pw.CloseWithError(err)
		}
	}()
	go func() {
		defer s.wg.Done()
		pw.CloseWithError(pace(sctx, pw, queue))
	}()
	return s, nil
}

// playlist fetches and parses one playlist. For a master playlist it
// returns a nil media and the chosen variant's URL.
func (o *HLSOpener) playlist(ctx context.Context, c Candidate, rawURL string) (*hlsMedia, string, error) {
	data, base, err := o.get(ctx, c, rawURL, o.connectTimeout(), hlsMaxPlaylist)
	if err != nil {
		return nil, "", err
	}
	media, variants, err := parseHLS(data, base)
	if err != nil {
		return nil, "", err
	}
	if media == nil {
		return nil, bestVariant(variants, o.Variants).URI, nil
	}
	return media, "", nil
}

func (o *HLSOpener) connectTimeout() time.Duration {
	if o.ConnectTimeout > 0 {
		return o.ConnectTimeout
	}
	return 10 * time.Second
}

// get downloads rawURL (whole body, capped at limit) within timeout and
// returns the body and the final URL after redirects, for resolving
// relative URIs. Errors never contain the raw URL.
func (o *HLSOpener) get(ctx context.Context, c Candidate, rawURL string, timeout time.Duration, limit int64) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, errors.New("invalid url")
	}
	for k, vs := range c.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := o.Client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // url.Error embeds the raw URL; keep only the cause
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = ErrConnectTimeout
		}
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("response over %d bytes", limit)
	}
	return data, resp.Request.URL, nil
}

// hlsSegmentData is a downloaded segment on its way to the pacer.
type hlsSegmentData struct {
	data          []byte
	duration      time.Duration
	discontinuity bool // mark the first packet per PID
}

// hlsFetcher follows a media playlist and queues new segments in order.
type hlsFetcher struct {
	o     *HLSOpener
	c     Candidate
	url   string
	safe  string
	queue chan<- hlsSegmentData

	next     int64             // next media sequence to fetch; -1 before the first pass
	markNext bool              // the next queued segment follows a discontinuity
	keys     map[string][]byte // AES-128 keys by URI
}

// run returns nil when an ENDLIST playlist is exhausted or ctx ends, and an
// error once the upstream keeps failing.
func (f *hlsFetcher) run(ctx context.Context, media *hlsMedia) error {
	f.next = -1
	playlistErrs, segErrs := 0, 0
	for {
		var wait time.Duration
		if media != nil {
			playlistErrs = 0
			got, err := f.fetchNew(ctx, media, &segErrs)
			if err != nil || ctx.Err() != nil {
				return err
			}
			if media.Ended && f.next > lastSeq(media) {
				return nil
			}
			// RFC 8216 6.3.4: reload after a target duration; if nothing
			// was new, after half of one.
			wait = media.TargetDuration
			if n := len(media.Segments); n > 0 {
				wait = media.Segments[n-1].Duration
			}
			if !got {
				wait /= 2
			}
		} else {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(max(wait, 50*time.Millisecond)):
		}
		m, _, err := f.o.playlist(ctx, f.c, f.url)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			if playlistErrs++; playlistErrs >= hlsMaxErrors {
				return fmt.Errorf("hls %s: playlist: %w", f.safe, err)
			}
			media = nil
		case m == nil:
			return fmt.Errorf("hls %s: playlist turned into a master playlist", f.safe)
		default:
			media = m
		}
	}
}

// fetchNew queues the playlist's segments from f.next on, reporting whether
// any were new. A gap in media sequence (the upstream outran us, or it
// restarted) is bridged with a discontinuity.
func (f *hlsFetcher) fetchNew(ctx context.Context, media *hlsMedia, segErrs *int) (bool, error) {
	segs := media.Segments
	if len(segs) == 0 {
		return false, nil
	}
	first, last := segs[0].Seq, lastSeq(media)
	switch {
	case f.next < 0: // join near the live edge (or at the start of a finished stream)
		f.next = first
		if !media.Ended {
			f.next = segs[max(0, len(segs)-hlsLiveEdge)].Seq
		}
	case f.next < first: // segments expired before we fetched them
		f.next, f.markNext = first, true
	case f.next > last+1: // sequence went backwards: the upstream restarted
		f.next, f.markNext = segs[max(0, len(segs)-hlsLiveEdge)].Seq, true
	}
	got := false
	for _, seg := range segs {
		if seg.Seq < f.next {
			continue
		}
		timeout := max(f.o.connectTimeout(), 2*media.TargetDuration)
		data, _, err := f.o.get(ctx, f.c, seg.URI, timeout, hlsMaxSegment)
		if err == nil && seg.Key != nil {
			data, err = f.decrypt(ctx, seg, data)
		}
		if ctx.Err() != nil {
			return got, nil
		}
		f.next = seg.Seq + 1
		if err != nil { // skip it: a gap beats stalling the channel
			if *segErrs++; *segErrs >= hlsMaxErrors {
				return got, fmt.Errorf("hls %s: segment %d: %w", f.safe, seg.Seq, err)
			}
			f.markNext = true
			continue
		}
		*segErrs = 0
		select {
		case f.queue <- hlsSegmentData{data: data, duration: seg.Duration, discontinuity: seg.Discontinuity || f.markNext}:
			f.markNext, got = false, true
		case <-ctx.Done():
			return got, nil
		}
	}
	return got, nil
}

func lastSeq(m *hlsMedia) int64 { return m.Segments[len(m.Segments)-1].Seq }

// decrypt undoes AES-128 (CBC, PKCS#7) on a segment, fetching its key once.
// Errors never carry key material or URLs.
func (f *hlsFetcher) decrypt(ctx context.Context, seg hlsSegment, data []byte) ([]byte, error) {
	key, ok := f.keys[seg.Key.URI]
	if !ok {
		k, _, err := f.o.get(ctx, f.c, seg.Key.URI, f.o.connectTimeout(), aes.BlockSize)
		if err != nil {
			return nil, fmt.Errorf("key: %w", err)
		}
		if len(k) != aes.BlockSize {
			return nil, fmt.Errorf("key: %d bytes, want %d", len(k), aes.BlockSize)
		}
		if f.keys == nil || len(f.keys) >= hlsKeyCache {
			f.keys = map[string][]byte{}
		}
		f.keys[seg.Key.URI], key = k, k
	}
	iv := seg.Key.IV
	if iv == nil { // RFC 8216 5.2: the media sequence number, big-endian
		iv = make([]byte, aes.BlockSize)
		binary.BigEndian.PutUint64(iv[8:], uint64(seg.Seq))
	}
	return decryptAES128(data, key, iv)
}

func decryptAES128(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("decrypt: segment is not whole AES blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("decrypt: bad key")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, errors.New("decrypt: bad padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("decrypt: bad padding")
		}
	}
	return data[:len(data)-pad], nil
}

// pace writes queued segments to w, each spread over its duration. The
// first segment goes out at once for a fast start, and a backlog of more
// than two segments is flushed unpaced to catch up with the live edge.
func pace(ctx context.Context, w io.Writer, queue <-chan hlsSegmentData) error {
	first := true
	for seg := range queue {
		data := seg.data[:len(seg.data)/TSPacketSize*TSPacketSize] // drop a torn tail
		if seg.discontinuity {
			markDiscontinuity(data)
		}
		if first || len(queue) > 2 || seg.duration <= 0 {
			first = false
			if _, err := w.Write(data); err != nil {
				return err
			}
			continue
		}
		start := time.Now()
		for off := 0; off < len(data); off += hlsPaceChunk {
			due := start.Add(time.Duration(float64(seg.duration) * float64(off) / float64(len(data))))
			if d := time.Until(due); d > 0 {
				select {
				case <-ctx.Done():
					return io.EOF
				case <-time.After(d):
				}
			}
			if _, err := w.Write(data[off:min(off+hlsPaceChunk, len(data))]); err != nil {
				return err
			}
		}
	}
	return io.EOF
}

// markDiscontinuity sets discontinuity_indicator on the first packet of
// each PID that carries an adaptation field, so continuity-counter jumps
// across the boundary read as intended.
func markDiscontinuity(pkts []byte) {
	if len(pkts) < TSPacketSize || pkts[0] != 0x47 {
		return // not aligned: leave it to the watcher's resync
	}
	marked := map[uint16]bool{}
	for i := 0; i+TSPacketSize <= len(pkts); i += TSPacketSize {
		p := pkts[i : i+TSPacketSize]
		if pid := PID(p); !marked[pid] && p[3]&0x20 != 0 && p[4] > 0 {
			p[5] |= 0x80
			marked[pid] = true
		}
	}
}

// hlsStream is the TS byte stream; Close stops both goroutines.
type hlsStream struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func (s *hlsStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *hlsStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.pr.Close()
		s.wg.Wait()
	})
	return nil
}

// ProtocolOpener picks the opener by URL: HLS for .m3u8 playlists,
// otherwise plain HTTP-TS. A TS open that turns out to be a playlist (by
// Content-Type, behind an extensionless URL) is retried as HLS.
type ProtocolOpener struct {
	TS  Opener
	HLS Opener
}

// NewProtocolOpener builds the default HTTP-TS + HLS opener.
func NewProtocolOpener(connectTimeout time.Duration, lim VariantLimit) *ProtocolOpener {
	return &ProtocolOpener{TS: NewHTTPOpener(connectTimeout), HLS: NewHLSOpener(connectTimeout, lim)}
}

func (p *ProtocolOpener) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) {
	if IsHLSURL(c.URL) {
		return p.HLS.Open(ctx, c)
	}
	rc, err := p.TS.Open(ctx, c)
	if pe, ok := errors.AsType[*PlaylistError](err); ok {
		c.URL = pe.url // after redirects, so a one-shot redirector is not re-asked
		return p.HLS.Open(ctx, c)
	}
	return rc, err
}

// IsHLSURL reports whether raw names an HLS playlist (.m3u8 path).
func IsHLSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}
