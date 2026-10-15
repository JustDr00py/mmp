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

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
)

// Store is what the customer outputs read (satisfied by *db.Queries).
type Store interface {
	ListUserChannels(ctx context.Context, userID int64) ([]db.ListUserChannelsRow, error)
	ListEnabledChannels(ctx context.Context) ([]db.ListEnabledChannelsRow, error)
	ListGuideProgrammes(ctx context.Context, arg db.ListGuideProgrammesParams) ([]db.ListGuideProgrammesRow, error)
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
	BaseURL string    // no trailing slash
	Log     *slog.Logger
	Now     func() time.Time // guide window clock; nil = time.Now
	// Limit locks out repeated password failures (nil = no limiting).
	Limit *auth.Limiter
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
}

// Channel is one playable channel as customers see it.
type Channel struct {
	ID        int64
	Number    *int32
	Name      string
	TvgID     string
	Logo      string
	GroupID   int64
	GroupName string
	GroupSort int32
	Added     time.Time
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
			out = append(out, Channel{r.ID, r.Number, r.Name, r.TvgID, r.LogoUrl, r.GroupID, r.GroupName, r.GroupSort, r.CreatedAt})
		}
		return out, nil
	}
	rows, err := h.Store.ListUserChannels(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, Channel{r.ID, r.Number, r.Name, r.TvgID, r.LogoUrl, r.GroupID, r.GroupName, r.GroupSort, r.CreatedAt})
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
// (per client IP and per username). Locked-out attempts return a
// *auth.LockedError; the caller picks the wire format its players expect.
func (h *Handlers) authPassword(r *http.Request, user, pass string) (db.User, error) {
	keys := auth.AttemptKeys(r, user)
	if ok, retry := h.Limit.Allowed(keys...); !ok {
		return db.User{}, &auth.LockedError{RetryAfter: retry}
	}
	u, err := h.Access.Customers.ByPassword(r.Context(), user, pass)
	switch { // only password verdicts count; infrastructure errors do not
	case err == nil:
		h.Limit.Record(true, keys...)
	case errors.Is(err, auth.ErrUnauthorized):
		h.Limit.Record(false, keys...)
	}
	return u, err
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
	h.servePlaylist(w, r, u, plus, h.xtreamStreamURL(user, pass, h.streamExt(q.Get("output"))), epg)
}

// tokenPlaylist is /playlist/{token}.m3u (always m3u_plus).
func (h *Handlers) tokenPlaylist(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	u, err := h.Access.Customers.ByToken(r.Context(), token)
	if err != nil {
		h.authFail(w, err)
		return
	}
	epg := h.BaseURL + "/epg/" + url.PathEscape(token) + ".xml"
	h.servePlaylist(w, r, u, true, h.tokenStreamURL(token), epg)
}

func (h *Handlers) servePlaylist(w http.ResponseWriter, r *http.Request, u db.User, plus bool, streamURL func(int64) string, epgURL string) {
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
	}
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
				attr(c.TvgID), attr(c.Name), attr(c.Logo), chno, attr(c.GroupName), name)
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
