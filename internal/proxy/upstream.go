package proxy

import (
	"context"
	"io"
	"net/http"
)

// Candidate is one entry of a channel's failover list, resolved for opening.
type Candidate struct {
	StreamID       int64
	ProviderID     int64
	Backbone       string // the provider's upstream network; "" = unknown
	MaxConnections int    // provider slot limit; 0 = unlimited
	URL            string // may embed credentials: always redact before logging
	UserAgent      string
	Headers        http.Header // extra request headers, optional
	// FFmpeg routes the stream through ffmpeg with FFmpegArgs (the
	// channel's stream profile in ffmpeg mode).
	FFmpeg     bool
	FFmpegArgs string
	// Health is the prober's health_score (1 = healthy, the default).
	Health float32
}

// Opener opens an upstream byte stream for a candidate. The returned stream
// ends when ctx is cancelled or it is closed. Implementations: HTTP-TS,
// HLS, and the ffmpeg stage of stream profiles.
type Opener interface {
	Open(ctx context.Context, c Candidate) (io.ReadCloser, error)
}
