// Package web is the embedded admin UI: server-rendered html/template pages
// enhanced with htmx, all under /admin. It authenticates with the same
// admin sessions as the REST API (internal/auth).
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/audit"
	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/backup"
	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/output"
	"github.com/JustDr00py/mmp/internal/proxy"
	"github.com/JustDr00py/mmp/internal/redact"
	"github.com/JustDr00py/mmp/internal/source"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Prefix is where the UI is mounted.
const Prefix = "/admin"

// LiveStatus reports and controls running channel sessions (satisfied by
// *proxy.Manager).
type LiveStatus interface {
	Channels() []proxy.ChannelStatus
	Stop(channelID int64) bool
	Switch(channelID, streamID int64) bool
}

// LiveViewers lists and disconnects the customers watching live channels
// (satisfied by *output.Access).
type LiveViewers interface {
	Viewers() []output.Viewer
	Kick(id uint64) bool
}

// Store is the persistence the pages need (satisfied by *db.Queries).
type Store interface {
	ListProviders(ctx context.Context) ([]db.Provider, error)
	GetProvider(ctx context.Context, id int64) (db.Provider, error)
	CreateProvider(ctx context.Context, arg db.CreateProviderParams) (db.Provider, error)
	UpdateProvider(ctx context.Context, arg db.UpdateProviderParams) (db.Provider, error)
	DeleteProvider(ctx context.Context, id int64) error
	SetProviderEnabled(ctx context.Context, arg db.SetProviderEnabledParams) error
	ListBackbones(ctx context.Context) ([]db.ListBackbonesRow, error)
	ListOutcomeHoursSince(ctx context.Context, since time.Time) ([]db.ListOutcomeHoursSinceRow, error)
	GetProvidersByIDs(ctx context.Context, ids []int64) ([]db.Provider, error)
	SetProvidersBackbone(ctx context.Context, arg db.SetProvidersBackboneParams) (int64, error)
	SetProvidersEnabled(ctx context.Context, arg db.SetProvidersEnabledParams) (int64, error)
	SetProvidersPriority(ctx context.Context, arg db.SetProvidersPriorityParams) (int64, error)
	AlignBackbonePriority(ctx context.Context, backbone string) ([]db.AlignBackbonePriorityRow, error)
	ProviderStreamCounts(ctx context.Context) ([]db.ProviderStreamCountsRow, error)
	SetupProgress(ctx context.Context) (db.SetupProgressRow, error)
	ProviderGroupSummary(ctx context.Context) ([]db.ProviderGroupSummaryRow, error)
	ListProviderGroups(ctx context.Context, providerID int64) ([]db.ListProviderGroupsRow, error)
	ClearProviderGroups(ctx context.Context, providerID int64) error
	AddProviderGroups(ctx context.Context, arg db.AddProviderGroupsParams) error
	SetProviderGroupFilter(ctx context.Context, arg db.SetProviderGroupFilterParams) error
	ListProviderStreams(ctx context.Context, arg db.ListProviderStreamsParams) ([]db.ProviderStream, error)
	SearchProviderStreams(ctx context.Context, arg db.SearchProviderStreamsParams) ([]db.ProviderStream, error)

	ListChannels(ctx context.Context, arg db.ListChannelsParams) ([]db.Channel, error)
	GetChannel(ctx context.Context, id int64) (db.Channel, error)
	CreateChannel(ctx context.Context, arg db.CreateChannelParams) (db.Channel, error)
	UpdateChannel(ctx context.Context, arg db.UpdateChannelParams) (db.Channel, error)
	SetChannelGuideMode(ctx context.Context, arg db.SetChannelGuideModeParams) error
	DeleteChannel(ctx context.Context, id int64) error
	DeleteChannels(ctx context.Context, channelIds []int64) (int64, error)
	DeleteGroupChannels(ctx context.Context, groupID *int64) (int64, error)
	NextChannelNumber(ctx context.Context) (int32, error)
	ChannelNumbersByIDs(ctx context.Context, ids []int64) ([]db.ChannelNumbersByIDsRow, error)
	ClearChannelNumbers(ctx context.Context, ids []int64) error
	SetChannelNumbers(ctx context.Context, arg db.SetChannelNumbersParams) error
	SetChannelGroupOrder(ctx context.Context, ids []int64) error
	ListChannelGroups(ctx context.Context) ([]db.ChannelGroup, error)
	GetChannelGroup(ctx context.Context, id int64) (db.ChannelGroup, error)
	CreateChannelGroup(ctx context.Context, arg db.CreateChannelGroupParams) (db.ChannelGroup, error)
	EnsureChannelGroup(ctx context.Context, name string) (db.ChannelGroup, error)
	UpdateChannelGroup(ctx context.Context, arg db.UpdateChannelGroupParams) (db.ChannelGroup, error)
	SetChannelGroupGuideMode(ctx context.Context, arg db.SetChannelGroupGuideModeParams) error
	DeleteChannelGroup(ctx context.Context, id int64) error
	ChannelGroupCounts(ctx context.Context) ([]db.ChannelGroupCountsRow, error)
	SetChannelsGroup(ctx context.Context, arg db.SetChannelsGroupParams) (int64, error)
	ChannelSourceGroups(ctx context.Context, channelIds []int64) ([]db.ChannelSourceGroupsRow, error)
	ListStreamProfiles(ctx context.Context) ([]db.StreamProfile, error)
	GetStreamProfile(ctx context.Context, id int64) (db.StreamProfile, error)
	CreateStreamProfile(ctx context.Context, arg db.CreateStreamProfileParams) (db.StreamProfile, error)
	UpdateStreamProfile(ctx context.Context, arg db.UpdateStreamProfileParams) (db.StreamProfile, error)
	DeleteStreamProfile(ctx context.Context, id int64) (int64, error)
	ProfileChannelCounts(ctx context.Context) ([]db.ProfileChannelCountsRow, error)
	ListEpgSourceStats(ctx context.Context) ([]db.ListEpgSourceStatsRow, error)
	GetEpgSource(ctx context.Context, id int64) (db.EpgSource, error)
	CreateExtraEpgSource(ctx context.Context, arg db.CreateExtraEpgSourceParams) (db.EpgSource, error)
	UpdateExtraEpgSource(ctx context.Context, arg db.UpdateExtraEpgSourceParams) (db.EpgSource, error)
	DeleteExtraEpgSource(ctx context.Context, id int64) (int64, error)
	ChannelGuideStatus(ctx context.Context, arg db.ChannelGuideStatusParams) ([]db.ChannelGuideStatusRow, error)
	ListEnabledChannels(ctx context.Context) ([]db.ListEnabledChannelsRow, error)
	ListGuidePrograms(ctx context.Context, arg db.ListGuideProgramsParams) ([]db.ListGuideProgramsRow, error)
	ChannelStreamCounts(ctx context.Context) ([]db.ChannelStreamCountsRow, error)
	ChannelBouquetCounts(ctx context.Context) ([]db.ChannelBouquetCountsRow, error)
	ListChannelStreams(ctx context.Context, channelID int64) ([]db.ListChannelStreamsRow, error)
	AddStreamToChannel(ctx context.Context, arg db.AddStreamToChannelParams) error
	RemoveStreamFromChannel(ctx context.Context, arg db.RemoveStreamFromChannelParams) error
	ReorderChannelStreams(ctx context.Context, arg db.ReorderChannelStreamsParams) error
	SortChannelStreamsByProvider(ctx context.Context, arg db.SortChannelStreamsByProviderParams) (int64, error)
	SearchStreamsAllProviders(ctx context.Context, arg db.SearchStreamsAllProvidersParams) ([]db.SearchStreamsAllProvidersRow, error)
	GetStreamsByIDs(ctx context.Context, ids []int64) ([]db.GetStreamsByIDsRow, error)

	ListUsers(ctx context.Context) ([]db.User, error)
	GetUser(ctx context.Context, id int64) (db.User, error)
	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error)
	UpdateUser(ctx context.Context, arg db.UpdateUserParams) (db.User, error)
	DeleteUser(ctx context.Context, id int64) error
	SetUserStreamToken(ctx context.Context, arg db.SetUserStreamTokenParams) (db.User, error)
	CountAdmins(ctx context.Context) (int64, error)
	ListUserBouquetIDs(ctx context.Context, userID int64) ([]int64, error)
	ClearUserBouquets(ctx context.Context, userID int64) error
	AddUserBouquets(ctx context.Context, arg db.AddUserBouquetsParams) error
	ListBouquets(ctx context.Context) ([]db.Bouquet, error)
	CreateBouquet(ctx context.Context, arg db.CreateBouquetParams) (db.Bouquet, error)
	CloneBouquet(ctx context.Context, arg db.CloneBouquetParams) (int64, error)
	GetBouquet(ctx context.Context, id int64) (db.Bouquet, error)
	UpdateBouquet(ctx context.Context, arg db.UpdateBouquetParams) (db.Bouquet, error)
	DeleteBouquet(ctx context.Context, id int64) error
	ListBouquetChannels(ctx context.Context, bouquetID int64) ([]db.Channel, error)
	BouquetCounts(ctx context.Context) ([]db.BouquetCountsRow, error)
	AddChannelsToBouquet(ctx context.Context, arg db.AddChannelsToBouquetParams) (int64, error)
	AddGroupToBouquet(ctx context.Context, arg db.AddGroupToBouquetParams) (int64, error)
	RemoveChannelsFromBouquet(ctx context.Context, arg db.RemoveChannelsFromBouquetParams) (int64, error)

	SetProviderVODEnabled(ctx context.Context, arg db.SetProviderVODEnabledParams) error
	ListVODCategoriesAdmin(ctx context.Context, arg db.ListVODCategoriesAdminParams) ([]db.ListVODCategoriesAdminRow, error)
	ListVODItemsAdmin(ctx context.Context, arg db.ListVODItemsAdminParams) ([]db.ListVODItemsAdminRow, error)
	ListSeriesAdmin(ctx context.Context, arg db.ListSeriesAdminParams) ([]db.ListSeriesAdminRow, error)
	AddVODCategoriesToBouquet(ctx context.Context, arg db.AddVODCategoriesToBouquetParams) (int64, error)
	RemoveVODCategoryFromBouquet(ctx context.Context, arg db.RemoveVODCategoryFromBouquetParams) (int64, error)
	ListBouquetVODCategories(ctx context.Context, bouquetID int64) ([]db.ListBouquetVODCategoriesRow, error)

	ListRecentEvents(ctx context.Context, arg db.ListRecentEventsParams) ([]db.Event, error)
	ListEvents(ctx context.Context, arg db.ListEventsParams) ([]db.Event, error)
	SetProviderGuidePriority(ctx context.Context, arg db.SetProviderGuidePriorityParams) (int64, error)
	UsersLastSeen(ctx context.Context) ([]db.UsersLastSeenRow, error)
	ListAudit(ctx context.Context, arg db.ListAuditParams) ([]db.AdminAudit, error)
	AuditUsernames(ctx context.Context) ([]string, error)
	AuditTargetTypes(ctx context.Context) ([]string, error)
	UserViewingSessions(ctx context.Context, arg db.UserViewingSessionsParams) ([]db.UserViewingSessionsRow, error)
	UserNetworks(ctx context.Context, arg db.UserNetworksParams) ([]db.UserNetworksRow, error)
	UserAuthFailures(ctx context.Context, arg db.UserAuthFailuresParams) ([]db.UserAuthFailuresRow, error)
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
	ChannelNamesByIDs(ctx context.Context, ids []int64) ([]db.ChannelNamesByIDsRow, error)
	ListChannelMatchKeys(ctx context.Context) ([]db.ListChannelMatchKeysRow, error)
	ListChannelStreamNames(ctx context.Context) ([]db.ListChannelStreamNamesRow, error)
	ProviderVODSize(ctx context.Context, providerID int64) (db.ProviderVODSizeRow, error)
	DeleteProviderVOD(ctx context.Context, providerID int64) (db.DeleteProviderVODRow, error)
	LiveChannelInfo(ctx context.Context, ids []int64) ([]db.LiveChannelInfoRow, error)
	LiveChannelStreams(ctx context.Context, ids []int64) ([]db.LiveChannelStreamsRow, error)

	ListMatchRules(ctx context.Context) ([]db.MatchRule, error)
	CreateMatchRule(ctx context.Context, arg db.CreateMatchRuleParams) (db.MatchRule, error)
	DeleteMatchRule(ctx context.Context, id int64) error
}

