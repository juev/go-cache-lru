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
	"github.com/juev/go-cache-lru/internal/storage"
)

const (
	// busyInterval is how often an active server checks for "go clean
	// -cache" while go commands run.
	busyInterval = 2 * time.Second
	// statsInterval is how often counters are saved while go commands run.
	statsInterval = 30 * time.Second
)

// Options configure a Server.
type Options struct {
	Cache *cache.Cache
	// Remote is an optional second storage tier.
	Remote storage.Backend
	// IdleTimeout stops the server after this long without sessions; 0
	// disables it.
	IdleTimeout time.Duration
	// DrainTimeout bounds how long shutdown waits for active sessions.
	DrainTimeout time.Duration
	// Executable is the daemon binary. When a new connection finds it
	// replaced, the server serves that connection and shuts down, so the
	// next go command starts the new binary. Empty disables the check.
	Executable string
	Logf       func(format string, args ...any)
}

// Server is the daemon.
type Server struct {
	opts    Options
	cache   *cache.Cache
	remote  *remote
	exe     *exeWatch
	logf    func(string, ...any)
	started time.Time

	shutdown     chan struct{} // closed to request shutdown
	shutdownOnce sync.Once
	activity     chan struct{} // a go command session started or ended

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
	s := &Server{
		opts:     opts,
		cache:    opts.Cache,
		logf:     logf,
		started:  time.Now(),
		shutdown: make(chan struct{}),
		activity: make(chan struct{}, 1),
		conns:    make(map[net.Conn]struct{}),
		idleFrom: time.Now(),
	}
	if opts.Remote != nil {
		s.remote = newRemote(opts.Remote, opts.Cache, logf)
	}
	if opts.Executable != "" {
		s.exe = newExeWatch(opts.Executable)
	}
	return s
}

// Shutdown asks Serve to stop gracefully.
func (s *Server) Shutdown() {
	// Under s.mu so that track either registers a session before drain
	// starts waiting or sees the shutdown and refuses it.
	s.mu.Lock()
	defer s.mu.Unlock()
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
			// Checked per connection rather than on a timer, so an idle
			// daemon does not wake up for it.
			if s.exe.replaced() {
				s.logf("%s was replaced, exiting after active sessions", s.exe.path)
				s.Shutdown()
			}
		}
	}()

	// Periodic work runs only while go commands are active, so an idle
	// daemon does not wake up.
	var (
		busy     *time.Ticker
		busyTick <-chan time.Time
		lastSave = time.Now()
	)
	stopBusy := func() {
		if busy != nil {
			busy.Stop()
			busy, busyTick = nil, nil
		}
	}
	defer stopBusy()

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
		case <-s.activity:
			s.saveStats()
			lastSave = time.Now()
			if s.activeSessions() == 0 {
				stopBusy()
			} else if busy == nil {
				busy = time.NewTicker(busyInterval)
				busyTick = busy.C
			}
		case <-busyTick:
			// A "go clean -cache" during a build; between builds the next
			// session start notices it.
			s.cache.CheckExternalClean()
			if time.Since(lastSave) >= statsInterval {
				s.saveStats()
				lastSave = time.Now()
			}
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
	if s.remote != nil {
		s.remote.close(s.opts.DrainTimeout)
	}
	cancelBg()
	bgDone.Wait()
	s.saveStats()
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

func (s *Server) saveStats() {
	if err := s.cache.SaveStats(); err != nil {
		s.logf("save stats: %v", err)
	}
}

func (s *Server) signalActivity() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *Server) activeSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
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
		s.signalActivity()
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
	protocol.SetBuffers(conn)
	ss := &session{s: s, w: protocol.NewWriter(conn), lease: s.cache.NewLease()}
	defer func() {
		// Answers to outstanding requests may still reference pinned
		// files, so release only after they are written.
		ss.pending.Wait()
		ss.lease.Release()
		_ = conn.Close()
		s.untrack(conn, ss.counted)
	}()

	// Nothing watches for "go clean -cache" while the daemon is idle, so
	// look before serving anything from a cache that may be gone.
	s.cache.CheckExternalClean()

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
			s.signalActivity()
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
	if !ok && s.remote != nil {
		e, ok = s.remote.fetch(l, req.ActionID)
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
	if s.remote != nil {
		s.remote.enqueue(req.ActionID, e)
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
	Breakdown      *cache.Breakdown `json:",omitempty"` // only from x-stats-full
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
	case protocol.CmdStatsFull:
		st := s.status()
		d := s.cache.Breakdown()
		st.Breakdown = &d
		v = st
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

// version is set by release builds:
// -ldflags "-X github.com/juev/go-cache-lru/internal/server.version=..."
var version string

// Version reports the release version, or the module version and VCS
// revision for other builds.
func Version() string {
	if version != "" {
		return version
	}
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
