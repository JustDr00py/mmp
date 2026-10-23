package channel

import (
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestCallSign(t *testing.T) {
	tests := []struct{ name, tvg, want string }{
		// The same station at five providers.
		{"US - CBS 12 JACKSON MS (WJTV) FHD", "cbs12wjtv.us", "cbs/wjtv"},
		{"|US| CBS 12 (WJTV) Jackson ᴴᴰ", "", "cbs/wjtv"},
		{"US: CBS 12 (WJTV) JACKSON HD", "", "cbs/wjtv"},
		{"US| CBS 12 JACKSON MS (WJTV)", "loc.CBS (WJTV) Jackson, MS", "cbs/wjtv"},
		{"US ★ CBS 12 JACKSON MS HD", "CBSWJTV.us", "cbs/wjtv"},
		// Bare call signs next to a network name.
		{"TV: SEATTLE, WA KOMO ABC 4 A3 HD SEATTLE ᶜᶦᵗʸ ᴿᴬᵂ", "", "abc/komo"},
		{"RK: KTRK ABC13 HOUSTON ᴿᴬᵂ", "", "abc/ktrk"},
		// Subchannels are other channels; -DT is the main one.
		{"TV: WHEELING, WV  WTOV-D2 FOX 10 A3 HD CLEVELAND", "", "fox/wtov-2"},
		{"US - CBS 7 JACKSON (WBBJ-DT3) FHD", "cbs12wjtv.us", "cbs/wbbj-3"}, // the name wins
		{"US - ABC 21 HELENA (KHBB-DT)", "", "abc/khbb"},
		{"|US| NBC (WPXIDT)", "", "nbc/wpxi"},
		{"US ★ NBC 16 LAFAYETTE HD", "NBCWPBIDT2.us", "nbc/wpbi-2"},
		// Three-letter call signs.
		{"US - CBS 3 PHILADELPHIA PA (KYW) FHD", "", "cbs/kyw"},
		{"CBS PHILADELPHIA", "cbs3kyw.us", "cbs/kyw"},
		// Glued tvg-ids.
		{"|US| ABC 13 ᴴᴰ [NORFOLK]", "abc13ktrktv.us", "abc/ktrk"},
		{"US ★ FOX 5 PLUS WASHINGTON", "Fox5PlusWDCA.us", "fox/wdca"},
		{"US: CW SAN FRANCISCO HD", "KMAX.us", "cw/kmax"},
		// One call sign, several networks; the network from the tvg-id.
		{"|US| MeTV (KITV)", "", "metv/kitv"},
		{"US ★ 13 HOUSTON", "ABCKTRK.us", "abc/ktrk"},
		{"KTRK", "KTRK.us", "ktrk"},
		{"TV: SEATTLE, WA KONG IND 16", "", "ind/kong"},
		// Not stations.
		{"PLAY KIDZ ABC HD", "", ""},
		{"FOX KIDS", "", ""},
		{"WWE NETWORK", "", ""},
		{"NBC WEST HD", "", ""},
		{"UK - SKY WITNESS FHD", "skywitness.uk", ""},
		{"NFL: RedZone", "", ""},
		{"KTLA 5 LOS ANGELES", "", ""}, // no network named
		{"DE - KIKA HD", "kika.de", ""},
		{"|US| WE ᴴᴰ", "wetv.us", ""},
		{"AU| KAYO - FOX SPORTS 502", "", ""},
		{"Discovery (West)", "", ""},
		{"FOX DEPORTES", "foxdeportes.us", ""},
		{"ESPN PLUS 99", "ESPN.PLUS.99", ""},
	}
	for _, tt := range tests {
		if got := CallSign(tt.name, tt.tvg); got != tt.want {
			t.Errorf("CallSign(%q, %q) = %q, want %q", tt.name, tt.tvg, got, tt.want)
		}
	}
}

func TestMixedStations(t *testing.T) {
	rows := []db.ListChannelStreamNamesRow{
		{ChannelID: 1, ChannelName: "NBC 3 LAS VEGAS (KSNV)", StreamID: 10, StreamName: "US - NBC 3 LAS VEGAS NV (KSNV) FHD"},
		{ChannelID: 1, ChannelName: "NBC 3 LAS VEGAS (KSNV)", StreamID: 11, StreamName: "US - NBC 20 FORT MYERS FL (WBBH) FHD", StreamTvgID: "nbcksnv.us"},
		{ChannelID: 2, ChannelName: "CBS (KYW)", StreamID: 20, StreamName: "|US| CBS 3 (KYW) Philadelphia"},
		{ChannelID: 2, ChannelName: "CBS (KYW)", StreamID: 21, StreamName: "CBS PHILADELPHIA HD"},
		{ChannelID: 3, ChannelName: "CNN", StreamID: 30, StreamName: "CNN"},
	}
	got := MixedStations(rows)
	if len(got) != 1 || got[0].ID != 1 || len(got[0].Streams) != 2 || got[0].Streams[1].CallSign != "nbc/wbbh" {
		t.Errorf("MixedStations = %+v, want channel 1 with ksnv and wbbh", got)
	}
}
