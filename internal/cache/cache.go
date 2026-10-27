// Package cache implements the local disk store behind the daemon: a
// content-addressed object store with an in-memory LRU index, a size limit
// and pinning of objects handed out to go commands.
//
// Layout inside the cache directory (normally GOCACHE):
//
//	xx/<action>-pa   index entry: output ID, size and put time;
//	                 its mtime is the last access time (LRU key)
//	xx/<output>-pd   object body, shared by all actions with this output
//	ff/.gocacheprog  sentinel that disappears on "go clean -cache"
//	                 (kept in GOCACHE when the cache lives elsewhere)
//	gocacheprog/     daemon state: temp files, stats, lock, socket
//
// The two-hex-digit directories are what "go clean -cache" removes, so with
// the cache in GOCACHE a clean wipes our objects too; with a separate
// directory the daemon empties it when the sentinel in GOCACHE disappears. The suffixes differ from the go command's
// own "-a"/"-d" files, so its trimming never touches our files.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	metaSuffix = "-pa"
	blobSuffix = "-pd"

	// StateDirName is the directory for daemon state inside the cache dir.
	StateDirName = "gocacheprog"

	sentinelDir  = "ff"
	sentinelName = ".gocacheprog"

	// blockSize approximates file system allocation for size accounting.
	blockSize = 4096

	// touchInterval limits how often an access rewrites the index entry's
	// mtime. The in-memory LRU order is always exact; the mtime only matters
	// for ordering after a restart.
	touchInterval = time.Minute
)

// ErrInvalidID is returned for an action or output ID of a bad length.
var ErrInvalidID = errors.New("invalid cache ID")

// Options configure a Cache.
type Options struct {
	// Dir is the cache directory, normally GOCACHE.
	Dir string
	// GoCache is the go command's GOCACHE. "go clean -cache" removes its
	// two-hex-digit subdirectories; a sentinel file there detects that.
	// Empty means Dir.
	GoCache string
	// MaxSize triggers eviction when the cache grows above it.
	MaxSize int64
	// TargetSize is the size eviction shrinks the cache to.
	TargetSize int64
	// Logf receives diagnostics. Nil discards them.
	Logf func(format string, args ...any)
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Entry is a cache hit or a stored object.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time // when the object was put
	Path     string    // file with the body
}

type entry struct {
	action   string // hex
	blob     *blob
	putTime  time.Time
	atime    time.Time
	touched  time.Time // last mtime written to disk
	lruEntry *list.Element
}

type blob struct {
	output string // hex
	size   int64
	refs   int // index entries pointing here
	pins   int // outstanding leases
}

// Cache is the local store. It is safe for concurrent use.
type Cache struct {
	dir      string
	goCache  string
	stateDir string
	tmpDir   string
	opts     Options
	now      func() time.Time
	logf     func(string, ...any)

	mu      sync.Mutex
	entries map[string]*entry
	blobs   map[string]*blob
	lru     *list.List // *entry, most recently used first
	size    int64

	evictCh chan struct{}
	stats   *Counters
}

