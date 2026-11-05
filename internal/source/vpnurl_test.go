package source

import (
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestProviderURL(t *testing.T) {
	defer SetBehindVPN(false)
	both := db.Provider{Url: "http://panel.example", VpnUrl: "http://vpn.panel.example"}
	plain := db.Provider{Url: "http://panel.example"}
	SetBehindVPN(false)
	if ProviderURL(both) != both.Url {
		t.Error("without the VPN the regular URL is used")
	}
	SetBehindVPN(true)
	if ProviderURL(both) != both.VpnUrl {
		t.Error("behind the VPN the VPN URL is used")
	}
	if ProviderURL(plain) != plain.Url {
		t.Error("no VPN URL: the regular one, VPN or not")
	}
	if ValidateVPNURL("") != nil || ValidateVPNURL("http://vpn.example:8080") != nil || ValidateVPNURL("vpn.example") == nil {
		t.Error("ValidateVPNURL")
	}
}
