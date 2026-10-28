package server

import (
	"os"
	"path/filepath"
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
