package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestCappedReader: input up to the cap reads through; past it the read
// fails instead of ending quietly.
func TestCappedReader(t *testing.T) {
	if b, err := io.ReadAll(&cappedReader{r: strings.NewReader("12345"), n: 5}); err != nil || string(b) != "12345" {
		t.Fatalf("at the cap: %q, %v", b, err)
	}
	if _, err := io.ReadAll(&cappedReader{r: strings.NewReader("123456"), n: 5}); !errors.Is(err, errGuideTooLarge) {
		t.Fatalf("past the cap: err = %v, want errGuideTooLarge", err)
	}
}

// TestCappedReaderGzipBomb: a small gzip stream inflating past the cap is
// refused.
func TestCappedReaderGzipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(make([]byte, 8<<20)) //nolint:errcheck // in-memory
	zw.Close()
	if buf.Len() > 64<<10 {
		t.Fatalf("bomb not small: %d bytes", buf.Len())
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, &cappedReader{r: zr, n: 1 << 20}); !errors.Is(err, errGuideTooLarge) {
		t.Fatalf("err = %v, want errGuideTooLarge", err)
	}
}

// TestBoundedTimesOut: a scheduled refresh that hangs is cut off, so the
// next provider gets its turn.
func TestBoundedTimesOut(t *testing.T) {
	start := time.Now()
	_, err := bounded(context.Background(), 50*time.Millisecond, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

// TestFetchM3UDropsNonHTTPEntries: entries MMP could never open (other
// schemes, relative paths) are not stored.
func TestFetchM3UDropsNonHTTPEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "#EXTM3U\n"+ //nolint:errcheck // test server
			"#EXTINF:-1,Good\nhttp://example.com/1.ts\n"+
			"#EXTINF:-1,File\nfile:///etc/passwd\n"+
			"#EXTINF:-1,Relative\n/live/2.ts\n"+
			"#EXTINF:-1,Secure\nhttps://example.com/3.m3u8\n")
	}))
	defer srv.Close()
	r := NewRefresher(slog.New(slog.DiscardHandler), nil, 80)
	got, err := r.fetchM3U(context.Background(), db.Provider{ID: 1, Type: "m3u", Url: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.name)
	}
	if strings.Join(names, ",") != "Good,Secure" {
		t.Fatalf("entries = %v, want [Good Secure]", names)
	}
}
