// Package client connects go commands and the CLI to the daemon, starting
// the daemon on demand.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/juev/go-cache-lru/internal/config"
	"github.com/juev/go-cache-lru/internal/protocol"
	"github.com/juev/go-cache-lru/internal/server"
)

// StartTimeout bounds how long a client waits for an autostarted daemon,
// on top of the shutdown timeout: a previous daemon may still be draining
// its sessions, and opening a large cache scans its index.
const StartTimeout = time.Minute

// Dial connects to the daemon without starting it.
func Dial(cfg config.Config) (net.Conn, error) {
	return net.Dial("unix", cfg.Socket)
}

// Connect connects to the daemon, starting it if it is not running.
func Connect(cfg config.Config) (net.Conn, error) {
	if conn, err := Dial(cfg); err == nil {
		return conn, nil
	}
	// Serialize autostart so that a parallel build does not spawn a daemon
	// per go command.
	wait := cfg.DrainTimeout + StartTimeout
	lock, err := server.LockFile(cfg.SpawnLockPath(), wait)
	if err != nil {
		return nil, fmt.Errorf("spawn lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if conn, err := Dial(cfg); err == nil {
		return conn, nil
	}
	if err := spawn(cfg); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		conn, err := Dial(cfg)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not start within %v (see %s): %w", wait, cfg.LogPath(), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func spawn(cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return err
	}
	logf, err := openLog(cfg.LogPath())
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	cmd := exec.Command(exe, append(cfg.Args(), "serve")...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Dir = "/"
	// A new session detaches the daemon from the go command's process
	// group, so Ctrl-C on a build does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return cmd.Process.Release()
}

// maxLog is the size at which the daemon log starts over.
const maxLog = 10 << 20

func openLog(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// Bridge runs as GOCACHEPROG: it relays the go command's stdin and stdout
// to a daemon session and returns when the daemon ends the session.
func Bridge(cfg config.Config, stdin io.Reader, stdout io.Writer) error {
	conn, err := Connect(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// Ctrl-C reaches the whole process group. The go command shuts the
	// cache down itself by closing our stdin, so keep relaying until then
	// instead of dying with requests in flight.
	signal.Ignore(os.Interrupt, syscall.SIGQUIT)

	go func() {
		_, _ = io.Copy(conn, stdin)
		// The go command closed stdin: end the session.
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	if _, err := io.Copy(stdout, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// Call sends one control command and decodes its payload into v.
func Call(conn net.Conn, cmd protocol.Cmd, v any) error {
	r := protocol.NewResponseReader(conn)
	hello, err := r.Next()
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}
	if len(hello.KnownCommands) == 0 {
		return errors.New("unexpected handshake from daemon")
	}
	w := protocol.NewWriter(conn)
	if err := w.WriteRequest(&protocol.Request{ID: 1, Command: cmd}); err != nil {
		return err
	}
	res, err := r.Next()
	if err != nil {
		return err
	}
	if res.Err != "" {
		return errors.New(res.Err)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(res.Data, v)
}
