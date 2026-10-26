package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/netguard"
	"github.com/JustDr00py/mmp/internal/redact"
)

// DefaultUserAgent is sent when the candidate sets none.
const DefaultUserAgent = "MMP/1.0"

// ErrConnectTimeout means the upstream did not return response headers
// within the connect timeout.
var ErrConnectTimeout = errors.New("upstream connect timeout")

// StatusError is a non-2xx upstream response.
type StatusError struct {
	Code int
	// RetryAfter is the response's Retry-After in seconds (0 if absent).
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream status %d %s", e.Code, http.StatusText(e.Code))
}

// statusError describes a non-2xx response.
func statusError(resp *http.Response) *StatusError {
	e := &StatusError{Code: resp.StatusCode}
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		e.RetryAfter = time.Duration(n) * time.Second
	}
	return e
}

// Provider busy: a 429 means the account has no free connection (often a
// viewer on another device), not that the stream is broken. The stream
// gets no cooldown; the provider is skipped for its Retry-After, kept
// between these bounds.
const (
	busyDefault = 10 * time.Second
	busyMax     = 30 * time.Second
)

// ProviderBusy reports whether err is the provider refusing for lack of
// connections, and how long to leave it alone.
func ProviderBusy(err error) (time.Duration, bool) {
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusTooManyRequests {
		return 0, false
	}
	if se.RetryAfter <= 0 {
		return busyDefault, true
	}
	return min(se.RetryAfter, busyMax), true
}

// PlaylistError means the upstream answered with an HLS playlist rather
// than a TS stream. It holds the final URL privately; Error() never shows it.
type PlaylistError struct {
	url string
}

func (e *PlaylistError) Error() string { return "upstream answered with an HLS playlist" }

// hlsContentTypes are the playlist media types seen in the wild (RFC 8216
// names the first; the rest are legacy aliases).
var hlsContentTypes = []string{
	"application/vnd.apple.mpegurl",
	"application/x-mpegurl",
	"audio/mpegurl",
	"audio/x-mpegurl",
}

func isHLSContentType(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	for _, t := range hlsContentTypes {
		if mt == t {
			return true
		}
	}
	return false
}

// HTTPOpener opens MPEG-TS streams over plain HTTP(S).
type HTTPOpener struct {
	Client *http.Client
	// ConnectTimeout bounds dial + response headers only; once bytes flow,
	// stalls are tswatch's job. 0 disables it.
	ConnectTimeout time.Duration
}

// NewHTTPOpener builds an opener. The client has no overall timeout: a live
// stream body is read indefinitely.
func NewHTTPOpener(connectTimeout time.Duration) *HTTPOpener {
	return &HTTPOpener{Client: netguard.Client(), ConnectTimeout: connectTimeout}
}

// Open issues the GET and returns the response body. Errors carry only the
// redacted URL.
func (o *HTTPOpener) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) {
	safe := redact.URL(c.URL)
	reqCtx, cancel := context.WithCancelCause(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.URL, nil)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("open %s: invalid url", safe) // err would echo the raw URL
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

	if o.ConnectTimeout > 0 {
		timer := time.AfterFunc(o.ConnectTimeout, func() { cancel(ErrConnectTimeout) })
		defer timer.Stop()
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		cause := context.Cause(reqCtx)
		cancel(nil)
		switch {
		case errors.Is(cause, ErrConnectTimeout):
			err = ErrConnectTimeout
		case ctx.Err() != nil:
			err = ctx.Err()
		default:
			if ue, ok := errors.AsType[*url.Error](err); ok {
				err = ue.Err // url.Error embeds the raw URL; keep only the cause
			}
		}
		return nil, fmt.Errorf("open %s: %w", safe, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // best-effort drain for conn reuse
		resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("open %s: %w", safe, statusError(resp))
	}
	if isHLSContentType(resp.Header.Get("Content-Type")) {
		final := resp.Request.URL.String()
		resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("open %s: %w", safe, &PlaylistError{url: final})
	}
	// Headers arrived: the connect timer is stopped by the deferred Stop.
	// If it fired in the gap, reads fail promptly and the session fails over.
	return &httpStream{body: resp.Body, cancel: cancel}, nil
}

// httpStream ties the body's lifetime to its request context.
type httpStream struct {
	body   io.ReadCloser
	cancel context.CancelCauseFunc
}

func (s *httpStream) Read(p []byte) (int, error) { return s.body.Read(p) }

func (s *httpStream) Close() error {
	s.cancel(nil)
	return s.body.Close()
}
