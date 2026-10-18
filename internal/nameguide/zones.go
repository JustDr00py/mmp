package nameguide

import (
	"strings"
	"time"
)

// zoneNames are the time zone abbreviations stream names use. Regional
// names (ET, PT…) use the region's rules, so EST written in summer still
// means New York time, as providers intend.
var zoneNames = map[string]string{
	"GMT": "UTC", "UTC": "UTC", "Z": "UTC", "WET": "Europe/Lisbon", "WEST": "Europe/Lisbon",
	"BST": "Europe/London", "UK": "Europe/London",
	"CET": "Europe/Paris", "CEST": "Europe/Paris", "MEZ": "Europe/Berlin", "MESZ": "Europe/Berlin",
	"EET": "Europe/Athens", "EEST": "Europe/Athens", "MSK": "Europe/Moscow",
	"ET": "America/New_York", "EST": "America/New_York", "EDT": "America/New_York",
	"CT": "America/Chicago", "CST": "America/Chicago", "CDT": "America/Chicago",
	"MT": "America/Denver", "MST": "America/Denver", "MDT": "America/Denver",
	"PT": "America/Los_Angeles", "PST": "America/Los_Angeles", "PDT": "America/Los_Angeles",
	"AEST": "Australia/Sydney", "AEDT": "Australia/Sydney", "AWST": "Australia/Perth",
	"NZST": "Pacific/Auckland", "NZDT": "Pacific/Auckland",
}

var zoneCache = map[string]*time.Location{}

func init() {
	for abbr, name := range zoneNames {
		if loc, err := time.LoadLocation(name); err == nil {
			zoneCache[abbr] = loc
		}
	}
}

// zoneOf is the zone an abbreviation names (case as written: zones are
// upper case in names, which keeps words like "at" or "et" in titles out).
func zoneOf(w string) *time.Location {
	if w != strings.ToUpper(w) {
		return nil
	}
	return zoneCache[w]
}
