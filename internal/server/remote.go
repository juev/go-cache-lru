package server

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/juev/go-cache-lru/internal/cache"
	"github.com/juev/go-cache-lru/internal/storage"
)

const (
	remoteGetTimeout = 10 * time.Second
	remotePutTimeout = time.Minute
	uploadQueue      = 4096
	uploadWorkers    = 4
)

// remote is the optional second tier. Local misses are looked up there and
// local puts are uploaded in the background, so a slow remote never holds
// up a build beyond remoteGetTimeout.
type remote struct {
	b      storage.Backend
	c      *cache.Cache
	logf   func(string, ...any)
	queue  chan upload
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

type upload struct {
	action, output []byte
	size           int64
	path           string
}

func newRemote(b storage.Backend, c *cache.Cache, logf func(string, ...any)) *remote {
	ctx, cancel := context.WithCancel(context.Background())
	r := &remote{b: b, c: c, logf: logf, queue: make(chan upload, uploadQueue), ctx: ctx, cancel: cancel}
	for range uploadWorkers {
		r.wg.Add(1)
		go r.uploader()
	}
	return r
}

// fetch copies an object from the remote into the local cache.
func (r *remote) fetch(l *cache.Lease, action []byte) (cache.Entry, bool) {
	k := r.c.Counters()
	ctx, cancel := context.WithTimeout(r.ctx, remoteGetTimeout)
	defer cancel()
	obj, err := r.b.Get(ctx, action)
	if errors.Is(err, storage.ErrNotFound) {
		k.RemoteMisses.Add(1)
		return cache.Entry{}, false
	}
	if err != nil {
		k.RemoteErrors.Add(1)
		r.logf("remote get %x: %v", action, err)
		return cache.Entry{}, false
	}
	defer func() { _ = obj.Body.Close() }()
	e, err := r.c.Put(l, action, obj.OutputID, obj.Size, obj.Body)
	if err != nil {
		k.RemoteErrors.Add(1)
		r.logf("remote get %x: store locally: %v", action, err)
		return cache.Entry{}, false
	}
	k.RemoteHits.Add(1)
	return e, true
}

// enqueue schedules an upload. When the queue is full the object is
// skipped: the remote is a cache too.
func (r *remote) enqueue(action []byte, e cache.Entry) {
	select {
	case r.queue <- upload{action: action, output: e.OutputID, size: e.Size, path: e.Path}:
	default:
		r.c.Counters().RemoteErrors.Add(1)
	}
}

func (r *remote) uploader() {
	defer r.wg.Done()
	for u := range r.queue {
		r.upload(u)
	}
}

func (r *remote) upload(u upload) {
	k := r.c.Counters()
	// An open file stays readable even if eviction unlinks it meanwhile.
	f, err := os.Open(u.path)
	if err != nil {
		return // evicted before upload
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(r.ctx, remotePutTimeout)
	defer cancel()
	if err := r.b.Put(ctx, u.action, u.output, u.size, f); err != nil {
		k.RemoteErrors.Add(1)
		r.logf("remote put %x: %v", u.action, err)
		return
	}
	k.RemotePuts.Add(1)
}

// close flushes queued uploads for up to timeout, then abandons the rest.
func (r *remote) close(timeout time.Duration) {
	close(r.queue)
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		r.logf("abandoning %d queued uploads after %v", len(r.queue), timeout)
		r.cancel()
		<-done
	}
	r.cancel()
	if err := r.b.Close(); err != nil {
		r.logf("close remote: %v", err)
	}
}
