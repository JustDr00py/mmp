// Package epg parses XMLTV files (EPG data) in a streaming fashion:
// channels and programs are emitted through callbacks so huge guides parse
// with bounded memory.
package epg

import (
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// Channel is one XMLTV <channel> element: the id programs refer to and
// the names it goes by.
type Channel struct {
	ID    string
	Names []string // <display-name>s, in file order, without blanks or repeats
}

// Program is one XMLTV <programme> element.
type Program struct {
	Channel  string // channel id (the join key against tvg_id)
	Start    time.Time
	Stop     time.Time
	Title    string
	SubTitle string
	Desc     string
}

// timeLayouts are the XMLTV datetime formats, most common first:
// "YYYYMMDDHHMMSS +ZZZZ" with optional offset.
var timeLayouts = []string{
	"20060102150405 -0700",
	"20060102150405",
	"20060102150405-0700",
}

// ParseTime parses an XMLTV datetime with its per-program UTC offset.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var lastErr error
	for _, layout := range timeLayouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, fmt.Errorf("epg: time %q: %w", s, lastErr)
}

// Parse reads an XMLTV document and emits each complete program.
// Programs missing a parseable start/stop or channel are skipped (real
// guides contain junk). emit's error aborts parsing and is returned.
func Parse(r io.Reader, emit func(Program) error) error {
	return ParseGuide(r, nil, emit)
}

// ParseGuide is Parse that also emits each <channel> with an id to
// onChannel (nil: channels are skipped). Either callback's error aborts
// parsing and is returned.
func ParseGuide(r io.Reader, onChannel func(Channel) error, emit func(Program) error) error {
	dec := xml.NewDecoder(r)
	// EPG files are commonly UTF-8 but ISO-8859-1 appears in the wild.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "iso-8859-1", "latin1":
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		case "utf-16", "utf-16le", "utf-16be":
			// The BOM handles the exact variant.
			return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder().Reader(input), nil
		default:
			return input, nil
		}
	}

	var p Program
	var ch Channel
	inProgram, inChannel := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("epg: xml: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "channel":
				if onChannel == nil || inProgram {
					continue
				}
				ch = Channel{}
				inChannel = true
				for _, a := range el.Attr {
					if a.Name.Local == "id" {
						ch.ID = strings.TrimSpace(a.Value)
					}
				}
			case "display-name":
				if !inChannel {
					continue
				}
				text, err := elementText(dec)
				if err != nil {
					return fmt.Errorf("epg: display-name: %w", err)
				}
				if text != "" && !slices.Contains(ch.Names, text) {
					ch.Names = append(ch.Names, text)
				}
			case "programme":
				p = Program{}
				inProgram = true
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "channel":
						p.Channel = a.Value
					case "start":
						p.Start, _ = ParseTime(a.Value)
					case "stop":
						p.Stop, _ = ParseTime(a.Value)
					}
				}
			case "title", "sub-title", "desc":
				if !inProgram {
					continue
				}
				name := el.Name.Local
				text, err := elementText(dec)
				if err != nil {
					return fmt.Errorf("epg: %s: %w", name, err)
				}
				// Multiple languages of the same element: keep the first
				// non-empty value.
				switch name {
				case "title":
					if p.Title == "" {
						p.Title = text
					}
				case "sub-title":
					if p.SubTitle == "" {
						p.SubTitle = text
					}
				case "desc":
					if p.Desc == "" {
						p.Desc = text
					}
				}
			}
		case xml.EndElement:
			if el.Name.Local == "channel" && inChannel {
				inChannel = false
				if ch.ID == "" {
					continue
				}
				if err := onChannel(ch); err != nil {
					return err
				}
			}
			if el.Name.Local == "programme" && inProgram {
				inProgram = false
				if p.Channel == "" || p.Start.IsZero() || p.Stop.IsZero() || !p.Stop.After(p.Start) {
					continue
				}
				if err := emit(p); err != nil {
					return err
				}
			}
		}
	}
}

// elementText consumes the current element's character content until its
// end tag.
func elementText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 1 {
				sb.Write(el)
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}
