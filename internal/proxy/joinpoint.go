package proxy

// keyTracker follows the program tables of a TS stream and finds the
// packets where a video keyframe starts (random_access_indicator on the
// video PID at a PES start, or, for providers that leave that flag unset, a
// keyframe or sequence header at the start of the PES data), so new viewers
// can join there and a switched-to upstream can start there.
type keyTracker struct {
	pmtPID, videoPID int
	videoType        byte // the video PID's stream_type in the PMT
	pat, pmt         []byte
}

func newKeyTracker() *keyTracker { return &keyTracker{pmtPID: -1, videoPID: -1} }

// scan reads aligned packets and returns the packet index of the last
// keyframe start in them with the PAT+PMT in force there, or -1.
func (k *keyTracker) scan(pkts []byte) (int, []byte) {
	last := -1
	var prefix []byte
	for i := 0; i+TSPacketSize <= len(pkts); i += TSPacketSize {
		pkt := pkts[i : i+TSPacketSize]
		if pkt[0] != tsSync {
			continue
		}
		pid := int(PID(pkt))
		pusi := pkt[1]&0x40 != 0
		afc := pkt[3] >> 4 & 0x3
		payload := pkt[4:]
		rai := false
		if afc&0x2 != 0 {
			alen := int(pkt[4])
			if 5+alen > TSPacketSize {
				continue
			}
			rai = alen > 0 && pkt[5]&0x40 != 0
			payload = pkt[5+alen:]
		}
		if afc&0x1 == 0 {
			payload = nil
		}
		switch {
		case pid == 0 && pusi:
			if prog := parsePAT(payload); prog >= 0 {
				k.pat = append(k.pat[:0], pkt...)
				if prog != k.pmtPID {
					k.pmtPID, k.pmt, k.videoPID = prog, nil, -1
				}
			}
		case pid == k.pmtPID && pusi:
			if video, _, ok := parsePMT(payload); ok {
				k.pmt = append(k.pmt[:0], pkt...)
				k.videoPID, k.videoType = video, pmtStreamType(payload, video)
			}
		case pid == k.videoPID && k.videoPID >= 0 && pusi && (rai || esKeyframe(k.videoType, payload)) && k.pat != nil && k.pmt != nil:
			last = i / TSPacketSize
			prefix = append(append(prefix[:0], k.pat...), k.pmt...)
		}
	}
	return last, prefix
}

// pmtStreamType is the stream_type the PMT in payload gives pid (0 if
// absent).
func pmtStreamType(payload []byte, pid int) byte {
	sec := psiSection(payload)
	if sec == nil || sec[0] != 0x02 || len(sec) < 12 {
		return 0
	}
	i := 12 + (int(sec[10]&0x0F)<<8 | int(sec[11]))
	for i+5 <= len(sec)-4 {
		if int(sec[i+1]&0x1F)<<8|int(sec[i+2]) == pid {
			return sec[i]
		}
		i += 5 + (int(sec[i+3]&0x0F)<<8 | int(sec[i+4]))
	}
	return 0
}

// esKeyframe reports whether a PES packet start (payload of a TS packet
// with payload_unit_start) begins a picture a decoder can start from: an
// H.264 IDR or SPS, an HEVC IRAP picture or VPS/SPS, or an MPEG-2
// sequence header, among the NAL units in this packet.
func esKeyframe(streamType byte, pes []byte) bool {
	if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return false
	}
	es := pes[min(9+int(pes[8]), len(pes)):]
	for i := 0; i+3 < len(es); i++ {
		if es[i] != 0 || es[i+1] != 0 || es[i+2] != 1 {
			continue
		}
		b := es[i+3]
		switch streamType {
		case 0x1B: // H.264
			if t := b & 0x1F; t == 5 || t == 7 {
				return true
			}
		case 0x24: // HEVC
			if t := b >> 1 & 0x3F; (t >= 16 && t <= 21) || t == 32 || t == 33 {
				return true
			}
		case 0x01, 0x02: // MPEG-1/2 video
			if b == 0xB3 {
				return true
			}
		}
	}
	return false
}
