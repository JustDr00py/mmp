package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// MountChannels registers channel, group, bouquet, and priority routes.
func MountChannels(r chi.Router, env *Env) {
	store := env.Store
	r.Route("/channel-groups", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			groups, err := store.ListChannelGroups(r.Context())
			if err != nil {
				env.Log.Error("api: list groups", "err", err)
				writeErr(w, http.StatusInternalServerError, "list groups failed")
				return
			}
			writeJSON(w, http.StatusOK, groups)
		})
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Name      string `json:"name"`
				SortOrder int32  `json:"sort_order"`
			}
			if err := readJSON(w, r, &in); err != nil || in.Name == "" {
				writeErr(w, http.StatusBadRequest, "name is required")
				return
			}
			g, err := store.CreateChannelGroup(r.Context(), db.CreateChannelGroupParams{Name: in.Name, SortOrder: in.SortOrder})
			if err != nil {
				env.Log.Error("api: create group", "err", err)
				writeErr(w, http.StatusInternalServerError, "create group failed")
				return
			}
			writeJSON(w, http.StatusCreated, g)
		})
		r.Route("/{id}", func(r chi.Router) {
			r.Put("/", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				var in struct {
					Name      string `json:"name"`
					SortOrder int32  `json:"sort_order"`
				}
				if err := readJSON(w, r, &in); err != nil || in.Name == "" {
					writeErr(w, http.StatusBadRequest, "name is required")
					return
				}
				g, err := store.UpdateChannelGroup(r.Context(), db.UpdateChannelGroupParams{ID: id, Name: in.Name, SortOrder: in.SortOrder})
				if isNotFound(err) {
					writeErr(w, http.StatusNotFound, "group not found")
					return
				}
				if err != nil {
					env.Log.Error("api: update group", "err", err)
					writeErr(w, http.StatusInternalServerError, "update group failed")
					return
				}
				writeJSON(w, http.StatusOK, g)
			})
			r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				if err := store.DeleteChannelGroup(r.Context(), id); err != nil {
					env.Log.Error("api: delete group", "err", err)
					writeErr(w, http.StatusInternalServerError, "delete group failed")
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
		})
	})

	r.Route("/channels", func(r chi.Router) {
		r.Get("/", env.listChannels)
		r.Post("/", env.createChannel)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", env.getChannel)
			r.Put("/", env.updateChannel)
			r.Delete("/", env.deleteChannel)
			r.Get("/stream", env.streamChannel)
			r.Get("/streams", env.listChannelStreams)
			r.Post("/streams", env.addChannelStream)
			r.Put("/streams/order", env.reorderChannelStreams)
			r.Delete("/streams/{psid}", env.removeChannelStream)
		})
	})

	r.Route("/bouquets", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			bs, err := store.ListBouquets(r.Context())
			if err != nil {
				env.Log.Error("api: list bouquets", "err", err)
				writeErr(w, http.StatusInternalServerError, "list bouquets failed")
				return
			}
			writeJSON(w, http.StatusOK, bs)
		})
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Name      string `json:"name"`
				SortOrder int32  `json:"sort_order"`
			}
			if err := readJSON(w, r, &in); err != nil || in.Name == "" {
				writeErr(w, http.StatusBadRequest, "name is required")
				return
			}
			b, err := store.CreateBouquet(r.Context(), db.CreateBouquetParams{Name: in.Name, SortOrder: in.SortOrder})
			if err != nil {
				env.Log.Error("api: create bouquet", "err", err)
				writeErr(w, http.StatusInternalServerError, "create bouquet failed")
				return
			}
			writeJSON(w, http.StatusCreated, b)
		})
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				b, err := store.GetBouquet(r.Context(), id)
				if isNotFound(err) {
					writeErr(w, http.StatusNotFound, "bouquet not found")
					return
				}
				if err != nil {
					env.Log.Error("api: get bouquet", "err", err)
					writeErr(w, http.StatusInternalServerError, "get bouquet failed")
					return
				}
				writeJSON(w, http.StatusOK, b)
			})
			r.Put("/", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				var in struct {
					Name      string `json:"name"`
					SortOrder int32  `json:"sort_order"`
				}
				if err := readJSON(w, r, &in); err != nil || in.Name == "" {
					writeErr(w, http.StatusBadRequest, "name is required")
					return
				}
				b, err := store.UpdateBouquet(r.Context(), db.UpdateBouquetParams{ID: id, Name: in.Name, SortOrder: in.SortOrder})
				if isNotFound(err) {
					writeErr(w, http.StatusNotFound, "bouquet not found")
					return
				}
				if err != nil {
					env.Log.Error("api: update bouquet", "err", err)
					writeErr(w, http.StatusInternalServerError, "update bouquet failed")
					return
				}
				writeJSON(w, http.StatusOK, b)
			})
			r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				if err := store.DeleteBouquet(r.Context(), id); err != nil {
					env.Log.Error("api: delete bouquet", "err", err)
					writeErr(w, http.StatusInternalServerError, "delete bouquet failed")
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			r.Get("/channels", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				cs, err := store.ListBouquetChannels(r.Context(), id)
				if err != nil {
					env.Log.Error("api: list bouquet channels", "err", err)
					writeErr(w, http.StatusInternalServerError, "list bouquet channels failed")
					return
				}
				writeJSON(w, http.StatusOK, cs)
			})
			r.Post("/channels", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				var in struct {
					ChannelID int64 `json:"channel_id"`
				}
				if err := readJSON(w, r, &in); err != nil || in.ChannelID <= 0 {
					writeErr(w, http.StatusBadRequest, "channel_id is required")
					return
				}
				if err := store.AddChannelToBouquet(r.Context(), db.AddChannelToBouquetParams{BouquetID: id, ChannelID: in.ChannelID}); err != nil {
					env.Log.Error("api: add bouquet channel", "err", err)
					writeErr(w, http.StatusInternalServerError, "add channel failed")
					return
				}
				writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
			})
			r.Delete("/channels/{cid}", func(w http.ResponseWriter, r *http.Request) {
				id, err := pathID(r)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
				if err != nil || cid <= 0 {
					writeErr(w, http.StatusBadRequest, "invalid channel id")
					return
				}
				if err := store.RemoveChannelFromBouquet(r.Context(), db.RemoveChannelFromBouquetParams{BouquetID: id, ChannelID: cid}); err != nil {
					env.Log.Error("api: remove bouquet channel", "err", err)
					writeErr(w, http.StatusInternalServerError, "remove channel failed")
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
		})
	})
}

