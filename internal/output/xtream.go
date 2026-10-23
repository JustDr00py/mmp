package output

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

// The Xtream API mirrors Xtream UI panels field for field: players are
// strict about names and about which values are strings and which numbers.

// xtreamTime is the panel's human-readable timestamp layout (server time;
// MMP always reports UTC).
const xtreamTime = "2006-01-02 15:04:05"

// shortEPGDefault is how many listings get_short_epg returns without ?limit.
const shortEPGDefault = 4

// uncategorized is the category for channels without a group.
const uncategorized = "Uncategorized"

type xtreamUserInfo struct {
	Username             string   `json:"username"`
	Password             string   `json:"password"`
	Message              string   `json:"message"`
	Auth                 int      `json:"auth"`
	Status               string   `json:"status"`
	ExpDate              *string  `json:"exp_date"` // unix seconds; null = never
	IsTrial              string   `json:"is_trial"`
	ActiveCons           string   `json:"active_cons"`
	CreatedAt            string   `json:"created_at"`
	MaxConnections       string   `json:"max_connections"`
	AllowedOutputFormats []string `json:"allowed_output_formats"`
}

type xtreamServerInfo struct {
	URL            string `json:"url"`
	Port           string `json:"port"`
	HTTPSPort      string `json:"https_port"`
	ServerProtocol string `json:"server_protocol"`
	RTMPPort       string `json:"rtmp_port"`
	Timezone       string `json:"timezone"`
	TimestampNow   int64  `json:"timestamp_now"`
	TimeNow        string `json:"time_now"`
	Process        bool   `json:"process"`
}

type xtreamCategory struct {
	CategoryID   string `json:"category_id"`
	CategoryName string `json:"category_name"`
	ParentID     int    `json:"parent_id"`
}

type xtreamLiveStream struct {
	Num               int     `json:"num"`
	Name              string  `json:"name"`
	StreamType        string  `json:"stream_type"`
	StreamID          int64   `json:"stream_id"`
	StreamIcon        string  `json:"stream_icon"`
	EPGChannelID      *string `json:"epg_channel_id"`
	Added             string  `json:"added"`
	IsAdult           string  `json:"is_adult"`
	CategoryID        string  `json:"category_id"`
	CategoryIDs       []int64 `json:"category_ids"`
	CustomSid         string  `json:"custom_sid"`
	TVArchive         int     `json:"tv_archive"`
	DirectSource      string  `json:"direct_source"`
	TVArchiveDuration int     `json:"tv_archive_duration"`
}

type xtreamListing struct {
	ID             string `json:"id"`
	EPGID          string `json:"epg_id"`
	Title          string `json:"title"` // base64
	Lang           string `json:"lang"`
	Start          string `json:"start"`
	End            string `json:"end"`
	Description    string `json:"description"` // base64
	ChannelID      string `json:"channel_id"`
	StartTimestamp string `json:"start_timestamp"`
	StopTimestamp  string `json:"stop_timestamp"`
}

// xtreamTableListing is a get_simple_data_table row.
type xtreamTableListing struct {
	xtreamListing
	NowPlaying int `json:"now_playing"`
	HasArchive int `json:"has_archive"`
}

// RootAPI answers Xtream calls that some apps (on Roku, among others)
// send to the server's root instead of player_api.php. It reports false,
// writing nothing, for a request without credentials.
func (h *Handlers) RootAPI(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("username") == "" || q.Get("password") == "" {
		return false
	}
	h.playerAPI(w, r)
	return true
}

