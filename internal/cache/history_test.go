package cache

import (
	"testing"
	"time"
)

func history(t *testing.T, c *Cache) []HourStats {
	t.Helper()
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	h, err := c.History()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHistoryRecordsHours(t *testing.T) {
	start := time.Date(2026, 10, 28, 10, 30, 0, 0, time.UTC)
	clk := &clock{t: start}
	c := openClock(t, t.TempDir(), 1<<30, 1<<29, clk)
	l := c.NewLease()
	put(t, c, l, action(1), body(1, 1000))
	mustHit(t, c, l, action(1), body(1, 1000))
	mustMiss(t, c, action(2))
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}

	clk.advance(time.Hour)
	put(t, c, l, action(2), body(2, 5000))
	l.Release()
	h := history(t, c)

	if len(h) != 2 {
		t.Fatalf("rows: %+v", h)
	}
	first, second := h[0], h[1]
	if !first.Hour.Equal(start.Truncate(time.Hour)) || !second.Hour.Equal(first.Hour.Add(time.Hour)) {
		t.Fatalf("hours %v %v", first.Hour, second.Hour)
	}
	if first.Puts != 1 || first.Hits != 1 || first.Misses != 1 || first.PutBytes != 1000 {
		t.Errorf("first hour: %+v", first)
	}
	if second.Puts != 1 || second.Hits != 0 || second.Misses != 0 || second.PutBytes != 5000 {
		t.Errorf("second hour: %+v", second)
	}
	if second.PeakSize != usage(1000)+usage(5000) {
		t.Errorf("peak %d, want %d", second.PeakSize, usage(1000)+usage(5000))
	}
	// Nothing changed: no new row and no change to the old ones.
	if again := history(t, c); len(again) != 2 || again[1] != second {
		t.Fatalf("idle save changed the history: %+v", again)
	}
}

func TestHistorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
	c := openClock(t, dir, 1<<30, 1<<29, clk)
	l := c.NewLease()
	put(t, c, l, action(1), []byte("a"))
	l.Release()
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := openClock(t, dir, 1<<30, 1<<29, clk)
	l = c2.NewLease()
	put(t, c2, l, action(2), []byte("b"))
	l.Release()
	h := history(t, c2)
	// The restart must neither lose the first put nor count it twice.
	if len(h) != 1 || h[0].Puts != 2 {
		t.Fatalf("history after restart: %+v", h)
	}
}

func TestHistoryRetention(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	c := openClock(t, t.TempDir(), 1<<30, 1<<29, clk)
	l := c.NewLease()
	put(t, c, l, action(1), []byte("old"))
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	clk.advance(HistoryWindow + 2*time.Hour)
	put(t, c, l, action(2), []byte("new"))
	l.Release()
	h := history(t, c)
	if len(h) != 1 || h[0].Puts != 1 {
		t.Fatalf("rows older than %v kept: %+v", HistoryWindow, h)
	}
	var n int
	if err := c.idx.db.QueryRow(`SELECT count(*) FROM hourly`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("table has %d rows (err %v), want the expired one deleted", n, err)
	}
}

func TestHistoryKeepsActivityAcrossCounterReset(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	put(t, c, l, action(1), []byte("a"))
	put(t, c, l, action(2), []byte("b"))
	l.Release()
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	c.Counters().Reset()
	l = c.NewLease()
	put(t, c, l, action(3), []byte("c"))
	l.Release()
	// Two puts before the reset and one after: all three are history.
	var puts int64
	for _, h := range history(t, c) {
		puts += h.Puts
	}
	if puts != 3 {
		t.Fatalf("history holds %d puts, want 3", puts)
	}
}

func TestEvictionRunsCounted(t *testing.T) {
	const obj = 10 * blockSize
	c := open(t, t.TempDir(), 3*(obj+blockSize), 2*(obj+blockSize))
	l := c.NewLease()
	for i := range 6 {
		put(t, c, l, action(i), body(i, obj))
	}
	l.Release()
	c.evictIfNeeded()
	c.evictIfNeeded() // already below the limit: not a run
	if runs := c.Counters().EvictionRuns.Load(); runs != 1 {
		t.Fatalf("eviction runs %d, want 1", runs)
	}
	h := history(t, c)
	if len(h) == 0 || h[len(h)-1].EvictionRuns != 1 {
		t.Fatalf("history: %+v", h)
	}
}
