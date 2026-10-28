package cache

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// Counters are cumulative statistics kept across daemon restarts.
type Counters struct {
	Since atomic.Int64 // unix nanos of the first start or last reset

	Hits     atomic.Int64
	Misses   atomic.Int64
	HitBytes atomic.Int64
	Puts     atomic.Int64
	PutBytes atomic.Int64

	Evictions      atomic.Int64 // index entries removed by eviction
	EvictedBytes   atomic.Int64
	Cleans         atomic.Int64 // explicit clean commands
	ExternalCleans atomic.Int64 // "go clean -cache" detected

	RemoteHits   atomic.Int64
	RemoteMisses atomic.Int64
	RemotePuts   atomic.Int64
	RemoteErrors atomic.Int64

	Sessions atomic.Int64 // go command sessions served
}

// CountersSnapshot is a plain copy of Counters.
type CountersSnapshot struct {
	Since          time.Time
	Hits           int64
	Misses         int64
	HitBytes       int64
	Puts           int64
	PutBytes       int64
	Evictions      int64
	EvictedBytes   int64
	Cleans         int64
	ExternalCleans int64
	RemoteHits     int64
	RemoteMisses   int64
	RemotePuts     int64
	RemoteErrors   int64
	Sessions       int64
}

// Snapshot copies the counters.
func (c *Counters) Snapshot() CountersSnapshot {
	return CountersSnapshot{
		Since:          time.Unix(0, c.Since.Load()),
		Hits:           c.Hits.Load(),
		Misses:         c.Misses.Load(),
		HitBytes:       c.HitBytes.Load(),
		Puts:           c.Puts.Load(),
		PutBytes:       c.PutBytes.Load(),
		Evictions:      c.Evictions.Load(),
		EvictedBytes:   c.EvictedBytes.Load(),
		Cleans:         c.Cleans.Load(),
		ExternalCleans: c.ExternalCleans.Load(),
		RemoteHits:     c.RemoteHits.Load(),
		RemoteMisses:   c.RemoteMisses.Load(),
		RemotePuts:     c.RemotePuts.Load(),
		RemoteErrors:   c.RemoteErrors.Load(),
		Sessions:       c.Sessions.Load(),
	}
}

func (c *Counters) restore(s CountersSnapshot) {
	c.Since.Store(s.Since.UnixNano())
	c.Hits.Store(s.Hits)
	c.Misses.Store(s.Misses)
	c.HitBytes.Store(s.HitBytes)
	c.Puts.Store(s.Puts)
	c.PutBytes.Store(s.PutBytes)
	c.Evictions.Store(s.Evictions)
	c.EvictedBytes.Store(s.EvictedBytes)
	c.Cleans.Store(s.Cleans)
	c.ExternalCleans.Store(s.ExternalCleans)
	c.RemoteHits.Store(s.RemoteHits)
	c.RemoteMisses.Store(s.RemoteMisses)
	c.RemotePuts.Store(s.RemotePuts)
	c.RemoteErrors.Store(s.RemoteErrors)
	c.Sessions.Store(s.Sessions)
}

// Reset zeroes the counters.
func (c *Counters) Reset() {
	c.restore(CountersSnapshot{Since: time.Now()})
}

func loadCounters(path string) *Counters {
	c := new(Counters)
	var s CountersSnapshot
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &s) == nil && !s.Since.IsZero() {
		c.restore(s)
	} else {
		c.Since.Store(time.Now().UnixNano())
	}
	return c
}

func (c *Counters) save(path, tmpDir string, snap CountersSnapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, "stats-")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
