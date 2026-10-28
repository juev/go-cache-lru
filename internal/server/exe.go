package server

import "os"

// exeWatch detects that the daemon binary was replaced on disk, by a new
// file under the same name or by a rewrite in place.
type exeWatch struct {
	path string
	info os.FileInfo
}

// newExeWatch remembers the current state of path. It returns nil if path
// cannot be read; a nil watch never reports a replacement.
func newExeWatch(path string) *exeWatch {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &exeWatch{path: path, info: info}
}

// replaced reports whether path now holds a different file. A missing file
// is not a replacement: an installer may be between removing the old binary
// and writing the new one.
func (w *exeWatch) replaced() bool {
	if w == nil {
		return false
	}
	cur, err := os.Stat(w.path)
	if err != nil {
		return false
	}
	return !os.SameFile(w.info, cur) || cur.Size() != w.info.Size() || !cur.ModTime().Equal(w.info.ModTime())
}