// Matcher proposes and applies channel groupings (satisfied by
// *channel.Service).
type Matcher interface {
	Propose(ctx context.Context) ([]channel.Proposal, error)
	Apply(ctx context.Context, opts channel.ApplyOptions) (channel.ApplyResult, error)
	KeyStreams(ctx context.Context, channelID int64, key string) ([]int64, error)
	MatchSettings(ctx context.Context) (channel.MatchSettings, error)
	SaveMatchSettings(ctx context.Context, m channel.MatchSettings) error
	AutoMatch(ctx context.Context, providerID int64) (channel.ApplyResult, bool, error)
	MatchNow(ctx context.Context, providerID int64) (channel.ApplyResult, error)
	Invalidate()
}

// Refresher refreshes a provider on demand (satisfied by *source.Refresher).
type Refresher interface {
	RefreshProvider(ctx context.Context, p db.Provider, force bool) (source.Result, error)
	RefreshEPG(ctx context.Context, p db.Provider) (int, error)
	RefreshEPGSource(ctx context.Context, src db.EpgSource) (int, error)
	RefreshVOD(ctx context.Context, p db.Provider, force bool) (source.VODResult, error)
	SeriesEpisodesByID(ctx context.Context, seriesID int64, ttl time.Duration) (db.Series, []db.SeriesEpisode, error)
}

