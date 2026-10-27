package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juev/go-cache-lru/internal/server"
)

// These tests drive the real go command through the plugin binary.

var pluginBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gcl-bin-")
	if err != nil {
		panic(err)
	}
	pluginBin = filepath.Join(dir, "go-cache-lru")
	out, err := exec.Command("go", "build", "-o", pluginBin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build plugin: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t     *testing.T
	cache string
	vars  []string
	ctl   []string // flags for control commands
}

func newEnv(t *testing.T, extra ...string) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e test")
	}
	e := &env{t: t, cache: t.TempDir()}
	e.vars = append(os.Environ(),
		"GOCACHE="+e.cache,
		"GOCACHEPROG="+pluginBin,
		"GOCACHELRU_DIR=",
		"GOCACHELRU_SOCKET=",
		"GOCACHELRU_REMOTE=",
		"GOFLAGS=-trimpath",
		"GOTOOLCHAIN=local",
	)
	e.vars = append(e.vars, extra...)
	t.Cleanup(func() {
		// Stop the daemon and wait for it to release the directory.
		_, _ = e.plugin("stop")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := e.plugin("stats"); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Error("daemon did not stop")
	})
	return e
}

func (e *env) run(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.vars
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) plugin(args ...string) (string, error) {
	return e.run("", pluginBin, append(append([]string{}, e.ctl...), args...)...)
}

func (e *env) stats() server.Status {
	e.t.Helper()
	out, err := e.plugin("stats", "-json")
	if err != nil {
		e.t.Fatalf("stats: %v\n%s", err, out)
	}
	var st server.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		e.t.Fatalf("decode stats: %v\n%s", err, out)
	}
	return st
}

// writeModule creates a module with a few packages and a test.
func writeModule(t *testing.T, variant string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.24\n",
		"main.go":       "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/m/a\"\n)\n\nfunc main() { fmt.Println(a.Hello()) }\n",
		"a/a.go":        "package a\n\nimport \"example.com/m/b\"\n\nfunc Hello() string { return b.Name() + \"" + variant + "\" }\n",
		"a/a_test.go":   "package a\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) { if Hello() == \"\" { t.Fatal() } }\n",
		"b/b.go":        "package b\n\nimport \"strings\"\n\nfunc Name() string { return strings.ToUpper(\"b\") }\n",
		"c/c.go":        "package c\n\nimport (\n\t\"encoding/json\"\n\t\"net/http\"\n)\n\nvar _ = json.Marshal\nvar _ = http.Get\n",
		"c/c_test.go":   "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
		"cmd/x/main.go": "package main\n\nimport _ \"example.com/m/c\"\n\nfunc main() {}\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildUsesSharedCache(t *testing.T) {
	e := newEnv(t)
	mod := writeModule(t, "")

	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("first build: %v\n%s", err, out)
	}
	first := e.stats()
	if first.Cache.Counters.Puts == 0 || first.Cache.Entries == 0 {
		t.Fatalf("first build stored nothing: %+v", first.Cache)
	}

	// A second worktree of the same module must hit the cache for all
	// packages (with -trimpath the directory does not matter).
	mod2 := writeModule(t, "")
	if out, err := e.run(mod2, "go", "build", "./..."); err != nil {
		t.Fatalf("second build: %v\n%s", err, out)
	}
	second := e.stats()
	if second.Cache.Counters.Hits <= first.Cache.Counters.Hits {
		t.Fatalf("second build had no hits: %+v", second.Cache.Counters)
	}

	// Tests are cached too, and their results come back as "(cached)".
	if out, err := e.run(mod, "go", "test", "./..."); err != nil {
		t.Fatalf("test: %v\n%s", err, out)
	}
	out, err := e.run(mod, "go", "test", "./...")
	if err != nil {
		t.Fatalf("test again: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(cached)") {
		t.Fatalf("second go test not cached:\n%s", out)
	}
	// go clean -testcache expires results through entry put times.
	if out, err := e.run(mod, "go", "clean", "-testcache"); err != nil {
		t.Fatalf("clean -testcache: %v\n%s", err, out)
	}
	if out, err = e.run(mod, "go", "test", "./..."); err != nil {
		t.Fatalf("test after clean -testcache: %v\n%s", err, out)
	}
	if strings.Contains(out, "(cached)") {
		t.Fatalf("go test cached after clean -testcache:\n%s", out)
	}

	// The go command's own cache must stay unused.
	native, _ := filepath.Glob(filepath.Join(e.cache, "*", "*-d"))
	if len(native) > 0 {
		t.Fatalf("go command wrote %d native cache files, e.g. %s", len(native), native[0])
	}
}

func TestParallelGoCommandsShareOneDaemon(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Half the builds compile identical code, so puts race.
			mod := writeModule(t, fmt.Sprint(i%2))
			if out, err := e.run(mod, "go", "build", "./..."); err != nil {
				errs <- fmt.Errorf("build %d: %v\n%s", i, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	st := e.stats()
	if st.Cache.Counters.Sessions < 8 {
		t.Fatalf("daemon saw %d sessions, want at least 8", st.Cache.Counters.Sessions)
	}
	locks, _ := filepath.Glob(filepath.Join(e.cache, "gocacheprog", "daemon.lock"))
	if len(locks) != 1 {
		t.Fatalf("lock files: %v", locks)
	}
}

func TestSizeLimitEvicts(t *testing.T) {
	e := newEnv(t, "GOCACHELRU_MAX_SIZE=4MiB", "GOCACHELRU_TARGET_SIZE=2MiB")
	mod := writeModule(t, "")
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Eviction runs in the background right after the put that crossed
	// the limit.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.stats()
		if st.Cache.Counters.Evictions > 0 && st.Cache.Size <= 4<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no eviction: %+v", st.Cache)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Evicted entries are rebuilt transparently.
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out)
	}
}

