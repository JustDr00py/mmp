package api

import (
	"errors"
	"net/http"

	"github.com/sysadmin/mmp/internal/proxy"
)

// StreamAttacher joins viewers to channel sessions (satisfied by
// *proxy.Manager).
type StreamAttacher interface {
	Attach(channelID int64) (*proxy.Viewer, error)
}

// streamChannel plays a channel through the proxy as MPEG-TS. It is the
// admin's playback test; customers get their own endpoints in M5.
func (env *Env) streamChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := env.Store.GetChannel(r.Context(), id); err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, "channel not found")
			return
		}
		env.Log.Error("api: stream: get channel", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "stream failed")
		return
	}
	streams, err := env.Store.ListChannelStreams(r.Context(), id)
	if err != nil {
		env.Log.Error("api: stream: list streams", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "stream failed")
		return
	}
	if len(streams) == 0 {
		writeErr(w, http.StatusConflict, "channel has no streams")
		return
	}

	v, err := env.Streams.Attach(id)
	if err != nil {
		if errors.Is(err, proxy.ErrManagerClosed) {
			writeErr(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		env.Log.Error("api: stream: attach", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "stream failed")
		return
	}
	defer v.Close()

	proxy.ServeViewer(w, r, v)
}
