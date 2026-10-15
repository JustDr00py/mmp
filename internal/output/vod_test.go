package output

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source/xtream"
)

// fakeVOD: categories 1 Action and 2 Kids (movies), 5 Drama (series).
// Alice (user 1) is granted Action and Drama; admins see everything.
type fakeVOD struct{ infoCalls int }

func f32(v float32) *float32 { return &v }
func tp(u int64) *time.Time  { t := time.Unix(u, 0); return &t }

func granted(userID int64, admin bool, cat int64) bool {
	return admin || (userID == 1 && (cat == 1 || cat == 5))
}

func (f *fakeVOD) ListUserVODCategories(_ context.Context, a db.ListUserVODCategoriesParams) ([]db.ListUserVODCategoriesRow, error) {
	all := []db.ListUserVODCategoriesRow{{ID: 1, Kind: "movie", Name: "Action"}, {ID: 2, Kind: "movie", Name: "Kids & <Family>"}, {ID: 5, Kind: "series", Name: "Drama"}}
	var out []db.ListUserVODCategoriesRow
	for _, c := range all {
		if c.Kind == a.Kind && granted(a.UserID, a.IsAdmin, c.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}

var movies = []db.ListUserVODItemsRow{
	{ID: 101, Name: "Die Hard", Icon: "http://img.example/dh.jpg", Rating: f32(7.5), ContainerExtension: "mkv", TmdbID: "562", AddedAt: tp(1700000000), CategoryID: 1},
	{ID: 102, Name: "Cars", ContainerExtension: "mp4", CategoryID: 2},
}

func (f *fakeVOD) ListUserVODItems(_ context.Context, a db.ListUserVODItemsParams) ([]db.ListUserVODItemsRow, error) {
	var out []db.ListUserVODItemsRow
	for _, m := range movies {
		if granted(a.UserID, a.IsAdmin, m.CategoryID) && (a.CategoryID == nil || *a.CategoryID == m.CategoryID) {
			out = append(out, m)
		}
	}
	return out, nil
}

var show = db.ListUserSeriesRow{ID: 7, Name: "The Show", Cover: "http://img.example/show.jpg", Plot: "A plot.", Cast: "A, B",
	Director: "D", Genre: "Drama", ReleaseDate: "2020-01-01", Rating: f32(8), LastModified: tp(1700000500), CategoryID: 5}

func (f *fakeVOD) ListUserSeries(_ context.Context, a db.ListUserSeriesParams) ([]db.ListUserSeriesRow, error) {
	if !granted(a.UserID, a.IsAdmin, show.CategoryID) || (a.CategoryID != nil && *a.CategoryID != 5) {
		return nil, nil
	}
	return []db.ListUserSeriesRow{show}, nil
}

func (f *fakeVOD) GetUserVODItem(_ context.Context, a db.GetUserVODItemParams) (db.GetUserVODItemRow, error) {
	for _, m := range movies {
		if m.ID == a.ID && granted(a.UserID, a.IsAdmin, m.CategoryID) {
			return db.GetUserVODItemRow{ID: m.ID, Name: m.Name, ContainerExtension: m.ContainerExtension, Icon: m.Icon,
				Rating: m.Rating, TmdbID: m.TmdbID, AddedAt: m.AddedAt, CategoryID: m.CategoryID}, nil
		}
	}
	return db.GetUserVODItemRow{}, pgx.ErrNoRows
}

func (f *fakeVOD) GetUserSeries(_ context.Context, a db.GetUserSeriesParams) (db.GetUserSeriesRow, error) {
	if a.ID == 8 && !a.IsAdmin { // series 8 sits in category 6, granted to nobody
		return db.GetUserSeriesRow{}, pgx.ErrNoRows
	}
	if a.ID != show.ID || !granted(a.UserID, a.IsAdmin, show.CategoryID) {
		return db.GetUserSeriesRow{}, pgx.ErrNoRows
	}
	return db.GetUserSeriesRow{ID: show.ID, Name: show.Name, Cover: show.Cover, Plot: show.Plot, Cast: show.Cast, Director: show.Director,
		Genre: show.Genre, ReleaseDate: show.ReleaseDate, Rating: show.Rating, LastModified: show.LastModified, CategoryID: show.CategoryID}, nil
}

func (f *fakeVOD) GetUserEpisode(context.Context, db.GetUserEpisodeParams) (db.GetUserEpisodeRow, error) {
	return db.GetUserEpisodeRow{}, pgx.ErrNoRows
}

// MovieInfo is a panel info block, including a provider playback URL that
// must never reach a customer.
func (f *fakeVOD) MovieInfo(_ context.Context, id int64) (json.RawMessage, error) {
	f.infoCalls++
	return json.RawMessage(`{"tmdb_id":"562","plot":"An officer...","genre":"Action","duration_secs":7920,"duration":"02:12:00",
		"backdrop_path":["http://img.example/bd.jpg"],"direct_source":"http://prov.example/movie/u/p/11.mkv",
		"trailer_stream":"http://prov.example:8080/movie/user/pass/11.mkv","name":"Panel Name"}`), nil
}

func (f *fakeVOD) SeriesEpisodes(_ context.Context, id int64) (db.Series, []db.SeriesEpisode, error) {
	return db.Series{ID: id, Info: json.RawMessage(`{"plot":"Panel plot","youtube_trailer":"abc","episode_run_time":"45"}`)},
		[]db.SeriesEpisode{
			{ID: 701, SeriesID: id, Season: 1, EpisodeNum: 1, Title: "Pilot", ContainerExtension: "mkv", DurationSecs: 2700, Plot: "First.", AddedAt: tp(1700000100)},
			{ID: 702, SeriesID: id, Season: 1, EpisodeNum: 2, Title: "Second", ContainerExtension: "mkv", DurationSecs: 2650},
			{ID: 801, SeriesID: id, Season: 2, EpisodeNum: 1, Title: "Return", ContainerExtension: "mp4", DurationSecs: 3000, Image: "http://img.example/s2e1.jpg"},
		}, nil
}

func newVODHandlers(t *testing.T) (http.Handler, *fakeVOD) {
	t.Helper()
	hs, _ := newTestHandlers(t)
	f := &fakeVOD{}
	hs.VOD = f
	return chiRouter(hs), f
}

func TestPlayerAPIVODGolden(t *testing.T) {
	h, _ := newVODHandlers(t)
	const alice = "/player_api.php?username=alice&password=alice-pw1"
	const root = "/player_api.php?username=root&password=root-pw1"
	for _, tc := range []struct{ name, path string }{
		{"vod_categories_customer", alice + "&action=get_vod_categories"},
		{"vod_categories_admin", root + "&action=get_vod_categories"},
		{"vod_streams_customer", alice + "&action=get_vod_streams"},
		{"vod_streams_category", root + "&action=get_vod_streams&category_id=2"},
		{"vod_streams_bad_category", root + "&action=get_vod_streams&category_id=abc"},
		{"vod_info", alice + "&action=get_vod_info&vod_id=101"},
		{"vod_info_forbidden", alice + "&action=get_vod_info&vod_id=102"},
		{"series_categories", alice + "&action=get_series_categories"},
		{"series_list", alice + "&action=get_series"},
		{"series_info", alice + "&action=get_series_info&series_id=7"},
		{"series_info_forbidden", alice + "&action=get_series_info&series_id=8"},
		{"series_info_unknown", alice + "&action=get_series_info&series_id=999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := get(t, h, tc.path)
			if code != 200 {
				t.Fatalf("%s = %d %s", tc.path, code, body)
			}
			golden(t, tc.name, body)
		})
	}
}

func TestVODInfoNeverLeaksProviderURLs(t *testing.T) {
	h, _ := newVODHandlers(t)
	_, _, body := get(t, h, "/player_api.php?username=alice&password=alice-pw1&action=get_vod_info&vod_id=101")
	if strings.Contains(body, "prov.example") || !strings.Contains(body, `"direct_source":""`) {
		t.Errorf("provider URL leaked or direct_source set:\n%s", body)
	}
	if !strings.Contains(body, `"name":"Die Hard"`) || strings.Contains(body, "Panel Name") || !strings.Contains(body, "img.example/bd.jpg") {
		t.Errorf("MMP's name must win and images stay:\n%s", body)
	}
}

// TestVODDecodesAsPanel: the repo's Xtream ingest client (written against
// real panels) reads MMP's movie and series answers.
func TestVODDecodesAsPanel(t *testing.T) {
	h, _ := newVODHandlers(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c, err := xtream.New(srv.URL, "root", "root-pw1", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cats, err := c.VODCategories(ctx)
	if err != nil || len(cats) != 2 || cats[1].CategoryName != "Kids & <Family>" {
		t.Fatalf("categories = %+v %v", cats, err)
	}
	ms, err := c.VODStreams(ctx)
	if err != nil || len(ms) != 2 || ms[0].StreamID.Int() != 101 || ms[0].ContainerExtension != "mkv" {
		t.Fatalf("movies = %+v %v", ms, err)
	}
	if x, ok := ms[0].Rating.Float(); !ok || x != 7.5 {
		t.Errorf("rating = %v", ms[0].Rating)
	}
	shows, err := c.Series(ctx)
	if err != nil || len(shows) != 1 || shows[0].SeriesID.Int() != 7 || shows[0].Released() != "2020-01-01" {
		t.Fatalf("series = %+v %v", shows, err)
	}
	info, err := c.SeriesInfo(ctx, "7")
	if err != nil || len(info.Episodes) != 3 {
		t.Fatalf("series info = %+v %v", info, err)
	}
	for _, e := range info.Episodes {
		if e.ID == "801" && (e.Season.Int() != 2 || e.ContainerExtension != "mp4" || e.Details().DurationSecs.Int() != 3000) {
			t.Errorf("episode 801 = %+v", e)
		}
	}
	if vi, err := c.VODInfo(ctx, "101"); err != nil || !strings.Contains(string(vi), `"tmdb_id":"562"`) {
		t.Errorf("vod info = %s %v", vi, err)
	}
}
