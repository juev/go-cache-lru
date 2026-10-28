package cache

import (
	"os"
	"testing"
	"time"
)

func waitRows(t *testing.T, c *Cache, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := c.idx.load()
		if err == nil && len(rows) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("index has %d rows (err %v), want %d", len(rows), err, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIndexFlushesWhenChangesArrive(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	put(t, c, l, action(1), []byte("a"))
	waitRows(t, c, 1)
	// The goroutine went back to sleep; a later change must wake it again.
	put(t, c, l, action(2), []byte("b"))
	l.Release()
	waitRows(t, c, 2)
}

func TestIndexRetriesFailedFlush(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	if _, err := c.idx.db.Exec(`ALTER TABLE entries RENAME TO broken`); err != nil {
		t.Fatal(err)
	}
	l := c.NewLease()
	put(t, c, l, action(1), []byte("a"))
	l.Release()
	time.Sleep(5 * flushInterval) // at least one failed attempt
	if _, err := c.idx.db.Exec(`ALTER TABLE broken RENAME TO entries`); err != nil {
		t.Fatal(err)
	}
	// No further change arrives: the retry must come on its own.
	waitRows(t, c, 1)
}

func TestSaveStatsSkipsUnchangedCounters(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.statsPath()); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.statsPath()); !os.IsNotExist(err) {
		t.Fatalf("unchanged counters written again: %v", err)
	}
	c.Counters().Hits.Add(1)
	if err := c.SaveStats(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.statsPath()); err != nil {
		t.Fatalf("changed counters not written: %v", err)
	}
}
