package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestEvictKeepsFilesOfConcurrentPuts puts the same few bodies under fresh
// actions while eviction keeps dropping them. An object is dropped under
// the lock but its file is removed after it, so a put of the same output
// in between must not lose the file it was just handed.
func TestEvictKeepsFilesOfConcurrentPuts(t *testing.T) {
	c := open(t, t.TempDir(), 1<<30, 1<<29)
	stop := make(chan struct{})
	var evictor sync.WaitGroup
	evictor.Add(1)
	go func() {
		defer evictor.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.Evict(0)
			}
		}
	}()

	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				b := body(i%251, 4<<10)
				out := sha256.Sum256(b)
				l := c.NewLease()
				e, err := c.Put(l, action(w*10000+i), out[:], int64(len(b)), bytes.NewReader(b))
				if err != nil {
					t.Error(err)
					l.Release()
					return
				}
				got, err := os.ReadFile(e.Path)
				l.Release()
				if err != nil {
					t.Errorf("pinned object lost: %v", err)
					return
				}
				if !bytes.Equal(got, b) {
					t.Error("pinned object changed")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	evictor.Wait()

	c.Evict(0)
	if s := c.Snapshot(); s.Entries != 0 || s.Objects != 0 || s.Size != 0 {
		t.Fatalf("after full eviction: %+v", s)
	}
	if left, _ := filepath.Glob(filepath.Join(c.Dir(), "*", "*"+blobSuffix)); len(left) != 0 {
		t.Fatalf("%d object files left after full eviction", len(left))
	}
	if left, _ := os.ReadDir(c.tmpDir); len(left) != 0 {
		t.Fatalf("%d files left in %s", len(left), c.tmpDir)
	}
}

// BenchmarkGetDuringEvict measures how long a get waits while eviction
// removes most of the cache. Every go command session competes for the
// same lock, so this is the stall a build sees when eviction runs.
//
//	go test -run '^$' -bench GetDuringEvict -benchtime 5x ./internal/cache
func BenchmarkGetDuringEvict(b *testing.B) {
	const (
		n    = 2000
		size = 256 << 10
	)
	var (
		gets, slow    int
		maxGet, evict time.Duration
	)
	buf := make([]byte, size)
	b.StopTimer()
	for range b.N {
		c, err := Open(Options{Dir: b.TempDir(), MaxSize: 1 << 50, TargetSize: 1 << 49})
		if err != nil {
			b.Fatal(err)
		}
		l := c.NewLease()
		for i := range n + 1 {
			binary.LittleEndian.PutUint64(buf, uint64(i))
			out := sha256.Sum256(buf)
			if _, err := c.Put(l, action(i), out[:], size, bytes.NewReader(buf)); err != nil {
				b.Fatal(err)
			}
		}
		l.Release()
		hot := action(n) // put last: the most recently used entry survives

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			rl := c.NewLease()
			defer rl.Release()
			for {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				if _, ok, err := c.Get(rl, hot); err != nil || !ok {
					b.Errorf("get: ok=%v err=%v", ok, err)
					return
				}
				// No allocation here: growing a slice of samples
				// would itself pause the reader.
				d := time.Since(start)
				gets++
				if d > time.Millisecond {
					slow++
				}
				maxGet = max(maxGet, d)
			}
		}()
		time.Sleep(time.Millisecond) // let the reader start

		b.StartTimer()
		start := time.Now()
		c.Evict(usage(size) * n / 10)
		evict += time.Since(start)
		b.StopTimer()

		close(stop)
		wg.Wait()
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(maxGet.Microseconds()), "max-get-µs")
	b.ReportMetric(float64(slow)/float64(b.N), "gets>1ms/op")
	b.ReportMetric(float64(gets)/float64(b.N), "gets/op")
	b.ReportMetric(float64(evict.Milliseconds())/float64(b.N), "evict-ms/op")
}
