package web

import (
	"context"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// A user's activity, on their page: where they connect from, what they
// watched and wrong passwords. Kept as long as events (Settings).

const (
	activityNetworks = 20
	activityWatched  = 50
	activityFailures = 20
	// sharedNetworks networks active within a day suggest a shared login.
	sharedNetworks = 3
)

type userActivity struct {
	LastSeen   *time.Time
	LastIP     string
	Networks   []networkRow
	ActiveDay  int // networks seen within the last 24 hours
	Shared     bool
	Watched    []watchRow
	Failures   []failureRow
	FailedDay  int // wrong passwords within the last 24 hours
	FailedMore bool
}

type networkRow struct {
	Network, LastIP string
	FirstSeen       string
	LastSeen        *time.Time
	Today           bool // seen within the last 24 hours
}

type watchRow struct {
	ChannelID       int64
	Channel         string // "" when the channel was deleted
	Started, Length string
	IP, Output      string
}

type failureRow struct {
	When, IP string
}

func (ui *UI) userActivity(ctx context.Context, userID int64) (userActivity, error) {
	var a userActivity
	now := time.Now()
	nets, err := ui.Store.UserNetworks(ctx, db.UserNetworksParams{UserID: userID, Limit: activityNetworks})
	if err != nil {
		return a, err
	}
	for i, n := range nets {
		row := networkRow{Network: n.Network, LastIP: n.LastIp, FirstSeen: n.FirstSeen.Local().Format("Jan 2 15:04"),
			LastSeen: &nets[i].LastSeen, Today: now.Sub(n.LastSeen) < 24*time.Hour}
		if row.Today {
			a.ActiveDay++
		}
		a.Networks = append(a.Networks, row)
	}
	if len(nets) > 0 { // newest first
		a.LastSeen, a.LastIP = &nets[0].LastSeen, nets[0].LastIp
	}
	a.Shared = a.ActiveDay >= sharedNetworks

	watched, err := ui.Store.UserViewingSessions(ctx, db.UserViewingSessionsParams{UserID: userID, Limit: activityWatched})
	if err != nil {
		return a, err
	}
	for _, v := range watched {
		a.Watched = append(a.Watched, watchRow{ChannelID: v.ChannelID, Channel: v.ChannelName,
			Started: v.StartedAt.Local().Format("Jan 2 15:04"), Length: shortDuration(v.EndedAt.Sub(v.StartedAt)),
			IP: v.Ip, Output: v.Output})
	}

	fails, err := ui.Store.UserAuthFailures(ctx, db.UserAuthFailuresParams{UserID: &userID, Limit: activityFailures + 1})
	if err != nil {
		return a, err
	}
	if len(fails) > activityFailures {
		fails, a.FailedMore = fails[:activityFailures], true
	}
	for _, f := range fails {
		if now.Sub(f.Ts) < 24*time.Hour {
			a.FailedDay++
		}
		a.Failures = append(a.Failures, failureRow{When: f.Ts.Local().Format("Jan 2 15:04:05"), IP: f.Ip})
	}
	return a, nil
}
