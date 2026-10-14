package proxy

// keyTracker follows the program tables of a TS stream and finds the
// packets where a video keyframe starts (random_access_indicator on the
// video PID at a PES start), so new viewers can join there.
type keyTracker struct {
	pmtPID, videoPID int
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
				k.videoPID = video
			}
		case pid == k.videoPID && k.videoPID >= 0 && pusi && rai && k.pat != nil && k.pmt != nil:
			last = i / TSPacketSize
			prefix = append(append(prefix[:0], k.pat...), k.pmt...)
		}
	}
	return last, prefix
}
