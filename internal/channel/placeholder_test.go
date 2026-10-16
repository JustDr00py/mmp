package channel

import "testing"

func TestIsPlaceholder(t *testing.T) {
	for _, name := range []string{
		"##### ABC IDAHO #####", "####### LNP PASS PPV #######", "### WOW SPORT ᴴᴰ ᴰᴼᴸᴮʸ ᴬᵁᴰᴵᴼ ###",
		"TR| ##### TR DINI KANALLARI #####", "  ===== UK SPORTS =====", "★★★ PPV ★★★", "US: ---- NEWS ----", "#####",
	} {
		if !IsPlaceholder(name) {
			t.Errorf("IsPlaceholder(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"US| WHAT IF... ᴿᴬᵂ", "24/7 MARRIED... WITH CHILDREN", "HT| FUSION TV...", "ESPN", "US: ESPN HD", "#1 Hits",
		"## Two hashes", "", "UK| BBC One",
	} {
		if IsPlaceholder(name) {
			t.Errorf("IsPlaceholder(%q) = true, want false", name)
		}
	}
}
