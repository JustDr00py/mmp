package channel

import "testing"

func TestSplitSlot(t *testing.T) {
	tests := []struct{ in, label, event string }{
		{"US - ESPN PLUS 01 : 2026 WSL CHAMPIONSHIP TOUR – MEO RIP CURL PRO PORTUGAL OCT 22 – 3:30 AM ET / 8:30 AM UK",
			"US - ESPN PLUS 01", "2026 WSL CHAMPIONSHIP TOUR – MEO RIP CURL PRO PORTUGAL OCT 22 – 3:30 AM ET / 8:30 AM UK"},
		{"US -  ESPN PLUS 172 : BRISBANE @ GLORY OCT 23 – 4:55 AM ET / 9:55 AM UK", "US - ESPN PLUS 172",
			"BRISBANE @ GLORY OCT 23 – 4:55 AM ET / 9:55 AM UK"},
		{"US - PARAMOUNT 42 :", "US - PARAMOUNT 42", ""},
		{"US ★ Flo 450 : [FC:2025 Mount Union vs Heidelberg _ Men`s] (2025-10-18 18:00:00)", "US ★ Flo 450",
			"[FC:2025 Mount Union vs Heidelberg _ Men`s] (2025-10-18 18:00:00)"},
		{"Flo (FLSP) 100: Jim Koch Wisconsin Open - 02/12 15:00", "Flo (FLSP) 100", "Jim Koch Wisconsin Open - 02/12 15:00"},
		{"Flo Hockey 01 :AHL: Ontario vs Calgary @ Feb 15 4:00 PM", "Flo Hockey 01", "AHL: Ontario vs Calgary @ Feb 15 4:00 PM"},
		{"College: SMU vs Arizona State @ Oct 22 5:00 PM :ESPN+  14", "ESPN+ 14", "College: SMU vs Arizona State @ Oct 22 5:00 PM"},
		{"WTA 500: Ningbo Day 4 @ Oct 22 6:00 AM :Stan Sports  01", "Stan Sports 01", "WTA 500: Ningbo Day 4 @ Oct 22 6:00 AM"},
		{":Kayo  100", "Kayo 100", ""},
		{"IE - GAATV 06 : ANTRIM – NAOMH SEAMUS @ NAOMH EANNA 2026", "IE - GAATV 06", "ANTRIM – NAOMH SEAMUS @ NAOMH EANNA 2026"},
		{"Paramount+ 44 :", "Paramount+ 44", ""},
		{"US ★ ESPN+ 2:", "US ★ ESPN+ 2", ""},
		{"ENDED | GRAND PRIX LYON (250) | Thu 22 Oct 14:00 CEST (SE) | 8K EXCLUSIVE | SE: TV4 PLAY PPV 6", "SE: TV4 PLAY PPV 6",
			"GRAND PRIX LYON (250) | Thu 22 Oct 14:00 CEST (SE) | 8K EXCLUSIVE"},
		{"S-EUR 001|  Adler Mannheim vs. Löwen Frankfurt - MagentaSport | Ice hockey", "S-EUR 001",
			"Adler Mannheim vs. Löwen Frankfurt - MagentaSport | Ice hockey"},
		{"CA (SN+ 001) | Sportsnet Central: Oct. 21 (2026-10-22 05:00:00)", "CA (SN+ 001)",
			"Sportsnet Central: Oct. 21 (2026-10-22 05:00:00)"},
		{"CA | CFL 00ⓧ: Montreal Alouettes vs Hamilton Tiger-Cats | Sat 8th Nov", "CA | CFL 00ⓧ",
			"Montreal Alouettes vs Hamilton Tiger-Cats | Sat 8th Nov"},
		{"QMJHL 00: Shawinigan @ Acadie-Bathurst - #451 | Fri 14th 11:00PM", "QMJHL 00",
			"Shawinigan @ Acadie-Bathurst - #451 | Fri 14th 11:00PM"},
	}
	for _, tt := range tests {
		label, event, ok := SplitSlot(tt.in)
		if !ok || label != tt.label || event != tt.event {
			t.Errorf("SplitSlot(%q) = %q, %q, %v; want %q, %q", tt.in, label, event, ok, tt.label, tt.event)
		}
	}
	for _, name := range []string{
		"US| ESPN 2 ᴴᴰ ⁶⁰ᶠᵖˢ", "US: ESPN HD", "ESPN 2 | East", "TV| ESPN+ 2 ᴿᴬᵂ",
		"Next | 2026-10-22 | 23:00 (GMT) | BKFC Fight Night Clearwater", "Live | Ararat-Armenia vs AZ | UEFA Europa League | CA",
		"DE | No Event", "Formula 1 - Andrea Kimi Antonelli: Onboard", "ESPN2: SportsCenter", "BBC One 12:30",
		"RS: Happy :) Parovi Uzivo 24h Kanal 1", "|TW|DAZN 3 HD (D): NPB 2026: Rakuten vs Lotte",
		"Fite TV 13 HD (D): TrillerTV-BKFC BKFC Manchester 2025: Jonno Chipchase vs Gary Fox - Sat,29 Mar 15:00 ET",
	} {
		if label, event, ok := SplitSlot(name); ok {
			t.Errorf("SplitSlot(%q) = %q, %q; want no slot", name, label, event)
		}
	}
}

func TestMatchNamesSlotsByLabel(t *testing.T) {
	streams := []Stream{
		{ID: 1, ProviderID: 1, Name: "US - ESPN PLUS 01 : THE GOLICS OCT 22 – 10:00 AM ET / 3:00 PM UK", TvgID: "ESPNPlus.1"},
		{ID: 2, ProviderID: 2, Name: "US - ESPN PLUS 01 : THE GOLICS OCT 22 – 10:00 AM ET / 3:00 PM UK", TvgID: "ESPNPlus.1"},
		{ID: 3, ProviderID: 3, Name: "College: SMU vs Arizona State @ Oct 22 5:00 PM :ESPN+  14"},
	}
	names := map[string]bool{}
	for _, p := range Match(streams, nil, nil, nil) {
		names[p.Name] = true
	}
	if !names["ESPN PLUS 01"] || !names["ESPN+ 14"] || len(names) != 2 {
		t.Errorf("names = %v, want ESPN PLUS 01 and ESPN+ 14", names)
	}
}
