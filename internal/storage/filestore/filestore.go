// Package filestore is a storage driver for a directory, typically on a
// network share: file:///mnt/share/gocache.
package filestore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/juev/go-cache-lru/internal/storage"
)

func init() {
	storage.Register("file", func(_ context.Context, u *url.URL) (storage.Backend, error) {
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("file storage: remote host %q not supported", u.Host)
		}
		return New(urlPath(u.Path))
	})
}

// Store keeps each object as "<aa>/<action>" with a "<action>.meta"
// sidecar holding the output ID and size. Files are written to a temp name
// and renamed, so readers never see partial objects.
type Store struct {
	root string
}

// New opens a store rooted at dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("file storage: empty path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: dir}, nil
}

func (s *Store) path(actionID []byte) string {
	a := hex.EncodeToString(actionID)
	return filepath.Join(s.root, a[:2], a)
}

// Get implements storage.Backend.
func (s *Store) Get(_ context.Context, actionID []byte) (*storage.Object, error) {
	if len(actionID) == 0 {
		return nil, storage.ErrNotFound
	}
	p := s.path(actionID)
	meta, err := os.ReadFile(p + ".meta")
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(meta))
	if len(f) != 2 {
		return nil, fmt.Errorf("file storage: malformed %s.meta", p)
	}
	out, err := hex.DecodeString(f[0])
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return nil, err
	}
	body, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &storage.Object{OutputID: out, Size: size, Body: body}, nil
}

// Put implements storage.Backend. The body goes first and the sidecar
// last, so a reader that finds the sidecar finds a matching body.
func (s *Store) Put(_ context.Context, actionID, outputID []byte, size int64, body io.Reader) error {
	if len(actionID) == 0 {
		return errors.New("file storage: empty action ID")
	}
	p := s.path(actionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(p, func(w io.Writer) error {
		n, err := io.Copy(w, body)
		if err == nil && n != size {
			err = fmt.Errorf("body is %d bytes, want %d", n, size)
		}
		return err
	}); err != nil {
		return err
	}
	return writeAtomic(p+".meta", func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%x %d\n", outputID, size)
		return err
	})
}

func writeAtomic(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// Close implements storage.Backend.
func (s *Store) Close() error { return nil }
