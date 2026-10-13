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
	u, err := h.Access.Customers.ByPassword(r.Context(), chi.URLParam(r, "user"), chi.URLParam(r, "pass"))
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

// streamID parses "{id}" or "{id}.ts". Other containers (.m3u8) are not
// served yet.
func streamID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSuffix(s, ".ts"), 10, 64)
	return id, err == nil && id > 0
}

// serveLive plays a channel u may watch: channels outside u's bouquets are
// reported as missing, a full connection allowance is 403, and the viewer
// joins the channel's shared upstream session.
func (h *Handlers) serveLive(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := streamID(chi.URLParam(r, "id"))
	if !ok {
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
