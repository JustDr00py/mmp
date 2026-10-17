package audit

import (
	"net/http"
	"testing"
)

func TestDescribe(t *testing.T) {
	for _, tt := range []struct {
		method, path            string
		action, targetType, tid string
	}{
		{"POST", "/admin/providers/new", "created", "provider", ""},
		{"POST", "/admin/providers/7/edit", "updated", "provider", "7"},
		{"POST", "/admin/providers/7/delete", "deleted", "provider", "7"},
		{"POST", "/admin/providers/7/refresh", "refreshed", "provider", "7"},
		{"POST", "/admin/channels/5", "updated", "channel", "5"},
		{"POST", "/admin/channels/5/streams/77/remove", "removed from", "channel", "5"},
		{"POST", "/admin/channels/5/streams/order", "reordered the streams of", "channel", "5"},
		{"POST", "/admin/channels/group", "moved channels to a group", "channel", ""},
		{"POST", "/admin/live/channels/5/stop", "stopped", "channel", "5"},
		{"POST", "/admin/live/viewers/9/kick", "disconnected", "viewer", "9"},
		{"POST", "/admin/users/3/password", "reset the password of", "user", "3"},
		{"POST", "/admin/users/3/token", "rotated the stream token of", "user", "3"},
		{"POST", "/admin/groups/fill", "filled channel groups", "channel group", ""},
		{"POST", "/admin/epg/sources/2/edit", "updated", "guide source", "2"},
		{"POST", "/admin/match/rules/4/delete", "deleted", "match rule", "4"},
		{"POST", "/admin/match/apply", "applied matches", "", ""},
		{"POST", "/admin/settings/match", "changed automatic matching", "settings", ""},
		{"POST", "/admin/backups/mmp-manual-20261017-120000.tar.gz/restore", "restored", "backup", "mmp-manual-20261017-120000.tar.gz"},
		{"GET", "/admin/backups/mmp-auto-20261017-030000.tar.gz/download", "downloaded", "backup", "mmp-auto-20261017-030000.tar.gz"},
		{"POST", "/admin/logout", "signed out", "", ""},
		{"POST", "/api/v1/providers", "created", "provider", ""},
		{"PUT", "/api/v1/providers/7", "updated", "provider", "7"},
		{"DELETE", "/api/v1/providers/7", "deleted", "provider", "7"},
		{"PUT", "/api/v1/providers/7/groups", "changed the groups of", "provider", "7"},
		{"DELETE", "/api/v1/tokens/3", "deleted", "API token", "3"},
		{"POST", "/api/v1/users/3/rotate-token", "rotated the stream token of", "user", "3"},
	} {
		d := Describe(tt.method, tt.path)
		if d.Action != tt.action || d.TargetType != tt.targetType || d.TargetID != tt.tid {
			t.Errorf("Describe(%s %s) = %q %q %q, want %q %q %q", tt.method, tt.path,
				d.Action, d.TargetType, d.TargetID, tt.action, tt.targetType, tt.tid)
		}
	}
}

func TestCreatedID(t *testing.T) {
	for loc, want := range map[string]string{
		"/admin/channels/12?msg=created": "12",
		"/admin/channels?msg=created":    "",
		"":                               "",
	} {
		if got := createdID(loc, "channels"); got != want {
			t.Errorf("createdID(%q) = %q, want %q", loc, got, want)
		}
	}
}

func TestAudited(t *testing.T) {
	for _, tt := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/admin/channels", false},
		{http.MethodGet, "/admin/backups/x/download", true},
		{http.MethodPost, "/admin/channels/1", true},
		{http.MethodOptions, "/api/v1/users", false},
	} {
		r, _ := http.NewRequest(tt.method, tt.path, nil)
		if got := audited(r); got != tt.want {
			t.Errorf("audited(%s %s) = %v", tt.method, tt.path, got)
		}
	}
}
