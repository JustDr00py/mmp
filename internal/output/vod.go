package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// VODSource is the movie/series catalog as one customer may see it, plus
// the lazily fetched panel details.
type VODSource interface {
	ListUserVODCategories(ctx context.Context, arg db.ListUserVODCategoriesParams) ([]db.ListUserVODCategoriesRow, error)
	ListUserVODItems(ctx context.Context, arg db.ListUserVODItemsParams) ([]db.ListUserVODItemsRow, error)
	ListUserSeries(ctx context.Context, arg db.ListUserSeriesParams) ([]db.ListUserSeriesRow, error)
	GetUserVODItem(ctx context.Context, arg db.GetUserVODItemParams) (db.GetUserVODItemRow, error)
	GetUserSeries(ctx context.Context, arg db.GetUserSeriesParams) (db.GetUserSeriesRow, error)
	GetUserEpisode(ctx context.Context, arg db.GetUserEpisodeParams) (db.GetUserEpisodeRow, error)
	GetProvider(ctx context.Context, id int64) (db.Provider, error)
	ListUserCachedEpisodes(ctx context.Context, arg db.ListUserCachedEpisodesParams) ([]db.ListUserCachedEpisodesRow, error)
	// MovieInfo is the panel's get_vod_info "info" block (cached).
	MovieInfo(ctx context.Context, itemID int64) (json.RawMessage, error)
	// SeriesEpisodes loads a series' episodes (cached) with its info block.
	SeriesEpisodes(ctx context.Context, seriesID int64) (db.Series, []db.SeriesEpisode, error)
}

// VODCatalog is the production VODSource: queries plus the refresher's
// cached panel lookups.
type VODCatalog struct {
	*db.Queries
	Refresher           *source.Refresher
	InfoTTL, EpisodeTTL time.Duration
}

func (c *VODCatalog) MovieInfo(ctx context.Context, itemID int64) (json.RawMessage, error) {
	return c.Refresher.MovieInfo(ctx, itemID, c.InfoTTL)
}

func (c *VODCatalog) SeriesEpisodes(ctx context.Context, seriesID int64) (db.Series, []db.SeriesEpisode, error) {
	return c.Refresher.SeriesEpisodesByID(ctx, seriesID, c.EpisodeTTL)
}

// --- wire shapes (as panels send them: ids as numbers where panels use
// numbers, and as strings where they use strings) ---

type xtreamVODCategory struct {
	CategoryID   string `json:"category_id"`
	CategoryName string `json:"category_name"`
	ParentID     int    `json:"parent_id"`
}

type xtreamVODStream struct {
	Num                int     `json:"num"`
	Name               string  `json:"name"`
	StreamType         string  `json:"stream_type"`
	StreamID           int64   `json:"stream_id"`
	StreamIcon         string  `json:"stream_icon"`
	Rating             string  `json:"rating"`
	Rating5            float64 `json:"rating_5based"`
	Added              string  `json:"added"`
	IsAdult            string  `json:"is_adult"`
	CategoryID         string  `json:"category_id"`
	CategoryIDs        []int64 `json:"category_ids"`
	ContainerExtension string  `json:"container_extension"`
	CustomSID          string  `json:"custom_sid"`
	DirectSource       string  `json:"direct_source"`
}

type xtreamMovieData struct {
	StreamID           int64  `json:"stream_id"`
	Name               string `json:"name"`
	Added              string `json:"added"`
	CategoryID         string `json:"category_id"`
	ContainerExtension string `json:"container_extension"`
	CustomSID          string `json:"custom_sid"`
	DirectSource       string `json:"direct_source"`
}

type xtreamSeries struct {
	Num            int      `json:"num"`
	Name           string   `json:"name"`
	SeriesID       int64    `json:"series_id"`
	Cover          string   `json:"cover"`
	Plot           string   `json:"plot"`
	Cast           string   `json:"cast"`
	Director       string   `json:"director"`
	Genre          string   `json:"genre"`
	ReleaseDate    string   `json:"releaseDate"`
	LastModified   string   `json:"last_modified"`
	Rating         string   `json:"rating"`
	Rating5        float64  `json:"rating_5based"`
	BackdropPath   []string `json:"backdrop_path"`
	YoutubeTrailer string   `json:"youtube_trailer"`
	EpisodeRunTime string   `json:"episode_run_time"`
	CategoryID     string   `json:"category_id"`
	CategoryIDs    []int64  `json:"category_ids"`
}

type xtreamEpisode struct {
	ID                 string            `json:"id"`
	EpisodeNum         int32             `json:"episode_num"`
	Title              string            `json:"title"`
	ContainerExtension string            `json:"container_extension"`
	Info               xtreamEpisodeInfo `json:"info"`
	CustomSID          string            `json:"custom_sid"`
	Added              string            `json:"added"`
	Season             int32             `json:"season"`
	DirectSource       string            `json:"direct_source"`
}

type xtreamEpisodeInfo struct {
	DurationSecs int32  `json:"duration_secs"`
	Duration     string `json:"duration"`
	Plot         string `json:"plot,omitempty"`
	MovieImage   string `json:"movie_image,omitempty"`
}

