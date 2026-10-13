package proxy

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrHLSUnsupported marks playlists MMP cannot remux into TS: SAMPLE-AES or
// DRM encryption, fMP4 (EXT-X-MAP) or byte-range segments.
var ErrHLSUnsupported = errors.New("unsupported hls playlist")

// hlsSegment is one media segment, its URI resolved to absolute.
type hlsSegment struct {
	Seq           int64
	URI           string
	Duration      time.Duration
	Discontinuity bool    // EXT-X-DISCONTINUITY precedes it
	Key           *hlsKey // AES-128 key in force, nil when clear
}

// hlsKey is an EXT-X-KEY with METHOD=AES-128.
type hlsKey struct {
	URI string // resolved to absolute
	IV  []byte // 16 bytes; nil means the media sequence number
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
	Height    int // from RESOLUTION; 0 when absent
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
	var key *hlsKey
	var bw int64
	var height int
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
			k, err := parseKey(hlsAttrs(val), base)
			if err != nil {
				return nil, nil, err
			}
			key = k
		case tag == "#EXT-X-MAP":
			return nil, nil, fmt.Errorf("%w: fMP4 segments", ErrHLSUnsupported)
		case tag == "#EXT-X-BYTERANGE":
			return nil, nil, fmt.Errorf("%w: byte-range segments", ErrHLSUnsupported)
		case tag == "#EXT-X-STREAM-INF":
			attrs := hlsAttrs(val)
			bw, _ = strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			height = 0
			if _, h, ok := strings.Cut(attrs["RESOLUTION"], "x"); ok {
				height, _ = strconv.Atoi(h)
			}
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
				variants = append(variants, hlsVariant{URI: u.String(), Bandwidth: bw, Height: height})
				wantVariant = false
			case haveInf:
				media.Segments = append(media.Segments, hlsSegment{Seq: seq, URI: u.String(), Duration: dur, Discontinuity: disc, Key: key})
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

// parseKey reads an EXT-X-KEY attribute list: nil for METHOD=NONE, the key
// for AES-128, ErrHLSUnsupported for anything else.
func parseKey(attrs map[string]string, base *url.URL) (*hlsKey, error) {
	switch m := attrs["METHOD"]; m {
	case "NONE":
		return nil, nil
	case "AES-128":
	default:
		return nil, fmt.Errorf("%w: encryption %s", ErrHLSUnsupported, m)
	}
	if f, ok := attrs["KEYFORMAT"]; ok && f != "identity" {
		return nil, fmt.Errorf("%w: key format %s", ErrHLSUnsupported, f)
	}
	if attrs["URI"] == "" {
		return nil, errors.New("hls: AES-128 key without URI")
	}
	u, err := base.Parse(attrs["URI"])
	if err != nil {
		return nil, errors.New("hls: bad key uri")
	}
	k := &hlsKey{URI: u.String()}
	if iv, ok := attrs["IV"]; ok {
		h := strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X")
		if k.IV, err = hex.DecodeString(h); err != nil || len(k.IV) != 16 {
			return nil, errors.New("hls: bad key IV")
		}
	}
	return k, nil
}

// VariantLimit caps the variant picked from a master playlist; zero fields
// mean no limit.
type VariantLimit struct {
	MaxBandwidth int64 // bits/s, from BANDWIDTH
	MaxHeight    int   // pixels, from RESOLUTION
}

func (l VariantLimit) allows(v hlsVariant) bool {
	return (l.MaxBandwidth <= 0 || v.Bandwidth <= l.MaxBandwidth) &&
		(l.MaxHeight <= 0 || v.Height <= l.MaxHeight)
}

// bestVariant picks the highest-bandwidth variant within lim (the first on
// ties). If none fits, it falls back to the lowest-bandwidth one: a channel
// that plays above the cap beats one that does not play.
func bestVariant(vs []hlsVariant, lim VariantLimit) hlsVariant {
	var best *hlsVariant
	for i, v := range vs {
		if lim.allows(v) && (best == nil || v.Bandwidth > best.Bandwidth) {
			best = &vs[i]
		}
	}
	if best != nil {
		return *best
	}
	low := vs[0]
	for _, v := range vs[1:] {
		if v.Bandwidth < low.Bandwidth {
			low = v
		}
	}
	return low
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