// Deps are the UI's collaborators.
type Deps struct {
	Auth       *auth.Authenticator
	Store      Store
	Refresh    Refresher
	Match      Matcher
	Live       LiveStatus           // optional
	Viewers    LiveViewers          // optional: who is watching, for the dashboard
	Slots      func() map[int64]int // provider slots in use; optional
	LoginLimit *auth.Limiter        // locks out repeated login failures; optional
	// Connections reports open customer streams per user; optional.
	Connections func() map[int64]int
	// InTx runs fn atomically (user + bouquet changes); nil runs directly.
	InTx func(ctx context.Context, fn func(Store) error) error
	// BaseURL is the public URL customers' players use; no trailing slash.
	BaseURL string
	// DefaultRefresh pre-fills a new provider's refresh interval
	// (refresh.interval); 0 means 6h.
	DefaultRefresh time.Duration
	// EpisodeTTL is how long a series' episode list is reused (vod.episode_ttl).
	EpisodeTTL time.Duration
	// Audit records sign-ins and every admin change; nil records nothing.
	Audit *audit.Recorder
	// Backups is the backup service; nil hides the page.
	Backups *backup.Service
	// Restart restarts MMP after a restore (nil: no restart).
	Restart func()
	// Preview powers the pop-up player on the match and channel lists;
	// nil hides it.
	Preview *Preview
	// RootAPI may answer a request for "/" (an app's Xtream call there);
	// false sends it on to the admin UI. Optional.
	RootAPI func(w http.ResponseWriter, r *http.Request) bool
	// Ctx ends long-lived responses (the live dashboard) at shutdown.
	Ctx     context.Context
	Log     *slog.Logger
	Version string
}

