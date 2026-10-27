// Package s3store is a storage driver for S3-compatible object storage:
// AWS S3, Cloudflare R2, MinIO and others.
//
// URL form:
//
//	s3://bucket/prefix?endpoint=host[:port]&region=auto&insecure=1
//
// endpoint defaults to s3.amazonaws.com; for R2 it is
// <account>.r2.cloudflarestorage.com with region=auto. Credentials come
// from AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, the shared AWS credentials
// file or instance metadata.
package s3store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/juev/go-cache-lru/internal/storage"
)

func init() {
	storage.Register("s3", func(ctx context.Context, u *url.URL) (storage.Backend, error) {
		return Open(ctx, u)
	})
}

const (
	metaOutputID = "Outputid" // canonical form of X-Amz-Meta-Outputid
	metaSize     = "Size"
)

// Store keeps each object at "<prefix>/<aa>/<action>" with the output ID
// and size in object metadata.
type Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// Open creates a store from its URL.
func Open(ctx context.Context, u *url.URL) (*Store, error) {
	if u.Host == "" {
		return nil, errors.New("s3 storage: missing bucket")
	}
	q := u.Query()
	endpoint := q.Get("endpoint")
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	insecure, _ := strconv.ParseBool(q.Get("insecure"))
	creds := credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{},
	})
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  creds,
		Secure: !insecure,
		Region: q.Get("region"),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 storage: %w", err)
	}
	return &Store{client: client, bucket: u.Host, prefix: strings.Trim(u.Path, "/")}, nil
}

func (s *Store) key(actionID []byte) string {
	a := hex.EncodeToString(actionID)
	return path.Join(s.prefix, a[:2], a)
}

// Get implements storage.Backend.
func (s *Store) Get(ctx context.Context, actionID []byte) (*storage.Object, error) {
	if len(actionID) == 0 {
		return nil, storage.ErrNotFound
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(actionID), minio.GetObjectOptions{})
	if err != nil {
		return nil, notFound(err)
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, notFound(err)
	}
	out, err := hex.DecodeString(info.UserMetadata[metaOutputID])
	if err != nil || len(out) == 0 {
		_ = obj.Close()
		return nil, fmt.Errorf("s3 storage: object %s has no output ID", s.key(actionID))
	}
	return &storage.Object{OutputID: out, Size: info.Size, Body: obj}, nil
}

func notFound(err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return storage.ErrNotFound
	}
	return err
}

// Put implements storage.Backend. Empty objects are not uploaded: the
// client would send them without a Content-Length, and they are free to
// rebuild anyway.
func (s *Store) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) error {
	if size == 0 {
		return nil
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.key(actionID), body, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		UserMetadata: map[string]string{
			metaOutputID: hex.EncodeToString(outputID),
			metaSize:     strconv.FormatInt(size, 10),
		},
	})
	return err
}

// Close implements storage.Backend.
func (s *Store) Close() error { return nil }