func (env *Env) listChannels(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	q := r.URL.Query()
	params := db.ListChannelsParams{
		Limit:  int32(clampInt(q.Get("limit"), 100, 1, 1000)),
		Offset: int32(clampInt(q.Get("offset"), 0, 0, 1<<30)),
	}
	if g := q.Get("group_id"); g != "" {
		v, err := strconv.ParseInt(g, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid group_id")
			return
		}
		params.GroupID = &v
	}
	if s := q.Get("q"); s != "" {
		v := "%" + s + "%"
		params.Search = &v
	}
	channels, err := store.ListChannels(r.Context(), params)
	if err != nil {
		env.Log.Error("api: list channels", "err", err)
		writeErr(w, http.StatusInternalServerError, "list channels failed")
		return
	}
	writeJSON(w, http.StatusOK, channels)
}

type channelInput struct {
	Number          *int32 `json:"number"`
	Name            string `json:"name"`
	TvgID           string `json:"tvg_id"`
	LogoURL         string `json:"logo_url"`
	GroupID         *int64 `json:"group_id"`
	StreamProfileID *int64 `json:"stream_profile_id"`
	Enabled         *bool  `json:"enabled"`
}

func (in *channelInput) params() db.CreateChannelParams {
	return db.CreateChannelParams{
		Number: in.Number, Name: in.Name, TvgID: in.TvgID, LogoUrl: in.LogoURL,
		GroupID: in.GroupID, StreamProfileID: in.StreamProfileID,
	}
}

