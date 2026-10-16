package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// MediaInfo describes what a stream carries, for the dashboard. Codecs come
// from the stream's PMT; resolution, frame rate and audio layout only when a
// MediaProbe (ffprobe) is configured.
type MediaInfo struct {
	VideoCodec string  `json:"video_codec,omitempty"` // e.g. "H264"
	AudioCodec string  `json:"audio_codec,omitempty"` // e.g. "AAC"
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	Audio      string  `json:"audio,omitempty"` // channel layout, e.g. "stereo", "5.1"
}

// Known reports whether anything was found.
func (m MediaInfo) Known() bool { return m != MediaInfo{} }

// merge fills m's empty fields from o.
func (m MediaInfo) merge(o MediaInfo) MediaInfo {
	if m.VideoCodec == "" {
		m.VideoCodec = o.VideoCodec
	}
	if m.AudioCodec == "" {
		m.AudioCodec = o.AudioCodec
	}
	if m.Width == 0 || m.Height == 0 {
		m.Width, m.Height = o.Width, o.Height
	}
	if m.FPS == 0 {
		m.FPS = o.FPS
	}
	if m.Audio == "" {
		m.Audio = o.Audio
	}
	return m
}

// MediaProbe inspects a sample of MPEG-TS (e.g. with ffprobe). It runs off
// the streaming path, once per upstream.
type MediaProbe func(ctx context.Context, sample []byte) (MediaInfo, error)

// mediaSampleSize is how much of a new upstream is kept for probing: a few
// seconds of HD, enough for ffprobe to find codec parameters.
const mediaSampleSize = 2 << 20

// tsMediaInfo reads the codecs of the first program's first video and audio
// streams from the PAT and PMT in aligned packets.
func tsMediaInfo(pkts []byte) MediaInfo {
	var m MediaInfo
	pmtPID := -1
	for i := 0; i+TSPacketSize <= len(pkts); i += TSPacketSize {
		pkt := pkts[i : i+TSPacketSize]
		if pkt[0] != tsSync || pkt[1]&0x40 == 0 { // only section starts
			continue
		}
		payload := pkt[4:]
		switch afc := pkt[3] >> 4 & 0x3; {
		case afc&0x1 == 0:
			continue
		case afc&0x2 != 0:
			alen := int(pkt[4])
			if 5+alen > TSPacketSize {
				continue
			}
			payload = pkt[5+alen:]
		}
		pid := int(PID(pkt))
		switch {
		case pid == 0 && pmtPID < 0:
			pmtPID = parsePAT(payload)
		case pid == pmtPID:
			return pmtCodecs(payload)
		}
	}
	return m
}

// pmtCodecs names the first video and audio codecs a PMT lists.
func pmtCodecs(payload []byte) MediaInfo {
	var m MediaInfo
	sec := psiSection(payload)
	if sec == nil || sec[0] != 0x02 || len(sec) < 12 {
		return m
	}
	i := 12 + (int(sec[10]&0x0F)<<8 | int(sec[11]))
	for i+5 <= len(sec)-4 {
		st := sec[i]
		esLen := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
		end := min(i+5+esLen, len(sec)-4)
		var desc []byte
		if i+5 <= end {
			desc = sec[i+5 : end]
		}
		v, a := streamTypeCodec(st, desc)
		if m.VideoCodec == "" {
			m.VideoCodec = v
		}
		if m.AudioCodec == "" {
			m.AudioCodec = a
		}
		i += 5 + esLen
	}
	return m
}

// streamTypeCodec maps a PMT stream_type (and, for private data, its
// descriptors) to a video or audio codec name.
func streamTypeCodec(st byte, desc []byte) (video, audio string) {
	switch st {
	case 0x01, 0x02:
		return "MPEG2", ""
	case 0x10:
		return "MPEG4", ""
	case 0x1B:
		return "H264", ""
	case 0x24:
		return "HEVC", ""
	case 0x42:
		return "AVS", ""
	case 0xEA:
		return "VC1", ""
	case 0x03, 0x04:
		return "", "MP2"
	case 0x0F:
		return "", "AAC"
	case 0x11:
		return "", "AAC-LATM"
	case 0x81:
		return "", "AC3"
	case 0x87:
		return "", "EAC3"
	case 0x06: // PES private data: the descriptors say what it is
		for j := 0; j+2 <= len(desc); j += 2 + int(desc[j+1]) {
			switch desc[j] {
			case 0x6A:
				return "", "AC3"
			case 0x7A:
				return "", "EAC3"
			case 0x7C:
				return "", "AAC"
			}
		}
	}
	return "", ""
}

// codecName normalises an ffprobe codec_name to the PMT style.
func codecName(s string) string {
	s = strings.ToUpper(s)
	if s == "MPEG2VIDEO" {
		return "MPEG2"
	}
	return s
}

// ffprobeTimeout bounds one ffprobe run over a sample.
const ffprobeTimeout = 10 * time.Second

// FFprobe returns a MediaProbe that runs ffprobe at path on a sample fed
// over stdin, so no URL or credential reaches its argv.
func FFprobe(path string) MediaProbe {
	return func(ctx context.Context, sample []byte) (MediaInfo, error) {
		ctx, cancel := context.WithTimeout(ctx, ffprobeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "-v", "error", "-f", "mpegts", "-i", "pipe:0",
			"-show_entries", "stream=codec_type,codec_name,width,height,avg_frame_rate,r_frame_rate,channels,channel_layout",
			"-of", "json")
		cmd.Stdin = bytes.NewReader(sample)
		out, err := cmd.Output()
		if err != nil && len(out) == 0 {
			return MediaInfo{}, fmt.Errorf("ffprobe: %w", err)
		}
		return parseFFprobe(out)
	}
}

// parseFFprobe reads ffprobe's JSON stream list: the first video and
// audio streams.
func parseFFprobe(out []byte) (MediaInfo, error) {
	var r struct {
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			AvgFrameRate  string `json:"avg_frame_rate"`
			RFrameRate    string `json:"r_frame_rate"`
			Channels      int    `json:"channels"`
			ChannelLayout string `json:"channel_layout"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe output: %w", err)
	}
	var m MediaInfo
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			if m.VideoCodec != "" {
				continue
			}
			m.VideoCodec, m.Width, m.Height = codecName(s.CodecName), s.Width, s.Height
			m.FPS = cmp.Or(frameRate(s.AvgFrameRate), frameRate(s.RFrameRate))
		case "audio":
			if m.AudioCodec != "" {
				continue
			}
			m.AudioCodec = codecName(s.CodecName)
			m.Audio = s.ChannelLayout
			if m.Audio == "" && s.Channels > 0 {
				m.Audio = map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"}[s.Channels]
			}
			if i := strings.IndexByte(m.Audio, '('); i > 0 { // "5.1(side)"
				m.Audio = m.Audio[:i]
			}
		}
	}
	return m, nil
}

// frameRate parses "30000/1001" (or "25") to frames per second; 0 if
// unknown.
func frameRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if !ok {
		return n
	}
	d, err := strconv.ParseFloat(den, 64)
	if err != nil || d <= 0 {
		return 0
	}
	return n / d
}
