package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildVersion builds the plugin as a release build of version, the way
// GoReleaser stamps it. Without a version it builds a development binary,
// even in a checkout of a release tag.
func buildVersion(t *testing.T, version string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), exeName)
	args := []string{"build", "-o", bin, "-buildvcs=false"}
	if version != "" {
		args = append(args, "-ldflags", "-X github.com/juev/go-cache-lru/internal/server.version="+version)
	}
	out, err := exec.Command("go", append(args, ".")...).CombinedOutput()
	if err != nil {
		t.Fatalf("build %q: %v\n%s", version, err, out)
	}
	return bin
}

// proxy is an HTTPS proxy that records the hosts the daemon connects to.
// With a tunnel address it passes connections there, whatever host they
// ask for; without one it refuses them.
type proxy struct {
	srv    *httptest.Server
	tunnel string
	mu     sync.Mutex
	hosts  []string
}

func newProxy(t *testing.T, tunnel string) *proxy {
	p := &proxy{tunnel: tunnel}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *proxy) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.mu.Unlock()
	if r.Method != http.MethodConnect || p.tunnel == "" {
		http.Error(w, "no network in tests", http.StatusForbidden)
		return
	}
	up, err := net.Dial("tcp", p.tunnel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() {
		_, _ = io.Copy(up, conn)
		_ = up.Close()
	}()
	_, _ = io.Copy(conn, up)
	_ = conn.Close()
}

func (p *proxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hosts)
}

// env routes the daemon's HTTPS through the proxy.
func (p *proxy) env() []string {
	return []string{"HTTPS_PROXY=" + p.srv.URL, "https_proxy=", "HTTP_PROXY=" + p.srv.URL, "http_proxy=", "NO_PROXY=", "no_proxy="}
}

// daemonLog returns the log of the daemon for the default cache directory.
func (e *env) daemonLog() string {
	data, _ := os.ReadFile(filepath.Join(e.cache, "gocacheprog", "daemon.log"))
	return string(data)
}

// waitLog waits until the daemon log contains want n times.
func (e *env) waitLog(want string, n int, d time.Duration) string {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for {
		log := e.daemonLog()
		if strings.Count(log, want) >= n {
			return log
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("daemon log lacks %d× %q after %v:\n%s", n, want, d, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *env) build(mod string) {
	e.t.Helper()
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		e.t.Fatalf("build: %v\n%s", err, out)
	}
}

// stopDaemon stops the daemon and waits for it to exit.
func (e *env) stopDaemon() {
	e.t.Helper()
	st := e.stats()
	if out, err := e.plugin("stop"); err != nil {
		e.t.Fatalf("stop: %v\n%s", err, out)
	}
	e.waitExit(st.PID, 30*time.Second)
}

// TestAutoUpdateOffUsesNoNetwork checks that the daemon does not connect
// anywhere unless auto-update is on and the binary is a release build.
func TestAutoUpdateOffUsesNoNetwork(t *testing.T) {
	release := buildVersion(t, "0.0.1")
	for _, tc := range []struct {
		name, bin, auto, log string
	}{
		{"disabled", release, "", ""},
		{"development build", buildVersion(t, ""), "true", "auto-update is off: development build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProxy(t, "")
			e := newEnv(t, append(p.env(), "GOCACHEPROG="+tc.bin, "GOCACHELRU_AUTO_UPDATE="+tc.auto)...)
			e.build(writeModule(t, ""))
			if tc.log != "" {
				e.waitLog(tc.log, 1, 10*time.Second)
			}
			// A check would start right after the daemon does.
			time.Sleep(time.Second)
			e.stopDaemon()
			if hosts := p.seen(); len(hosts) > 0 {
				t.Fatalf("daemon connected to %q", hosts)
			}
			if strings.Contains(e.daemonLog(), "checking") {
				t.Fatalf("daemon checked for updates:\n%s", e.daemonLog())
			}
		})
	}
}

// TestAutoUpdateChecksOncePerInterval runs a release build with
// auto-update on and the network refused. The failed check must not
// affect go commands, and a restarted daemon must not check again before
// the interval has passed.
func TestAutoUpdateChecksOncePerInterval(t *testing.T) {
	p := newProxy(t, "")
	e := newEnv(t, append(p.env(), "GOCACHEPROG="+buildVersion(t, "0.0.1"), "GOCACHELRU_AUTO_UPDATE=1")...)
	mod := writeModule(t, "")
	e.build(mod)
	if runtime.GOOS == "windows" {
		e.waitLog("auto-update is off: automatic replacement is not supported on windows", 1, 10*time.Second)
		time.Sleep(time.Second)
		if hosts := p.seen(); len(hosts) > 0 {
			t.Fatalf("daemon connected to %q", hosts)
		}
		return
	}
	e.waitLog("auto-update: check failed", 1, 30*time.Second)
	if hosts := p.seen(); len(hosts) != 1 || hosts[0] != "api.github.com:443" {
		t.Fatalf("daemon connected to %q, want one connection to api.github.com:443", hosts)
	}
	e.build(writeModule(t, "x")) // the cache still works

	e.stopDaemon()
	e.build(mod)
	log := e.waitLog("auto-update: on for v0.0.1", 2, 10*time.Second)
	last := log[strings.LastIndex(log, "auto-update: on for"):]
	if strings.Contains(last[:strings.IndexByte(last, '\n')], "next check now") {
		t.Fatalf("restarted daemon checks right away:\n%s", log)
	}
	time.Sleep(time.Second)
	if hosts := p.seen(); len(hosts) != 1 {
		t.Fatalf("restarted daemon connected again: %q", hosts)
	}
}
