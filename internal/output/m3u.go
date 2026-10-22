package output

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/activity"
	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// Store is what the customer outputs read (satisfied by *db.Queries).
type Store interface {
	ListUserChannels(ctx context.Context, userID int64) ([]db.ListUserChannelsRow, error)
	ListEnabledChannels(ctx context.Context) ([]db.ListEnabledChannelsRow, error)
	ListGuidePrograms(ctx context.Context, arg db.ListGuideProgramsParams) ([]db.ListGuideProgramsRow, error)
	GetChannelAccess(ctx context.Context, arg db.GetChannelAccessParams) (db.GetChannelAccessRow, error)
}

// Handlers serves the customer-facing endpoints. Every URL they emit is
// built from BaseURL: customers never see a provider URL.
type Handlers struct {
	Access  *Access
	Store   Store
	Streams StreamAttacher
	HLS     *HLS      // nil: .m3u8 stream URLs are not served
	VOD     VODSource // nil: movie/series actions answer empty
	// Slots enforces provider connection limits for VOD playback (shared
	// with live sessions); nil = unenforced (tests).
	Slots *proxy.Capacity
	// VODClient fetches movies/episodes; nil = a default client.
	VODClient *http.Client
	vodLeases vodLeases
	BaseURL   string // no trailing slash
	Log       *slog.Logger
	Now       func() time.Time // guide window clock; nil = time.Now
	// Limit locks out repeated password failures (nil = no limiting).
	Limit *auth.Limiter
	// Activity records where customers authenticate from and their wrong
	// passwords (nil = not recorded).
	Activity *activity.Tracker
	// NameGuide gives the stream-name guide settings for channels without
	// a guide (nil = no stream-name guides).
	NameGuide func(ctx context.Context) nameguide.Settings
}

// Mount registers the customer routes on the root router.
func (h *Handlers) Mount(r chi.Router) {
	r.Get("/get.php", h.getPHP)
	r.Get("/playlist/{token}.m3u", h.tokenPlaylist)
	r.Get("/xmltv.php", h.xmltvPHP)
	r.Get("/epg/{token}.xml", h.tokenGuide)
	r.Get("/player_api.php", h.playerAPI)
	r.Post("/player_api.php", h.playerAPI)
	r.Get("/live/{user}/{pass}/{id}", h.liveXtream)
	r.Get("/t/{token}/{id}", h.liveToken)
	if h.HLS != nil {
		h.HLS.Mount(r)
	}
	h.mountVOD(r)
}

// Channel is one playable channel as customers see it.
type Channel struct {
	ID         int64
	Number     *int32
	Name       string
	TvgID      string
	Logo       string
	GroupID    int64
	GroupName  string
	GroupSort  int32
	Added      time.Time
	StreamName string // its first failover stream's name, for stream-name guides
	GuideMode  string // where its guide comes from: GuideAuto, GuideXMLTV or GuideName
}

// Guide modes (Channel.GuideMode).
const (
	GuideAuto  = "auto"  // the XMLTV guide, else the stream-name guide
	GuideXMLTV = "xmltv" // the XMLTV guide only
	GuideName  = "name"  // the stream-name guide, else the XMLTV guide
)

// GuideID links the channel to its guide in playlists and the XMLTV:
// its tvg-id, or one made from its id when it has none (its guide then
// comes from the stream name).
func (c Channel) GuideID() string {
	if id := strings.TrimSpace(c.TvgID); id != "" {
		return id
	}
	return "mmp." + strconv.FormatInt(c.ID, 10)
}

// channelsFor lists what u may watch: admins see every enabled channel,
// customers the enabled channels of their bouquets (none if no bouquets).
func (h *Handlers) channelsFor(ctx context.Context, u db.User) ([]Channel, error) {
	var out []Channel
	if u.Role == auth.RoleAdmin {
		rows, err := h.Store.ListEnabledChannels(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, Channel{r.ID, r.Number, r.Name, r.TvgID, r.LogoUrl, r.GroupID, r.GroupName, r.GroupSort, r.CreatedAt, r.StreamName, r.GuideMode})
		}
		return out, nil
	}
	rows, err := h.Store.ListUserChannels(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, Channel{r.ID, r.Number, r.Name, r.TvgID, r.LogoUrl, r.GroupID, r.GroupName, r.GroupSort, r.CreatedAt, r.StreamName, r.GuideMode})
	}
	return out, nil
}

