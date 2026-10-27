package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type scannedMeta struct {
	action  string
	output  string
	size    int64
	putTime time.Time
	atime   time.Time
}

type scanResult struct {
	blobs  map[string]struct{} // outputs with an object file
	legacy []string            // index entry files of the file-based index
}

// loadLocked rebuilds the in-memory index from the database and reconciles
// it with the objects on disk: entries without an object and objects
// without an entry are deleted. This also repairs what a crash or
// "go clean -cache" left behind.
func (c *Cache) loadLocked() error {
	for i := range 256 {
		if err := os.MkdirAll(filepath.Join(c.dir, fmt.Sprintf("%02x", i)), 0o755); err != nil {
			return err
		}
	}
	if err := c.idx.flush(); err != nil {
		return err
	}
	res := c.scan()
	if len(res.legacy) > 0 {
		if err := c.importLegacy(res.legacy); err != nil {
			return err
		}
	}
	rows, err := c.idx.load()
	if err != nil {
		return err
	}
	c.entries = make(map[string]*entry, len(rows))
	c.blobs = make(map[string]*blob, len(res.blobs))
	c.lru = list.New()
	c.size = 0

	// Rows come oldest first, so PushFront leaves the most recent at the
	// front.
	for _, r := range rows {
		if _, ok := res.blobs[r.output]; !ok {
			c.idx.delete(r.action)
			continue
		}
		b := c.blobs[r.output]
		if b == nil {
			b = &blob{output: r.output, size: r.size}
			c.blobs[r.output] = b
			c.size += usage(r.size)
		} else if b.size != r.size {
			c.idx.delete(r.action)
			continue
		}
		e := &entry{action: r.action, blob: b, putTime: r.putTime, atime: r.atime, touched: r.atime}
		e.lruEntry = c.lru.PushFront(e)
		c.entries[r.action] = e
		b.refs++
	}
	for output := range res.blobs {
		if c.blobs[output] == nil {
			_ = os.Remove(c.blobPath(output))
		}
	}
	return c.writeSentinel()
}

// scan lists object files by name only: no per-file open or stat.
func (c *Cache) scan() scanResult {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		res = scanResult{blobs: make(map[string]struct{})}
		ch  = make(chan string)
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sub := range ch {
				des, err := os.ReadDir(sub)
				if err != nil {
					continue
				}
				mu.Lock()
				for _, de := range des {
					name := de.Name()
					switch {
					case strings.HasSuffix(name, blobSuffix):
						res.blobs[strings.TrimSuffix(name, blobSuffix)] = struct{}{}
					case strings.HasSuffix(name, metaSuffix):
						res.legacy = append(res.legacy, filepath.Join(sub, name))
					}
				}
				mu.Unlock()
			}
		}()
	}
	for i := range 256 {
		ch <- filepath.Join(c.dir, fmt.Sprintf("%02x", i))
	}
	close(ch)
	wg.Wait()
	return res
}

// importLegacy moves index entries of the file-based index into the
// database and removes the files.
func (c *Cache) importLegacy(paths []string) error {
	n := 0
	for _, p := range paths {
		m, err := c.readMeta(p)
		if err == nil {
			c.idx.add(op{kind: opUpsert, action: m.action, output: m.output, size: m.size,
				putTime: m.putTime.UnixNano(), atime: m.atime.UnixNano()})
			n++
		}
	}
	if err := c.idx.flush(); err != nil {
		return err
	}
	for _, p := range paths {
		_ = os.Remove(p)
	}
	c.logf("imported %d entries of the file-based index", n)
	return nil
}

func (c *Cache) readMeta(path string) (scannedMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scannedMeta{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return scannedMeta{}, err
	}
	f := strings.Fields(string(data))
	if len(f) != 4 || f[0] != "v1" {
		return scannedMeta{}, fmt.Errorf("malformed")
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return scannedMeta{}, err
	}
	nanos, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return scannedMeta{}, err
	}
	action := strings.TrimSuffix(filepath.Base(path), metaSuffix)
	if len(action) < 2 || len(f[1]) < 2 {
		return scannedMeta{}, fmt.Errorf("malformed ID")
	}
	return scannedMeta{
		action:  action,
		output:  f[1],
		size:    size,
		putTime: time.Unix(0, nanos),
		atime:   info.ModTime(),
	}, nil
}

func (c *Cache) removeStaleTemps() {
	des, err := os.ReadDir(c.tmpDir)
	if err != nil {
		return
	}
	for _, de := range des {
		_ = os.Remove(filepath.Join(c.tmpDir, de.Name()))
	}
}

func (c *Cache) sentinelPath() string {
	name := sentinelName
	if c.goCache != c.dir {
		// Several caches may watch one GOCACHE.
		sum := sha256.Sum256([]byte(c.dir))
		name += "-" + hex.EncodeToString(sum[:6])
	}
	return filepath.Join(c.goCache, sentinelDir, name)
}

func (c *Cache) writeSentinel() error {
	p := c.sentinelPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("go-cache-lru: removed by go clean -cache\n"), 0o644)
}

// CheckExternalClean detects "go clean -cache" (or a manual wipe) by the
// missing sentinel and rebuilds the index from what is left on disk.
// It reports whether a rebuild happened.
func (c *Cache) CheckExternalClean() bool {
	if _, err := os.Stat(c.sentinelPath()); err == nil || !os.IsNotExist(err) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := os.Stat(c.sentinelPath()); !os.IsNotExist(err) {
		return false
	}
	before := c.size
	c.stats.ExternalCleans.Add(1)
	if c.goCache != c.dir {
		// Our files are elsewhere: do what the user asked for and drop
		// everything not in use.
		if err := c.writeSentinel(); err != nil {
			c.logf("restore sentinel: %v", err)
		}
		c.mu.Unlock()
		n, _ := c.evict(0, true)
		c.mu.Lock()
		c.logf("go clean -cache detected: removed %d entries, size %d -> %d", n, before, c.size)
		return true
	}
	// Pinned objects of live sessions are detached; leases still hold the
	// old records and simply forget them on release.
	if err := c.loadLocked(); err != nil {
		c.logf("rebuild index after external clean: %v", err)
	}
	c.logf("cache directory was cleaned externally: index rebuilt, size %d -> %d", before, c.size)
	return true
}

// Run performs background work until ctx is done: eviction after puts,
// detection of "go clean -cache" and periodic saving of counters.
func (c *Cache) Run(ctx context.Context) {
	cleanTick := time.NewTicker(2 * time.Second)
	defer cleanTick.Stop()
	statsTick := time.NewTicker(30 * time.Second)
	defer statsTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.evictCh:
			c.evictIfNeeded()
		case <-cleanTick.C:
			c.CheckExternalClean()
		case <-statsTick.C:
			if err := c.SaveStats(); err != nil {
				c.logf("save stats: %v", err)
			}
		}
	}
}
