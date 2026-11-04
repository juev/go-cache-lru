//go:build !windows

package update

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// probeTimeout bounds the run of a downloaded binary before installation.
const probeTimeout = 30 * time.Second

// writable checks, before any download, that the daemon may replace the
// binary: the directory must be writable and the file its own.
// Installations owned by root or a package manager are left to their
// owners.
func (u *Updater) writable() error {
	dir := filepath.Dir(u.target)
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return fmt.Errorf("cannot replace %s: %w", u.target, &os.PathError{Op: "write", Path: dir, Err: err})
	}
	if st, ok := u.info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("cannot replace %s: owned by another user: %w", u.target, os.ErrPermission)
	}
	return nil
}

// lockDir takes an exclusive lock on dir, waiting until ctx is done. The
// lock on the binary's directory serializes installations by all daemons
// that run the binary, whatever their cache directories: unlike the
// binary, the directory stays the same file across the rename.
func lockDir(ctx context.Context, dir string) (unlock func(), err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", dir, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// install puts bin, which must report version, in place of the target.
// The new binary is written to a private temporary file next to the
// target, run, and renamed over the target: the running binary is never
// rewritten, and a failure at any step leaves it in place.
func (u *Updater) install(ctx context.Context, bin []byte, version semver) error {
	dir := filepath.Dir(u.target)
	unlock, err := lockDir(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	// Another daemon or a manual update may have replaced the binary
	// while this one downloaded.
	if err := u.unchanged(); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+binaryName+"-update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	_, err = f.Write(bin)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, u.info.Mode().Perm())
	}
	if err != nil {
		return err
	}
	if err := checkPlatform(tmp); err != nil {
		return err
	}
	if err := probe(ctx, tmp, version); err != nil {
		return err
	}
	if err := u.unchanged(); err != nil {
		return err
	}
	if err := os.Rename(tmp, u.target); err != nil {
		return err
	}
	renamed = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// checkPlatform checks that path is a native executable for this
// operating system and architecture.
func checkPlatform(path string) error {
	var want, got string
	switch runtime.GOOS {
	case "linux":
		f, err := elf.Open(path)
		if err != nil {
			return fmt.Errorf("downloaded binary is not an ELF executable: %w", err)
		}
		defer func() { _ = f.Close() }()
		want = map[string]string{"amd64": elf.EM_X86_64.String(), "arm64": elf.EM_AARCH64.String()}[runtime.GOARCH]
		got = f.Machine.String()
	case "darwin":
		f, err := macho.Open(path)
		if err != nil {
			return fmt.Errorf("downloaded binary is not a Mach-O executable: %w", err)
		}
		defer func() { _ = f.Close() }()
		want = map[string]string{"amd64": macho.CpuAmd64.String(), "arm64": macho.CpuArm64.String()}[runtime.GOARCH]
		got = f.Cpu.String()
	default:
		return errUnsupported
	}
	if want == "" || got != want {
		return fmt.Errorf("downloaded binary is for %s, not %s", got, runtime.GOARCH)
	}
	return nil
}

// probe runs "path version" and checks that it reports version.
func probe(ctx context.Context, path string, version semver) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var out []byte
	var err error
	for range 10 {
		var stdout limitedBuffer
		cmd := exec.CommandContext(ctx, path, "version")
		cmd.Stdout = &stdout
		cmd.WaitDelay = time.Second
		err = cmd.Run()
		out = stdout.Bytes()
		// A file just written may still be open in a child forked
		// meanwhile by another goroutine.
		if !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("run downloaded binary: %w", err)
	}
	v, ok := parseVersion(strings.TrimSpace(string(out)))
	if !ok || v != version {
		return fmt.Errorf("downloaded binary reports version %q, want %v", bytes.TrimSpace(out), version)
	}
	return nil
}

// limitedBuffer keeps the first 1 KiB written to it and fails after. It
// does not embed bytes.Buffer: io.Copy would use the promoted ReadFrom and
// bypass the limit.
type limitedBuffer struct{ buf bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > 1<<10 {
		return 0, io.ErrShortWrite
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