func TestGoCleanCache(t *testing.T) {
	e := newEnv(t)
	mod := writeModule(t, "")
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if e.stats().Cache.Entries == 0 {
		t.Fatal("nothing cached")
	}
	if out, err := e.run(mod, "go", "clean", "-cache"); err != nil {
		t.Fatalf("go clean: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := e.stats()
		if st.Cache.Counters.ExternalCleans == 1 && st.Cache.Entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("go clean -cache not detected: %+v", st.Cache)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build after clean: %v\n%s", err, out)
	}
	if e.stats().Cache.Entries == 0 {
		t.Fatal("nothing cached after clean")
	}
}

func TestCleanAndStatsCommands(t *testing.T) {
	e := newEnv(t)
	mod := writeModule(t, "")
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := e.plugin("stats")
	if err != nil || !strings.Contains(out, "hits") {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	if out, err := e.plugin("clean"); err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	st := e.stats()
	if st.Cache.Entries != 0 || st.Cache.Size != 0 {
		t.Fatalf("after clean: %+v", st.Cache)
	}
	if out, err := e.plugin("stats", "-reset"); err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}
	if c := e.stats().Cache.Counters; c.Puts != 0 || c.Hits != 0 {
		t.Fatalf("counters not reset: %+v", c)
	}
}

func TestStopWaitsForActiveSession(t *testing.T) {
	e := newEnv(t)
	mod := writeModule(t, "")
	// A test that runs for a while keeps a session open.
	slow := filepath.Join(mod, "slow", "slow_test.go")
	if err := os.MkdirAll(filepath.Dir(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slow, []byte("package slow\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(3 * time.Second) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var out string
	go func() {
		var err error
		out, err = e.run(mod, "go", "test", "-count=1", "./slow")
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for e.statsOrZero().ActiveSessions == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session never became active")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err := e.plugin("stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if err := <-done; err != nil {
		t.Fatalf("go test failed across daemon stop: %v\n%s", err, out)
	}
}

func (e *env) statsOrZero() server.Status {
	out, err := e.plugin("stats", "-json")
	if err != nil {
		return server.Status{}
	}
	var st server.Status
	_ = json.Unmarshal([]byte(out), &st)
	return st
}

// TestFlagsSeparateDir mirrors the documented setup: the cache lives
// outside GOCACHE and is configured through flags in GOCACHEPROG.
func TestFlagsSeparateDir(t *testing.T) {
	dir := t.TempDir()
	prog := pluginBin + " --dir=" + dir + " --max-size=1GiB --target-size=512MiB"
	e := newEnv(t, "GOCACHEPROG="+prog)
	e.ctl = []string{"--dir=" + dir}
	mod := writeModule(t, "")
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	st := e.stats()
	if st.Cache.Dir != dir || st.Cache.MaxSize != 1<<30 || st.Cache.TargetSize != 512<<20 {
		t.Fatalf("flags not applied: %+v", st.Cache)
	}
	if st.Cache.Entries == 0 {
		t.Fatal("nothing cached")
	}
	objs, _ := filepath.Glob(filepath.Join(dir, "*", "*-pd"))
	if len(objs) == 0 {
		t.Fatal("no objects in --dir")
	}

	// go clean -cache knows only GOCACHE; the daemon must notice and
	// empty the separate directory as well.
	if out, err := e.run(mod, "go", "clean", "-cache"); err != nil {
		t.Fatalf("go clean: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := e.stats()
		if st.Cache.Counters.ExternalCleans == 1 && st.Cache.Entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("go clean -cache not applied to --dir: %+v", st.Cache)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestRemoteTier builds on one machine and reads the results on another
// through shared remote storage.
func TestRemoteTier(t *testing.T) {
	shared := "--remote=file://" + t.TempDir()
	dirA, dirB := t.TempDir(), t.TempDir()
	a := newEnv(t, "GOCACHEPROG="+pluginBin+" --dir="+dirA+" "+shared)
	a.ctl = []string{"--dir=" + dirA}
	mod := writeModule(t, "")
	if out, err := a.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build A: %v\n%s", err, out)
	}
	// Uploads are asynchronous; stopping the daemon flushes them.
	if out, err := a.plugin("stop"); err != nil {
		t.Fatalf("stop A: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := a.plugin("stats"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon A did not stop")
		}
		time.Sleep(50 * time.Millisecond)
	}

	b := newEnv(t, "GOCACHEPROG="+pluginBin+" --dir="+dirB+" "+shared)
	b.ctl = []string{"--dir=" + dirB}
	if out, err := b.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build B: %v\n%s", err, out)
	}
	k := b.stats().Cache.Counters
	if k.RemoteHits == 0 || k.RemoteErrors != 0 {
		t.Fatalf("remote counters on B: %+v", k)
	}
}

// TestGoCommandDuringDrain starts a build while the previous daemon is
// still waiting for a long test: the build must wait for the successor
// daemon instead of failing.
func TestGoCommandDuringDrain(t *testing.T) {
	e := newEnv(t)
	mod := writeModule(t, "")
	slow := filepath.Join(mod, "slow", "slow_test.go")
	if err := os.MkdirAll(filepath.Dir(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slow, []byte("package slow\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(15 * time.Second) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slowDone := make(chan error, 1)
	go func() {
		out, err := e.run(mod, "go", "test", "-count=1", "./slow")
		if err != nil {
			err = fmt.Errorf("%v\n%s", err, out)
		}
		slowDone <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for e.statsOrZero().ActiveSessions == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session never became active")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err := e.plugin("stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if out, err := e.run(mod, "go", "build", "./..."); err != nil {
		t.Fatalf("build during drain: %v\n%s", err, out)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow test: %v", err)
	}
}
