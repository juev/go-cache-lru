package cache

import "time"

// Bucket bounds of a Breakdown. The last bucket of each list is open.
var (
	sizeBounds   = []int64{64 << 10, 1 << 20, 10 << 20, 100 << 20}
	accessBounds = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}
)

// SizeBucket counts objects of at least the previous bucket's Below and
// less than Below bytes. Below is 0 in the last, open bucket.
type SizeBucket struct {
	Below   int64
	Objects int
	Bytes   int64 // accounted disk usage
}

// AccessBucket counts objects last used at least the previous bucket's
// Below and less than Below ago. Below is 0 in the last, open bucket.
type AccessBucket struct {
	Below   time.Duration
	Objects int
	Bytes   int64 // accounted disk usage
}

// Breakdown shows what occupies the cache: objects by size and by time
// since the last get or put of any entry that points to them.
type Breakdown struct {
	BySize   []SizeBucket
	ByAccess []AccessBucket
}

// Breakdown walks the whole index under the lock, so it is computed only
// on request rather than as part of every Snapshot.
func (c *Cache) Breakdown() Breakdown {
	var d Breakdown
	for _, b := range sizeBounds {
		d.BySize = append(d.BySize, SizeBucket{Below: b})
	}
	d.BySize = append(d.BySize, SizeBucket{})
	for _, b := range accessBounds {
		d.ByAccess = append(d.ByAccess, AccessBucket{Below: b})
	}
	d.ByAccess = append(d.ByAccess, AccessBucket{})

	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[*blob]struct{}, len(c.blobs))
	// The LRU list runs from the most recent use, so the first entry seen
	// for an object carries its last access.
	for el := c.lru.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry)
		b := e.blob
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		u := usage(b.size)

		i := 0
		for i < len(sizeBounds) && b.size >= sizeBounds[i] {
			i++
		}
		d.BySize[i].Objects++
		d.BySize[i].Bytes += u

		age := now.Sub(e.atime)
		j := 0
		for j < len(accessBounds) && age >= accessBounds[j] {
			j++
		}
		d.ByAccess[j].Objects++
		d.ByAccess[j].Bytes += u
	}
	return d
}
