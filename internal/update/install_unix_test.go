//go:build !windows

package update

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// installed copies the release 1.0.0 binary to a directory of its own, as
// a user-managed installation, and returns its path.
func installed(t *testing.T, mode os.FileMode) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), binaryName)
	if err := os.WriteFile(exe, readFile(t, releaseBinary(t, "1.0.0")), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exe, mode); err != nil {
		t.Fatal(err)
	}
	return exe
}

// updaterFor returns an updater for exe, running release 1.0.0, that
// fetches from rel.
func updaterFor(t *testing.T, exe string, rel *fakeRelease) (*Updater, *logs) {
	t.Helper()
	l := new(logs)
	u, err := New(Options{
		Executable: exe,
		Version:    "1.0.0",
		Interval:   time.Hour,
		StatePath:  filepath.Join(t.TempDir(), "update-check"),
		Logf:       l.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	rel.use(u)
	return u, l
}

func runVersion(t *testing.T, exe string) string {
	t.Helper()
	out, err := exec.Command(exe, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", exe, err)
	}
	return strings.TrimSpace(string(out))
}

// checkIntact fails unless exe is still the 1.0.0 binary and no temporary
// file is left next to it.
func checkIntact(t *testing.T, exe string) {
	t.Helper()
	if !bytes.Equal(readFile(t, exe), readFile(t, releaseBinary(t, "1.0.0"))) {
		t.Fatal("the installed binary was modified")
	}
	if v := runVersion(t, exe); v != "1.0.0" {
		t.Fatalf("installed binary reports %q, want 1.0.0", v)
	}
	noTemp(t, filepath.Dir(exe))
}

func noTemp(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".*"))
	if len(left) > 0 {
		t.Fatalf("temporary files left behind: %q", left)
	}
}

// TestInstallsRelease updates a real 1.0.0 binary to a real 1.1.0 one
// through Run, which must stop by itself once the new binary is in place.
func TestInstallsRelease(t *testing.T) {
	exe := installed(t, 0o750)
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	u, l := updaterFor(t, exe, rel)
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	u.Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("Run did not stop after installing:\n%s", l)
	}
	if v := runVersion(t, exe); v != "1.1.0" {
		t.Fatalf("binary reports %q after the update, want 1.1.0\n%s", v, l)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("binary rewritten in place instead of replaced by a rename")
	}
	if after.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v after the update, want 0750", after.Mode().Perm())
	}
	noTemp(t, filepath.Dir(exe))
	if !strings.Contains(l.String(), "installed v1.1.0") {
		t.Fatalf("log lacks the installation:\n%s", l)
	}
}

