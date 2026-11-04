package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// binDir holds binaries built by the tests.
var binDir string

func TestMain(m *testing.M) {
	var err error
	if binDir, err = os.MkdirTemp("", "gcl-update-"); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(binDir)
	os.Exit(code)
}

var builds sync.Map // version -> func() (string, error)

// releaseBinary builds go-cache-lru as release version, the way GoReleaser
// stamps it, and returns its path.
func releaseBinary(t testing.TB, version string) string {
	t.Helper()
	f, _ := builds.LoadOrStore(version, sync.OnceValues(func() (string, error) {
		bin := filepath.Join(binDir, version, binaryName)
		out, err := exec.Command("go", "build", "-o", bin,
			"-ldflags", "-X github.com/juev/go-cache-lru/internal/server.version="+version,
			modulePath+"/cmd/go-cache-lru").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("build %s: %v\n%s", version, err, out)
		}
		return bin, nil
	}))
	bin, err := f.(func() (string, error))()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fakeRelease serves a synthetic release tag of archive and checksums,
// and counts the requests for each path.
type fakeRelease struct {
	srv      *httptest.Server
	tag      string
	files    map[string][]byte
	mu       sync.Mutex
	requests map[string]int
	block    chan struct{} // if set, the latest release query waits for it
}

// assetName is the archive name for version on this platform.
func assetName(version string) string {
	return fmt.Sprintf("go-cache-lru_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}

// newRelease serves bin as release tag ("v1.1.0"), with a correct
// checksums.txt.
func newRelease(t testing.TB, tag string, bin []byte) *fakeRelease {
	archive := releaseArchive(t, bin)
	name := assetName(tag[1:])
	f := &fakeRelease{tag: tag, requests: make(map[string]int), files: map[string][]byte{
		name:            archive,
		"checksums.txt": checksums(map[string][]byte{name: archive, "go-cache-lru_" + tag[1:] + "_other_arch.tar.gz": []byte("x")}),
	}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.URL.Path]++
	files, tag, block := f.files, f.tag, f.block
	f.mu.Unlock()
	if r.URL.Path == "/repos/juev/go-cache-lru/releases/latest" {
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "draft": false, "prerelease": false})
		return
	}
	for name, body := range files {
		if r.URL.Path == "/juev/go-cache-lru/releases/download/"+tag+"/"+name {
			_, _ = w.Write(body)
			return
		}
	}
	http.NotFound(w, r)
}

func (f *fakeRelease) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func (f *fakeRelease) checks() int { return f.count("/repos/juev/go-cache-lru/releases/latest") }

func (f *fakeRelease) downloads() int {
	return f.count("/juev/go-cache-lru/releases/download/" + f.tag + "/" + assetName(f.tag[1:]))
}

// use points u at the fake release.
func (f *fakeRelease) use(u *Updater) {
	t := testUpdater(f.srv)
	u.latestURL, u.downloadURL, u.client = t.latestURL, t.downloadURL, t.client
}

// logs collects log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var buf bytes.Buffer
	for _, s := range l.lines {
		buf.WriteString(s + "\n")
	}
	return buf.String()
}

// entry is a file in a test archive.
type entry struct {
	name string
	body []byte
	typ  byte // tar.TypeReg if zero
	mode int64
	link string
	pax  map[string]string // PAX records of the header
}

// tarGz builds a .tar.gz archive like GoReleaser's: files at the top.
func tarGz(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body)), Linkname: e.link}
		if e.pax != nil {
			h.PAXRecords, h.Format = e.pax, tar.FormatPAX
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseArchive is an archive with the binary and the files GoReleaser
// adds next to it.
func releaseArchive(t testing.TB, bin []byte) []byte {
	return tarGz(t,
		entry{name: "LICENSE", body: []byte("MIT"), mode: 0o644},
		entry{name: "README.md", body: []byte("# go-cache-lru"), mode: 0o644},
		entry{name: binaryName, body: bin},
	)
}

// checksums renders a checksums.txt file in GoReleaser's format.
func checksums(files map[string][]byte) []byte {
	var buf bytes.Buffer
	for name, body := range files {
		fmt.Fprintf(&buf, "%x  %s\n", sha256.Sum256(body), name)
	}
	return buf.Bytes()
}
