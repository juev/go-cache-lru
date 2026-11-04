package update

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFindChecksum(t *testing.T) {
	a, b := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	good := fmt.Sprintf("%x  x_1.0.0_linux_amd64.tar.gz\n%x  x_1.0.0_darwin_arm64.tar.gz\n", a, b)
	got, err := findChecksum([]byte(good), "x_1.0.0_darwin_arm64.tar.gz")
	if err != nil || !bytes.Equal(got, b[:]) {
		t.Fatalf("findChecksum = %x, %v; want %x", got, err, b)
	}
	for name, sums := range map[string]string{
		"missing":         fmt.Sprintf("%x  x_1.0.0_linux_amd64.tar.gz\n", a),
		"suffix only":     fmt.Sprintf("%x  old-x_1.0.0_darwin_arm64.tar.gz\n", a),
		"duplicate":       good + fmt.Sprintf("%x  x_1.0.0_darwin_arm64.tar.gz\n", b),
		"conflicting":     good + fmt.Sprintf("%x  x_1.0.0_darwin_arm64.tar.gz\n", a),
		"short hash":      fmt.Sprintf("%x  x_1.0.0_darwin_arm64.tar.gz\n", b[:31]),
		"not hex":         strings.Repeat("g", 64) + "  x_1.0.0_darwin_arm64.tar.gz\n",
		"one space":       fmt.Sprintf("%x x_1.0.0_darwin_arm64.tar.gz\n", b),
		"extra field":     fmt.Sprintf("%x  x_1.0.0_darwin_arm64.tar.gz extra\n", b),
		"malformed other": good + "garbage\n",
		"empty":           "",
	} {
		if got, err := findChecksum([]byte(sums), "x_1.0.0_darwin_arm64.tar.gz"); err == nil {
			t.Errorf("%s: findChecksum = %x; want an error", name, got)
		}
	}
}

func TestExtract(t *testing.T) {
	bin := []byte("\x7fELF binary")
	got, err := extract(releaseArchive(t, bin), binaryName, 1<<20)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("extract = %q, %v; want %q", got, err, bin)
	}

	many := []entry{{name: binaryName, body: bin}}
	for i := range maxEntries {
		many = append(many, entry{name: fmt.Sprintf("doc%d.txt", i), body: []byte("x")})
	}
	for name, archive := range map[string][]byte{
		"no binary":         tarGz(t, entry{name: "LICENSE", body: []byte("MIT")}),
		"binary in subdir":  tarGz(t, entry{name: "dist/" + binaryName, body: bin}),
		"duplicate binary":  tarGz(t, entry{name: binaryName, body: bin}, entry{name: binaryName, body: []byte("other")}),
		"binary is symlink": tarGz(t, entry{name: binaryName, typ: tar.TypeSymlink, link: "/bin/sh"}),
		"binary is dir":     tarGz(t, entry{name: binaryName, typ: tar.TypeDir}),
		"parent path":       tarGz(t, entry{name: "../evil", body: []byte("x")}, entry{name: binaryName, body: bin}),
		"nested parent":     tarGz(t, entry{name: "a/../../evil", body: []byte("x")}, entry{name: binaryName, body: bin}),
		"absolute path":     tarGz(t, entry{name: "/etc/evil", body: []byte("x")}, entry{name: binaryName, body: bin}),
		"backslash path":    tarGz(t, entry{name: `..\evil`, body: []byte("x")}, entry{name: binaryName, body: bin}),
		"too large":         tarGz(t, entry{name: binaryName, body: bytes.Repeat([]byte("x"), 1<<20+1)}),
		"too many entries":  tarGz(t, many...),
		// Entries other than the binary count against the limit too.
		"too much in total": tarGz(t, entry{name: "a.txt", body: bytes.Repeat([]byte("x"), 400<<10)},
			entry{name: "b.txt", body: bytes.Repeat([]byte("x"), 400<<10)}, entry{name: binaryName, body: bytes.Repeat([]byte("x"), 400<<10)}),
		"not gzip":  []byte("PK\x03\x04 zip archive"),
		"truncated": releaseArchive(t, bin)[:40],
	} {
		if got, err := extract(archive, binaryName, 1<<20); err == nil {
			t.Errorf("%s: extract = %d bytes; want an error", name, len(got))
		}
	}

	// With a 1 KiB limit, a small binary next to a larger file, or with
	// metadata that tar skips by itself, expands beyond it.
	for name, archive := range map[string][]byte{
		"oversized other entry": tarGz(t, entry{name: "README.md", body: bytes.Repeat([]byte("x"), 4096), mode: 0o644},
			entry{name: binaryName, body: bin}),
		"large metadata": tarGz(t, entry{name: binaryName, body: bin, pax: map[string]string{"comment": strings.Repeat("x", 512<<10)}}),
	} {
		if len(archive) > 4096 {
			t.Fatalf("%s: test archive is %d bytes compressed, want a small one", name, len(archive))
		}
		if got, err := extract(archive, binaryName, 1024); err == nil {
			t.Errorf("%s: extract with a 1 KiB limit = %d bytes; want an error", name, len(got))
		}
	}
	if got, err := extract(tarGz(t, entry{name: "LICENSE", body: []byte("MIT")}, entry{name: binaryName, body: bin}), binaryName, 1024); err != nil || !bytes.Equal(got, bin) {
		t.Errorf("small archive with a 1 KiB limit: extract = %q, %v; want the binary", got, err)
	}
}

