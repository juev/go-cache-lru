package protocol

import (
	"net"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSetBuffers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	get := func(opt int) int {
		t.Helper()
		var got int
		var gerr error
		if err := raw.Control(func(fd uintptr) {
			got, gerr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, opt)
		}); err != nil || gerr != nil {
			t.Fatal(err, gerr)
		}
		return got
	}
	opts := []int{syscall.SO_SNDBUF, syscall.SO_RCVBUF}
	before := []int{get(opts[0]), get(opts[1])}
	SetBuffers(conn)
	for i, opt := range opts {
		// Linux caps the request at net.core.[rw]mem_max for unprivileged
		// processes, so require growth rather than the full size.
		if got := get(opt); got <= before[i] && before[i] < socketBuffer {
			t.Errorf("socket option %d: buffer %d, was %d", opt, got, before[i])
		}
	}

	SetBuffers(nil) // not a unix connection: no-op
}
