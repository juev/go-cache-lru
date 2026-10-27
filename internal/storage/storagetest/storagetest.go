// Package storagetest checks that a storage driver behaves as the cache
// expects.
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/juev/go-cache-lru/internal/storage"
)

// Run exercises b. Drivers that skip empty objects pass skipEmpty.
func Run(t *testing.T, b storage.Backend, skipEmpty bool) {
	t.Helper()
	ctx := context.Background()
	action := sha256.Sum256([]byte("action"))

	if _, err := b.Get(ctx, action[:]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get missing: %v, want ErrNotFound", err)
	}

	bodies := [][]byte{[]byte("first"), bytes.Repeat([]byte("x"), 1<<20)}
	if !skipEmpty {
		bodies = append(bodies, []byte{})
	}
	for _, body := range bodies {
		out := sha256.Sum256(body)
		if err := b.Put(ctx, action[:], out[:], int64(len(body)), bytes.NewReader(body)); err != nil {
			t.Fatalf("put %d bytes: %v", len(body), err)
		}
		obj, err := b.Get(ctx, action[:])
		if err != nil {
			t.Fatalf("get after put: %v", err)
		}
		got, err := io.ReadAll(obj.Body)
		_ = obj.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) || obj.Size != int64(len(body)) || !bytes.Equal(obj.OutputID, out[:]) {
			t.Fatalf("get returned size %d, %d bytes, output %x; want %d bytes, output %x",
				obj.Size, len(got), obj.OutputID, len(body), out)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}