// UI serves the admin pages.
type UI struct {
	Deps

	pages     map[string]*template.Template
	assetHash string // busts browser caches when the embedded assets change

	bulkRefreshing atomic.Bool // a bulk provider refresh is running
}

// navItem is one entry of the sidebar navigation; Section starts a new
// labelled group, Icon names a symbol in the layout's icon sprite.
type navItem struct {
	Path, Label, Icon, Section string
}

var nav = []navItem{
	{Prefix + "/", "Dashboard", "home", ""},
	{Prefix + "/events", "Events", "clock", ""},
	{Prefix + "/guide", "Guide", "guide", ""},
	{Prefix + "/providers", "Providers", "server", "Sources"},
	{Prefix + "/epg", "EPG", "epg", ""},
	{Prefix + "/match", "Match", "match", "Lineup"},
	{Prefix + "/channels", "Channels", "tv", ""},
	{Prefix + "/groups", "Groups", "folder", ""},
	{Prefix + "/bouquets", "Bouquets", "stack", ""},
	{Prefix + "/vod", "Movies & Series", "film", ""},
	{Prefix + "/users", "Users", "users", "Customers"},
	{Prefix + "/settings", "Settings", "gear", "Settings"},
	{Prefix + "/profiles", "Profiles", "sliders", ""},
	{Prefix + "/backups", "Backups", "archive", ""},
	{Prefix + "/audit", "Audit log", "eye", ""},
}

