package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPOpenStreamsWithHeaders(t *testing.T) {
	payload := tsStream(0x100, 100)
	var gotUA, gotX string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotX = r.UserAgent(), r.Header.Get("X-Token")
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(payload)
	}))
	defer srv.Close()

	o := NewHTTPOpener(2 * time.Second)
	rc, err := o.Open(context.Background(), Candidate{
		URL: srv.URL + "/live/u/p/1.ts", UserAgent: "VLC/3.0.20",
		Headers: http.Header{"X-Token": {"abc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body mismatch: %d vs %d bytes", len(got), len(payload))
	}
	if gotUA != "VLC/3.0.20" || gotX != "abc" {
		t.Errorf("headers: ua=%q x=%q", gotUA, gotX)
	}

	// Default UA when none is configured.
	rc, err = o.Open(context.Background(), Candidate{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if gotUA != DefaultUserAgent {
		t.Errorf("default ua = %q", gotUA)
	}
}

func TestHTTPOpenStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := NewHTTPOpener(time.Second).Open(context.Background(), Candidate{URL: srv.URL})
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want StatusError 503", err)
	}
}

func TestHTTPOpenConnectTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never sends headers in time
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, err := NewHTTPOpener(150*time.Millisecond).Open(context.Background(), Candidate{URL: srv.URL})
	if !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("err = %v, want ErrConnectTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v", d)
	}
}

// TestHTTPConnectTimeoutDoesNotCutBody: once headers arrive, a body slower
// than the connect timeout keeps streaming.
func TestHTTPConnectTimeoutDoesNotCutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := range 4 {
			time.Sleep(100 * time.Millisecond)
			w.Write(tsPkt(0x100, uint8(i)))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	rc, err := NewHTTPOpener(150*time.Millisecond).Open(context.Background(), Candidate{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("body cut after %d bytes: %v", len(got), err)
	}
	if len(got) != 4*TSPacketSize {
		t.Errorf("got %d bytes", len(got))
	}
}

func TestHTTPCancelAndCloseUnblockRead(t *testing.T) {
	serverDone := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(tsPkt(0x100, 0))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // endless live stream until the client goes away
		serverDone <- struct{}{}
	}))
	defer srv.Close()
	o := NewHTTPOpener(time.Second)

	for _, how := range []string{"cancel", "close"} {
		t.Run(how, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rc, err := o.Open(ctx, Candidate{URL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			buf := make([]byte, TSPacketSize)
			if _, err := io.ReadFull(rc, buf); err != nil {
				t.Fatal(err)
			}
			errc := make(chan error, 1)
			go func() {
				_, err := rc.Read(buf) // blocks: server sends nothing more
				errc <- err
			}()
			time.Sleep(50 * time.Millisecond)
			if how == "cancel" {
				cancel()
			} else {
				rc.Close()
			}
			select {
			case err := <-errc:
				if err == nil {
					t.Error("read returned nil error after " + how)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("read not unblocked by %s", how)
			}
			select {
			case <-serverDone: // upstream connection really released
			case <-time.After(2 * time.Second):
				t.Fatalf("server still streaming after %s", how)
			}
		})
	}
}

// TestHTTPErrorsRedactCredentials: Xtream-style URLs carry user/pass in the
// path; errors must never echo them (AGENTS hard rule 1).
func TestHTTPErrorsRedactCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // closed port: connection refused

	urls := []string{
		srv.URL + "/live/alice/s3cretPW/123.ts",
		"http://alice:s3cretPW@" + strings.TrimPrefix(srv.URL, "http://") + "/stream.ts",
		srv.URL + "/get.php?username=alice&password=s3cretPW",
	}
	for _, u := range urls {
		_, err := NewHTTPOpener(time.Second).Open(context.Background(), Candidate{URL: u})
		if err == nil {
			t.Fatalf("expected error for %s", u)
		}
		if strings.Contains(err.Error(), "s3cretPW") {
			t.Errorf("credential leaked in error: %v", err)
		}
	}

	// Status errors too.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv2.Close()
	_, err := NewHTTPOpener(time.Second).Open(context.Background(), Candidate{URL: srv2.URL + "/live/alice/s3cretPW/1.ts"})
	if err == nil || strings.Contains(err.Error(), "s3cretPW") {
		t.Errorf("status error = %v", err)
	}
}