// authFail answers a failed customer auth. Players expect a bare 401.
func (h *Handlers) authFail(w http.ResponseWriter, err error) {
	var locked *auth.LockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	if !errors.Is(err, auth.ErrUnauthorized) {
		h.Log.Error("output: auth lookup", "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authPassword authenticates a password-style request with failure lockout
// (per client network and per username; see auth.Limiter). Locked-out attempts return a
// *auth.LockedError; the caller picks the wire format its players expect.
func (h *Handlers) authPassword(r *http.Request, user, pass string) (db.User, error) {
	attempt := auth.NewAttempt(r, user)
	if ok, retry := h.Limit.Allow(attempt); !ok {
		return db.User{}, &auth.LockedError{RetryAfter: retry}
	}
	u, err := h.Access.Customers.ByPassword(r.Context(), user, pass)
	switch { // only password verdicts count; infrastructure errors do not
	case err == nil:
		h.Limit.Done(attempt, true)
		h.seen(r, u)
	case errors.Is(err, auth.ErrUnauthorized):
		h.Limit.Done(attempt, false)
		h.Activity.AuthFailed(user, auth.RemoteIP(r))
	}
	return u, err
}

// authToken authenticates a stream token.
func (h *Handlers) authToken(r *http.Request, token string) (db.User, error) {
	u, err := h.Access.Customers.ByToken(r.Context(), token)
	if err == nil {
		h.seen(r, u)
	}
	return u, err
}

// seen records where u authenticated from.
func (h *Handlers) seen(r *http.Request, u db.User) {
	h.Activity.Seen(u.ID, auth.RemoteIP(r), auth.ClientNetwork(r))
}

// xtreamStreamURL is /live/<user>/<pass>/<id>.<ext> on MMP.
func (h *Handlers) xtreamStreamURL(user, pass, ext string) func(int64) string {
	prefix := h.BaseURL + "/live/" + url.PathEscape(user) + "/" + url.PathEscape(pass) + "/"
	return func(id int64) string { return prefix + strconv.FormatInt(id, 10) + ext }
}

// streamExt is the container for get.php's output= parameter: .m3u8 for
// output=m3u8 (or hls) when HLS is enabled, else .ts.
func (h *Handlers) streamExt(output string) string {
	if h.HLS != nil && (output == "m3u8" || output == "hls") {
		return ".m3u8"
	}
	return ".ts"
}

// tokenStreamURL is /t/<token>/<id>.ts on MMP: no password in the playlist.
func (h *Handlers) tokenStreamURL(token string) func(int64) string {
	prefix := h.BaseURL + "/t/" + url.PathEscape(token) + "/"
	return func(id int64) string { return prefix + strconv.FormatInt(id, 10) + ".ts" }
}

// getPHP is the Xtream playlist: /get.php?username=&password=&type=m3u_plus.
func (h *Handlers) getPHP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	user, pass := q.Get("username"), q.Get("password")
	u, err := h.authPassword(r, user, pass)
	if err != nil {
		h.authFail(w, err)
		return
	}
	plus := q.Get("type") != "m3u" // m3u_plus is the default
	epg := h.BaseURL + "/xmltv.php?username=" + url.QueryEscape(user) + "&password=" + url.QueryEscape(pass)
	vodBase := h.BaseURL + "/"
	vod := h.playlistVOD(r, vodBase)
	if vod != nil { // Xtream form: /movie/<user>/<pass>/<id>.<ext>
		up := url.PathEscape(user) + "/" + url.PathEscape(pass) + "/"
		vod.movie = func(id int64, ext string) string {
			return vodBase + "movie/" + up + strconv.FormatInt(id, 10) + "." + ext
		}
		vod.episode = func(id int64, ext string) string {
			return vodBase + "series/" + up + strconv.FormatInt(id, 10) + "." + ext
		}
	}
	h.servePlaylist(w, r, u, plus, h.xtreamStreamURL(user, pass, h.streamExt(q.Get("output"))), epg, vod)
}

// tokenPlaylist is /playlist/{token}.m3u (always m3u_plus).
func (h *Handlers) tokenPlaylist(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	u, err := h.authToken(r, token)
	if err != nil {
		h.authFail(w, err)
		return
	}
	epg := h.BaseURL + "/epg/" + url.PathEscape(token) + ".xml"
	h.servePlaylist(w, r, u, true, h.tokenStreamURL(token), epg, h.playlistVOD(r, h.BaseURL+"/t/"+url.PathEscape(token)+"/"))
}

// vodURLs builds movie and episode URLs for a playlist (Xtream or token
// form); a nil vodURLs means the playlist has no VOD entries.
type vodURLs struct {
	movies, series bool
	movie, episode func(id int64, ext string) string
}

// playlistVOD reads the opt-in flags: &vod=1 adds movies, &series=1 adds
// episodes. Most M3U players expect live channels only, hence off.
func (h *Handlers) playlistVOD(r *http.Request, base string) *vodURLs {
	q := r.URL.Query()
	v := &vodURLs{movies: q.Get("vod") == "1", series: q.Get("series") == "1"}
	if h.VOD == nil || (!v.movies && !v.series) {
		return nil
	}
	v.movie = func(id int64, ext string) string { return base + "movie/" + strconv.FormatInt(id, 10) + "." + ext }
	v.episode = func(id int64, ext string) string { return base + "series/" + strconv.FormatInt(id, 10) + "." + ext }
	return v
}

func (h *Handlers) servePlaylist(w http.ResponseWriter, r *http.Request, u db.User, plus bool, streamURL func(int64) string, epgURL string, vod *vodURLs) {
	chans, err := h.channelsFor(r.Context(), u)
	if err != nil {
		h.Log.Error("output: list channels", "user_id", u.ID, "err", err)
		http.Error(w, "playlist unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="playlist.m3u"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := WriteM3U(w, chans, plus, streamURL, epgURL); err != nil {
		h.Log.Warn("output: write playlist", "user_id", u.ID, "err", err)
		return
	}
	if vod != nil {
		if err := h.writeVODEntries(r.Context(), w, u, plus, vod); err != nil {
			h.Log.Warn("output: write playlist vod", "user_id", u.ID, "err", err)
		}
	}
}

// writeVODEntries appends movies (group "Movies: <category>") and cached
// episodes (group "Series: <show>") after the channels.
func (h *Handlers) writeVODEntries(ctx context.Context, w io.Writer, u db.User, plus bool, v *vodURLs) error {
	admin := u.Role == auth.RoleAdmin
	bw := bufio.NewWriter(w)
	entry := func(name, logo, group, url string) {
		if plus {
			fmt.Fprintf(bw, "#EXTINF:-1 tvg-name=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n", attr(name), attr(logo), attr(group), line(name))
		} else {
			fmt.Fprintf(bw, "#EXTINF:-1,%s\n", line(name))
		}
		bw.WriteString(url)
		bw.WriteByte('\n')
	}
	if v.movies {
		cats, err := h.VOD.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: "movie", UserID: u.ID, IsAdmin: admin})
		if err != nil {
			return err
		}
		catName := map[int64]string{}
		for _, c := range cats {
			catName[c.ID] = c.Name
		}
		movies, err := h.VOD.ListUserVODItems(ctx, db.ListUserVODItemsParams{UserID: u.ID, IsAdmin: admin})
		if err != nil {
			return err
		}
		for _, m := range movies {
			entry(m.Name, m.Icon, "Movies: "+catName[m.CategoryID], v.movie(m.ID, m.ContainerExtension))
		}
	}
	if v.series {
		eps, err := h.VOD.ListUserCachedEpisodes(ctx, db.ListUserCachedEpisodesParams{UserID: u.ID, IsAdmin: admin})
		if err != nil {
			return err
		}
		for _, e := range eps {
			name := fmt.Sprintf("%s S%02dE%02d", e.SeriesName, e.Season, e.EpisodeNum)
			if e.Title != "" {
				name += " " + e.Title
			}
			entry(name, e.Cover, "Series: "+e.SeriesName, v.episode(e.ID, e.ContainerExtension))
		}
	}
	return bw.Flush()
}

// WriteM3U renders an extended M3U. plus adds tvg-* and group-title
// attributes (m3u_plus); plain m3u carries only the name.
func WriteM3U(w io.Writer, chans []Channel, plus bool, streamURL func(int64) string, epgURL string) error {
	bw := bufio.NewWriter(w)
	if plus && epgURL != "" {
		fmt.Fprintf(bw, "#EXTM3U x-tvg-url=\"%s\" url-tvg=\"%s\"\n", attr(epgURL), attr(epgURL))
	} else {
		bw.WriteString("#EXTM3U\n")
	}
	for _, c := range chans {
		name := line(c.Name)
		if plus {
			chno := ""
			if c.Number != nil {
				chno = strconv.Itoa(int(*c.Number))
			}
			fmt.Fprintf(bw, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" tvg-logo=\"%s\" tvg-chno=\"%s\" group-title=\"%s\",%s\n",
				attr(c.GuideID()), attr(c.Name), attr(c.Logo), chno, attr(c.GroupName), name)
		} else {
			fmt.Fprintf(bw, "#EXTINF:-1,%s\n", name)
		}
		bw.WriteString(streamURL(c.ID))
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// line strips characters that would start a new playlist line.
func line(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// attr makes s safe inside a double-quoted M3U attribute.
func attr(s string) string {
	return strings.NewReplacer("\"", "'", "\r", " ", "\n", " ").Replace(s)
}
