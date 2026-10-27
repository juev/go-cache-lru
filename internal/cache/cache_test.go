package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func open(t *testing.T, dir string, maxSize, target int64) *Cache {
	t.Helper()
	clk := &clock{t: time.Now().Add(-time.Hour)}
	c, err := Open(Options{Dir: dir, MaxSize: maxSize, TargetSize: target, Now: clk.Now, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func action(i int) []byte {
	h := sha256.Sum256(fmt.Appendf(nil, "action-%d", i))
	return h[:]
}

func put(t *testing.T, c *Cache, l *Lease, a []byte, body []byte) Entry {
	t.Helper()
	out := sha256.Sum256(body)
	e, err := c.Put(l, a, out[:], int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustHit(t *testing.T, c *Cache, l *Lease, a []byte, want []byte) {
	t.Helper()
	e, ok, err := c.Get(l, a)
	if err != nil || !ok {
		t.Fatalf("get %x: ok=%v err=%v", a[:4], ok, err)
	}
	got, err := os.ReadFile(e.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("get %x: body mismatch", a[:4])
	}
	if sum := sha256.Sum256(want); !bytes.Equal(e.OutputID, sum[:]) {
		t.Fatalf("get %x: output ID mismatch", a[:4])
	}
}

func mustMiss(t *testing.T, c *Cache, a []byte) {
	t.Helper()
	l := c.NewLease()
	defer l.Release()
	if _, ok, err := c.Get(l, a); err != nil || ok {
		t.Fatalf("get %x: want miss, got ok=%v err=%v", a[:4], ok, err)
	}
}

func body(i, n int) []byte {
	return bytes.Repeat([]byte{byte(i)}, n)
}

func TestPutGet(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	defer l.Release()
	mustMiss(t, c, action(1))
	put(t, c, l, action(1), []byte("hello"))
	mustHit(t, c, l, action(1), []byte("hello"))
	put(t, c, l, action(2), nil)
	mustHit(t, c, l, action(2), nil)

	s := c.Snapshot()
	if s.Counters.Hits != 2 || s.Counters.Misses != 1 || s.Counters.Puts != 2 {
		t.Fatalf("counters: %+v", s.Counters)
	}
}

func TestRejectsCorruptBody(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	defer l.Release()
	sum := sha256.Sum256([]byte("good"))
	if _, err := c.Put(l, action(1), sum[:], 3, bytes.NewReader([]byte("bad"))); err == nil {
		t.Fatal("want hash mismatch error")
	}
	if _, err := c.Put(l, action(1), sum[:], 10, bytes.NewReader([]byte("good"))); err == nil {
		t.Fatal("want size mismatch error")
	}
	mustMiss(t, c, action(1))
	if c.Size() != 0 {
		t.Fatalf("size %d after failed puts", c.Size())
	}
}

func TestDedupSharesObject(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	b := body(1, 10000)
	e1 := put(t, c, l, action(1), b)
	e2 := put(t, c, l, action(2), b)
	if e1.Path != e2.Path {
		t.Fatalf("paths differ: %s %s", e1.Path, e2.Path)
	}
	l.Release()
	if s := c.Snapshot(); s.Entries != 2 || s.Objects != 1 {
		t.Fatalf("snapshot %+v", s)
	}
	// Dropping one entry keeps the object for the other.
	c.mu.Lock()
	c.removeEntryLocked(c.entries[fmt.Sprintf("%x", action(1))])
	c.mu.Unlock()
	l = c.NewLease()
	defer l.Release()
	mustHit(t, c, l, action(2), b)
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	const obj = 100 * blockSize
	// Room for about 5 objects; shrink to about 3.
	c := open(t, t.TempDir(), 5*(obj+blockSize), 3*(obj+blockSize))
	for i := range 5 {
		l := c.NewLease()
		put(t, c, l, action(i), body(i, obj))
		l.Release()
	}
	// Touch 0 and 1 so 2, 3 and 4 become the oldest.
	for i := range 2 {
		l := c.NewLease()
		mustHit(t, c, l, action(i), body(i, obj))
		l.Release()
	}
	l := c.NewLease()
	put(t, c, l, action(5), body(5, obj)) // over the limit now
	l.Release()
	c.evictIfNeeded()

	for _, i := range []int{2, 3, 4} {
		mustMiss(t, c, action(i))
	}
	for _, i := range []int{0, 1, 5} {
		l := c.NewLease()
		mustHit(t, c, l, action(i), body(i, obj))
		l.Release()
	}
	if got := c.Snapshot().Counters.Evictions; got != 3 {
		t.Fatalf("evictions = %d, want 3", got)
	}
	if c.Size() > 3*(obj+blockSize) {
		t.Fatalf("size %d above target", c.Size())
	}
}

func TestPinnedObjectsSurviveEviction(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	held := c.NewLease()
	e := put(t, c, held, action(1), []byte("in use"))
	other := c.NewLease()
	put(t, c, other, action(2), []byte("free"))
	other.Release()

	c.Clean()
	if _, err := os.Stat(e.Path); err != nil {
		t.Fatalf("pinned object deleted: %v", err)
	}
	mustMiss(t, c, action(2))

	held.Release()
	c.Clean()
	if _, err := os.Stat(e.Path); !os.IsNotExist(err) {
		t.Fatalf("object still present after release and clean: %v", err)
	}
	if c.Size() != 0 {
		t.Fatalf("size %d after clean", c.Size())
	}
}

func TestReplacedOutputDeletedAfterRelease(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	held := c.NewLease()
	old := put(t, c, held, action(1), []byte("v1"))
	l := c.NewLease()
	put(t, c, l, action(1), []byte("v2"))
	l.Release()
	if _, err := os.Stat(old.Path); err != nil {
		t.Fatalf("pinned old output deleted early: %v", err)
	}
	held.Release()
	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatalf("old output kept after release: %v", err)
	}
	l = c.NewLease()
	defer l.Release()
	mustHit(t, c, l, action(1), []byte("v2"))
}

func TestReopenKeepsEntriesAndOrder(t *testing.T) {
	dir := t.TempDir()
	const obj = 10 * blockSize
	c := open(t, dir, 1<<30, 1<<29)
	for i := range 3 {
		l := c.NewLease()
		put(t, c, l, action(i), body(i, obj))
		l.Release()
	}
	// Make entry 0 the most recent on disk.
	past := time.Now().Add(-48 * time.Hour)
	for i := range 3 {
		mt := past.Add(time.Duration(i) * time.Hour)
		if i == 0 {
			mt = time.Now()
		}
		if err := os.Chtimes(c.metaPath(fmt.Sprintf("%x", action(i))), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	size := c.Size()

	c2 := open(t, dir, 1<<30, 1<<29)
	if c2.Size() != size {
		t.Fatalf("size after reopen %d, want %d", c2.Size(), size)
	}
	// Evict down to one entry: the survivor must be entry 0.
	c2.Evict(obj + blockSize)
	l := c2.NewLease()
	defer l.Release()
	mustHit(t, c2, l, action(0), body(0, obj))
	mustMiss(t, c2, action(1))
	mustMiss(t, c2, action(2))
}

func TestReopenDropsDanglingFiles(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir, 1<<30, 1<<29)
	l := c.NewLease()
	e := put(t, c, l, action(1), []byte("x"))
	l.Release()
	if err := os.Remove(e.Path); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "ab", "ab00"+blobSuffix)
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2 := open(t, dir, 1<<30, 1<<29)
	mustMiss(t, c2, action(1))
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan object not removed")
	}
	if c2.Size() != 0 {
		t.Fatalf("size %d", c2.Size())
	}
}

// simulateGoClean does what "go clean -cache" does: remove every
// two-hex-digit subdirectory of the cache directory.
func simulateGoClean(t *testing.T, dir string) {
	t.Helper()
	subdirs, err := filepath.Glob(filepath.Join(dir, "[0-9a-f][0-9a-f]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range subdirs {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectsGoClean(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir, 1<<30, 1<<29)
	for i := range 10 {
		l := c.NewLease()
		put(t, c, l, action(i), body(i, 5000))
		l.Release()
	}
	held := c.NewLease()
	mustHit(t, c, held, action(0), body(0, 5000))

	simulateGoClean(t, dir)
	if !c.CheckExternalClean() {
		t.Fatal("clean not detected")
	}
	if c.CheckExternalClean() {
		t.Fatal("sentinel not restored")
	}
	if s := c.Snapshot(); s.Size != 0 || s.Entries != 0 || s.Counters.ExternalCleans != 1 {
		t.Fatalf("snapshot after clean: %+v", s)
	}
	held.Release() // must not underflow or touch the new index

	l := c.NewLease()
	defer l.Release()
	mustMiss(t, c, action(1))
	put(t, c, l, action(1), body(1, 5000))
	mustHit(t, c, l, action(1), body(1, 5000))
}

func TestGetAfterFileRemovedIsMiss(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	l := c.NewLease()
	e := put(t, c, l, action(1), []byte("x"))
	l.Release()
	if err := os.Remove(e.Path); err != nil {
		t.Fatal(err)
	}
	mustMiss(t, c, action(1))
	if c.Snapshot().Entries != 0 {
		t.Fatal("stale entry kept")
	}
	// A new put with the same content must recreate the file.
	l = c.NewLease()
	defer l.Release()
	put(t, c, l, action(2), []byte("x"))
	mustHit(t, c, l, action(2), []byte("x"))
}

func TestConcurrentSessions(t *testing.T) {
	const obj = 2 * blockSize
	// Small limit so that eviction races with puts and gets.
	c := open(t, t.TempDir(), 40*(obj+blockSize), 20*(obj+blockSize))
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 50 {
				l := c.NewLease()
				for k := range 10 {
					i := (w + round + k) % 60
					a := action(i)
					if e, ok, err := c.Get(l, a); err != nil {
						t.Error(err)
					} else if ok {
						got, err := os.ReadFile(e.Path)
						if err != nil || !bytes.Equal(got, body(i, obj)) {
							t.Errorf("pinned object %d unreadable or wrong: %v", i, err)
						}
						continue
					}
					b := body(i, obj)
					sum := sha256.Sum256(b)
					e, err := c.Put(l, a, sum[:], obj, bytes.NewReader(b))
					if err != nil {
						t.Error(err)
						continue
					}
					if got, err := os.ReadFile(e.Path); err != nil || !bytes.Equal(got, b) {
						t.Errorf("put object %d unreadable or wrong: %v", i, err)
					}
				}
				if round%5 == 0 {
					c.evictIfNeeded()
				}
				l.Release()
			}
		}()
	}
	wg.Wait()
	c.evictIfNeeded()

	// The accounted size must match what is on disk.
	c.mu.Lock()
	defer c.mu.Unlock()
	var want int64
	for _, b := range c.blobs {
		if b.pins != 0 || b.refs == 0 {
			t.Errorf("object %s: pins=%d refs=%d after all leases released", b.output[:8], b.pins, b.refs)
		}
		want += usage(b.size)
	}
	want += int64(len(c.entries)) * blockSize
	if want != c.size {
		t.Fatalf("accounted size %d, recomputed %d", c.size, want)
	}
	if c.size > c.opts.MaxSize {
		t.Fatalf("size %d above limit", c.size)
	}
}

func TestEvictsAfterPinsReleased(t *testing.T) {
	const obj = 10 * blockSize
	c := open(t, t.TempDir(), 3*(obj+blockSize), 2*(obj+blockSize))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// One session pins everything it puts, so eviction cannot run yet.
	l := c.NewLease()
	for i := range 6 {
		put(t, c, l, action(i), body(i, obj))
	}
	time.Sleep(50 * time.Millisecond)
	if c.Snapshot().Entries != 6 {
		t.Fatal("pinned entries evicted")
	}
	l.Release()
	deadline := time.Now().Add(5 * time.Second)
	for c.Size() > 2*(obj+blockSize) {
		if time.Now().After(deadline) {
			t.Fatalf("no eviction after release, size %d", c.Size())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSeparateDirWatchesGoCache(t *testing.T) {
	dir, gocache := t.TempDir(), t.TempDir()
	c, err := Open(Options{Dir: dir, GoCache: gocache, MaxSize: 1 << 30, TargetSize: 1 << 29, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	l := c.NewLease()
	held := put(t, c, l, action(1), []byte("in use"))
	other := c.NewLease()
	put(t, c, other, action(2), []byte("free"))
	other.Release()

	simulateGoClean(t, gocache)
	if !c.CheckExternalClean() {
		t.Fatal("clean not detected")
	}
	mustMiss(t, c, action(2))
	if _, err := os.Stat(held.Path); err != nil {
		t.Fatalf("pinned object removed: %v", err)
	}
	if c.CheckExternalClean() {
		t.Fatal("sentinel not restored")
	}
	l.Release()
}
