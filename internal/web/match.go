package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/db"
)

// proposalLimit caps suggestions rendered at once; a first run against
// big providers can produce thousands.
const proposalLimit = 300

func (ui *UI) mountMatch(r chi.Router) {
	r.Get("/match", ui.matchPage)
	r.Post("/match/apply", ui.matchApply)
	r.Post("/match/rules", ui.matchRuleCreate)
	r.Post("/match/rules/{id}/delete", ui.matchRuleDelete)
}

type proposalStream struct {
	ID       int64
	Name     string
	Provider string
}

type proposalView struct {
	channel.Proposal
	Streams []proposalStream
}

type matchData struct {
	Proposals []proposalView
	Total     int // proposals before the limit / filter
	Hidden    int // filtered out by MinProviders
	MinProv   int
	Rules     []db.MatchRule
	RuleError string
	RuleForm  db.CreateMatchRuleParams
	Flash     string
}

func (ui *UI) matchData(r *http.Request, minProv int) (matchData, error) {
	d := matchData{MinProv: minProv}
	props, err := ui.Match.Propose(r.Context())
	if err != nil {
		return d, err
	}
	var shown []channel.Proposal
	for _, p := range props {
		if p.Providers < minProv {
			d.Hidden++
			continue
		}
		shown = append(shown, p)
	}
	d.Total = len(shown)
	if len(shown) > proposalLimit {
		shown = shown[:proposalLimit]
	}
	var ids []int64
	for _, p := range shown {
		ids = append(ids, p.StreamIDs...)
	}
	streams := map[int64]db.GetStreamsByIDsRow{}
	if len(ids) > 0 {
		rows, err := ui.Store.GetStreamsByIDs(r.Context(), ids)
		if err != nil {
			return d, err
		}
		for _, s := range rows {
			streams[s.ID] = s
		}
	}
	for _, p := range shown {
		v := proposalView{Proposal: p}
		for _, id := range p.StreamIDs {
			s := streams[id]
			v.Streams = append(v.Streams, proposalStream{ID: id, Name: s.Name, Provider: s.ProviderName})
		}
		d.Proposals = append(d.Proposals, v)
	}
	if d.Rules, err = ui.Store.ListMatchRules(r.Context()); err != nil {
		return d, err
	}
	return d, nil
}

var matchFlashes = map[string]string{"rule": "Rule added.", "ruledel": "Rule deleted."}

func (ui *UI) matchPage(w http.ResponseWriter, r *http.Request) {
	minProv := 2
	if v := r.URL.Query().Get("min"); v != "" {
		minProv = max(atoi(v), 1)
	}
	d, err := ui.matchData(r, minProv)
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	d.Flash = matchFlashes[r.URL.Query().Get("msg")]
	if msg := r.URL.Query().Get("applied"); msg != "" {
		created, attached, _ := strings.Cut(msg, "-")
		d.Flash = "Applied: " + strconv.Itoa(atoi(created)) + " channels created, " + strconv.Itoa(atoi(attached)) + " streams attached."
	}
	ui.render(w, r, http.StatusOK, "match", "Match suggestions", d)
}

// matchApply commits the ticked proposals. Keys are recomputed server
// side by the matcher, so a stale page only applies what still matches.
func (ui *UI) matchApply(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	keys := r.PostForm["key"]
	if len(keys) == 0 {
		ui.redirect(w, r, Prefix+"/match")
		return
	}
	res, err := ui.Match.Apply(r.Context(), channel.ApplyOptions{Keys: keys})
	if err != nil {
		ui.fail(w, "match apply", err)
		return
	}
	ui.Log.Info("web: match applied", "keys", len(keys), "created", res.Created, "attached", res.Attached)
	ui.redirect(w, r, Prefix+"/match?applied="+strconv.Itoa(res.Created)+"-"+strconv.Itoa(res.Attached)+"&min="+r.PostForm.Get("min"))
}

func (ui *UI) matchRuleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f := db.CreateMatchRuleParams{
		// The pattern is kept verbatim: surrounding spaces can be part of a regex.
		Pattern: r.PostForm.Get("pattern"), ChannelName: strings.TrimSpace(r.PostForm.Get("channel_name")),
		Priority: int32(atoi(r.PostForm.Get("priority"))), Enabled: true,
	}
	var msg string
	switch {
	case strings.TrimSpace(f.Pattern) == "" || f.ChannelName == "":
		msg = "Pattern and channel name are required."
	default:
		if _, err := channel.CompileRule(f.Pattern); err != nil {
			msg = "Invalid pattern: " + err.Error()
		}
	}
	if msg == "" {
		if _, err := ui.Store.CreateMatchRule(r.Context(), f); err != nil {
			ui.fail(w, "create match rule", err)
			return
		}
		ui.redirect(w, r, Prefix+"/match?msg=rule")
		return
	}
	d, err := ui.matchData(r, 2)
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	d.RuleError, d.RuleForm = msg, f
	ui.render(w, r, http.StatusUnprocessableEntity, "match", "Match suggestions", d)
}

func (ui *UI) matchRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := ui.Store.DeleteMatchRule(r.Context(), id); err != nil {
		ui.fail(w, "delete match rule", err)
		return
	}
	ui.redirect(w, r, Prefix+"/match?msg=ruledel")
}
