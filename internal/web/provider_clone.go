package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// Cloning a provider: another account at the same service (or a
// reseller's second line) gets the source's settings, its group choice and
// its movie & series category choice, with only a new name, login and,
// optionally, URL asked for.

type providerCloneForm struct {
	Source   db.Provider
	Summary  db.ProviderCloneSummaryRow
	Name     string
	Username string
	URL      string // blank keeps the source's URL
	Error    string
}

// RefreshMinutes is the source's refresh interval, as the form shows it.
func (f providerCloneForm) RefreshMinutes() int32 { return f.Source.RefreshIntervalSecs / 60 }

func (ui *UI) cloneSummary(w http.ResponseWriter, r *http.Request, f *providerCloneForm) bool {
	s, err := ui.Store.ProviderCloneSummary(r.Context(), f.Source.ID)
	if err != nil {
		ui.fail(w, "provider clone summary", err)
		return false
	}
	f.Summary = s
	return true
}

func (ui *UI) providerClonePage(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	f := providerCloneForm{Source: p, Name: p.Name + " (copy)"}
	if !ui.cloneSummary(w, r, &f) {
		return
	}
	ui.render(w, r, http.StatusOK, "provider_clone", "Clone "+p.Name, f)
}

func (ui *UI) providerClone(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	f := providerCloneForm{Source: p, Name: strings.TrimSpace(r.PostForm.Get("name")),
		Username: strings.TrimSpace(r.PostForm.Get("username")), URL: strings.TrimSpace(r.PostForm.Get("url"))}
	password := r.PostForm.Get("password")
	target := f.URL
	if target == "" {
		target = p.Url
	}
	epgURL := swapCredentials(p.EpgUrl, p.Username, p.Password, f.Username, password)
	err := cloneError(p, f.Name, target, epgURL, f.Username, password)
	if err == nil {
		err = ui.inTx(r.Context(), func(s Store) error {
			c, err := s.CloneProvider(r.Context(), db.CloneProviderParams{SourceID: p.ID, Name: f.Name, Url: target,
				Username: f.Username, Password: password, EpgUrl: epgURL})
			if err != nil {
				return err
			}
			if _, err := s.CopyProviderGroups(r.Context(), db.CopyProviderGroupsParams{SourceID: p.ID, TargetID: c.ID}); err != nil {
				return err
			}
			if _, err := s.CopyVODCategories(r.Context(), db.CopyVODCategoriesParams{SourceID: p.ID, TargetID: c.ID}); err != nil {
				return err
			}
			if _, err := s.CopyBouquetVODGrants(r.Context(), db.CopyBouquetVODGrantsParams{SourceID: p.ID, TargetID: c.ID}); err != nil {
				return err
			}
			ui.Log.Info("web: provider cloned", "provider_id", c.ID, "source_id", p.ID)
			return nil
		})
		if err != nil {
			ui.fail(w, "clone provider", err)
			return
		}
		ui.redirect(w, r, Prefix+"/providers?msg=cloned")
		return
	}
	f.Error = err.Error()
	if !ui.cloneSummary(w, r, &f) {
		return
	}
	ui.render(w, r, http.StatusUnprocessableEntity, "provider_clone", "Clone "+p.Name, f)
}

// cloneError checks a clone's name, login and URL.
func cloneError(src db.Provider, name, rawURL, epgURL, username, password string) error {
	if err := source.ValidateProvider(name, src.Type, rawURL, epgURL, src.MaxConnections, src.RefreshIntervalSecs); err != nil {
		return errors.New(humanize(err.Error()))
	}
	if src.Type == "xtream" && (username == "" || password == "") {
		return formError("An Xtream account needs a username and a password.")
	}
	if strings.EqualFold(rawURL, src.Url) && username == src.Username && (src.Type == "xtream" || username != "") {
		return formError("That is " + src.Name + "'s own login. Enter the other account's username and password.")
	}
	if src.Type == "m3u" && username == "" && password == "" && rawURL == src.Url {
		return formError("Enter the other account's playlist URL (an M3U playlist's URL usually holds its login), or its username and password.")
	}
	return nil
}

// swapCredentials gives a guide URL that carries the source's login (an
// Xtream xmltv.php link) the clone's login instead. Other URLs stay.
func swapCredentials(raw, oldUser, oldPass, newUser, newPass string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || oldUser == "" {
		return raw
	}
	q := u.Query()
	if q.Get("username") != oldUser {
		return raw
	}
	q.Set("username", newUser)
	if q.Get("password") == oldPass {
		q.Set("password", newPass)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
