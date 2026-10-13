package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrHLSUnsupported marks playlists MMP cannot remux into TS: encrypted,
// fMP4 (EXT-X-MAP) or byte-range segments.
var ErrHLSUnsupported = errors.New("unsupported hls playlist")

// hlsSegment is one media segment, its URI resolved to absolute.
type hlsSegment struct {
	Seq           int64
	URI           string
	Duration      time.Duration
	Discontinuity bool // EXT-X-DISCONTINUITY precedes it
}

// hlsMedia is a parsed media playlist.
type hlsMedia struct {
	TargetDuration time.Duration
	Segments       []hlsSegment
	Ended          bool // EXT-X-ENDLIST: no more segments will appear
}

// hlsVariant is one EXT-X-STREAM-INF entry of a master playlist.
type hlsVariant struct {
	URI       string
	Bandwidth int64
}

// parseHLS parses a playlist fetched from base. Exactly one of the results
// is non-nil on success: a master playlist yields its variants.
func parseHLS(data []byte, base *url.URL) (*hlsMedia, []hlsVariant, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	if !sc.Scan() || !strings.HasPrefix(strings.TrimPrefix(sc.Text(), "\ufeff"), "#EXTM3U") {
		return nil, nil, errors.New("hls: missing #EXTM3U")
	}
	media := &hlsMedia{}
	var variants []hlsVariant
	var seq int64
	var dur time.Duration
	var disc, haveInf, wantVariant bool
	var bw int64
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		tag, val, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXT-X-TARGETDURATION":
			n, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("hls: bad target duration %q", val)
			}
			media.TargetDuration = seconds(n)
		case tag == "#EXT-X-MEDIA-SEQUENCE":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("hls: bad media sequence %q", val)
			}
			seq = n
		case tag == "#EXTINF":
			d, _, _ := strings.Cut(val, ",")
			n, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
			if err != nil {
				return nil, nil, fmt.Errorf("hls: bad EXTINF %q", val)
			}
			dur, haveInf = seconds(n), true
		case tag == "#EXT-X-DISCONTINUITY":
			disc = true
		case tag == "#EXT-X-ENDLIST":
			media.Ended = true
		case tag == "#EXT-X-KEY":
			if m := hlsAttrs(val)["METHOD"]; m != "NONE" {
				return nil, nil, fmt.Errorf("%w: encryption %s", ErrHLSUnsupported, m)
			}
		case tag == "#EXT-X-MAP":
			return nil, nil, fmt.Errorf("%w: fMP4 segments", ErrHLSUnsupported)
		case tag == "#EXT-X-BYTERANGE":
			return nil, nil, fmt.Errorf("%w: byte-range segments", ErrHLSUnsupported)
		case tag == "#EXT-X-STREAM-INF":
			bw, _ = strconv.ParseInt(hlsAttrs(val)["BANDWIDTH"], 10, 64)
			wantVariant = true
		case strings.HasPrefix(line, "#"):
			// other tags and comments are irrelevant for remuxing
		default: // a URI line
			u, err := base.Parse(line)
			if err != nil {
				return nil, nil, errors.New("hls: bad uri")
			}
			switch {
			case wantVariant:
				variants = append(variants, hlsVariant{URI: u.String(), Bandwidth: bw})
				wantVariant = false
			case haveInf:
				media.Segments = append(media.Segments, hlsSegment{Seq: seq, URI: u.String(), Duration: dur, Discontinuity: disc})
				seq++
				disc, haveInf = false, false
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("hls: %w", err)
	}
	if len(variants) > 0 {
		return nil, variants, nil
	}
	if media.TargetDuration <= 0 {
		media.TargetDuration = 10 * time.Second
	}
	return media, nil, nil
}

// bestVariant picks the highest-bandwidth variant (the first on ties).
func bestVariant(vs []hlsVariant) hlsVariant {
	best := vs[0]
	for _, v := range vs[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	return best
}

// hlsAttrs parses an attribute list (KEY=value,KEY="quoted, value").
func hlsAttrs(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				val, rest = rest[1:], ""
			} else {
				val, rest = rest[1:end+1], rest[end+2:]
			}
			rest = strings.TrimPrefix(rest, ",")
		} else {
			val, rest, _ = strings.Cut(rest, ",")
		}
		out[strings.ToUpper(strings.TrimSpace(key))] = val
		s = rest
	}
	return out
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
