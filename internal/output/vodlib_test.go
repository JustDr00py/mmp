package output

import (
	"context"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/vodmeta"
)

func TestVODName(t *testing.T) {
	for _, tc := range []struct {
		raw, title, tmdb string
		year             int32
		quality          string
		clean            bool
		want             string
	}{
		{"EN | The Matrix 4K (1999)", "The Matrix", "", 1999, "4k", true, "The Matrix (1999) [4K]"},
		{"EN | The Matrix (1999)", "The Matrix", "", 1999, "fhd", true, "The Matrix (1999)"},
		{"EN | Untitled", "Untitled", "", 0, "", true, "Untitled"},
		{"EN | The Matrix (1999)", "The Matrix", "", 1999, "", false, "EN | The Matrix (1999)"},
		{"Not parsed yet", "", "", 0, "", true, "Not parsed yet"},
		// A matched title shows as TMDB names it; the panel's name when off.
		{"EN - The Matrix 2 Reloaded 4K (2003)", "The Matrix 2 Reloaded", "The Matrix Reloaded", 2003, "4k", true, "The Matrix Reloaded (2003) [4K]"},
		{"EN - The Matrix 2 Reloaded (2003)", "The Matrix 2 Reloaded", "The Matrix Reloaded", 2003, "", false, "EN - The Matrix 2 Reloaded (2003)"},
	} {
		if got := vodName(tc.raw, tc.title, tc.tmdb, "English", tc.year, tc.quality, tc.clean); got != tc.want {
			t.Errorf("vodName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFillFromTMDB(t *testing.T) {
	info := map[string]any{"plot": "Panel plot.", "genre": "", "backdrop_path": []any{}}
	fillFromTMDB(info, []byte(`{"overview":"TMDB plot.","poster_path":"/p.jpg","backdrop_path":"/b.jpg","runtime":132,
		"vote_average":7.84,"release_date":"1988-07-15","genres":[{"name":"Action"},{"name":"Thriller"}]}`), false)
	for k, want := range map[string]any{
		"plot": "Panel plot.", "description": "TMDB plot.", "genre": "Action, Thriller", "rating": "7.8",
		"movie_image": "https://image.tmdb.org/t/p/w500/p.jpg", "releasedate": "1988-07-15", "duration_secs": 7920,
		"duration": "02:12:00",
	} {
		if info[k] != want {
			t.Errorf("%s = %#v, want %#v", k, info[k], want)
		}
	}
	if bd, _ := info["backdrop_path"].([]string); len(bd) != 1 || bd[0] != "https://image.tmdb.org/t/p/w1280/b.jpg" {
		t.Errorf("backdrop = %#v", info["backdrop_path"])
	}
	series := map[string]any{"cover": "http://panel/c.jpg"}
	fillFromTMDB(series, []byte(`{"poster_path":"/s.jpg","first_air_date":"2008-01-20"}`), true)
	if series["cover"] != "http://panel/c.jpg" || series["releaseDate"] != "2008-01-20" {
		t.Errorf("series = %#v", series)
	}
	empty := map[string]any{}
	fillFromTMDB(empty, []byte(`not json`), false)
	if len(empty) != 0 {
		t.Errorf("bad details filled %v", empty)
	}
}

// TestVODCleanTitles: with clean titles on, players get "Title (Year)"
// and what the panel left out comes from TMDB; the panel's values win.
func TestVODCleanTitles(t *testing.T) {
	hs, _ := newTestHandlers(t)
	f := &fakeVOD{tmdb: map[string]string{"movie/562": `{"overview":"TMDB plot.","release_date":"1988-07-15","genres":[{"name":"Thriller"}]}`}}
	hs.VOD = f
	clean := false
	hs.VODLibrary = func(context.Context) vodmeta.Settings { return vodmeta.Settings{CleanTitles: clean} }
	h := chiRouter(hs)
	const alice = "/player_api.php?username=alice&password=alice-pw1"
	if _, _, body := get(t, h, alice+"&action=get_vod_streams"); !strings.Contains(body, `"name":"Die Hard"`) {
		t.Errorf("clean titles off: %s", body)
	}
	clean = true
	if _, _, body := get(t, h, alice+"&action=get_vod_streams"); !strings.Contains(body, `"name":"Die Hard (1988)"`) {
		t.Errorf("clean titles on: %s", body)
	}
	_, _, body := get(t, h, alice+"&action=get_vod_info&vod_id=101")
	for _, want := range []string{`"name":"Die Hard (1988)"`, `"releasedate":"1988-07-15"`, `"genre":"Action"`, `"plot":"An officer..."`} {
		if !strings.Contains(body, want) {
			t.Errorf("info lacks %s:\n%s", want, body)
		}
	}
}

// TestVODCleanEpisodeTitles: with clean titles on, an episode's title is
// its own name, not the panel's "EN ★ Show - S01E01 - Name [HD]".
func TestVODCleanEpisodeTitles(t *testing.T) {
	hs, _ := newTestHandlers(t)
	hs.VOD = &fakeVOD{episodes: []db.SeriesEpisode{
		{ID: 701, Season: 1, EpisodeNum: 1, Title: "EN ★ The Show - S01E01 - Minimum Viable Product [SD]", ContainerExtension: "mkv"},
		{ID: 702, Season: 1, EpisodeNum: 2, Title: "EN ★ The Show - S01E02 - [HD]", ContainerExtension: "mkv"},
	}}
	clean := false
	hs.VODLibrary = func(context.Context) vodmeta.Settings { return vodmeta.Settings{CleanTitles: clean} }
	h := chiRouter(hs)
	const info = "/player_api.php?username=alice&password=alice-pw1&action=get_series_info&series_id=7"
	if _, _, body := get(t, h, info); !strings.Contains(body, `"title":"EN ★ The Show - S01E01 - Minimum Viable Product [SD]"`) {
		t.Errorf("clean titles off: %s", body)
	}
	clean = true
	_, _, body := get(t, h, info)
	for _, want := range []string{`"title":"Minimum Viable Product"`, `"title":"Episode 2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("clean titles on lacks %s:\n%s", want, body)
		}
	}
}