// pageNames are the templates/<name>.html pages, each parsed with the layout.
var pageNames = []string{"login", "dashboard", "providers", "provider_form", "streams", "channels", "channel_new", "channel", "match", "users", "user_new", "user", "bouquets", "bouquet", "epg", "profiles", "profile_form", "vod", "vod_series", "provider_groups", "backups", "backup_restored", "epg_source", "channel_groups", "settings", "events", "audit", "guide"}

// New parses the embedded templates.
func New(d Deps) (*UI, error) {
	ui := &UI{Deps: d, pages: map[string]*template.Template{}}
	h := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		h.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	ui.assetHash = hex.EncodeToString(h.Sum(nil))[:12]

	funcs := template.FuncMap{
		"asset": func(name string) string {
			return Prefix + "/static/" + name + "?v=" + ui.assetHash
		},
		"redact":     redact.URL,
		"ago":        ago,
		"pct":        func(f float32) int { return int(f*100 + 0.5) },
		"future":     func(t *time.Time) bool { return t != nil && t.After(time.Now()) },
		"streamsURL": streamsQuery,
		"add":        func(a, b int) int { return a + b },
		"inc":        func(i int) int { return i + 1 },
		"list":       func(xs ...int) []int { return xs },
		"field":      func(id, label, value string) copyField { return copyField{id, label, value} },
		"last":       func(i, n int) bool { return i == n-1 },
		"mbps":       mbps,
		"bytesize":   byteSize,
		"fps":        fps,
		"upper":      strings.ToUpper,
		"size":       humanSize,
		"when":       func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
		"clock":      func(t time.Time) string { return t.Local().Format("15:04") },
		"preview":    func() bool { return ui.Preview != nil },
		"guideModes": func() []struct{ Value, Label string } { return guideModes },
	}
	for _, page := range pageNames {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		ui.pages[page] = t
	}
	return ui, nil
}

// waitString renders a wait for humans: "45 seconds", "5 minutes".
func waitString(d time.Duration) string {
	if d < time.Minute {
		s := int(d.Seconds())
		if s < 1 {
			s = 1
		}
		return fmt.Sprintf("%d seconds", s)
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes())+1)
}

// ago renders a past time coarsely ("3m ago"); nil is "never".
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Mount registers the UI routes on the root router.
func (ui *UI) Mount(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if ui.RootAPI != nil && ui.RootAPI(w, r) {
			return
		}
		http.Redirect(w, r, Prefix+"/", http.StatusFound)
	})
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { // browsers ask regardless
		http.Redirect(w, r, Prefix+"/static/favicon.svg?v="+ui.assetHash, http.StatusMovedPermanently)
	})
	r.Get(Prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
	})
	r.Route(Prefix, func(r chi.Router) {
		r.Use(securityHeaders, sameOrigin)
		static, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix(Prefix+"/static/", cacheStatic(http.FileServerFS(static))))
		r.Get("/login", ui.loginPage)
		r.Post("/login", ui.login)
		r.Group(func(r chi.Router) {
			r.Use(ui.requireAdmin, ui.Audit.Middleware("ui"), ui.invalidateMatch)
			r.Post("/logout", ui.logout)
			r.Get("/", ui.dashboard)
			r.Get("/live", ui.liveStream)
			ui.mountProviders(r)
			ui.mountChannels(r)
			ui.mountChannelGroups(r)
			ui.mountSettings(r)
			ui.mountEvents(r)
			ui.mountAudit(r)
			ui.mountMatch(r)
			ui.mountUsers(r)
			ui.mountBouquets(r)
			ui.mountEPG(r)
			ui.mountGuide(r)
			ui.mountProfiles(r)
			ui.mountVOD(r)
			ui.mountBackups(r)
			ui.mountLive(r)
			ui.mountPreview(r)
		})
	})
}

// invalidateMatch drops the cached match proposals after every change an
// admin makes: providers, groups, channels, rules and settings all feed
// matching, and tracking each one would miss some.
func (ui *UI) invalidateMatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && ui.Match != nil {
			ui.Match.Invalidate()
		}
	})
}