// testUpdater returns an updater whose client trusts srv and may follow
// redirects only to srv.
func testUpdater(srv *httptest.Server) *Updater {
	host := strings.TrimPrefix(srv.URL, "https://")
	return &Updater{
		latestURL:   srv.URL + "/repos/juev/go-cache-lru/releases/latest",
		downloadURL: srv.URL + "/juev/go-cache-lru/releases/download/",
		client:      newClient(srv.Client().Transport, func(h string) bool { return h == host }),
		logf:        func(string, ...any) {},
	}
}

func TestGetHardening(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer plain.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer other.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hello")) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(make([]byte, 2048)) })
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, r *http.Request) {
		for range 4 {
			_, _ = w.Write(make([]byte, 512))
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/redirect-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/ok", http.StatusFound)
	})
	mux.HandleFunc("/redirect-foreign", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/ok", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/missing", http.NotFound)
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	u := testUpdater(srv)
	ctx := context.Background()

	for _, path := range []string{"/ok", "/redirect"} {
		if body, err := u.get(ctx, srv.URL+path, 1024); err != nil || string(body) != "hello" {
			t.Errorf("get %s = %q, %v; want hello", path, body, err)
		}
	}
	for _, path := range []string{"/big", "/big-chunked", "/redirect-http", "/redirect-foreign", "/loop", "/missing"} {
		if body, err := u.get(ctx, srv.URL+path, 1024); err == nil {
			t.Errorf("get %s = %d bytes; want an error", path, len(body))
		}
	}
	// The first request is held to the same rules as redirects.
	if _, err := u.get(ctx, plain.URL+"/ok", 1024); err == nil {
		t.Error("get over plain HTTP succeeded")
	}
	if _, err := u.get(ctx, other.URL+"/ok", 1024); err == nil {
		t.Error("get from a host that is not allowed succeeded")
	}
}

func TestProductionHosts(t *testing.T) {
	for _, h := range []string{"github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"} {
		if !githubHost(h) {
			t.Errorf("githubHost(%q) = false", h)
		}
	}
	for _, h := range []string{"github.com:8443", "evilgithub.com", "githubusercontent.com.evil.net", "evil.net", "", "github.com.", "githubusercontent.com"} {
		if githubHost(h) {
			t.Errorf("githubHost(%q) = true", h)
		}
	}
	for _, raw := range []string{latestURL, downloadURL} {
		p, err := url.Parse(raw)
		if err != nil || p.Scheme != "https" || !githubHost(p.Host) || !strings.Contains(p.Path, "/juev/go-cache-lru/") {
			t.Errorf("endpoint %s is not the project on GitHub", raw)
		}
	}
}

func TestLatest(t *testing.T) {
	var body string
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/juev/go-cache-lru/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	u := testUpdater(srv)

	body = `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`
	rel, err := u.latest(context.Background())
	if err != nil || rel.tag != "v1.2.0" || rel.version != (semver{1, 2, 0}) {
		t.Fatalf("latest = %+v, %v; want v1.2.0", rel, err)
	}
	for name, b := range map[string]string{
		"pre-release":   `{"tag_name":"v1.3.0-rc.1","prerelease":true}`,
		"flagged pre":   `{"tag_name":"v1.3.0","prerelease":true}`,
		"draft":         `{"tag_name":"v1.3.0","draft":true}`,
		"no v":          `{"tag_name":"1.3.0"}`,
		"other tag":     `{"tag_name":"nightly"}`,
		"path in tag":   `{"tag_name":"v1.3.0/../../x"}`,
		"no tag":        `{}`,
		"not JSON":      `<html>`,
		"oversized":     `{"tag_name":"v1.3.0","body":"` + strings.Repeat("x", maxReleaseJSON) + `"}`,
		"trailing data": `{"tag_name":"v1.3.0"} {"tag_name":"v9.0.0"}`,
	} {
		body = b
		if rel, err := u.latest(context.Background()); err == nil {
			t.Errorf("%s: latest = %+v; want an error", name, rel)
		}
	}
	body, status = `{"message":"API rate limit exceeded"}`, http.StatusForbidden
	if _, err := u.latest(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("rate limited: err = %v; want the status", err)
	}
}
