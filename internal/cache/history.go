package cache

import (
	"fmt"
	"time"
)

// HistoryWindow is how long hourly statistics are kept.
const HistoryWindow = 7 * 24 * time.Hour

// HourStats is the change of the counters within one hour.
type HourStats struct {
	Hour         time.Time // start of the hour
	Hits         int64
	Misses       int64
	HitBytes     int64
	Puts         int64
	PutBytes     int64
	Evictions    int64
	EvictedBytes int64
	EvictionRuns int64
	Sessions     int64
	PeakSize     int64 // largest accounted size seen in the hour
}

// Gets returns the number of lookups in the hour.
func (h HourStats) Gets() int64 { return h.Hits + h.Misses }

// MissRate returns misses per get, or 0 for an hour without gets.
func (h HourStats) MissRate() float64 {
	if h.Gets() == 0 {
		return 0
	}
	return float64(h.Misses) / float64(h.Gets())
}

// Add sums two periods. The peak is the larger of the two.
func (h HourStats) Add(o HourStats) HourStats {
	return HourStats{
		Hour:         h.Hour,
		Hits:         h.Hits + o.Hits,
		Misses:       h.Misses + o.Misses,
		HitBytes:     h.HitBytes + o.HitBytes,
		Puts:         h.Puts + o.Puts,
		PutBytes:     h.PutBytes + o.PutBytes,
		Evictions:    h.Evictions + o.Evictions,
		EvictedBytes: h.EvictedBytes + o.EvictedBytes,
		EvictionRuns: h.EvictionRuns + o.EvictionRuns,
		Sessions:     h.Sessions + o.Sessions,
		PeakSize:     max(h.PeakSize, o.PeakSize),
	}
}

// recordHistory adds the change of the counters since the previous record
// to the row of the current hour. Called with c.saveMu held.
func (c *Cache) recordHistory(snap CountersSnapshot) error {
	base := c.histBase
	c.mu.Lock()
	peak := c.peak
	c.peak = c.size
	c.mu.Unlock()
	if !snap.Since.Equal(base.Since) {
		// The counters were reset to zero since the last record, so
		// everything they hold now happened after the reset.
		base = CountersSnapshot{Since: snap.Since}
	}
	d := HourStats{
		Hits:         snap.Hits - base.Hits,
		Misses:       snap.Misses - base.Misses,
		HitBytes:     snap.HitBytes - base.HitBytes,
		Puts:         snap.Puts - base.Puts,
		PutBytes:     snap.PutBytes - base.PutBytes,
		Evictions:    snap.Evictions - base.Evictions,
		EvictedBytes: snap.EvictedBytes - base.EvictedBytes,
		EvictionRuns: snap.EvictionRuns - base.EvictionRuns,
		Sessions:     snap.Sessions - base.Sessions,
		PeakSize:     peak,
	}
	now := c.now()
	if err := c.idx.addHour(now.Truncate(time.Hour), d, now.Add(-HistoryWindow)); err != nil {
		return fmt.Errorf("hourly history: %w", err)
	}
	c.histBase = snap
	return nil
}

// History returns the hourly statistics of the last HistoryWindow, oldest
// first. Hours without activity have no row.
func (c *Cache) History() ([]HourStats, error) {
	return c.idx.loadHours(c.now().Add(-HistoryWindow))
}
