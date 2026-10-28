package cache

import (
	"strings"
	"testing"
	"time"
)

// hours builds n consecutive active hours with the given miss rate, each
// with 1000 gets.
func hours(start time.Time, n int, missRate float64, peak int64) []HourStats {
	var res []HourStats
	for i := range n {
		misses := int64(missRate * 1000)
		res = append(res, HourStats{Hour: start.Add(time.Duration(i) * time.Hour), Hits: 1000 - misses, Misses: misses, PeakSize: peak})
	}
	return res
}

func TestAdvise(t *testing.T) {
	const max, target = 30 * gb, 25 * gb
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

	// Every eighth hour has an eviction; the hour after it has missRate.
	withEvictions := func(after float64) []HourStats {
		h := hours(start, 48, 0.01, 29*gb)
		for i := 4; i+1 < len(h); i += 8 {
			h[i].EvictionRuns = 1
			misses := int64(after * 1000)
			h[i+1].Hits, h[i+1].Misses = 1000-misses, misses
		}
		return h
	}

	tests := []struct {
		name     string
		hours    []HourStats
		kind     string
		max, tgt int64
		text     string
	}{
		{"short history", hours(start, 10, 0.01, gb), AdviceCollecting, 0, 0, "10 of 24 active hours"},
		{"idle hours do not count", append(hours(start, 23, 0.01, gb), HourStats{Hour: start.Add(30 * time.Hour), Puts: 5}), AdviceCollecting, 0, 0, "23 of 24"},
		{"small peak", hours(start, 30, 0.01, 4*gb), AdviceLower, 5 * gb, 4 * gb, "--max-size=5GiB --target-size=4GiB"},
		{"peak near the limit", hours(start, 30, 0.01, 20*gb), AdviceFits, 0, 0, "no evictions"},
		{"evictions without harm", withEvictions(0.012), AdviceFits, 0, 0, "1.2x the usual rate"},
		{"evictions that cost hits", withEvictions(0.03), AdviceRaise, 45 * gb, 37 * gb, "--max-size=45GiB --target-size=37GiB"},
		{"misses only after evictions", func() []HourStats {
			h := withEvictions(0.02)
			for i := range h {
				if h[i].EvictionRuns == 0 && (i == 0 || h[i-1].EvictionRuns == 0) {
					h[i].Hits, h[i].Misses = 1000, 0
				}
			}
			return h
		}(), AdviceRaise, 45 * gb, 37 * gb, "misses were above the usual rate (2.0% vs 0.0%)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Advise(tt.hours, max, target)
			if a.Kind != tt.kind || a.MaxSize != tt.max || a.TargetSize != tt.tgt || !strings.Contains(a.Text, tt.text) {
				t.Fatalf("got %+v\nwant kind %s, limits %d/%d, text with %q", a, tt.kind, tt.max, tt.tgt, tt.text)
			}
		})
	}
}

func TestAdviseNeedsComparableEvictions(t *testing.T) {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	h := hours(start, 30, 0.01, 29*gb)
	h[5].EvictionRuns = 1
	h[6].Misses, h[6].Hits = 500, 500 // one bad hour is not enough
	// An eviction in the last hour has no following hour to compare.
	h[29].EvictionRuns = 1
	a := Advise(h, 30*gb, 25*gb)
	if a.Kind != AdviceFits || a.EvictionsCompared != 1 || !strings.Contains(a.Text, "1 evictions can be compared, 3 needed") {
		t.Fatalf("got %+v", a)
	}
}
