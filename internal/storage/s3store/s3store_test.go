package s3store

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/juev/go-cache-lru/internal/storage"
	"github.com/juev/go-cache-lru/internal/storage/storagetest"
)

func TestStore(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("cache"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	host := strings.TrimPrefix(srv.URL, "http://")
	b, err := storage.Open(context.Background(), "s3://cache/go/build?endpoint="+host+"&insecure=1&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, b, true)
}
