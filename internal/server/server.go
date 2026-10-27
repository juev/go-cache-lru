// Package server implements the daemon shared by all go commands. Each
// connection on its unix socket is one go command session speaking the
// GOCACHEPROG protocol; the CLI uses the same socket for control commands.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/juev/go-cache-lru/internal/cache"
	"github.com/juev/go-cache-lru/internal/protocol"
)

// Options configure a Server.
type Options struct {
	Cache *cache.Cache
	// IdleTimeout stops the server after this long without sessions; 0
	// disables it.
	IdleTimeout time.Duration
	// DrainTimeout bounds how long shutdown waits for active sessions.
	DrainTimeout time.Duration
	Logf         func(format string, args ...any)
}

// Server is the daemon.
type Server struct {
	opts    Options
	cache   *cache.Cache
	logf    func(string, ...any)
	started time.Time

	shutdown     chan struct{} // closed to request shutdown
	shutdownOnce sync.Once

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	active   int // go command sessions
	idleFrom time.Time
	sessions sync.WaitGroup
}

// New returns a server.
func New(opts Options) *Server {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		opts:     opts,
		cache:    opts.Cache,
		logf:     logf,
		started:  time.Now(),
		shutdown: make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
		idleFrom: time.Now(),
	}
}

// Shutdown asks Serve to stop gracefully.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// Serve accepts sessions on ln until ctx is done, Shutdown is called or
// the idle timeout fires. It then stops accepting, waits up to DrainTimeout
// for active sessions to finish, closes the rest and saves statistics.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	bg, cancelBg := context.WithCancel(context.Background())
	var bgDone sync.WaitGroup
	bgDone.Add(1)
	go func() {
		defer bgDone.Done()
		s.cache.Run(bg)
	}()

	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			if !s.track(conn) {
				_ = conn.Close()
				continue
			}
			go s.handle(conn)
		}
	}()

	var idleTick <-chan time.Time
	if s.opts.IdleTimeout > 0 {
		t := time.NewTicker(min(s.opts.IdleTimeout/4+time.Millisecond, 10*time.Second))
		defer t.Stop()
		idleTick = t.C
	}

	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			s.logf("shutdown: %v", context.Cause(ctx))
			break loop
		case <-s.shutdown:
			s.logf("shutdown requested")
			break loop
		case err = <-acceptErr:
			s.logf("accept: %v", err)
			break loop
		case <-idleTick:
			if s.idleFor() >= s.opts.IdleTimeout {
				s.logf("idle for %v, exiting", s.opts.IdleTimeout)
				break loop
			}
		}
	}

	s.Shutdown()
	_ = ln.Close()
	s.drain()
	cancelBg()
	bgDone.Wait()
	if serr := s.cache.SaveStats(); serr != nil {
		s.logf("save stats: %v", serr)
	}
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.sessions.Wait()
		close(done)
	}()
	s.mu.Lock()
	n := s.active
	s.mu.Unlock()
	if n > 0 {
		s.logf("waiting up to %v for %d active sessions", s.opts.DrainTimeout, n)
	}
	select {
	case <-done:
		return
	case <-time.After(s.opts.DrainTimeout):
	}
	s.mu.Lock()
	s.logf("closing %d sessions still active after %v", len(s.conns), s.opts.DrainTimeout)
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	<-done
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.shutdown:
		return false
	default:
	}
	s.conns[conn] = struct{}{}
	s.sessions.Add(1)
	return true
}

func (s *Server) untrack(conn net.Conn, wasSession bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
	if wasSession {
		s.active--
		if s.active == 0 {
			s.idleFrom = time.Now()
		}
	}
	s.sessions.Done()
}

func (s *Server) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		return 0
	}
	return time.Since(s.idleFrom)
}

// session is one connection.
type session struct {
	s       *Server
	w       *protocol.Writer
	lease   *cache.Lease
	pending sync.WaitGroup
	counted bool // counted as a go command session
}

