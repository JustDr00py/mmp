package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Some providers put a source label before every stream name ("AT&T: MTV",
// "SLING | ESPN"). The admin lists those labels on the Settings page;
// matching and suggested channel names leave them out, so "AT&T: MTV"
// matches "MTV". Only labels on the list are removed: cutting whatever
// comes before a colon would also split names like "NFL: RedZone".

const ignorePrefixesKey = "match_ignore_prefixes"

// Limits on the list, so a pasted channel list cannot slow every match.
const (
	maxIgnorePrefixes  = 100
	maxIgnorePrefixLen = 40
)

// prefixSeparators may follow a label; prefixPasses is how many stacked
// labels are removed ("SLING | AT&T: MTV" needs two).
const (
	prefixSeparators = ":|-/"
	prefixPasses     = 4
)

// SettingsStore reads and writes stored settings (satisfied by *db.Queries).
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}

// LoadIgnorePrefixes reads the prefix list (empty if never saved).
func LoadIgnorePrefixes(ctx context.Context, q SettingsStore) ([]string, error) {
	raw, err := q.GetSetting(ctx, ignorePrefixesKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ignored name prefixes: %w", err)
	}
	var p []string
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ignored name prefixes: %w", err)
	}
	return p, nil
}

// SaveIgnorePrefixes stores the list; the next match uses it.
func SaveIgnorePrefixes(ctx context.Context, q SettingsStore, prefixes []string) error {
	if err := ValidateIgnorePrefixes(prefixes); err != nil {
		return err
	}
	if prefixes == nil {
		prefixes = []string{}
	}
	raw, _ := json.Marshal(prefixes)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: ignorePrefixesKey, Value: raw}); err != nil {
		return fmt.Errorf("ignored name prefixes: %w", err)
	}
	return nil
}

// ValidateIgnorePrefixes checks a list from the form.
func ValidateIgnorePrefixes(prefixes []string) error {
	if len(prefixes) > maxIgnorePrefixes {
		return fmt.Errorf("at most %d prefixes", maxIgnorePrefixes)
	}
	for _, p := range prefixes {
		if utf8.RuneCountInString(p) > maxIgnorePrefixLen {
			return fmt.Errorf("prefix %q is longer than %d characters", p, maxIgnorePrefixLen)
		}
	}
	return nil
}

// ParseIgnorePrefixes reads the form's text: one prefix per line or
// comma-separated. A trailing separator is dropped ("AT&T:" is "AT&T"),
// and duplicates (ignoring case) are listed once.
func ParseIgnorePrefixes(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		for _, p := range strings.Split(line, ",") {
			p = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(p), prefixSeparators))
			if p == "" || seen[strings.ToLower(p)] {
				continue
			}
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	return out
}

// StripPrefixes removes listed source labels from the start of name when
// a separator follows them: with "AT&T", "AT&T: MTV" and "at&t | MTV"
// become "MTV", while "AT&T SportsNet" stays. Case is ignored. A label is
// kept when nothing would follow it.
func StripPrefixes(name string, prefixes []string) string {
	s := strings.TrimSpace(name)
	for range prefixPasses {
		cut := false
		for _, p := range prefixes {
			if p == "" || len(s) < len(p) || !strings.EqualFold(s[:len(p)], p) {
				continue
			}
			rest := strings.TrimLeft(s[len(p):], " ")
			if rest == "" || !strings.ContainsRune(prefixSeparators, rune(rest[0])) {
				continue
			}
			if rest = strings.TrimSpace(rest[1:]); rest != "" {
				s, cut = rest, true
				break
			}
		}
		if !cut {
			break
		}
	}
	return s
}
