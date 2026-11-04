package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeGitHub serves a synthetic release under the real GitHub host names,
// with a certificate from a test CA. It returns the server and the CA
// file for SSL_CERT_FILE, which Go honors on Linux.
func fakeGitHub(t *testing.T, tag string, files map[string][]byte) (*httptest.Server, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "go-cache-lru test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{"api.github.com", "github.com"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("api.github.com/repos/juev/go-cache-lru/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, tag)
	})
	for name, body := range files {
		mux.HandleFunc("github.com/juev/go-cache-lru/releases/download/"+tag+"/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
		})
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caFile
}

// releaseFiles packs bin the way GoReleaser does and adds checksums.txt.
func releaseFiles(t *testing.T, version string, bin string) map[string][]byte {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{{"LICENSE", 0o644, []byte("MIT")}, {"README.md", 0o644, []byte("# go-cache-lru")}, {"go-cache-lru", 0o755, data}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := fmt.Sprintf("go-cache-lru_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	return map[string][]byte{
		archive:         buf.Bytes(),
		"checksums.txt": fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(buf.Bytes()), archive),
	}
}

func versionOf(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}

// TestAutoUpdateInstallsRelease runs a daemon built as release 1.0.0 with
// auto-update on against a synthetic release 1.1.0 behind the GitHub host
// names. The daemon must install it in place of its binary, and the next
// go commands must be served by a daemon running 1.1.0.
func TestAutoUpdateInstallsRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test")
	}
	files := releaseFiles(t, "1.1.0", buildVersion(t, "1.1.0"))
	gh, caFile := fakeGitHub(t, "v1.1.0", files)
	p := newProxy(t, gh.Listener.Addr().String())

	bin := filepath.Join(t.TempDir(), exeName)
	data, err := os.ReadFile(buildVersion(t, "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, append(p.env(), "GOCACHEPROG="+bin, "GOCACHELRU_AUTO_UPDATE=1", "SSL_CERT_FILE="+caFile, "SSL_CERT_DIR=")...)
	mod := writeModule(t, "")
	e.build(mod)
	old := e.stats()
	if old.Version != "1.0.0" {
		t.Fatalf("daemon runs %q, want 1.0.0", old.Version)
	}

	e.waitLog("auto-update: installed v1.1.0", 1, 60*time.Second)
	if v := versionOf(t, bin); v != "1.1.0" {
		t.Fatalf("%s reports %q after the update, want 1.1.0", bin, v)
	}
	if info, err := os.Stat(bin); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v, %v; want mode 0755", info.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".*")); len(left) > 0 {
		t.Fatalf("temporary files left next to the binary: %q", left)
	}

	// The daemon keeps running 1.0.0 until a go command notices the new
	// binary; then it exits, and the next go command starts 1.1.0.
	e.build(writeModule(t, "x"))
	e.waitExit(old.PID, 30*time.Second)
	e.build(mod)
	st := e.stats()
	if st.Version != "1.1.0" || st.PID == old.PID {
		t.Fatalf("daemon after the update: pid %d version %q, want a new pid with 1.1.0", st.PID, st.Version)
	}
	if st.Cache.Entries < old.Cache.Entries {
		t.Fatalf("new daemon lost the cache: %d entries, had %d", st.Cache.Entries, old.Cache.Entries)
	}

	// The new daemon is up to date and checked just now: no new requests.
	e.waitLog("auto-update: on for v1.1.0", 1, 10*time.Second)
	n := len(p.seen())
	time.Sleep(time.Second)
	if hosts := p.seen(); len(hosts) != n {
		t.Fatalf("new daemon checked again right away: %q", hosts[n:])
	}
}
