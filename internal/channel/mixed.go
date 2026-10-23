package channel

import "github.com/JustDr00py/mmp/internal/db"

// MixedChannel is a channel whose streams are of different local stations,
// usually joined by a tvg-id a provider copied between stations.
type MixedChannel struct {
	ID      int64
	Name    string
	Streams []MixedStream
}

// MixedStream is one stream of a MixedChannel.
type MixedStream struct {
	ID       int64
	Name     string
	Provider string
	CallSign string // "" when the stream names no station
}

// MixedStations returns the channels in rows (ordered by channel) whose
// streams carry two or more different call signs.
func MixedStations(rows []db.ListChannelStreamNamesRow) []MixedChannel {
	var out []MixedChannel
	for i := 0; i < len(rows); {
		ch := MixedChannel{ID: rows[i].ChannelID, Name: rows[i].ChannelName}
		calls := map[string]bool{}
		for ; i < len(rows) && rows[i].ChannelID == ch.ID; i++ {
			r := rows[i]
			cs := CallSign(r.StreamName, r.StreamTvgID)
			if cs != "" {
				calls[cs] = true
			}
			ch.Streams = append(ch.Streams, MixedStream{ID: r.StreamID, Name: r.StreamName, Provider: r.ProviderName, CallSign: cs})
		}
		if len(calls) > 1 {
			out = append(out, ch)
		}
	}
	return out
}
