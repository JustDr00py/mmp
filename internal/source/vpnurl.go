package source

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// behindVPN: MMP runs behind gluetun (gluetun.url is set), so providers
// are reached at their VPN address where they have one.
var behindVPN atomic.Bool

// SetBehindVPN says whether MMP's traffic goes through the VPN (set once
// at start).
func SetBehindVPN(on bool) { behindVPN.Store(on) }

// BehindVPN reports what SetBehindVPN set.
func BehindVPN() bool { return behindVPN.Load() }

// ProviderURL is the address MMP reaches p at: its VPN URL behind the VPN
// (some providers give VPN users their own host and refuse VPN addresses
// on the regular one), else its URL.
func ProviderURL(p db.Provider) string {
	if p.VpnUrl != "" && behindVPN.Load() {
		return p.VpnUrl
	}
	return p.Url
}

// vpnModeKey stores whether the last run was behind the VPN.
const vpnModeKey = "providers.behind_vpn"

// RefreshOnVPNChange refreshes the providers with a VPN URL when MMP now
// runs behind the VPN and last time did not, or the other way round (the
// VPN compose file swapped for the plain one): their stored stream
// addresses came from the other URL. Run it once at start.
func (r *Refresher) RefreshOnVPNChange(ctx context.Context) {
	q := db.New(r.pool)
	now := []byte(strconv.FormatBool(BehindVPN()))
	last, err := q.GetSetting(ctx, vpnModeKey)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.log.Error("refresher: vpn mode", "err", err)
		return
	}
	if bytes.Equal(last, now) {
		return
	}
	// Recorded first: a failed refresh is retried by the schedule anyway.
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: vpnModeKey, Value: now}); err != nil {
		r.log.Error("refresher: vpn mode", "err", err)
		return
	}
	if last == nil && !BehindVPN() { // first start, no VPN: nothing came from a VPN URL
		return
	}
	providers, err := q.ListProvidersWithVPNURL(ctx)
	if err != nil {
		r.log.Error("refresher: providers with a vpn url", "err", err)
		return
	}
	for _, p := range providers {
		r.log.Info("refresher: VPN on or off since the last start; refreshing for the other URL", "provider_id", p.ID, "behind_vpn", BehindVPN())
		if _, err := bounded(ctx, r.Timeout, func(ctx context.Context) (Result, error) { return r.RefreshProvider(ctx, p, false) }); err != nil {
			r.log.Error("refresher: refresh failed", "provider_id", p.ID, "err", err.Error())
		}
	}
}
