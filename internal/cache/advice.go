package cache

import (
	"fmt"
	"slices"
	"time"
)

// Thresholds of the size advice. They are a first guess, to be revised
// once real histories are available.
const (
	adviceMinActiveHours = 24  // history needed before any advice
	adviceMinEvictions   = 3   // evictions with a following active hour
	adviceMissRatio      = 1.5 // miss rate after evictions vs the usual one
	adviceLowerBelow     = 0.6 // peak share of --max-size that suggests lowering it
	adviceLowerHeadroom  = 1.25
	adviceRaiseFactor    = 1.5
)

// Advice kinds.
const (
	AdviceCollecting = "collecting" // not enough history
	AdviceFits       = "fits"
	AdviceLower      = "lower"
	AdviceRaise      = "raise"
)

// Advice is a hint about --max-size derived from the hourly history. It
// compares miss rates around evictions; misses also come from code changes
// and branch switches, so the numbers behind it are part of the result.
type Advice struct {
	Kind        string
	Text        string
	ActiveHours int   // hours with at least one get
	PeakSize    int64 // largest size in the history
	// Evictions compared: automatic evictions followed by an active hour.
	EvictionsCompared int
	MissRateAfter     float64 // median over the hours after evictions
	MissRateUsual     float64 // median over the other active hours
	// Suggested limits for AdviceLower and AdviceRaise.
	MaxSize, TargetSize int64
}

// Advise derives the advice from hours (oldest first, as History returns
// them) for a cache with the given limits.
func Advise(hours []HourStats, maxSize, targetSize int64) Advice {
	var a Advice
	evicted := false
	byHour := make(map[time.Time]HourStats, len(hours))
	for _, h := range hours {
		byHour[h.Hour] = h
		a.PeakSize = max(a.PeakSize, h.PeakSize)
		if h.Gets() > 0 {
			a.ActiveHours++
		}
		if h.EvictionRuns > 0 {
			evicted = true
		}
	}
	if a.ActiveHours < adviceMinActiveHours {
		a.Kind = AdviceCollecting
		a.Text = fmt.Sprintf("not enough history yet: %d of %d active hours", a.ActiveHours, adviceMinActiveHours)
		return a
	}
	if !evicted {
		if float64(a.PeakSize) < adviceLowerBelow*float64(maxSize) {
			a.Kind = AdviceLower
			a.MaxSize, a.TargetSize = scaleLimits(maxSize, targetSize, float64(a.PeakSize)*adviceLowerHeadroom/float64(maxSize))
			a.Text = fmt.Sprintf("the cache never reached the limit in %s (peak %s of %s); --max-size=%s --target-size=%s would do",
				historyLabel, gib(a.PeakSize), gib(maxSize), flagSize(a.MaxSize), flagSize(a.TargetSize))
			return a
		}
		a.Kind = AdviceFits
		a.Text = fmt.Sprintf("limit fits: no evictions in %s, peak %s of %s", historyLabel, gib(a.PeakSize), gib(maxSize))
		return a
	}

	// The hour after an eviction is compared with active hours that neither
	// had an eviction nor followed one.
	after := map[time.Time]bool{}
	var afterRates, usualRates []float64
	for _, h := range hours {
		if h.EvictionRuns == 0 {
			continue
		}
		next, ok := byHour[h.Hour.Add(time.Hour)]
		if ok && next.Gets() > 0 {
			after[next.Hour] = true
			afterRates = append(afterRates, next.MissRate())
		}
	}
	for _, h := range hours {
		if h.Gets() > 0 && h.EvictionRuns == 0 && !after[h.Hour] {
			usualRates = append(usualRates, h.MissRate())
		}
	}
	a.EvictionsCompared = len(afterRates)
	if len(afterRates) < adviceMinEvictions || len(usualRates) == 0 {
		a.Kind = AdviceFits
		a.Text = fmt.Sprintf("no sign the limit is too small yet: %d evictions can be compared, %d needed",
			len(afterRates), adviceMinEvictions)
		return a
	}
	a.MissRateAfter, a.MissRateUsual = median(afterRates), median(usualRates)
	ratio := "the same as"
	switch {
	case a.MissRateUsual > 0:
		ratio = fmt.Sprintf("%.1fx", a.MissRateAfter/a.MissRateUsual)
	case a.MissRateAfter > 0:
		ratio = "above"
	}
	if a.MissRateAfter > 0 && a.MissRateAfter >= adviceMissRatio*a.MissRateUsual {
		a.Kind = AdviceRaise
		a.MaxSize, a.TargetSize = scaleLimits(maxSize, targetSize, adviceRaiseFactor)
		a.Text = fmt.Sprintf("limit looks too small: after %d evictions misses were %s the usual rate (%.1f%% vs %.1f%%); try --max-size=%s --target-size=%s",
			a.EvictionsCompared, ratio, 100*a.MissRateAfter, 100*a.MissRateUsual, flagSize(a.MaxSize), flagSize(a.TargetSize))
		return a
	}
	a.Kind = AdviceFits
	a.Text = fmt.Sprintf("limit fits: after %d evictions misses were %s the usual rate (%.1f%% vs %.1f%%)",
		a.EvictionsCompared, ratio, 100*a.MissRateAfter, 100*a.MissRateUsual)
	return a
}

const historyLabel = "7 days"

// scaleLimits multiplies --max-size by f, rounds it up to a flag-friendly
// value and keeps the ratio of --target-size to it.
func scaleLimits(maxSize, targetSize int64, f float64) (int64, int64) {
	m := roundUp(int64(float64(maxSize) * f))
	t := int64(float64(m) * float64(targetSize) / float64(maxSize))
	return m, roundDown(min(t, m))
}

const (
	mib = int64(1) << 20
	gb  = int64(1) << 30
)

func unitFor(n int64) int64 {
	if n >= gb {
		return gb
	}
	return 64 * mib
}

func roundUp(n int64) int64 {
	u := unitFor(n)
	return max((n+u-1)/u*u, u)
}

func roundDown(n int64) int64 {
	u := unitFor(n)
	return max(n/u*u, 64*mib)
}

// flagSize renders a rounded size the way --max-size accepts it.
func flagSize(n int64) string {
	if n%gb == 0 {
		return fmt.Sprintf("%dGiB", n/gb)
	}
	return fmt.Sprintf("%dMiB", n/mib)
}

func gib(n int64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/float64(gb))
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
