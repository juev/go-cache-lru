package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestExeWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go-cache-lru")
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "v1")
	// The daemon runs its binary, which keeps the file open: a file
	// written after it is removed cannot get its inode, and with the same
	// size and mtime look unchanged. Windows removes neither an open file
	// nor a running binary.
	if runtime.GOOS != "windows" {
		running, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = running.Close() }()
	}

	w := newExeWatch(path)
	if w.replaced() {
		t.Fatal("unchanged file reported as replaced")
	}

	// An installer that removes the old binary first leaves a gap.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w.replaced() {
		t.Fatal("missing file reported as replaced")
	}

	// A new file renamed into place, as go install and install(1) do.
	write(path+".new", "v2")
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if !w.replaced() {
		t.Fatal("renamed-in file not reported")
	}

	// A rewrite in place keeps the inode; size and mtime change.
	w = newExeWatch(path)
	write(path, "v3")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if !w.replaced() {
		t.Fatal("file rewritten in place not reported")
	}

	var none *exeWatch
	if none.replaced() || newExeWatch(filepath.Join(dir, "missing")) != nil {
		t.Fatal("watch of a missing file must be nil and never fire")
	}
}