// playerAPI is /player_api.php?username=&password=[&action=...].
// Credentials may come in the query or a POSTed form.
func (h *Handlers) playerAPI(w http.ResponseWriter, r *http.Request) {
	user, pass := r.FormValue("username"), r.FormValue("password")
	u, err := h.authPassword(r, user, pass)
	if err != nil {
		var locked *auth.LockedError
		if errors.As(err, &locked) || errors.Is(err, auth.ErrUnauthorized) {
			// Panels answer bad credentials (and throttling) with auth 0,
			// not an HTTP error.
			writeXtream(w, map[string]any{"user_info": map[string]int{"auth": 0}})
			return
		}
		h.authFail(w, err)
		return
	}

	action := r.FormValue("action")
	switch action {
	case "", "get_account_info", "login": // the login; some clients name it (IPTVnator, Roku apps)
		writeXtream(w, map[string]any{
			"user_info":   h.userInfo(u, user, pass),
			"server_info": h.serverInfo(),
		})
		return
	case "get_live_categories", "get_live_streams", "get_short_epg", "get_simple_data_table":
	default:
		if h.serveVOD(w, r, u, action) {
			return
		}
		writeXtream(w, []any{}) // unknown action: empty, as panels answer
		return
	}

	chans, err := h.channelsFor(r.Context(), u)
	if err != nil {
		h.Log.Error("output: list channels", "user_id", u.ID, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	switch action {
	case "get_live_categories":
		writeXtream(w, liveCategories(chans))
	case "get_live_streams":
		writeXtream(w, liveStreams(chans, r.FormValue("category_id")))
	case "get_short_epg":
		limit, err := strconv.Atoi(r.FormValue("limit"))
		if err != nil || limit <= 0 {
			limit = shortEPGDefault
		}
		h.serveListings(w, r, u, chans, false, limit)
	case "get_simple_data_table":
		h.serveListings(w, r, u, chans, true, 0)
	}
}

func (h *Handlers) userInfo(u db.User, user, pass string) xtreamUserInfo {
	info := xtreamUserInfo{
		Username: user, Password: pass, Auth: 1, Status: "Active", IsTrial: "0",
		ActiveCons:           strconv.Itoa(h.Access.ActiveConnections(u.ID)),
		CreatedAt:            unixString(u.CreatedAt),
		MaxConnections:       strconv.Itoa(int(u.MaxConnections)),
		AllowedOutputFormats: []string{"ts"},
	}
	if h.HLS != nil {
		info.AllowedOutputFormats = []string{"m3u8", "ts"}
	}
	if u.ExpiresAt != nil {
		exp := unixString(*u.ExpiresAt)
		info.ExpDate = &exp
	}
	return info
}

// serverInfo describes MMP as players should reach it: host and port from
// BaseURL, never anything upstream.
func (h *Handlers) serverInfo() xtreamServerInfo {
	now := h.now().UTC()
	info := xtreamServerInfo{
		Timezone: "UTC", TimestampNow: now.Unix(), TimeNow: now.Format(xtreamTime), Process: true,
		ServerProtocol: "http", HTTPSPort: "443", RTMPPort: "0",
	}
	b, err := url.Parse(h.BaseURL)
	if err != nil {
		return info
	}
	info.URL = b.Hostname()
	info.ServerProtocol = b.Scheme
	info.Port = b.Port()
	if info.Port == "" {
		info.Port = map[string]string{"https": "443"}[b.Scheme]
		if info.Port == "" {
			info.Port = "80"
		}
	}
	if b.Scheme == "https" {
		info.HTTPSPort = info.Port
	}
	return info
}

// liveCategories lists the groups of chans in group order; channels without
// a group fall under one "Uncategorized" category with id "0".
func liveCategories(chans []Channel) []xtreamCategory {
	type group struct {
		id   int64
		name string
		sort int32
	}
	seen := map[int64]bool{}
	var groups []group
	for _, c := range chans {
		if seen[c.GroupID] {
			continue
		}
		seen[c.GroupID] = true
		g := group{c.GroupID, c.GroupName, c.GroupSort}
		if c.GroupID == 0 {
			g.name = uncategorized
		}
		groups = append(groups, g)
	}
	slices.SortStableFunc(groups, func(a, b group) int {
		if (a.id == 0) != (b.id == 0) { // uncategorized last
			if a.id == 0 {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(a.sort, b.sort), strings.Compare(a.name, b.name), cmp.Compare(a.id, b.id))
	})
	out := make([]xtreamCategory, len(groups))
	for i, g := range groups {
		out[i] = xtreamCategory{CategoryID: strconv.FormatInt(g.id, 10), CategoryName: g.name}
	}
	return out
}

// liveStreams lists chans (optionally one category) in channel order. num is
// the channel number; unnumbered channels count on after the highest one, so
// nums never collide.
func liveStreams(chans []Channel, categoryID string) []xtreamLiveStream {
	next := 0
	for _, c := range chans {
		if c.Number != nil {
			next = max(next, int(*c.Number))
		}
	}
	out := []xtreamLiveStream{}
	for _, c := range chans {
		num := 0
		if c.Number != nil {
			num = int(*c.Number)
		} else {
			next++
			num = next
		}
		cat := strconv.FormatInt(c.GroupID, 10)
		if categoryID != "" && categoryID != cat {
			continue
		}
		epgID := c.GuideID()
		out = append(out, xtreamLiveStream{
			Num: num, Name: c.Name, StreamType: "live", StreamID: c.ID, StreamIcon: c.Logo,
			EPGChannelID: &epgID, Added: unixString(c.Added), IsAdult: "0",
			CategoryID: cat, CategoryIDs: []int64{c.GroupID},
		})
	}
	return out
}

// serveListings answers get_short_epg (upcoming, at most limit) and
// get_simple_data_table (the whole guide window) for ?stream_id. Channels
// the user cannot watch, or without a guide, get an empty list.
func (h *Handlers) serveListings(w http.ResponseWriter, r *http.Request, u db.User, chans []Channel, table bool, limit int) {
	empty := map[string]any{"epg_listings": []any{}}
	id, err := strconv.ParseInt(r.FormValue("stream_id"), 10, 64)
	if err != nil {
		writeXtream(w, empty)
		return
	}
	i := slices.IndexFunc(chans, func(c Channel) bool { return c.ID == id })
	if i < 0 {
		writeXtream(w, empty)
		return
	}
	tvgID := chans[i].GuideID()

	now := h.now()
	since := now
	if table {
		since = now.Add(-guideBehind)
	}
	rows, err := h.programs(r.Context(), tvgID, since, now.Add(guideAhead), now)
	if err != nil {
		h.Log.Error("output: short epg", "user_id", u.ID, "channel_id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	rows = GuideRows(chans[i], rows, h.nameGuideFor(r.Context(), now), since, now.Add(guideAhead))
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	if !table {
		out := make([]xtreamListing, len(rows))
		for i, p := range rows {
			out[i] = listing(p, tvgID)
		}
		writeXtream(w, map[string]any{"epg_listings": out})
		return
	}
	out := make([]xtreamTableListing, len(rows))
	for i, p := range rows {
		out[i] = xtreamTableListing{xtreamListing: listing(p, tvgID)}
		if !now.Before(p.StartTime) && now.Before(p.StopTime) {
			out[i].NowPlaying = 1
		}
	}
	writeXtream(w, map[string]any{"epg_listings": out})
}

func (h *Handlers) programs(ctx context.Context, tvgID string, since, until, now time.Time) ([]db.ListGuideProgramsRow, error) {
	return h.Store.ListGuidePrograms(ctx, db.ListGuideProgramsParams{
		TvgKeys: []string{strings.ToLower(tvgID)}, Since: since, Until: until, Now: now,
	})
}

func listing(p db.ListGuideProgramsRow, tvgID string) xtreamListing {
	id := strconv.FormatInt(p.ID, 10)
	return xtreamListing{
		ID: id, EPGID: id, Lang: "",
		Title:          base64.StdEncoding.EncodeToString([]byte(p.Title)),
		Description:    base64.StdEncoding.EncodeToString([]byte(p.Description)),
		Start:          p.StartTime.UTC().Format(xtreamTime),
		End:            p.StopTime.UTC().Format(xtreamTime),
		ChannelID:      tvgID,
		StartTimestamp: unixString(p.StartTime),
		StopTimestamp:  unixString(p.StopTime),
	}
}

func unixString(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.Unix(), 10)
}

// writeXtream writes v as JSON without HTML escaping, so names containing
// &, < or > arrive as written.
func writeXtream(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
