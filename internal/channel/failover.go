package channel

import "slices"

// FailoverEntry is a stream on a failover list with its provider's
// priority (lower is preferred).
type FailoverEntry struct {
	StreamID         int64
	ProviderPriority int32
}

// SlotByProvider merges added into list, a failover list, and returns the
// stream ids in their new order. list keeps its order (including any
// order set by hand); each added stream, in turn, goes after the last
// stream whose provider priority is the same or better, so it joins the
// end of its provider's level.
func SlotByProvider(list, added []FailoverEntry) []int64 {
	out := slices.Clone(list)
	for _, a := range added {
		pos := 0
		for i, e := range out {
			if e.ProviderPriority <= a.ProviderPriority {
				pos = i + 1
			}
		}
		out = slices.Insert(out, pos, a)
	}
	ids := make([]int64, len(out))
	for i, e := range out {
		ids[i] = e.StreamID
	}
	return ids
}