func (env *Env) createChannel(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	var in channelInput
	if err := readJSON(w, r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	params := in.params()
	if params.Number == nil {
		n, err := store.NextChannelNumber(r.Context())
		if err == nil {
			params.Number = &n
		}
	}
	c, err := store.CreateChannel(r.Context(), params)
	if pgCode(err) == pgForeignKeyViolation {
		writeErr(w, http.StatusBadRequest, "unknown group_id or stream_profile_id")
		return
	}
	if err != nil {
		env.Log.Error("api: create channel", "err", err)
		writeErr(w, http.StatusInternalServerError, "create channel failed")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (env *Env) getChannel(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := store.GetChannel(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		env.Log.Error("api: get channel", "err", err)
		writeErr(w, http.StatusInternalServerError, "get channel failed")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (env *Env) updateChannel(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in channelInput
	if err := readJSON(w, r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	c, err := store.UpdateChannel(r.Context(), db.UpdateChannelParams{
		ID: id, Number: in.Number, Name: in.Name, TvgID: in.TvgID, LogoUrl: in.LogoURL,
		GroupID: in.GroupID, StreamProfileID: in.StreamProfileID, Enabled: enabled,
	})
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if pgCode(err) == pgForeignKeyViolation {
		writeErr(w, http.StatusBadRequest, "unknown group_id or stream_profile_id")
		return
	}
	if err != nil {
		env.Log.Error("api: update channel", "err", err)
		writeErr(w, http.StatusInternalServerError, "update channel failed")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (env *Env) deleteChannel(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := store.DeleteChannel(r.Context(), id); err != nil {
		env.Log.Error("api: delete channel", "err", err)
		writeErr(w, http.StatusInternalServerError, "delete channel failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (env *Env) listChannelStreams(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	streams, err := store.ListChannelStreams(r.Context(), id)
	if err != nil {
		env.Log.Error("api: list channel streams", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "list channel streams failed")
		return
	}
	writeJSON(w, http.StatusOK, streams)
}

func (env *Env) addChannelStream(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		ProviderStreamID int64 `json:"provider_stream_id"`
		Priority         int32 `json:"priority"`
	}
	if err := readJSON(w, r, &in); err != nil || in.ProviderStreamID <= 0 {
		writeErr(w, http.StatusBadRequest, "provider_stream_id is required")
		return
	}
	// Default priority: append after the current last entry.
	if in.Priority == 0 {
		current, err := store.ListChannelStreams(r.Context(), id)
		if err == nil && len(current) > 0 {
			in.Priority = current[len(current)-1].Priority + 1
		}
	}
	if err := store.AddStreamToChannel(r.Context(), db.AddStreamToChannelParams{
		ChannelID: id, ProviderStreamID: in.ProviderStreamID, Priority: in.Priority,
	}); err != nil {
		env.Log.Error("api: add channel stream", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "add stream failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (env *Env) removeChannelStream(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	psid, err := strconv.ParseInt(chi.URLParam(r, "psid"), 10, 64)
	if err != nil || psid <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid provider stream id")
		return
	}
	if err := store.RemoveStreamFromChannel(r.Context(), db.RemoveStreamFromChannelParams{
		ChannelID: id, ProviderStreamID: psid,
	}); err != nil {
		env.Log.Error("api: remove channel stream", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "remove stream failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (env *Env) reorderChannelStreams(w http.ResponseWriter, r *http.Request) {
	store := env.Store
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		ProviderStreamIDs []int64 `json:"provider_stream_ids"`
	}
	if err := readJSON(w, r, &in); err != nil || len(in.ProviderStreamIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "provider_stream_ids is required")
		return
	}
	if err := store.ReorderChannelStreams(r.Context(), db.ReorderChannelStreamsParams{
		ChannelID: id, StreamIds: in.ProviderStreamIDs,
	}); err != nil {
		env.Log.Error("api: reorder channel streams", "channel_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "reorder failed")
		return
	}
	streams, err := store.ListChannelStreams(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reordered but listing failed")
		return
	}
	writeJSON(w, http.StatusOK, streams)
}
