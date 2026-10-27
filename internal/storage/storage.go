// Package storage defines the remote storage tier behind the local disk
// cache. The go command only reads objects from local files, so the local
// cache is always the primary store; a Backend shares objects between
// machines or survives a wiped disk. Drivers register themselves by URL
// scheme, the way database/sql drivers do.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is returned by Get for a missing object.
var ErrNotFound = errors.New("storage: object not found")

// Object is a stored cache object.
type Object struct {
	OutputID []byte
	Size     int64
	Body     io.ReadCloser
}

// Backend stores objects by action ID.
type Backend interface {
	// Get returns the object stored under actionID or ErrNotFound.
	Get(ctx context.Context, actionID []byte) (*Object, error)
	// Put stores size bytes of body under actionID.
	Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) error
	// Close releases resources.
	Close() error
}

// OpenFunc creates a backend from its URL.
type OpenFunc func(ctx context.Context, u *url.URL) (Backend, error)

var (
	mu      sync.Mutex
	drivers = map[string]OpenFunc{}
)

// Register makes a driver available under a URL scheme.
func Register(scheme string, open OpenFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := drivers[scheme]; dup {
		panic("storage: driver registered twice: " + scheme)
	}
	drivers[scheme] = open
}

// Open opens the backend for rawURL, e.g. "file:///mnt/share/gocache" or
// "s3://bucket/prefix?endpoint=...".
func Open(ctx context.Context, rawURL string) (Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("storage: parse URL: %w", err)
	}
	mu.Lock()
	open := drivers[u.Scheme]
	mu.Unlock()
	if open == nil {
		return nil, fmt.Errorf("storage: unknown scheme %q (known: %s)", u.Scheme, strings.Join(Schemes(), ", "))
	}
	return open(ctx, u)
}

// Schemes lists registered URL schemes.
func Schemes() []string {
	mu.Lock()
	defer mu.Unlock()
	s := make([]string, 0, len(drivers))
	for k := range drivers {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