// pageData is what every page template receives.
type pageData struct {
	Title   string
	User    *db.User
	Nav     []navItem
	Active  string
	Version string
	Data    any
}

func (ui *UI) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	ui.renderTemplate(w, r, status, page, "layout", title, data)
}

// fragment renders one named block of a page with data (not wrapped in
// pageData), for htmx swaps.
func (ui *UI) fragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	ui.execute(w, http.StatusOK, page, name, data)
}

func (ui *UI) renderTemplate(w http.ResponseWriter, r *http.Request, status int, page, name, title string, data any) {
	pd := pageData{Title: title, Nav: nav, Active: activeNav(r.URL.Path), Version: ui.Version, Data: data}
	if u, ok := auth.UserFrom(r.Context()); ok {
		pd.User = &u
	}
	ui.execute(w, status, page, name, pd)
}

func (ui *UI) execute(w http.ResponseWriter, status int, page, name string, data any) {
	var buf bytes.Buffer // render fully first: a template error must not leave half a page
	if err := ui.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		ui.Log.Error("web: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// copyField is a read-only value with a copy button.
type copyField struct{ ID, Label, Value string }

// formError is a validation message shown to the admin as written (a
// sentence, unlike Go error strings).
type formError string

func (e formError) Error() string { return string(e) }

// activeNav is the nav entry a path belongs to (its longest prefix).
func activeNav(path string) string {
	best := ""
	for _, n := range nav {
		if (path == n.Path || strings.HasPrefix(path, n.Path+"/")) && len(n.Path) > len(best) {
			best = n.Path
		}
	}
	return best
}

// redirect after a POST: htmx gets HX-Redirect (a full navigation), plain
// forms a 303.
func (ui *UI) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// requireAdmin sends visitors without a valid admin session to the login
// page, remembering where they were going.
func (ui *UI) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := ui.Auth.Authenticate(r)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				ui.Log.Error("web: auth lookup", "err", err)
				http.Error(w, "auth unavailable", http.StatusInternalServerError)
				return
			}
			target := Prefix + "/login"
			if r.Method == http.MethodGet && r.URL.Path != Prefix+"/" {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			if r.Header.Get("HX-Request") == "true" { // htmx: full-page redirect, not a swapped fragment
				w.Header().Set("HX-Redirect", target)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

type loginData struct {
	Username string
	Next     string
	Error    string
}

func (ui *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := ui.Auth.Authenticate(r); err == nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	ui.render(w, r, http.StatusOK, "login", "Sign in", loginData{Next: safeNext(r.URL.Query().Get("next"))})
}

func (ui *UI) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username, password := strings.TrimSpace(r.PostForm.Get("username")), r.PostForm.Get("password")
	next := safeNext(r.PostForm.Get("next"))
	attempt := auth.NewAttempt(r, username)
	if ok, retry := ui.LoginLimit.Allow(attempt); !ok {
		msg := "Too many attempts. Try again in " + waitString(retry) + "."
		ui.render(w, r, http.StatusTooManyRequests, "login", "Sign in", loginData{Username: username, Next: next, Error: msg})
		return
	}
	token, u, err := ui.Auth.Login(r.Context(), username, password)
	switch { // only password verdicts count; infrastructure errors do not
	case err == nil:
		ui.LoginLimit.Done(attempt, true)
		ui.Audit.SignIn(r, &u, username, "ui")
	case errors.Is(err, auth.ErrUnauthorized):
		ui.LoginLimit.Done(attempt, false)
		ui.Audit.SignIn(r, nil, username, "ui")
	}
	if err != nil {
		status, msg := http.StatusUnauthorized, "Invalid username or password."
		if !errors.Is(err, auth.ErrUnauthorized) {
			ui.Log.Error("web: login", "err", err)
			status, msg = http.StatusInternalServerError, "Sign-in is unavailable right now."
		}
		ui.render(w, r, status, "login", "Sign in", loginData{Username: username, Next: next, Error: msg})
		return
	}
	ui.Auth.SetSessionCookie(w, token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (ui *UI) logout(w http.ResponseWriter, r *http.Request) {
	if err := ui.Auth.Logout(r.Context(), r); err != nil {
		ui.Log.Warn("web: logout", "err", err)
	}
	ui.Auth.SetSessionCookie(w, "")
	http.Redirect(w, r, Prefix+"/login", http.StatusSeeOther)
}

func (ui *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	snap, err := ui.snapshot(r.Context())
	if err != nil {
		ui.fail(w, "dashboard snapshot", err)
		return
	}
	d := dashboardData{Live: snap, Flash: liveFlashes[r.URL.Query().Get("msg")]}
	prog, err := ui.Store.SetupProgress(r.Context())
	if err != nil {
		ui.fail(w, "setup progress", err)
		return
	}
	d.Setup = setupSteps(prog)
	d.Totals = prog
	ps, err := ui.Store.ListProviders(r.Context())
	if err != nil {
		ui.fail(w, "list providers", err)
		return
	}
	for _, p := range ps {
		if p.Enabled && p.LastError != "" {
			d.Failing = append(d.Failing, p)
		}
	}
	ui.render(w, r, http.StatusOK, "dashboard", "Dashboard", d)
}

type dashboardData struct {
	Flash  string
	Live   liveSnapshot
	Setup  *setupView // nil once every required step is done
	Totals db.SetupProgressRow
	// Failing are the enabled providers whose last refresh or health
	// check got no answer.
	Failing []db.Provider
}

type setupStep struct {
	Title, Text, Link, Action string
	Done, Next, Optional      bool
}

type setupView struct {
	Steps       []setupStep
	Done, Total int
	Percent     int
}

// setupSteps is the getting-started checklist, worked out from what exists.
func setupSteps(p db.SetupProgressRow) *setupView {
	steps := []setupStep{
		{Title: "Add a provider", Text: "An Xtream Codes account or an M3U playlist from your IPTV supplier.",
			Link: Prefix + "/providers/new", Action: "Add provider", Done: p.Providers > 0},
		{Title: "Import its streams", Text: "Press Refresh on the provider; it also refreshes on its own schedule.",
			Link: Prefix + "/providers", Action: "Open providers", Done: p.Streams > 0},
		{Title: "Choose the groups you want", Text: "Hide the regional groups you will never use, so matching only offers what you need.",
			Link: Prefix + "/providers", Action: "Choose groups", Done: p.FilteredProviders > 0, Optional: true},
		{Title: "Create channels", Text: "Match builds channels with failover from the same channel at several providers.",
			Link: Prefix + "/match", Action: "Open Match", Done: p.Channels > 0},
		{Title: "Put channels in a bouquet", Text: "A bouquet is the channel package a customer gets.",
			Link: Prefix + "/bouquets", Action: "Open bouquets", Done: p.Bouquets > 0},
		{Title: "Add a customer", Text: "Give them a bouquet, then copy their player login or playlist link.",
			Link: Prefix + "/users/new", Action: "Add customer", Done: p.Customers > 0},
	}
	v := &setupView{}
	next := false
	for i := range steps {
		if steps[i].Optional {
			continue
		}
		v.Total++
		if steps[i].Done {
			v.Done++
		}
	}
	for i := range steps {
		if !steps[i].Done && !next {
			steps[i].Next, next = true, true
		}
	}
	if v.Done == v.Total {
		return nil
	}
	v.Steps, v.Percent = steps, v.Done*100/v.Total
	return v
}

// safeNext keeps post-login redirects inside the admin UI: anything that
// is not a plain /admin/ path (absolute URLs, //host, backslashes) falls
// back to the dashboard.
func safeNext(next string) string {
	if !strings.HasPrefix(next, Prefix+"/") || strings.ContainsAny(next, "\\\r\n") || strings.HasPrefix(next, "//") {
		return Prefix + "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return Prefix + "/"
	}
	return next
}

// securityHeaders: no framing, no third-party anything, no MIME sniffing.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https: http:; style-src 'self'; script-src 'self'; media-src 'self' blob:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects state-changing requests sent from another site. The
// session cookie is SameSite=Strict already; this also covers the login
// form (login CSRF) and browsers that ignore SameSite.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !originMatches(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originMatches(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		// Old browsers and non-browser clients send no Origin; fall back to
		// Referer when present.
		ref := r.Header.Get("Referer")
		if ref == "" {
			return origin == ""
		}
		origin = ref
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// cacheStatic lets browsers keep versioned assets (?v=hash) for a year.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// humanSize formats a byte count: "512 B", "3.4 MB".
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
