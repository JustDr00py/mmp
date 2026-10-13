package m3u

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseFile(t *testing.T, name string) []Entry {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Entry
	if err := Parse(f, func(e Entry) error { out = append(out, e); return nil }); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

func TestParseClassic(t *testing.T) {
	// Fixture has a UTF-8 BOM and CRLF line endings.
	entries := parseFile(t, "classic.m3u")
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	want := Entry{
		Name: "ABC (US) HD", URL: "http://provider-a.tv/stream/abc.ts",
		TVGID: "us.abc.east", TVGName: "ABC US",
		Logo: "https://logo.tv/abc.png", Group: "US Entertainment",
	}
	if entries[0] != want {
		t.Errorf("entry 0 = %+v, want %+v", entries[0], want)
	}
	// Missing attributes must come through as empty strings, not crashes.
	if entries[2].Name != "Plain Channel With No Attributes" || entries[2].TVGID != "" || entries[2].Group != "" {
		t.Errorf("entry 2 = %+v", entries[2])
	}
}

func TestParseMessy(t *testing.T) {
	entries := parseFile(t, "messy.m3u")
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5: %+v", len(entries), entries)
	}
	byURL := map[string]Entry{}
	for _, e := range entries {
		byURL[e.URL] = e
	}

	// Comma inside the title survives; directive lines are skipped.
	if e := byURL["http://provider-b.tv:8080/tve"]; e.Name != "TVE 1 con, coma" || e.Group != "España;Entretenimiento" || e.TVGID != "sp.tve" {
		t.Errorf("tve entry = %+v", e)
	}
	// #EXTGRP supplies the group when EXTINF had none; #KODIPROP ignored.
	if e := byURL["http://provider-b.tv:8080/news24"]; e.TVGName != "News 24" || e.Group != "News" {
		t.Errorf("news24 entry = %+v", e)
	}
	// Dangling EXTINF without a URL is abandoned when the next one arrives.
	if e := byURL["http://provider-b.tv:8080/bbc1"]; e.TVGID != "uk.bbc1" || e.Group != "UK" {
		t.Errorf("bbc1 entry = %+v", e)
	}
	// Unquoted values stop at the next space; a stray token between
	// attributes does not abort attribute scanning.
	if e := byURL["http://provider-b.tv:8080/dasErste"]; e.TVGID != "de.das" || e.Group != "Germany" || e.Name != "Das Erste" {
		t.Errorf("dasErste entry = %+v", e)
	}
}

func TestParseTitleWithoutComma(t *testing.T) {
	// "#EXTINF:-1 Some Channel": token after the duration, no comma.
	var got Entry
	err := Parse(strings.NewReader("#EXTINF:-1 Some Channel\nhttp://x/1\n"),
		func(e Entry) error { got = e; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Some Channel" || got.URL != "http://x/1" {
		t.Errorf("entry = %+v", got)
	}
}

func TestParseEmitErrorStops(t *testing.T) {
	sentinel := errors.New("stop")
	calls := 0
	err := Parse(strings.NewReader("#EXTINF:-1,A\nhttp://x/1\n#EXTINF:-1,B\nhttp://x/2\n"),
		func(Entry) error {
			calls++
			if calls == 1 {
				return sentinel
			}
			return nil
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestParseLargeStreamed covers the bounded-memory AC: a 100k-entry playlist
// parses via the callback without accumulating entries, and quickly.
func TestParseLargeStreamed(t *testing.T) {
	const n = 100_000
	r, w := io.Pipe()
	go func() {
		bw := bufio.NewWriterSize(w, 64*1024)
		for i := 0; i < n; i++ {
			fmt.Fprintf(bw, "#EXTINF:-1 tvg-id=\"ch.%d\" tvg-logo=\"http://l/%d.png\" group-title=\"G%d\",Channel %d\nhttp://provider.tv/s/%d.ts\n", i, i, i%7, i, i)
		}
		bw.Flush()
		w.Close()
	}()

	count, sample := 0, Entry{}
	start := time.Now()
	err := Parse(r, func(e Entry) error {
		if e.TVGID == "ch.99999" {
			sample = e
		}
		count++
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if count != n {
		t.Errorf("count = %d, want %d", count, n)
	}
	if sample.Name != "Channel 99999" || sample.URL != "http://provider.tv/s/99999.ts" || sample.Group != "G4" {
		t.Errorf("sample = %+v", sample)
	}
	if elapsed > 30*time.Second {
		t.Errorf("parse took %v for %d entries", elapsed, n)
	}
	t.Logf("parsed %d entries in %v", count, elapsed)
}
