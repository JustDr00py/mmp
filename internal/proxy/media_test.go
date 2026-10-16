package proxy

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// psiPkt wraps a PSI section (without CRC; a zero CRC is appended) in a
// section-start packet on pid.
func psiPkt(pid uint16, section []byte) []byte {
	b := tsPkt(pid, 0)
	b[1] |= 0x40
	sec := append(append([]byte(nil), section...), 0, 0, 0, 0)
	n := len(sec) - 3
	sec[1], sec[2] = 0xB0|byte(n>>8), byte(n)
	b[4] = 0 // pointer field
	copy(b[5:], sec)
	for i := 5 + len(sec); i < len(b); i++ {
		b[i] = 0xFF
	}
	return b
}

func TestTSMediaInfo(t *testing.T) {
	pat := psiPkt(0, []byte{0x00, 0, 0, 0, 1, 0xC1, 0, 0, 0, 1, 0xF0, 0x00}) // program 1 -> PMT PID 0x1000
	pmt := psiPkt(0x1000, []byte{0x02, 0, 0, 0, 1, 0xC1, 0, 0, 0xE1, 0x00, 0xF0, 0x00,
		0x06, 0xE1, 0x02, 0xF0, 0x03, 0x6A, 0x01, 0x00, // private data with an AC-3 descriptor
		0x1B, 0xE1, 0x01, 0xF0, 0x00, // H.264
	})
	var pkts []byte
	pkts = append(pkts, dataPkt('x', 0)...) // before the PAT: ignored
	pkts = append(pkts, pmt...)             // before the PAT: not yet known as the PMT
	pkts = append(pkts, pat...)
	pkts = append(pkts, pmt...)
	got := tsMediaInfo(pkts)
	if got != (MediaInfo{VideoCodec: "H264", AudioCodec: "AC3"}) {
		t.Errorf("tsMediaInfo = %+v", got)
	}
	if m := tsMediaInfo(pkts[:3*TSPacketSize]); m.Known() {
		t.Errorf("without the PMT after the PAT: %+v, want nothing", m)
	}
}

func TestParseFFprobe(t *testing.T) {
	out := []byte(`{"streams":[
		{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"30000/1001","r_frame_rate":"60000/1001"},
		{"codec_type":"audio","codec_name":"aac","channels":6,"channel_layout":"5.1(side)"},
		{"codec_type":"audio","codec_name":"mp2","channels":2}]}`)
	m, err := parseFFprobe(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.VideoCodec != "H264" || m.Width != 1280 || m.Height != 720 || m.FPS < 29.96 || m.FPS > 29.98 ||
		m.AudioCodec != "AAC" || m.Audio != "5.1" {
		t.Errorf("parseFFprobe = %+v", m)
	}
	if m, _ := parseFFprobe([]byte(`{"streams":[{"codec_type":"audio","codec_name":"ac3","channels":2}]}`)); m.Audio != "stereo" {
		t.Errorf("layout from channel count = %q", m.Audio)
	}
	for in, want := range map[string]float64{"25/1": 25, "50": 50, "0/0": 0, "": 0, "x/1": 0} {
		if got := frameRate(in); got != want {
			t.Errorf("frameRate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFFprobeMedia(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	data := makeTS(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := FFprobe(ffprobe)(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if m.VideoCodec != "H264" || m.Width != 320 || m.Height != 240 || m.FPS != 25 || m.AudioCodec != "AAC" || m.Audio == "" {
		t.Errorf("FFprobe = %+v", m)
	}
	if pm := tsMediaInfo(data); pm.VideoCodec != "H264" || pm.AudioCodec != "AAC" {
		t.Errorf("tsMediaInfo of real TS = %+v", pm)
	}
}
