package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestOpenScale measures how long Open takes to load a large cache. The
// generated cache uses the file-based index format, so the first run
// includes importing it into SQLite and later runs load from SQLite.
// It is a measurement, not a check, and runs only on request:
//
//	GOCACHELRU_SCALE=300000 go test -run TestOpenScale -v ./internal/cache
//
// GOCACHELRU_SCALE_DIR reuses a directory between runs (generated once),
// for example to measure with a cold page cache after "sudo purge".
func TestOpenScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("GOCACHELRU_SCALE"))
	if n <= 0 {
		t.Skip("set GOCACHELRU_SCALE to the number of entries")
	}
	dir := os.Getenv("GOCACHELRU_SCALE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if _, err := os.Stat(filepath.Join(dir, sentinelDir, sentinelName)); err != nil {
		start := time.Now()
		generate(t, dir, n)
		t.Logf("generated %d entries in %v", n, time.Since(start).Round(time.Millisecond))
	}

	for run := range 3 {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		c, err := Open(Options{Dir: dir, MaxSize: 1 << 50, TargetSize: 1 << 49})
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		s := c.Snapshot()
		t.Logf("run %d: Open %v, %d entries, %d objects, accounted %.1f GiB, heap +%.0f MiB",
			run+1, elapsed.Round(time.Millisecond), s.Entries, s.Objects,
			float64(s.Size)/(1<<30), float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20))
		runtime.KeepAlive(c)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// generate writes n index entries over n/2 objects (the ratio seen on a
// real cache) with the size distribution flattened to 100 KiB on average.
// Objects are sparse files: sizes are real, disk usage is not.
func generate(t *testing.T, dir string, n int) {
	t.Helper()
	for i := range 256 {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("%02x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	objects := max(n/2, 1)
	now := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < n; i += 16 {
				a := sha256.Sum256(fmt.Appendf(nil, "a%d", i))
				o := sha256.Sum256(fmt.Appendf(nil, "o%d", i%objects))
				ah, oh := hex.EncodeToString(a[:]), hex.EncodeToString(o[:])
				size := int64(4<<10 + (i%objects)%(192<<10))
				if i < objects {
					p := filepath.Join(dir, oh[:2], oh+blobSuffix)
					f, err := os.Create(p)
					if err == nil {
						err = f.Truncate(size)
						if cerr := f.Close(); err == nil {
							err = cerr
						}
					}
					if err != nil {
						errs <- err
						return
					}
				}
				meta := filepath.Join(dir, ah[:2], ah+metaSuffix)
				if err := os.WriteFile(meta, fmt.Appendf(nil, "v1 %s %d %d\n", oh, size, now.UnixNano()), 0o644); err != nil {
					errs <- err
					return
				}
				mt := now.Add(-time.Duration(i) * time.Second)
				if err := os.Chtimes(meta, mt, mt); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, sentinelDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sentinelDir, sentinelName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
