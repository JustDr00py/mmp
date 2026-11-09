package output

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestLiveCategoriesIcons: category_icon is sent only with the setting on,
// and only for groups that have an icon; the rest of the answer is as before.
func TestLiveCategoriesIcons(t *testing.T) {
	icons := false
	hs, h := newTestHandlers(t)
	store := hs.Store.(*fakeStore)
	store.all = append(store.all, db.ListEnabledChannelsRow{
		ID: 14, Name: "ESPN", GroupID: 2, GroupName: "Sports Extra", GroupSort: 1, GroupLogo: "https://mmp.example/logos/sports.png",
	})
	hs.GroupIcons = func(context.Context) bool { return icons }
	const root = "/player_api.php?username=root&password=root-pw1&action=get_live_categories"

	cats := func() []map[string]any {
		t.Helper()
		_, _, body := get(t, h, root)
		var out []map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%v\n%s", err, body)
		}
		return out
	}
	for _, c := range cats() {
		if _, ok := c["category_icon"]; ok {
			t.Errorf("icons off, got %v", c)
		}
	}
	icons = true
	var with int
	for _, c := range cats() {
		icon, ok := c["category_icon"]
		switch {
		case c["category_name"] == "Sports Extra":
			if icon != "https://mmp.example/logos/sports.png" {
				t.Errorf("Sports Extra icon = %v", icon)
			}
			with++
		case ok:
			t.Errorf("group without an icon sends one: %v", c)
		}
	}
	if with != 1 {
		t.Errorf("Sports Extra category missing")
	}
}
