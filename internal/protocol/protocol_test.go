package protocol

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	bodies := [][]byte{nil, []byte("x"), []byte("hello world"), bytes.Repeat([]byte{0, 1, 2, '"', 0xff}, 100_000)}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, b := range bodies {
		req := &Request{ID: int64(i + 1), Command: CmdPut, ActionID: []byte{1, 2}, OutputID: []byte{3}, BodySize: int64(len(b)), Body: bytes.NewReader(b)}
		if err := w.WriteRequest(req); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteRequest(&Request{ID: 99, Command: CmdClose}); err != nil {
		t.Fatal(err)
	}

	r := NewReader(&buf)
	for i, want := range bodies {
		req, err := r.Next()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if req.ID != int64(i+1) || req.Command != CmdPut || req.BodySize != int64(len(want)) {
			t.Fatalf("request %d: got %+v", i, req)
		}
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("request %d body: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("request %d: body mismatch (%d vs %d bytes)", i, len(got), len(want))
		}
	}
	req, err := r.Next()
	if err != nil || req.Command != CmdClose {
		t.Fatalf("close: %+v %v", req, err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestUnreadBodyIsDrained(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRequest(&Request{ID: 1, Command: CmdPut, BodySize: 5, Body: strings.NewReader("abcde")}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRequest(&Request{ID: 2, Command: CmdGet, ActionID: []byte{7}}); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&buf)
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	req, err := r.Next()
	if err != nil || req.ID != 2 {
		t.Fatalf("got %+v %v", req, err)
	}
}

func TestBodySizeMismatch(t *testing.T) {
	in := `{"ID":1,"Command":"put","BodySize":10}` + "\n" + `"YWJj"` + "\n"
	r := NewReader(strings.NewReader(in))
	req, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("want size mismatch error")
	}
}

func TestTruncatedStream(t *testing.T) {
	in := `{"ID":1,"Command":"put","BodySize":3}` + "\n" + `"YWJ`
	r := NewReader(strings.NewReader(in))
	req, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("want error on truncated body")
	}
}