type xtreamSeason struct {
	SeasonNumber int32  `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	Cover        string `json:"cover"`
}

// --- helpers ---

func ratingStrings(r *float32) (string, float64) {
	if r == nil {
		return "", 0
	}
	five := math.Round(float64(*r)/2*10) / 10
	return strconv.FormatFloat(float64(*r), 'f', -1, 32), five
}

func unixPtr(t *time.Time) string {
	if t == nil {
		return "0"
	}
	return unixString(*t)
}

func hms(secs int32) string {
	return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
}

// scrubInfo decodes a panel info block and drops anything that could point
// a player at the provider instead of MMP: direct_source-style keys and
// strings that look like provider playback or API URLs.
func scrubInfo(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return map[string]any{}
	}
	for k, v := range m {
		lk := strings.ToLower(k)
		if lk == "direct_source" || lk == "stream_url" || lk == "url" {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && providerURLish(s) {
			delete(m, k)
		}
	}
	return m
}

func providerURLish(s string) bool {
	l := strings.ToLower(s)
	if !strings.Contains(l, "://") {
		return false
	}
	return strings.Contains(l, "/movie/") || strings.Contains(l, "/series/") || strings.Contains(l, "/live/") ||
		strings.Contains(l, "player_api.php") || strings.Contains(l, "password=") || strings.Contains(l, "username=")
}

// emptyInfo is the panels' answer for an unknown or forbidden id.
var emptyInfo = map[string]any{"info": []any{}, "movie_data": []any{}}

// --- actions ---

// serveVOD answers the movie and series actions; it reports false for any
// other action.
func (h *Handlers) serveVOD(w http.ResponseWriter, r *http.Request, u db.User, action string) bool {
	switch action {
	case "get_vod_categories", "get_vod_streams", "get_vod_info",
		"get_series_categories", "get_series", "get_series_info":
	default:
		return false
	}
	if h.VOD == nil {
		writeXtream(w, []any{}) // a live-only panel
		return true
	}
	ctx := r.Context()
	admin := u.Role == auth.RoleAdmin
	var err error
	switch action {
	case "get_vod_categories":
		err = h.vodCategories(w, ctx, u, admin, "movie")
	case "get_series_categories":
		err = h.vodCategories(w, ctx, u, admin, "series")
	case "get_vod_streams":
		err = h.vodStreams(w, ctx, u, admin, r.FormValue("category_id"))
	case "get_series":
		err = h.seriesList(w, ctx, u, admin, r.FormValue("category_id"))
	case "get_vod_info":
		err = h.vodInfo(w, ctx, u, admin, r.FormValue("vod_id"))
	case "get_series_info":
		err = h.seriesInfo(w, ctx, u, admin, r.FormValue("series_id"))
	}
	if err != nil {
		h.Log.Error("output: "+action, "user_id", u.ID, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}
	return true
}

// categoryFilter parses an optional category_id; ok is false for junk,
// which (like panels) yields an empty list.
func categoryFilter(v string) (*int64, bool) {
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil, false
	}
	return &n, true
}

func (h *Handlers) vodCategories(w http.ResponseWriter, ctx context.Context, u db.User, admin bool, kind string) error {
	rows, err := h.VOD.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: kind, UserID: u.ID, IsAdmin: admin})
	if err != nil {
		return err
	}
	out := make([]xtreamVODCategory, 0, len(rows))
	for _, c := range rows {
		out = append(out, xtreamVODCategory{CategoryID: strconv.FormatInt(c.ID, 10), CategoryName: c.Name})
	}
	writeXtream(w, out)
	return nil
}

func (h *Handlers) vodStreams(w http.ResponseWriter, ctx context.Context, u db.User, admin bool, category string) error {
	cid, ok := categoryFilter(category)
	if !ok {
		writeXtream(w, []any{})
		return nil
	}
	rows, err := h.VOD.ListUserVODItems(ctx, db.ListUserVODItemsParams{CategoryID: cid, UserID: u.ID, IsAdmin: admin})
	if err != nil {
		return err
	}
	out := make([]xtreamVODStream, 0, len(rows))
	for i, m := range rows {
		rating, five := ratingStrings(m.Rating)
		out = append(out, xtreamVODStream{Num: i + 1, Name: m.Name, StreamType: "movie", StreamID: m.ID,
			StreamIcon: m.Icon, Rating: rating, Rating5: five, Added: unixPtr(m.AddedAt), IsAdult: "0",
			CategoryID: strconv.FormatInt(m.CategoryID, 10), CategoryIDs: []int64{m.CategoryID},
			ContainerExtension: m.ContainerExtension})
	}
	writeXtream(w, out)
	return nil
}

func (h *Handlers) seriesList(w http.ResponseWriter, ctx context.Context, u db.User, admin bool, category string) error {
	cid, ok := categoryFilter(category)
	if !ok {
		writeXtream(w, []any{})
		return nil
	}
	rows, err := h.VOD.ListUserSeries(ctx, db.ListUserSeriesParams{CategoryID: cid, UserID: u.ID, IsAdmin: admin})
	if err != nil {
		return err
	}
	out := make([]xtreamSeries, 0, len(rows))
	for i, s := range rows {
		rating, five := ratingStrings(s.Rating)
		out = append(out, xtreamSeries{Num: i + 1, Name: s.Name, SeriesID: s.ID, Cover: s.Cover, Plot: s.Plot,
			Cast: s.Cast, Director: s.Director, Genre: s.Genre, ReleaseDate: s.ReleaseDate,
			LastModified: unixPtr(s.LastModified), Rating: rating, Rating5: five, BackdropPath: []string{},
			CategoryID: strconv.FormatInt(s.CategoryID, 10), CategoryIDs: []int64{s.CategoryID}})
	}
	writeXtream(w, out)
	return nil
}

func (h *Handlers) vodInfo(w http.ResponseWriter, ctx context.Context, u db.User, admin bool, idStr string) error {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeXtream(w, emptyInfo)
		return nil
	}
	m, err := h.VOD.GetUserVODItem(ctx, db.GetUserVODItemParams{ID: id, UserID: u.ID, IsAdmin: admin})
	if errors.Is(err, pgx.ErrNoRows) {
		writeXtream(w, emptyInfo)
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := h.VOD.MovieInfo(ctx, m.ID)
	if err != nil {
		h.Log.Warn("output: movie info", "vod_item_id", m.ID, "err", err) // serve what we have
	}
	info := scrubInfo(raw)
	// Our own fields win where panels and players expect them.
	info["name"] = m.Name
	if _, ok := info["movie_image"]; !ok && m.Icon != "" {
		info["movie_image"] = m.Icon
	}
	if m.TmdbID != "" {
		if _, ok := info["tmdb_id"]; !ok {
			info["tmdb_id"] = m.TmdbID
		}
	}
	if rating, _ := ratingStrings(m.Rating); rating != "" {
		if _, ok := info["rating"]; !ok {
			info["rating"] = rating
		}
	}
	writeXtream(w, map[string]any{
		"info": info,
		"movie_data": xtreamMovieData{StreamID: m.ID, Name: m.Name, Added: unixPtr(m.AddedAt),
			CategoryID: strconv.FormatInt(m.CategoryID, 10), ContainerExtension: m.ContainerExtension},
	})
	return nil
}

func (h *Handlers) seriesInfo(w http.ResponseWriter, ctx context.Context, u db.User, admin bool, idStr string) error {
	empty := map[string]any{"seasons": []any{}, "info": []any{}, "episodes": []any{}}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeXtream(w, empty)
		return nil
	}
	s, err := h.VOD.GetUserSeries(ctx, db.GetUserSeriesParams{ID: id, UserID: u.ID, IsAdmin: admin})
	if errors.Is(err, pgx.ErrNoRows) {
		writeXtream(w, empty)
		return nil
	}
	if err != nil {
		return err
	}
	row, eps, err := h.VOD.SeriesEpisodes(ctx, s.ID)
	if err != nil {
		h.Log.Warn("output: series episodes", "series_id", s.ID, "err", err)
		row.Info = s.Info // still answer with the series itself
	}
	info := scrubInfo(row.Info)
	rating, five := ratingStrings(s.Rating)
	for k, v := range map[string]any{
		"name": s.Name, "cover": s.Cover, "plot": s.Plot, "cast": s.Cast, "director": s.Director,
		"genre": s.Genre, "releaseDate": s.ReleaseDate, "last_modified": unixPtr(s.LastModified),
		"rating": rating, "rating_5based": five, "category_id": strconv.FormatInt(s.CategoryID, 10),
	} {
		if cur, ok := info[k]; !ok || cur == "" || k == "name" || k == "category_id" {
			info[k] = v
		}
	}
	if _, ok := info["backdrop_path"]; !ok {
		info["backdrop_path"] = []string{}
	}

	bySeason := map[string][]xtreamEpisode{}
	count := map[int32]int{}
	for _, e := range eps {
		key := strconv.Itoa(int(e.Season))
		bySeason[key] = append(bySeason[key], xtreamEpisode{ID: strconv.FormatInt(e.ID, 10), EpisodeNum: e.EpisodeNum,
			Title: e.Title, ContainerExtension: e.ContainerExtension, Added: unixPtr(e.AddedAt), Season: e.Season,
			Info: xtreamEpisodeInfo{DurationSecs: e.DurationSecs, Duration: hms(e.DurationSecs), Plot: e.Plot, MovieImage: e.Image}})
		count[e.Season]++
	}
	var seasons []xtreamSeason
	for n, c := range count {
		seasons = append(seasons, xtreamSeason{SeasonNumber: n, Name: "Season " + strconv.Itoa(int(n)), EpisodeCount: c, Cover: s.Cover})
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].SeasonNumber < seasons[j].SeasonNumber })
	var episodes any = bySeason
	if len(bySeason) == 0 {
		episodes = []any{} // panels send [] rather than {} when there are none
	}
	if seasons == nil {
		seasons = []xtreamSeason{}
	}
	writeXtream(w, map[string]any{"seasons": seasons, "info": info, "episodes": episodes})
	return nil
}
