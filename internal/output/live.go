package output

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
)

// StreamAttacher joins viewers to shared channel sessions (satisfied by
// *proxy.Manager).
type StreamAttacher interface {
	Attach(channelID int64) (*proxy.Viewer, error)
}

// liveXtream is /live/{user}/{pass}/{id}.ts, the Xtream stream URL.
func (h *Handlers) liveXtream(w http.ResponseWriter, r *http.Request) {
	u, err := h.authPassword(r, chi.URLParam(r, "user"), chi.URLParam(r, "pass"))
	if err != nil {
		h.authFail(w, err)
		return
	}
	h.serveLive(w, r, u)
}

// liveToken is /t/{token}/{id}.ts, the password-free stream URL.
func (h *Handlers) liveToken(w http.ResponseWriter, r *http.Request) {
	u, err := h.Access.Customers.ByToken(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.authFail(w, err)
		return
	}
	h.serveLive(w, r, u)
}

// streamID parses "{id}", "{id}.ts" or "{id}.m3u8".
func streamID(s string) (int64, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(s, ".ts"), ".m3u8")
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// serveLive plays a channel u may watch: channels outside u's bouquets are
// reported as missing, a full connection allowance is 403, and the viewer
// joins the channel's shared upstream session.
func (h *Handlers) serveLive(w http.ResponseWriter, r *http.Request, u db.User) {
	raw := chi.URLParam(r, "id")
	id, ok := streamID(raw)
	hls := isHLSRequest(raw)
	if !ok || (hls && h.HLS == nil) {
		http.NotFound(w, r)
		return
	}
	acc, err := h.Store.GetChannelAccess(r.Context(), db.GetChannelAccessParams{
		ChannelID: id, UserID: u.ID, IsAdmin: u.Role == auth.RoleAdmin,
	})
	if err != nil {
		h.Log.Error("output: channel access", "user_id", u.ID, "channel_id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !acc.Allowed {
		http.NotFound(w, r)
		return
	}
	if acc.Streams == 0 {
		http.Error(w, "channel unavailable", http.StatusServiceUnavailable)
		return
	}

	if hls {
		h.startHLS(w, r, u, id)
		return
	}

	slot, err := h.Access.Acquire(u)
	if err != nil {
		h.Log.Info("output: connection limit", "user_id", u.ID, "channel_id", id, "max", u.MaxConnections)
		http.Error(w, "connection limit reached", http.StatusForbidden)
		return
	}
	defer slot.Release()

	v, err := h.Streams.Attach(id)
	if err != nil {
		if !errors.Is(err, proxy.ErrManagerClosed) {
			h.Log.Error("output: attach", "user_id", u.ID, "channel_id", id, "err", err)
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer v.Close()

	h.Log.Info("viewer started", "user_id", u.ID, "channel_id", id)
	proxy.ServeViewer(w, r, v)
	h.Log.Info("viewer stopped", "user_id", u.ID, "channel_id", id)
}

// startHLS leases the channel for u and redirects to the lease's playlist.
func (h *Handlers) startHLS(w http.ResponseWriter, r *http.Request, u db.User, id int64) {
	key, err := h.HLS.lease(u, id)
	switch {
	case errors.Is(err, ErrTooManyConnections):
		h.Log.Info("output: connection limit", "user_id", u.ID, "channel_id", id, "max", u.MaxConnections)
		http.Error(w, "connection limit reached", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	h.Log.Info("viewer started", "user_id", u.ID, "channel_id", id, "output", "hls")
	allowCORS(w) // hls.js follows the redirect over XHR
	http.Redirect(w, r, h.BaseURL+"/hls/"+key+"/index.m3u8", http.StatusFound)
}