// TestInstallKeepsSymlink updates a binary reached through a symlink: the
// file it points to is replaced and the link stays.
func TestInstallKeepsSymlink(t *testing.T) {
	target := installed(t, 0o755)
	link := filepath.Join(t.TempDir(), binaryName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	u, l := updaterFor(t, link, rel)
	if err := u.check(context.Background()); err != nil {
		t.Fatalf("check: %v\n%s", err, l)
	}
	if dest, err := os.Readlink(link); err != nil || dest != target {
		t.Fatalf("link now %q, %v; want a link to %s", dest, err, target)
	}
	if v := runVersion(t, link); v != "1.1.0" {
		t.Fatalf("binary reports %q, want 1.1.0", v)
	}
	noTemp(t, filepath.Dir(link))
	noTemp(t, filepath.Dir(target))
}

// patchArch makes a native binary claim another architecture.
func patchArch(t *testing.T, bin []byte) []byte {
	bin = bytes.Clone(bin)
	switch runtime.GOOS {
	case "darwin":
		bin[4] ^= 0xff // cputype
	default:
		bin[18] ^= 0xff // e_machine
	}
	return bin
}

// TestRejectsBadCandidate checks that a release whose binary cannot be
// verified leaves the installed binary alone.
func TestRejectsBadCandidate(t *testing.T) {
	newBin := readFile(t, releaseBinary(t, "1.1.0"))
	for _, tc := range []struct {
		name  string
		tag   string
		bin   []byte
		setup func(*fakeRelease)
	}{
		{"checksum mismatch", "v1.1.0", newBin, func(f *fakeRelease) {
			f.files["checksums.txt"] = checksums(map[string][]byte{assetName("1.1.0"): []byte("other")})
		}},
		{"no checksum", "v1.1.0", newBin, func(f *fakeRelease) {
			f.files["checksums.txt"] = checksums(map[string][]byte{"go-cache-lru_1.1.0_plan9_mips.tar.gz": []byte("x")})
		}},
		{"no checksums file", "v1.1.0", newBin, func(f *fakeRelease) { delete(f.files, "checksums.txt") }},
		{"no archive", "v1.1.0", newBin, func(f *fakeRelease) { delete(f.files, assetName("1.1.0")) }},
		// The tag says 1.2.0, the binary says 1.1.0.
		{"wrong version", "v1.2.0", newBin, nil},
		// A script that prints the right version is not a native binary.
		{"script", "v1.1.0", []byte("#!/bin/sh\necho 1.1.0\n"), nil},
		{"other architecture", "v1.1.0", patchArch(t, newBin), nil},
		// Native programs that do not answer "version" correctly.
		{"fails", "v1.1.0", readFile(t, "/bin/sleep"), nil},
		{"floods", "v1.1.0", readFile(t, "/usr/bin/yes"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := installed(t, 0o755)
			rel := newRelease(t, tc.tag, tc.bin)
			if tc.setup != nil {
				rel.mu.Lock()
				tc.setup(rel)
				rel.mu.Unlock()
			}
			u, l := updaterFor(t, exe, rel)
			err := u.check(context.Background())
			if err == nil || errors.Is(err, errUpToDate) {
				t.Fatalf("check = %v, want a failure\n%s", err, l)
			}
			t.Log(err)
			checkIntact(t, exe)
		})
	}
}

func TestNeverDowngrades(t *testing.T) {
	for _, tag := range []string{"v1.0.0", "v0.9.0"} {
		exe := installed(t, 0o755)
		rel := newRelease(t, tag, readFile(t, releaseBinary(t, "1.0.0")))
		u, _ := updaterFor(t, exe, rel)
		if err := u.check(context.Background()); !errors.Is(err, errUpToDate) {
			t.Fatalf("%s: check = %v, want errUpToDate", tag, err)
		}
		if n := rel.downloads(); n != 0 {
			t.Fatalf("%s: downloaded the archive %d times", tag, n)
		}
		checkIntact(t, exe)
	}
}

// replaceBy renames a file with body over path, as an installer does.
func replaceBy(t *testing.T, path string, body []byte) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestLeavesManualUpdate replaces the binary after the daemon started, as
// a manual update does. The updater must not overwrite it.
func TestLeavesManualUpdate(t *testing.T) {
	manual := []byte("#!/bin/sh\necho manual\n")
	exe := installed(t, 0o755)
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	u, _ := updaterFor(t, exe, rel)
	replaceBy(t, exe, manual)
	if err := u.check(context.Background()); !errors.Is(err, errReplaced) {
		t.Fatalf("check = %v, want errReplaced", err)
	}
	if !bytes.Equal(readFile(t, exe), manual) {
		t.Fatal("manual update overwritten")
	}

	// A symlink pointed at another installation.
	target := installed(t, 0o755)
	link := filepath.Join(t.TempDir(), binaryName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	u, _ = updaterFor(t, link, rel)
	other := installed(t, 0o755)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := u.check(context.Background()); !errors.Is(err, errReplaced) {
		t.Fatalf("check after the link moved = %v, want errReplaced", err)
	}
	checkIntact(t, target)
	checkIntact(t, other)
}

// TestInstallWaitsForLock holds the installation lock while another
// installer replaces the binary. The updater must wait for the lock and
// then see that the binary is no longer the one it runs.
func TestInstallWaitsForLock(t *testing.T) {
	exe := installed(t, 0o755)
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	u, l := updaterFor(t, exe, rel)
	unlock, err := lockDir(context.Background(), filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- u.check(context.Background()) }()
	deadline := time.Now().Add(time.Minute)
	for rel.downloads() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("archive never downloaded\n%s", l)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("check finished while another process held the lock: %v\n%s", err, l)
	case <-time.After(500 * time.Millisecond):
	}
	manual := []byte("#!/bin/sh\necho manual\n")
	replaceBy(t, exe, manual)
	unlock()
	if err := <-done; !errors.Is(err, errReplaced) {
		t.Fatalf("check = %v, want errReplaced\n%s", err, l)
	}
	if !bytes.Equal(readFile(t, exe), manual) {
		t.Fatal("the other installer's binary was overwritten")
	}
	noTemp(t, filepath.Dir(exe))
}

// TestLockGivesUpWithContext checks that waiting for the lock ends with
// the context, as on daemon shutdown.
func TestLockGivesUpWithContext(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if unlock2, err := lockDir(ctx, dir); err == nil {
		unlock2()
		t.Fatal("second lock taken while the first is held")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("lockDir returned %v after the context ended", d)
	}
}

// TestConcurrentUpdaters runs two daemons' updaters, for different cache
// directories, on one binary at once. Exactly one installs.
func TestConcurrentUpdaters(t *testing.T) {
	exe := installed(t, 0o755)
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	var us []*Updater
	for range 2 {
		u, _ := updaterFor(t, exe, rel)
		us = append(us, u)
	}
	errs := make([]error, len(us))
	var wg sync.WaitGroup
	for i, u := range us {
		wg.Go(func() { errs[i] = u.check(context.Background()) })
	}
	wg.Wait()
	installs := 0
	for _, err := range errs {
		switch {
		case err == nil:
			installs++
		case errors.Is(err, errReplaced):
		default:
			t.Fatalf("check: %v", err)
		}
	}
	if installs != 1 {
		t.Fatalf("%d installations, want 1 (errors %v)", installs, errs)
	}
	if v := runVersion(t, exe); v != "1.1.0" {
		t.Fatalf("binary reports %q, want 1.1.0", v)
	}
	noTemp(t, filepath.Dir(exe))
}

// TestReadOnlyInstallation checks that an installation the daemon cannot
// write to is reported without a download and left alone.
func TestReadOnlyInstallation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	exe := installed(t, 0o755)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	rel := newRelease(t, "v1.1.0", readFile(t, releaseBinary(t, "1.1.0")))
	u, _ := updaterFor(t, exe, rel)
	if err := u.check(context.Background()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("check = %v, want a permission error", err)
	}
	if n := rel.downloads(); n != 0 {
		t.Fatalf("downloaded the archive %d times for a read-only installation", n)
	}
	checkIntact(t, exe)
}
