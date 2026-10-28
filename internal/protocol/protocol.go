// Package protocol implements the wire format of the go command's
// GOCACHEPROG protocol (see cmd/go/internal/cacheprog).
//
// Requests are JSON objects, one per line. A "put" request with a non-zero
// BodySize is followed by a line holding the body as a base64-encoded JSON
// string literal. Responses are JSON objects, one per line, in any order.
package protocol

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Cmd is a request command.
type Cmd string

// Commands of the go command protocol.
const (
	CmdPut   Cmd = "put"
	CmdGet   Cmd = "get"
	CmdClose Cmd = "close"
)

// Control commands understood by the daemon. The go command never sends
// them; they are used by the go-cache-lru CLI over the same socket.
const (
	CmdStats      Cmd = "x-stats"
	CmdStatsReset Cmd = "x-stats-reset"
	CmdStatsFull  Cmd = "x-stats-full" // stats with the cache breakdown
	CmdClean      Cmd = "x-clean"
	CmdTrim       Cmd = "x-trim"
	CmdShutdown   Cmd = "x-shutdown"
)

// Request is a message from the go command.
type Request struct {
	ID       int64
	Command  Cmd
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`

	// Body streams the decoded body of a "put" request. It must be read to
	// EOF (or drained) before the next call to Reader.Next.
	Body io.Reader `json:"-"`
}

// Response is a message to the go command.
type Response struct {
	ID            int64      `json:"ID"`
	Err           string     `json:",omitempty"`
	KnownCommands []Cmd      `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`

	// Data carries the payload of control commands.
	Data json.RawMessage `json:",omitempty"`
}

// maxLine bounds the size of a request line. Request lines carry only
// hex-sized IDs, so anything larger means a broken peer.
const maxLine = 1 << 20

// Reader decodes requests from a stream.
type Reader struct {
	br   *bufio.Reader
	body *bodyReader
}

// NewReader returns a Reader for r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next request. It returns io.EOF at a clean end of stream.
// The body of the previous request is drained first if the caller did not
// consume it.
func (r *Reader) Next() (*Request, error) {
	if r.body != nil {
		if _, err := io.Copy(io.Discard, r.body); err != nil {
			return nil, err
		}
		r.body = nil
	}
	line, err := r.readLine()
	if err != nil {
		return nil, err
	}
	req := new(Request)
	if err := json.Unmarshal(line, req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if req.BodySize < 0 {
		return nil, fmt.Errorf("request %d: negative BodySize", req.ID)
	}
	if req.BodySize > 0 {
		b, err := r.openBody(req.BodySize)
		if err != nil {
			return nil, fmt.Errorf("request %d body: %w", req.ID, err)
		}
		r.body = b
		req.Body = b
	} else {
		req.Body = eofReader{}
	}
	return req, nil
}

func (r *Reader) readLine() ([]byte, error) {
	for {
		line, err := r.readRawLine()
		if err != nil {
			return nil, err
		}
		if len(line) > 0 {
			return line, nil
		}
		// Tolerate blank lines between messages.
	}
}

func (r *Reader) readRawLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, errors.New("request line too long")
		}
		switch {
		case err == nil:
			return trimEOL(buf), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// openBody positions the stream after the opening quote of the body literal.
func (r *Reader) openBody(size int64) (*bodyReader, error) {
	for {
		c, err := r.br.ReadByte()
		if err != nil {
			return nil, unexpected(err)
		}
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '"':
			q := &quoted{br: r.br}
			return &bodyReader{
				q:    q,
				dec:  base64.NewDecoder(base64.StdEncoding, q),
				want: size,
			}, nil
		default:
			return nil, fmt.Errorf("expected body string, got %q", c)
		}
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// quoted yields the bytes of a JSON string literal up to the closing quote
// and consumes the trailing newline. Base64 never needs JSON escapes.
type quoted struct {
	br   *bufio.Reader
	done bool
}

func (q *quoted) Read(p []byte) (int, error) {
	if q.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Fill from what is already buffered to avoid a syscall per byte.
	if q.br.Buffered() == 0 {
		if _, err := q.br.Peek(1); err != nil {
			return 0, unexpected(err)
		}
	}
	n := min(len(p), q.br.Buffered())
	peek, _ := q.br.Peek(n)
	for i, c := range peek {
		if c == '"' {
			copy(p, peek[:i])
			_, _ = q.br.Discard(i + 1) // bytes are buffered
			q.done = true
			if err := q.skipEOL(); err != nil {
				return i, err
			}
			if i == 0 {
				return 0, io.EOF
			}
			return i, nil
		}
		if c == '\\' {
			return 0, errors.New("unexpected escape in body string")
		}
	}
	copy(p, peek)
	_, _ = q.br.Discard(n) // bytes are buffered
	return n, nil
}

func (q *quoted) skipEOL() error {
	c, err := q.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if c == '\r' {
		c, err = q.br.ReadByte()
		if err != nil {
			return unexpected(err)
		}
	}
	if c != '\n' {
		return q.br.UnreadByte()
	}
	return nil
}

// bodyReader decodes a body and checks its length against BodySize.
type bodyReader struct {
	q    *quoted
	dec  io.Reader
	want int64
	got  int64
	err  error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.dec.Read(p)
	b.got += int64(n)
	if b.got > b.want {
		b.err = fmt.Errorf("body longer than BodySize %d", b.want)
		return n, b.err
	}
	if errors.Is(err, io.EOF) {
		if b.got != b.want {
			b.err = fmt.Errorf("body is %d bytes, BodySize is %d", b.got, b.want)
			return n, b.err
		}
		// Make sure the closing quote was consumed even if the decoder
		// stopped right before it.
		if !b.q.done {
			if _, err := io.Copy(io.Discard, b.q); err != nil {
				b.err = err
				return n, err
			}
		}
		b.err = io.EOF
	} else if err != nil {
		b.err = err
	}
	return n, err
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// Writer encodes responses. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	bw *bufio.Writer
}

// NewWriter returns a Writer for w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{bw: bufio.NewWriter(w)}
}

// Write sends a response and flushes it.
func (w *Writer) Write(res *Response) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	return w.bw.Flush()
}

// WriteRequest encodes a request the way the go command does. Body, if
// non-nil, must yield exactly BodySize bytes. It is used by the CLI and
// tests to talk to the daemon.
func (w *Writer) WriteRequest(req *Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	if req.BodySize > 0 {
		if err := w.bw.WriteByte('"'); err != nil {
			return err
		}
		enc := base64.NewEncoder(base64.StdEncoding, w.bw)
		n, err := io.Copy(enc, req.Body)
		if err != nil {
			return err
		}
		if n != req.BodySize {
			return fmt.Errorf("body is %d bytes, BodySize is %d", n, req.BodySize)
		}
		if err := enc.Close(); err != nil {
			return err
		}
		if _, err := w.bw.WriteString("\"\n"); err != nil {
			return err
		}
	}
	return w.bw.Flush()
}

// ResponseReader decodes responses. It is the client-side counterpart of
// Reader.
type ResponseReader struct {
	dec *json.Decoder
}

// NewResponseReader returns a ResponseReader for r.
func NewResponseReader(r io.Reader) *ResponseReader {
	return &ResponseReader{dec: json.NewDecoder(r)}
}

// Next returns the next response.
func (r *ResponseReader) Next() (*Response, error) {
	res := new(Response)
	if err := r.dec.Decode(res); err != nil {
		return nil, err
	}
	return res, nil
}
