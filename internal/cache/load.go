package cache

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	metas []scannedMeta
	blobs map[string]int64 // output -> size
}

// loadLocked rebuilds the in-memory index from disk. Index entries whose
// object is missing and objects nobody references are deleted.
func (c *Cache) loadLocked() error {
	for i := range 256 {
		if err := os.MkdirAll(filepath.Join(c.dir, fmt.Sprintf("%02x", i)), 0o755); err != nil {
			return err
		}
	}
	res := c.scan()
	c.entries = make(map[string]*entry, len(res.metas))
	c.blobs = make(map[string]*blob, len(res.blobs))
	c.lru = list.New()
	c.size = 0

	for output, size := range res.blobs {
		c.blobs[output] = &blob{output: output, size: size}
		c.size += usage(size)
	}
	// Oldest last so that PushFront leaves the most recent at the front.
	sort.Slice(res.metas, func(i, j int) bool { return res.metas[i].atime.Before(res.metas[j].atime) })
	for _, m := range res.metas {
		b := c.blobs[m.output]
		if b == nil || b.size != m.size {
			_ = os.Remove(c.metaPath(m.action))
			continue
		}
		e := &entry{action: m.action, blob: b, putTime: m.putTime, atime: m.atime, touched: m.atime}
		e.lruEntry = c.lru.PushFront(e)
		c.entries[m.action] = e
		b.refs++
		c.size += blockSize
	}
	for _, b := range c.blobs {
		c.maybeDropBlobLocked(b)
	}
	return c.writeSentinel()
}

func (c *Cache) scan() scanResult {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		res = scanResult{blobs: make(map[string]int64)}
		ch  = make(chan string)
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sub := range ch {
				metas, blobs := c.scanDir(sub)
				mu.Lock()
				res.metas = append(res.metas, metas...)
				for k, v := range blobs {
					res.blobs[k] = v
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

func (c *Cache) scanDir(dir string) ([]scannedMeta, map[string]int64) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var metas []scannedMeta
	blobs := make(map[string]int64)
	for _, de := range des {
		name := de.Name()
		switch {
		case strings.HasSuffix(name, metaSuffix):
			m, err := c.readMeta(filepath.Join(dir, name))
			if err != nil {
				c.logf("drop bad index entry %s: %v", name, err)
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			metas = append(metas, m)
		case strings.HasSuffix(name, blobSuffix):
			info, err := de.Info()
			if err != nil {
				continue
			}
			blobs[strings.TrimSuffix(name, blobSuffix)] = info.Size()
		}
	}
	return metas, blobs
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
	return filepath.Join(c.dir, sentinelDir, sentinelName)
}

func (c *Cache) writeSentinel() error {
	p := c.sentinelPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("go-cache-plugin: removed by go clean -cache\n"), 0o644)
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
	// Pinned objects of live sessions are detached; leases still hold the
	// old records and simply forget them on release.
	if err := c.loadLocked(); err != nil {
		c.logf("rebuild index after external clean: %v", err)
	}
	c.stats.ExternalCleans.Add(1)
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
