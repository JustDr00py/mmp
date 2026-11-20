package web

import (
	"context"
	"net/http"
	"time"

	"github.com/JustDr00py/mmp/internal/health"
)

// channelPlay judges how each source in the channels' failover lists played
// for viewers, keyed by channel. Channels with no play history are left out.
func (ui *UI) channelPlay(ctx context.Context, channelIDs []int64) (map[int64]health.ChannelPlay, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	now := time.Now()
	buckets, err := ui.outcomeBuckets(ctx, now)
	if err != nil {
		return nil, err
	}
	rows, err := ui.Store.ListChannelStreamSources(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	lists := map[int64][]health.ChannelStream{}
	for _, r := range rows {
		lists[r.ChannelID] = append(lists[r.ChannelID],
			health.ChannelStream{StreamID: r.ProviderStreamID, ProviderID: r.ProviderID, Backbone: r.Backbone})
	}
	out := map[int64]health.ChannelPlay{}
	for id, list := range lists {
		if p := health.ChannelPlayHealth(buckets, list, now); len(p.Sources) > 0 {
			out[id] = p
		}
	}
	return out, nil
}

// channelApplyPlayOrder saves the suggested failover order, worked out
// afresh so it matches the list as it is now.
func (ui *UI) channelApplyPlayOrder(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	play, err := ui.channelPlay(r.Context(), []int64{c.ID})
	if err != nil {
		ui.fail(w, "channel play health", err)
		return
	}
	if order := play[c.ID].Order; order != nil {
		if err := ui.saveOrder(r, c.ID, order); err != nil {
			ui.fail(w, "reorder", err)
			return
		}
	}
	ui.renderStreamList(w, r, c.ID)
}
