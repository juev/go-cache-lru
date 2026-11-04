package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// binaryName is the binary's name in release archives.
const binaryName = "go-cache-lru"

// Limits on what the updater reads from the network and from an archive.
const (
	maxReleaseJSON = 1 << 20
	maxChecksums   = 64 << 10
	maxArchive     = 64 << 20
	maxContents    = 193 << 20 // all files in an archive: the binary and a little for README and LICENSE
	tarOverhead    = 128 << 10 // tar headers and metadata on top of the files
	maxEntries     = 64
	maxRedirects   = 5
)

// githubHost reports whether host serves the project's releases: the API,
// the release pages and the storage that release downloads redirect to.
func githubHost(host string) bool {
	return host == "github.com" || host == "api.github.com" ||
		(strings.HasSuffix(host, ".githubusercontent.com") && !strings.ContainsAny(host, ":/"))
}

func newTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
		IdleConnTimeout:       time.Minute,
		ForceAttemptHTTP2:     true,
	}
}

// newClient returns the HTTP client for GitHub. allowHost decides which
// hosts requests may go to, redirects included; all of them must use
// HTTPS.
func newClient(rt http.RoundTripper, allowHost func(string) bool) *http.Client {
	check := func(req *http.Request) error {
		if req.URL.Scheme != "https" || !allowHost(req.URL.Host) {
			return fmt.Errorf("refusing to fetch %s", req.URL.Redacted())
		}
		return nil
	}
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := check(req); err != nil {
				return nil, err
			}
			return rt.RoundTrip(req)
		}),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// get downloads url, failing if the response is larger than limit.
func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "go-cache-lru/"+u.current.String())
	if url == u.latestURL {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	res, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	if res.ContentLength > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	return data, nil
}

// release is the latest stable release.
type release struct {
	tag     string // "v1.2.3"
	version semver
}

// latest asks GitHub for the latest stable release.
func (u *Updater) latest(ctx context.Context) (release, error) {
	data, err := u.get(ctx, u.latestURL, maxReleaseJSON)
	if err != nil {
		return release{}, err
	}
	var r struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return release{}, fmt.Errorf("latest release: %w", err)
	}
	if dec.More() {
		return release{}, errors.New("latest release: trailing data")
	}
	v, ok := parseVersion(r.Tag)
	if !ok || r.Tag != v.String() || r.Draft || r.Prerelease {
		return release{}, fmt.Errorf("latest release %q is not a stable version", r.Tag)
	}
	return release{tag: r.Tag, version: v}, nil
}

// findChecksum returns the SHA-256 sum of name from a checksums.txt file
// in the format of sha256sum: "<64 hex digits>  <name>" per line. Any
// malformed line, or a name listed more than once, makes the whole file
// untrusted.
func findChecksum(sums []byte, name string) ([]byte, error) {
	var found []byte
	for line := range strings.Lines(string(sums)) {
		line = strings.TrimSuffix(line, "\n")
		hexSum, file, ok := strings.Cut(line, "  ")
		sum, err := hex.DecodeString(hexSum)
		if !ok || err != nil || len(sum) != 32 || file == "" || strings.ContainsAny(file, " \t\r") {
			return nil, fmt.Errorf("checksums.txt: malformed line %q", line)
		}
		if file != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("checksums.txt: %s listed more than once", name)
		}
		found = sum
	}
	if found == nil {
		return nil, fmt.Errorf("checksums.txt: no checksum for %s", name)
	}
	return found, nil
}

// errExpands means an archive decompresses to more than its limit.
var errExpands = errors.New("archive expands beyond its size limit")

// extract returns the contents of the regular file name from a .tar.gz
// archive. limit bounds the files of the archive together, skipped ones
// included, and the decompressed tar stream may exceed it only by
// tarOverhead, so that metadata tar skips by itself cannot expand without
// bound either. Reading stops at the end of the tar archive; anything the
// gzip stream holds after it is not decompressed. extract rejects archives
// with unsafe paths or too many entries, although it writes nothing but
// that one file.
func extract(archive []byte, name string, limit int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(&boundedReader{r: gz, n: limit + tarOverhead})
	var bin []byte
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if n >= maxEntries {
			return nil, errors.New("too many entries in the archive")
		}
		if h.Size < 0 || h.Size > limit-total {
			return nil, fmt.Errorf("%w: files larger than %d bytes", errExpands, limit)
		}
		total += h.Size
		if h.Name == "" || strings.ContainsAny(h.Name, `\`) || path.IsAbs(h.Name) || path.Clean(h.Name) != h.Name ||
			h.Name == ".." || strings.HasPrefix(h.Name, "../") {
			return nil, fmt.Errorf("unsafe path %q in the archive", h.Name)
		}
		if h.Name != name {
			continue
		}
		if bin != nil {
			return nil, fmt.Errorf("%s twice in the archive", name)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s is not a regular file in the archive", name)
		}
		// At most h.Size bytes, which is within the limit.
		if bin, err = io.ReadAll(tr); err != nil {
			return nil, err
		}
	}
	if bin == nil {
		return nil, fmt.Errorf("no %s in the archive", name)
	}
	return bin, nil
}

// boundedReader fails with errExpands once more than n bytes have been
// read from r.
type boundedReader struct {
	r io.Reader
	n int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > b.n+1 {
		p = p[:b.n+1]
	}
	k, err := b.r.Read(p)
	if b.n -= int64(k); b.n < 0 {
		return k, errExpands
	}
	return k, err
}
