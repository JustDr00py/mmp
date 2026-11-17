package source

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JustDr00py/mmp/internal/source/xtream"
)

// ULTRA8K-style panels answer a bare get_series with [] and only list
// shows per category.
func TestSeriesByCategory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("category_id") {
		case "1":
			io.WriteString(w, `[{"name":"A","series_id":10,"category_id":"1"},{"name":"B","series_id":11}]`)
		case "2":
			io.WriteString(w, `[{"name":"C","series_id":12,"category_id":"2"}]`)
		case "3":
			http.Error(w, "nope", http.StatusInternalServerError)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	c, _ := xtream.New(srv.URL, "u", "p", "")
	ctx := context.Background()

	shows, err := seriesByCategory(ctx, c, []xtream.VODCategory{{CategoryID: "1"}, {CategoryID: "2"}, {CategoryID: "9"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 3 {
		t.Fatalf("shows = %+v", shows)
	}
	if shows[1].Name != "B" || shows[1].CategoryID != "1" {
		t.Errorf("show without category_id should take the one it was listed under: %+v", shows[1])
	}

	if _, err := seriesByCategory(ctx, c, []xtream.VODCategory{{CategoryID: "1"}, {CategoryID: "3"}}); err == nil {
		t.Error("a failed category should fail the list, not shrink it")
	}
}
