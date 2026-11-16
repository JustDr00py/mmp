package output

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/JustDr00py/mmp/internal/vodmeta"
)

// tmdbImages is TMDB's image root; a size ("w500") and a path follow.
const tmdbImages = "https://image.tmdb.org/t/p/"

// tmdbImage is the URL of a TMDB image path at size, or "" without one.
func tmdbImage(path, size string) string {
	if path == "" || !strings.HasPrefix(path, "/") {
		return ""
	}
	return tmdbImages + size + path
}

// vodSettings are the VOD library settings (all off without a source).
func (h *Handlers) vodSettings(ctx context.Context) vodmeta.Settings {
	if h.VODLibrary == nil {
		return vodmeta.Settings{}
	}
	return h.VODLibrary(ctx)
}

// vodName is what customers see a title as: the panel's name, or with
// clean titles on "Title (Year)" (TMDB's title for a matched one, see
// vodmeta.DisplayTitle), and " [4K]" for a 4K copy (kept apart from the HD
// one, so players can tell them apart).
func vodName(raw, title, tmdbTitle, lang string, year int32, quality string, clean bool) string {
	if !clean || title == "" {
		return raw
	}
	n := vodmeta.DisplayTitle(title, tmdbTitle, lang)
	if year > 0 {
		n += " (" + strconv.Itoa(int(year)) + ")"
	}
	if quality == "4k" {
		n += " [4K]"
	}
	return n
}

// tmdbDetails is the part of a cached TMDB movie or tv answer that fills
// what a panel left out.
type tmdbDetails struct {
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
	Runtime      int     `json:"runtime"`
	VoteAverage  float64 `json:"vote_average"`
	Genres       []struct {
		Name string `json:"name"`
	} `json:"genres"`
}

// fillFromTMDB sets the info keys a panel left out or empty from cached
// TMDB details; the panel's own values always win. series uses the keys
// get_series_info answers carry.
func fillFromTMDB(info map[string]any, raw []byte, series bool) {
	var d tmdbDetails
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		return
	}
	set := func(k string, v any) {
		if cur, ok := info[k]; !blank(v) && (!ok || blank(cur)) {
			info[k] = v
		}
	}
	var genres []string
	for _, g := range d.Genres {
		genres = append(genres, g.Name)
	}
	set("plot", d.Overview)
	set("genre", strings.Join(genres, ", "))
	if d.BackdropPath != "" {
		set("backdrop_path", []string{tmdbImage(d.BackdropPath, "w1280")})
	}
	if d.VoteAverage > 0 {
		set("rating", strconv.FormatFloat(d.VoteAverage, 'f', 1, 64))
	}
	poster := tmdbImage(d.PosterPath, "w500")
	if series {
		set("cover", poster)
		set("releaseDate", d.FirstAirDate)
		return
	}
	set("description", d.Overview)
	set("movie_image", poster)
	set("cover_big", poster)
	set("releasedate", d.ReleaseDate)
	if d.Runtime > 0 {
		set("duration_secs", d.Runtime*60)
		set("duration", hms(int32(d.Runtime*60)))
	}
}

// blank: a value a player would show as nothing.
func blank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	}
	return false
}
