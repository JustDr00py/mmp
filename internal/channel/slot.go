package channel

import (
	"regexp"
	"strings"
)

// Event feeds are numbered slots that providers rename for each event they
// carry, so a slot's name is its label plus whatever is on now. Panels put
// the label first ("US - ESPN PLUS 01 : WSL PRO PORTUGAL OCT 22 – 3:30 AM
// ET", "Flo (FLSP) 100: …", "S-EUR 001|  …") or last ("College: SMU vs
// Arizona State @ Oct 22 5:00 PM :ESPN+  14", ":Kayo  100" when empty).

var (
	// slotTail is a label glued to a closing " :" (or a leading ":"); a
	// spaced " : " is a label-first slot's ("GAATV 06 : … EANNA 2026").
	slotTail = regexp.MustCompile(`(?:^|\s):(\pL[^:|]*\s\d{1,4})\s*$`)
	// slotNum ends a label: the slot number after a space or "(", maybe
	// closed by ")" and marked "ⓧ".
	slotNum = regexp.MustCompile(`[\s(]\d{1,4}\)?\s*ⓧ?$`)
	letter  = regexp.MustCompile(`\pL`)
)

// SplitSlot splits an event slot's name into its stable label and the
// event it now carries ("" for an empty slot). ok is false for a name
// that is not a numbered slot's ("US: ESPN 2 HD", "Live | Jets @ Patriots").
func SplitSlot(name string) (label, event string, ok bool) {
	name = strings.TrimSpace(name)
	if parts := strings.Split(name, "|"); len(parts) > 2 && isStatus(parts[0]) {
		// "ENDED | GRAND PRIX LYON (250) | Thu 22 Oct 14:00 CEST | SE: TV4 PLAY PPV 6"
		last := strings.TrimSpace(parts[len(parts)-1])
		if !slotNum.MatchString(" "+last) || !letter.MatchString(last) {
			return "", "", false
		}
		return strings.Join(strings.Fields(last), " "), trimEvent(strings.Join(parts[1:len(parts)-1], "|")), true
	}
	if m := slotTail.FindStringSubmatchIndex(name); m != nil {
		return strings.Join(strings.Fields(name[m[2]:m[3]]), " "), trimEvent(name[:m[0]]), true
	}
	// The label ends at the first separator past any short tag ("US:",
	// "|TW|"); a later one would take part of the event ("Fite TV 13 HD
	// (D): BKFC Manchester 2025: …").
	for i, r := range name {
		if r != ':' && r != '|' {
			continue
		}
		prefix := strings.TrimSpace(name[:i])
		before, after := byteAt(name, i-1), byteAt(name, i+1)
		switch {
		case r == ':' && before != ' ' && after != ' ' && after != 0: // "3:30", "FC:2025"
			continue
		case r == '|' && !isDigit(before) && !strings.HasSuffix(prefix, ")"): // "001|", "(SN+ 001) |", not "ESPN 2 | East"
			continue
		case len(prefix) <= 6 && !strings.ContainsAny(prefix, "0123456789"):
			continue
		}
		if !slotNum.MatchString(" "+prefix) || !letter.MatchString(prefix) {
			break
		}
		return strings.Join(strings.Fields(prefix), " "), trimEvent(name[i+1:]), true
	}
	return "", "", false
}

func trimEvent(s string) string { return strings.Trim(s, " \t|:-–—@") }

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isStatus reports the status a piped event name starts with ("Next | …").
func isStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "next", "live", "end", "ended", "upcoming", "now", "replay":
		return true
	}
	return false
}
