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
	MaxConnections int    // provider slot limit; 0 = unlimited
	URL            string // may embed credentials: always redact before logging
	UserAgent      string
	Headers        http.Header // extra request headers, optional
}

// Opener opens an upstream byte stream for a candidate. The returned stream
// ends when ctx is cancelled or it is closed. Implementations: HTTP-TS
// (here), HLS and ffmpeg later.
type Opener interface {
	Open(ctx context.Context, c Candidate) (io.ReadCloser, error)
}
