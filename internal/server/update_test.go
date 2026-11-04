package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juev/go-cache-lru/internal/cache"
)

// startServer serves a fresh cache with opts until the test ends. It
// returns the socket path and the result of Serve.
func startServer(t *testing.T, ctx context.Context, opts Options) (*Server, string, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(cache.Options{Dir: dir, MaxSize: 1 << 30, TargetSize: 1 << 29})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// A short path: macOS limits socket paths to 104 bytes.
	sockDir, err := os.MkdirTemp("", "gcl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	opts.Cache = c
	if opts.DrainTimeout == 0 {
		opts.DrainTimeout = 5 * time.Second
	}
	srv := New(opts)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	return srv, sock, done
}

func TestUpdateStopsBeforeServeReturns(t *testing.T) {
	for _, how := range []string{"shutdown", "context"} {
		t.Run(how, func(t *testing.T) {
			started := make(chan struct{})
			var returned atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv, _, done := startServer(t, ctx, Options{Update: func(ctx context.Context) {
				close(started)
				<-ctx.Done()
				// Work that outlives the cancellation by a little must
				// still finish before Serve returns.
				time.Sleep(100 * time.Millisecond)
				returned.Store(true)
			}})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("Update never started")
			}
			if how == "shutdown" {
				srv.Shutdown()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Serve did not return after the update was cancelled")
			}
			if !returned.Load() {
				t.Fatal("Serve returned before Update did")
			}
		})
	}
}

// TestServesAfterUpdateReturns checks that an updater that is done, for
// example after installing a new binary, does not stop the server.
func TestServesAfterUpdateReturns(t *testing.T) {
	ran := make(chan struct{})
	srv, sock, done := startServer(t, context.Background(), Options{Update: func(context.Context) { close(ran) }})
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Update never ran")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("server stopped serving after Update returned: %v", err)
	}
	_ = conn.Close()
	select {
	case err := <-done:
		t.Fatalf("Serve returned after Update did: %v", err)
	default:
	}
	srv.Shutdown()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