func (s *Server) handle(conn net.Conn) {
	ss := &session{s: s, w: protocol.NewWriter(conn), lease: s.cache.NewLease()}
	defer func() {
		// Answers to outstanding requests may still reference pinned
		// files, so release only after they are written.
		ss.pending.Wait()
		ss.lease.Release()
		_ = conn.Close()
		s.untrack(conn, ss.counted)
	}()

	hello := &protocol.Response{KnownCommands: []protocol.Cmd{protocol.CmdGet, protocol.CmdPut, protocol.CmdClose}}
	if err := ss.w.Write(hello); err != nil {
		return
	}
	r := protocol.NewReader(conn)
	for {
		req, err := r.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logf("session: %v", err)
			}
			return
		}
		if !ss.counted && (req.Command == protocol.CmdGet || req.Command == protocol.CmdPut) {
			ss.counted = true
			s.mu.Lock()
			s.active++
			s.mu.Unlock()
			s.cache.Counters().Sessions.Add(1)
		}
		switch req.Command {
		case protocol.CmdGet:
			// Gets may wait on remote storage; answer them concurrently.
			ss.pending.Add(1)
			go func() {
				defer ss.pending.Done()
				ss.reply(s.get(ss.lease, req))
			}()
		case protocol.CmdPut:
			// The body follows the request on the stream, so it is
			// consumed before the next request is read.
			ss.reply(s.put(ss.lease, req))
		case protocol.CmdClose:
			ss.pending.Wait()
			ss.reply(&protocol.Response{ID: req.ID})
			return
		default:
			ss.reply(s.control(req))
			if req.Command == protocol.CmdShutdown {
				return
			}
		}
	}
}

func (ss *session) reply(res *protocol.Response) {
	if err := ss.w.Write(res); err != nil && !errors.Is(err, net.ErrClosed) {
		ss.s.logf("write response %d: %v", res.ID, err)
	}
}

func (s *Server) get(l *cache.Lease, req *protocol.Request) *protocol.Response {
	e, ok, err := s.cache.Get(l, req.ActionID)
	if err != nil {
		return &protocol.Response{ID: req.ID, Err: err.Error()}
	}
	if !ok {
		return &protocol.Response{ID: req.ID, Miss: true}
	}
	return &protocol.Response{ID: req.ID, OutputID: e.OutputID, Size: e.Size, Time: &e.Time, DiskPath: e.Path}
}

func (s *Server) put(l *cache.Lease, req *protocol.Request) *protocol.Response {
	e, err := s.cache.Put(l, req.ActionID, req.OutputID, req.BodySize, req.Body)
	if err != nil {
		return &protocol.Response{ID: req.ID, Err: err.Error()}
	}
	return &protocol.Response{ID: req.ID, DiskPath: e.Path}
}

// Status is returned by the stats control command.
type Status struct {
	PID            int
	Version        string
	Started        time.Time
	ActiveSessions int
	Cache          cache.Snapshot
}

// CleanResult is returned by the clean and trim control commands.
type CleanResult struct {
	Entries int
	Bytes   int64
	Size    int64
}

func (s *Server) control(req *protocol.Request) *protocol.Response {
	var v any
	switch req.Command {
	case protocol.CmdStats:
		v = s.status()
	case protocol.CmdStatsReset:
		s.cache.Counters().Reset()
		v = s.status()
	case protocol.CmdClean:
		n, b := s.cache.Clean()
		v = CleanResult{Entries: n, Bytes: b, Size: s.cache.Size()}
	case protocol.CmdTrim:
		n, b := s.cache.Trim()
		v = CleanResult{Entries: n, Bytes: b, Size: s.cache.Size()}
	case protocol.CmdShutdown:
		s.Shutdown()
		v = s.status()
	default:
		return &protocol.Response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return &protocol.Response{ID: req.ID, Err: err.Error()}
	}
	return &protocol.Response{ID: req.ID, Data: data}
}

func (s *Server) status() Status {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	return Status{
		PID:            os.Getpid(),
		Version:        Version(),
		Started:        s.started,
		ActiveSessions: active,
		Cache:          s.cache.Snapshot(),
	}
}

// Version reports the module version of the binary.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		v := bi.Main.Version
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" && len(st.Value) >= 12 {
				v += " " + st.Value[:12]
			}
		}
		return v
	}
	return "unknown"
}
