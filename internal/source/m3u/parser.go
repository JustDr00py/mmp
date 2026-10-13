// Package m3u parses extended M3U playlists (IPTV provider exports) in a
// streaming fashion: entries are emitted through a callback so arbitrarily
// large playlists parse with bounded memory.
package m3u

import (
	"bufio"
	"io"
	"strings"
)

// Entry is one playable item with the attributes MMP cares about.
type Entry struct {
	Name    string // display name (text after the last top-level comma)
	URL     string
	TVGID   string
	TVGName string
	Logo    string
	Group   string
}

// Parse reads an extended M3U playlist and calls emit for every entry.
// Parsing stops with emit's error if emit returns one. Bare URLs with no
// preceding #EXTINF are skipped: they carry no metadata to build a stream
// from.
func Parse(r io.Reader, emit func(Entry) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // long lines: many attributes
	first := true
	var pending Entry
	havePending := false
	haveGroup := false // EXTINF carried a group-title

	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\uFEFF") // UTF-8 BOM
			first = false
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			upper := strings.ToUpper(line)
			if rest, ok := strings.CutPrefix(upper, "#EXTINF:"); ok {
				// A new EXTINF before a URL abandons the dangling one.
				pending = parseExtINF(line[len("#EXTINF:"):])
				_ = rest // upper copy only used for the prefix test
				havePending = true
				haveGroup = pending.Group != ""
			} else if rest, ok := strings.CutPrefix(upper, "#EXTGRP:"); ok {
				// Group override from a following #EXTGRP directive.
				if havePending && !haveGroup {
					pending.Group = strings.TrimSpace(line[len("#EXTGRP:"):])
					_ = rest
				}
			}
			// Every other directive (#EXTM3U, #EXTVLCOPT, #KODIPROP, …)
			// and plain comments are ignored.
		default:
			// A media URL line.
			if havePending {
				pending.URL = line
				if err := emit(pending); err != nil {
					return err
				}
				havePending = false
			}
		}
	}
	return sc.Err()
}

// parseExtINF splits "#EXTINF:<duration>[ attrs],<title>" into attributes
// and title. The title separator is the first comma that is not inside a
// quoted attribute value, so titles and values containing commas survive.
func parseExtINF(s string) Entry {
	var e Entry
	// Skip the duration: [-][0-9.] and no more.
	i := 0
	for i < len(s) && (s[i] == '-' || s[i] == '+' || s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}

	title := ""
	attrs := map[string]string{}
	val := ""
	pos := i
	// junkStart remembers where a non-attribute token began; if the line
	// turns out to have no title comma, that token starts the title.
	junkStart := -1
	for {
		// Skip spaces between tokens.
		for pos < len(s) && s[pos] == ' ' {
			pos++
		}
		if pos >= len(s) {
			break
		}
		if s[pos] == ',' {
			title = s[pos+1:]
			break
		}
		// Read a key.
		start := pos
		for pos < len(s) && s[pos] != '=' && s[pos] != ' ' && s[pos] != ',' {
			pos++
		}
		key := strings.ToLower(strings.TrimSpace(s[start:pos]))
		if pos >= len(s) || s[pos] != '=' {
			// Token without '=': not an attribute (e.g. a stray word).
			// Skip it and keep scanning for real attributes; the title
			// comma (if any) still terminates the attribute section.
			if junkStart < 0 {
				junkStart = start
			}
			continue
		}
		pos++ // consume '='
		// Read a value.
		if pos < len(s) && s[pos] == '"' {
			pos++
			vs := pos
			for pos < len(s) && s[pos] != '"' {
				pos++
			}
			val = s[vs:pos]
			if pos < len(s) {
				pos++ // closing quote
			}
		} else {
			vs := pos
			for pos < len(s) && s[pos] != ' ' && s[pos] != ',' {
				pos++
			}
			val = s[vs:pos]
		}
		if key != "" {
			attrs[key] = val
		}
	}
	if title == "" && junkStart >= 0 {
		// No title comma anywhere: a stray token after the duration is the
		// title (e.g. "#EXTINF:-1 Some Channel").
		title = s[junkStart:]
	}

	e.Name = strings.TrimSpace(title)
	e.TVGID = attrs["tvg-id"]
	e.TVGName = attrs["tvg-name"]
	e.Logo = attrs["tvg-logo"]
	e.Group = attrs["group-title"]
	return e
}
