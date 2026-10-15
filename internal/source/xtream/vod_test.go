package xtream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

func TestFlexString(t *testing.T) {
	var v struct {
		A, B, C, D, E, F FlexString
	}
	err := jsonUnmarshal(`{"A":"12","B":12,"C":7.5,"D":null,"E":true,"F":[]}`, &v)
	if err != nil {
		t.Fatal(err)
	}
	if v.A.Int() != 12 || v.B.Int() != 12 || v.C.Int() != 7 || v.D != "" || v.E != "true" || v.F != "" {
		t.Errorf("decoded %+v", v)
	}
	if x, ok := v.C.Float(); !ok || x != 7.5 {
		t.Errorf("Float = %v %v", x, ok)
	}
	if _, ok := v.D.Float(); ok {
		t.Error("empty parsed as a number")
	}
}

func TestSeriesInfoShapes(t *testing.T) {
	for name, body := range map[string]string{
		"by season map": `{"info":{"name":"S"},"episodes":{"1":[{"id":"11","episode_num":1,"title":"a","info":{"plot":"p","duration_secs":"60"}}],"2":[{"id":"21","episode_num":"1","title":"b","info":[]}]}}`,
		"nested arrays": `{"info":{"name":"S"},"episodes":[[{"id":"11","episode_num":1,"title":"a","info":{"plot":"p","duration_secs":60}}],[{"id":21,"episode_num":1,"title":"b"}]]}`,
		"flat list":     `{"info":{"name":"S"},"episodes":[{"id":"11","season":1,"episode_num":1,"title":"a","info":{"plot":"p","duration_secs":60}},{"id":"21","season":"2","episode_num":1,"title":"b"}]}`,
	} {
		var si SeriesInfo
		if err := jsonUnmarshal(body, &si); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sort.Slice(si.Episodes, func(i, j int) bool { return si.Episodes[i].ID < si.Episodes[j].ID })
		if len(si.Episodes) != 2 || si.Episodes[0].Season.Int() != 1 || si.Episodes[1].Season.Int() != 2 ||
			si.Episodes[0].Details().Plot != "p" || si.Episodes[0].Details().DurationSecs.Int() != 60 || si.Episodes[1].Details().Plot != "" {
			t.Errorf("%s: episodes %+v", name, si.Episodes)
		}
		if string(si.Info) != `{"name":"S"}` {
			t.Errorf("%s: info %s", name, si.Info)
		}
	}
}

func TestVODListsTolerateEmptyAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_vod_streams":
			io.WriteString(w, `{}`) // some panels answer an empty catalog with an object
		case "get_series":
			io.WriteString(w, `null`)
		case "get_vod_categories":
			io.WriteString(w, `[{"category_id":5,"category_name":"Action"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "u", "p", "")
	ctx := context.Background()
	if m, err := c.VODStreams(ctx); err != nil || len(m) != 0 {
		t.Errorf("movies = %v, %v", m, err)
	}
	if s, err := c.Series(ctx); err != nil || len(s) != 0 {
		t.Errorf("series = %v, %v", s, err)
	}
	if cats, err := c.VODCategories(ctx); err != nil || len(cats) != 1 || cats[0].CategoryID != "5" {
		t.Errorf("categories = %+v, %v", cats, err)
	}
	if got := c.MovieURL("42", "mkv"); got != srv.URL+"/movie/u/p/42.mkv" {
		t.Errorf("MovieURL = %s", got)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