// Open opens or creates the cache in opts.Dir and loads its index.
func Open(opts Options) (*Cache, error) {
	if opts.Dir == "" {
		return nil, errors.New("cache: empty directory")
	}
	if opts.TargetSize <= 0 || opts.MaxSize < opts.TargetSize {
		return nil, fmt.Errorf("cache: need 0 < target size (%d) <= max size (%d)", opts.TargetSize, opts.MaxSize)
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	c := &Cache{
		dir:      dir,
		stateDir: filepath.Join(dir, StateDirName),
		opts:     opts,
		now:      opts.Now,
		logf:     opts.Logf,
		evictCh:  make(chan struct{}, 1),
	}
	c.tmpDir = filepath.Join(c.stateDir, "tmp")
	c.goCache = dir
	if opts.GoCache != "" {
		if c.goCache, err = filepath.Abs(opts.GoCache); err != nil {
			return nil, err
		}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(c.tmpDir, 0o700); err != nil {
		return nil, err
	}
	c.removeStaleTemps()
	c.stats = loadCounters(c.statsPath())
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.dir }

// StateDir returns the directory for daemon state.
func (c *Cache) StateDir() string { return c.stateDir }

// Counters returns the cumulative counters.
func (c *Cache) Counters() *Counters { return c.stats }

func (c *Cache) statsPath() string { return filepath.Join(c.stateDir, "stats.json") }

func (c *Cache) metaPath(action string) string {
	return filepath.Join(c.dir, action[:2], action+metaSuffix)
}

func (c *Cache) blobPath(output string) string {
	return filepath.Join(c.dir, output[:2], output+blobSuffix)
}

func usage(size int64) int64 {
	return (size + blockSize - 1) / blockSize * blockSize
}

func encodeID(id []byte) (string, error) {
	if len(id) == 0 || len(id) > 64 {
		return "", fmt.Errorf("%w: %d bytes", ErrInvalidID, len(id))
	}
	return hex.EncodeToString(id), nil
}

// Lease pins the objects returned through it until Release. A go command
// session holds one lease, because the protocol requires returned files to
// exist until the session's "close".
type Lease struct {
	c    *Cache
	pins map[*blob]int
}

// NewLease returns an empty lease.
func (c *Cache) NewLease() *Lease {
	return &Lease{c: c, pins: make(map[*blob]int)}
}

// Release unpins everything pinned through the lease and deletes objects
// that lost their last reference while pinned.
func (l *Lease) Release() {
	c := l.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for b, n := range l.pins {
		b.pins -= n
		c.maybeDropBlobLocked(b)
	}
	clear(l.pins)
	// Eviction skips pinned objects, so a session that pinned most of the
	// cache may have left it above the limit.
	c.signalEvictLocked()
}

func (c *Cache) signalEvictLocked() {
	if c.size > c.opts.MaxSize {
		select {
		case c.evictCh <- struct{}{}:
		default:
		}
	}
}

func (l *Lease) pinLocked(b *blob) {
	b.pins++
	l.pins[b]++
}

func (l *Lease) unpinLocked(b *blob) {
	if l.pins[b] == 0 {
		return
	}
	b.pins--
	l.pins[b]--
	if l.pins[b] == 0 {
		delete(l.pins, b)
	}
	l.c.maybeDropBlobLocked(b)
}

// Get looks up an action. On a hit the object stays pinned by l.
func (c *Cache) Get(l *Lease, actionID []byte) (Entry, bool, error) {
	action, err := encodeID(actionID)
	if err != nil {
		return Entry{}, false, err
	}
	now := c.now()

	c.mu.Lock()
	e := c.entries[action]
	if e == nil {
		c.mu.Unlock()
		c.stats.Misses.Add(1)
		return Entry{}, false, nil
	}
	b := e.blob
	l.pinLocked(b)
	c.lru.MoveToFront(e.lruEntry)
	e.atime = now
	touch := now.Sub(e.touched) >= touchInterval
	if touch {
		e.touched = now
	}
	res := Entry{Size: b.size, Time: e.putTime, Path: c.blobPath(b.output)}
	output := b.output
	c.mu.Unlock()

	if _, err := os.Stat(res.Path); err != nil {
		// Removed behind our back, most likely by "go clean -cache".
		c.mu.Lock()
		l.unpinLocked(b)
		if c.entries[action] == e {
			c.removeEntryLocked(e)
		}
		c.mu.Unlock()
		c.stats.Misses.Add(1)
		return Entry{}, false, nil
	}
	if touch {
		_ = os.Chtimes(c.metaPath(action), now, now) // best effort, LRU order after restart
	}
	res.OutputID, _ = hex.DecodeString(output)
	c.stats.Hits.Add(1)
	c.stats.HitBytes.Add(res.Size)
	return res, true, nil
}

// Put stores body under actionID. body must yield exactly size bytes whose
// SHA-256 is outputID (the go command's definition of an output ID). The
// object stays pinned by l.
func (c *Cache) Put(l *Lease, actionID, outputID []byte, size int64, body io.Reader) (Entry, error) {
	action, err := encodeID(actionID)
	if err != nil {
		return Entry{}, err
	}
	output, err := encodeID(outputID)
	if err != nil {
		return Entry{}, err
	}
	if size < 0 {
		return Entry{}, fmt.Errorf("negative size %d", size)
	}
	blobPath := c.blobPath(output)

	// If the object is already stored, pin it and skip the write.
	c.mu.Lock()
	b := c.blobs[output]
	if b != nil && b.size == size {
		l.pinLocked(b)
	} else {
		b = nil
	}
	c.mu.Unlock()
	if b != nil {
		if _, err := os.Stat(blobPath); err != nil {
			c.mu.Lock()
			l.unpinLocked(b)
			c.mu.Unlock()
			b = nil
		}
	}

	var tmp string
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	for {
		if b == nil && tmp == "" {
			tmp, err = c.writeTemp(outputID, size, body)
			if err != nil {
				return Entry{}, err
			}
		}
		c.mu.Lock()
		if b == nil || c.blobs[output] == b {
			break
		}
		// A rescan after "go clean -cache" detached the object we pinned.
		// Use what the rescan found, or write the body after all; it has
		// not been read yet.
		if cur := c.blobs[output]; cur != nil && cur.size == size {
			l.pinLocked(cur)
			b = cur
			break
		}
		b = nil
		c.mu.Unlock()
	}
	defer c.mu.Unlock()

	now := c.now()
	if b == nil {
		if err := renameMkdir(tmp, blobPath); err != nil {
			return Entry{}, err
		}
		tmp = ""
		if old := c.blobs[output]; old != nil {
			// Same content; reuse the record so pins and refs stay together.
			c.size -= usage(old.size)
			old.size = size
			b = old
		} else {
			b = &blob{output: output, size: size}
			c.blobs[output] = b
		}
		c.size += usage(size)
		l.pinLocked(b)
	}
	if err := c.writeMetaLocked(action, output, size, now); err != nil {
		return Entry{}, err
	}
	e := c.entries[action]
	if e == nil {
		e = &entry{action: action}
		e.lruEntry = c.lru.PushFront(e)
		c.entries[action] = e
		c.size += blockSize
	} else {
		c.lru.MoveToFront(e.lruEntry)
	}
	if e.blob != b {
		if e.blob != nil {
			e.blob.refs--
			c.maybeDropBlobLocked(e.blob)
		}
		e.blob = b
		b.refs++
	}
	e.putTime, e.atime, e.touched = now, now, now

	c.stats.Puts.Add(1)
	c.stats.PutBytes.Add(size)
	c.signalEvictLocked()
	return Entry{OutputID: outputID, Size: size, Time: now, Path: blobPath}, nil
}

func (c *Cache) writeTemp(outputID []byte, size int64, body io.Reader) (string, error) {
	f, err := os.CreateTemp(c.tmpDir, "put-")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(c.tmpDir, 0o700); err != nil {
				return "", err
			}
			f, err = os.CreateTemp(c.tmpDir, "put-")
		}
		if err != nil {
			return "", err
		}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("body is %d bytes, want %d", n, size)
	}
	if err == nil && len(outputID) == sha256.Size && string(h.Sum(nil)) != string(outputID) {
		err = fmt.Errorf("body does not match output ID %x", outputID)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (c *Cache) writeMetaLocked(action, output string, size int64, put time.Time) error {
	data := fmt.Sprintf("v1 %s %d %d\n", output, size, put.UnixNano())
	f, err := os.CreateTemp(c.tmpDir, "meta-")
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = renameMkdir(f.Name(), c.metaPath(action))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// renameMkdir renames old to new, recreating new's directory if a
// concurrent "go clean -cache" removed it.
func renameMkdir(oldPath, newPath string) error {
	err := os.Rename(oldPath, newPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			return err
		}
		err = os.Rename(oldPath, newPath)
	}
	return err
}

func (c *Cache) removeEntryLocked(e *entry) {
	_ = os.Remove(c.metaPath(e.action))
	c.lru.Remove(e.lruEntry)
	delete(c.entries, e.action)
	c.size -= blockSize
	if e.blob != nil {
		e.blob.refs--
		c.maybeDropBlobLocked(e.blob)
		e.blob = nil
	}
}

// maybeDropBlobLocked deletes an object nobody references or pins. An
// object detached by a rescan is no longer ours to delete.
func (c *Cache) maybeDropBlobLocked(b *blob) {
	if b.refs > 0 || b.pins > 0 || c.blobs[b.output] != b {
		return
	}
	_ = os.Remove(c.blobPath(b.output))
	delete(c.blobs, b.output)
	c.size -= usage(b.size)
}

// Evict removes least recently used entries until the cache is no larger
// than target. Pinned entries are skipped. It returns what was removed.
func (c *Cache) Evict(target int64) (entries int, bytes int64) {
	const batch = 256 // bound lock hold time
	for {
		c.mu.Lock()
		if c.size <= target {
			c.mu.Unlock()
			return entries, bytes
		}
		removed := 0
		for el := c.lru.Back(); el != nil && removed < batch && c.size > target; {
			e := el.Value.(*entry)
			el = el.Prev()
			if e.blob.pins > 0 {
				continue
			}
			before := c.size
			c.removeEntryLocked(e)
			bytes += before - c.size
			removed++
		}
		entries += removed
		size := c.size
		c.mu.Unlock()
		if removed == 0 {
			c.logf("evict: %d bytes left above target, the rest is pinned", size-target)
			return entries, bytes
		}
	}
}

// evictIfNeeded runs eviction when the cache is above the size limit.
func (c *Cache) evictIfNeeded() {
	if c.Size() <= c.opts.MaxSize {
		return
	}
	start := c.now()
	n, bytes := c.Evict(c.opts.TargetSize)
	c.stats.Evictions.Add(int64(n))
	c.stats.EvictedBytes.Add(bytes)
	c.logf("evicted %d entries (%d bytes) in %v, size now %d", n, bytes, c.now().Sub(start).Round(time.Millisecond), c.Size())
}

// Trim shrinks the cache to the target size even if it is below the limit.
func (c *Cache) Trim() (entries int, bytes int64) {
	n, b := c.Evict(c.opts.TargetSize)
	c.stats.Evictions.Add(int64(n))
	c.stats.EvictedBytes.Add(b)
	return n, b
}

// Clean removes every entry that is not pinned.
func (c *Cache) Clean() (entries int, bytes int64) {
	n, b := c.Evict(0)
	c.stats.Cleans.Add(1)
	return n, b
}

// Size returns the accounted disk usage in bytes.
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// Snapshot is the current state plus cumulative counters.
type Snapshot struct {
	Dir        string
	MaxSize    int64
	TargetSize int64
	Size       int64
	Entries    int
	Objects    int
	Pinned     int // pinned objects
	Counters   CountersSnapshot
}

// Snapshot returns the current state.
func (c *Cache) Snapshot() Snapshot {
	c.mu.Lock()
	s := Snapshot{
		Dir:        c.dir,
		MaxSize:    c.opts.MaxSize,
		TargetSize: c.opts.TargetSize,
		Size:       c.size,
		Entries:    len(c.entries),
		Objects:    len(c.blobs),
	}
	for _, b := range c.blobs {
		if b.pins > 0 {
			s.Pinned++
		}
	}
	c.mu.Unlock()
	s.Counters = c.stats.Snapshot()
	return s
}

// SaveStats persists the cumulative counters.
func (c *Cache) SaveStats() error {
	return c.stats.save(c.statsPath(), c.tmpDir)
}
