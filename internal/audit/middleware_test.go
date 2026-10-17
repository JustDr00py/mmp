package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

type fakeStore struct{ rows []db.InsertAuditParams }

func (f *fakeStore) InsertAudit(_ context.Context, p db.InsertAuditParams) error {
	f.rows = append(f.rows, p)
	return nil
}

func TestMiddleware(t *testing.T) {
	names := map[string]string{"provider/7": "Atlas TV", "channel/12": "ESPN"}
	st := &fakeStore{}
	rec := &Recorder{Store: st, Namer: func(_ context.Context, typ, id string) string { return names[typ+"/"+id] }}
	admin := db.User{ID: 1, Username: "boss"}
	handler := rec.Middleware("ui")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/providers/7/delete":
			delete(names, "provider/7") // gone once the handler ran
			http.Redirect(w, r, "/admin/providers?msg=deleted", http.StatusSeeOther)
		case "/admin/channels/new":
			names["channel/12"] = "ESPN"
			http.Redirect(w, r, "/admin/channels/12?msg=created", http.StatusSeeOther)
		case "/admin/bouquets":
			names["bouquet/4"] = "Basic"
			w.Header().Set("HX-Redirect", "/admin/bouquets/4") // an htmx form
			w.WriteHeader(http.StatusNoContent)
		case "/admin/users/3/password":
			http.Error(w, "nope", http.StatusUnprocessableEntity)
		default:
			w.Write([]byte("ok"))
		}
	}))
	do := func(method, path string) {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(httptest.NewRecorder(), r.WithContext(auth.WithUser(r.Context(), admin)))
	}
	do(http.MethodPost, "/admin/providers/7/delete")
	do(http.MethodPost, "/admin/channels/new")
	do(http.MethodPost, "/admin/bouquets")
	do(http.MethodPost, "/admin/users/3/password") // refused: not recorded
	do(http.MethodGet, "/admin/channels")          // a read: not recorded
	do(http.MethodGet, "/admin/backups/mmp-auto-1.tar.gz/download")

	if len(st.rows) != 4 {
		t.Fatalf("rows = %+v, want 4", st.rows)
	}
	del, created, htmx, dl := st.rows[0], st.rows[1], st.rows[2], st.rows[3]
	if htmx.Action != "created" || htmx.TargetID != "4" || htmx.TargetName != "Basic" {
		t.Errorf("htmx create = %+v", htmx)
	}
	if del.Action != "deleted" || del.TargetType != "provider" || del.TargetName != "Atlas TV" || *del.UserID != 1 ||
		del.Username != "boss" || del.Ip != "203.0.113.7" || del.Via != "ui" || del.Method != "POST" {
		t.Errorf("delete = %+v (the name must be read before the handler)", del)
	}
	if created.Action != "created" || created.TargetID != "12" || created.TargetName != "ESPN" {
		t.Errorf("create = %+v (id and name from the redirect)", created)
	}
	if dl.Action != "downloaded" || dl.TargetType != "backup" || dl.TargetID != "mmp-auto-1.tar.gz" {
		t.Errorf("download = %+v", dl)
	}

	var nilRec *Recorder // records nothing, passes requests through
	h := nilRec.Middleware("ui")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/x", nil))
	if w.Code != 204 {
		t.Errorf("nil recorder changed the response: %d", w.Code)
	}
	nilRec.SignIn(httptest.NewRequest(http.MethodPost, "/admin/login", nil), nil, "x", "ui")
}
