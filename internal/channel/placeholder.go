package channel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsPlaceholder reports whether a stream name is a provider's section
// header dressed up as a stream ("##### ABC IDAHO #####", "TR| ===== X"):
// it starts, after an optional short "XX|" or "XX:" prefix, with a run of
// three or more of the same symbol. Such entries play nothing and must not
// become channels. An ellipsis inside a name ("WHAT IF...") is not a run
// at the start, so real names are kept.
func IsPlaceholder(name string) bool {
	s := strings.TrimSpace(name)
	if i := strings.IndexAny(s, "|:"); i > 0 && i <= 6 {
		if rest := strings.TrimSpace(s[i+1:]); rest != "" && symbolRun(rest) >= 3 {
			return true
		}
	}
	return symbolRun(s) >= 3
}

// symbolRun is the length of the run of one repeated non-letter, non-digit,
// non-space rune that s starts with.
func symbolRun(s string) int {
	first, size := utf8.DecodeRuneInString(s)
	if size == 0 || unicode.IsLetter(first) || unicode.IsDigit(first) || unicode.IsSpace(first) {
		return 0
	}
	n := 0
	for _, r := range s {
		if r != first {
			break
		}
		n++
	}
	return n
}
