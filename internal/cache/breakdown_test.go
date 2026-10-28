package cache

import (
	"testing"
	"time"
)

func TestBreakdown(t *testing.T) {
	clk := &clock{t: time.Now().Add(-48 * time.Hour)}
	c := openClock(t, t.TempDir(), 1<<30, 1<<29, clk)
	l := c.NewLease()
	defer l.Release()

	small, medium, large := body(1, 1<<10), body(2, 100<<10), body(3, 2<<20)
	put(t, c, l, action(1), small)
	clk.advance(25 * time.Hour)
	put(t, c, l, action(2), medium)
	clk.advance(3 * time.Hour)
	put(t, c, l, action(3), large)
	clk.advance(2 * time.Hour)
	// A second entry for the medium object makes it recently used.
	put(t, c, l, action(4), medium)

	d := c.Breakdown()
	wantSize := []SizeBucket{
		{Below: 64 << 10, Objects: 1, Bytes: usage(1 << 10)},
		{Below: 1 << 20, Objects: 1, Bytes: usage(100 << 10)},
		{Below: 10 << 20, Objects: 1, Bytes: usage(2 << 20)},
		{Below: 100 << 20},
		{},
	}
	if len(d.BySize) != len(wantSize) {
		t.Fatalf("size buckets: %+v", d.BySize)
	}
	for i, w := range wantSize {
		if d.BySize[i] != w {
			t.Errorf("size bucket %d: got %+v, want %+v", i, d.BySize[i], w)
		}
	}
	wantAccess := []AccessBucket{
		{Below: time.Hour, Objects: 1, Bytes: usage(100 << 10)},   // medium, used again just now
		{Below: 6 * time.Hour, Objects: 1, Bytes: usage(2 << 20)}, // large, 2h ago
		{Below: 24 * time.Hour},
		{Objects: 1, Bytes: usage(1 << 10)}, // small, 30h ago
	}
	if len(d.ByAccess) != len(wantAccess) {
		t.Fatalf("access buckets: %+v", d.ByAccess)
	}
	for i, w := range wantAccess {
		if d.ByAccess[i] != w {
			t.Errorf("access bucket %d: got %+v, want %+v", i, d.ByAccess[i], w)
		}
	}
}
