package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/juev/go-cache-lru/internal/protocol"
)

// BenchmarkPutProtocolBody stores a body decoded from the wire format, the
// way a go command's put arrives: base64 that decodes in pieces of at most
// 768 bytes. The object is dropped after each put, so every put writes.
//
//	go test -run '^$' -bench PutProtocolBody ./internal/cache
func BenchmarkPutProtocolBody(b *testing.B) {
	const size = 4 << 20
	body := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	out := sha256.Sum256(body)
	wire := fmt.Appendf(nil, "{\"ID\":1,\"Command\":\"put\",\"BodySize\":%d}\n\"%s\"\n",
		size, base64.StdEncoding.EncodeToString(body))
	c, err := Open(Options{Dir: b.TempDir(), MaxSize: 1 << 50, TargetSize: 1 << 49})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	b.SetBytes(size)
	b.ResetTimer()
	for i := range b.N {
		req, err := protocol.NewReader(bytes.NewReader(wire)).Next()
		if err != nil {
			b.Fatal(err)
		}
		l := c.NewLease()
		if _, err := c.Put(l, action(i), out[:], size, req.Body); err != nil {
			b.Fatal(err)
		}
		l.Release()
		c.Clean()
	}
}
