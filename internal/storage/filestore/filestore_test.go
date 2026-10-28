package filestore

import (
	"context"
	"testing"

	"github.com/juev/go-cache-lru/internal/storage"
	"github.com/juev/go-cache-lru/internal/storage/storagetest"
)

func TestStore(t *testing.T) {
	b, err := storage.Open(context.Background(), storagetest.FileURL(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, b, false)
}
