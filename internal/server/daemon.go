package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/juev/go-cache-plugin/internal/cache"
	"github.com/juev/go-cache-plugin/internal/config"
)

// ErrAlreadyRunning means another daemon owns the cache directory.
var ErrAlreadyRunning = errors.New("daemon already running for this cache directory")

// LockFile takes an exclusive flock on path, retrying for up to wait.
// The lock dies with the process, so a crashed daemon never blocks the
// next one.
func LockFile(path string, wait time.Duration) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrAlreadyRunning
			}
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Run starts the daemon for cfg and blocks until it stops. lockWait is how
// long to wait for a previous daemon that is still draining.
func Run(ctx context.Context, cfg config.Config, lockWait time.Duration, logf func(string, ...any)) error {
	lock, err := LockFile(cfg.LockPath(), lockWait)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	_ = lock.Truncate(0)
	_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())

	start := time.Now()
	c, err := cache.Open(cache.Options{
		Dir:        cfg.Dir,
		GoCache:    cfg.GoCache,
		MaxSize:    cfg.MaxSize,
		TargetSize: cfg.TargetSize,
		Logf:       logf,
	})
	if err != nil {
		return err
	}
	snap := c.Snapshot()
	logf("opened %s in %v: %d entries, %s (limit %s, target %s)", cfg.Dir, time.Since(start).Round(time.Millisecond),
		snap.Entries, config.FormatSize(snap.Size), config.FormatSize(cfg.MaxSize), config.FormatSize(cfg.TargetSize))

	// We hold the lock, so any socket file left behind is stale.
	_ = os.Remove(cfg.Socket)
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	defer func() { _ = os.Remove(cfg.Socket) }()
	logf("listening on %s (pid %d)", cfg.Socket, os.Getpid())

	srv := New(Options{
		Cache:        c,
		IdleTimeout:  cfg.IdleTimeout,
		DrainTimeout: cfg.DrainTimeout,
		Logf:         logf,
	})
	// Evict right away if the limit shrank since the last run.
	if c.Size() > cfg.MaxSize {
		go c.Trim()
	}
	err = srv.Serve(ctx, ln)
	logf("stopped")
	return err
}
