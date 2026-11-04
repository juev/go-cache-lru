package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesNonRelease(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := Options{Executable: exe, Interval: time.Hour, StatePath: filepath.Join(t.TempDir(), "s")}
	for name, o := range map[string]Options{
		"test binary":    base, // a development build
		"custom version": func() Options { o := base; o.Version = "replaced-in-test"; return o }(),
		"no executable":  func() Options { o := base; o.Version = "1.0.0"; o.Executable = ""; return o }(),
		"missing binary": func() Options {
			o := base
			o.Version = "1.0.0"
			o.Executable = filepath.Join(t.TempDir(), "gone")
			return o
		}(),
		"pseudo-version":   func() Options { o := base; o.BuildInfo = buildInfo("v1.0.1-0.20260101000000-abcdefabcdef"); return o }(),
		"interval missing": func() Options { o := base; o.Version = "1.0.0"; o.Interval = 0; return o }(),
	} {
		if u, err := New(o); err == nil || u != nil {
			t.Errorf("%s: New = %v, %v; want an error", name, u, err)
		}
	}
	o := base
	o.Version = "1.0.0"
	u, err := New(o)
	if runtime.GOOS == "windows" {
		if !errors.Is(err, errUnsupported) || !strings.Contains(err.Error(), "not supported on windows") {
			t.Fatalf("New on Windows = %v, %v; want errUnsupported", u, err)
		}
		return
	}
	if err != nil || u == nil {
		t.Fatalf("New for a release build = %v, %v", u, err)
	}
}

// scheduled returns an updater that runs 1.0.0, checks every interval
// against rel and keeps its state in dir.
func scheduled(rel *fakeRelease, interval time.Duration, dir string) (*Updater, *logs) {
	l := new(logs)
	u := &Updater{current: semver{1, 0, 0}, interval: interval, statePath: filepath.Join(dir, "update-check"), logf: l.logf}
	rel.use(u)
	return u, l
}

// runFor runs u for d, or until it stops on its own.
func runFor(u *Updater, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	u.Run(ctx)
}

func writeState(t *testing.T, dir string, last time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "update-check"), []byte(last.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunChecksRightAwayFirstTime(t *testing.T) {
	rel := newRelease(t, "v1.0.0", []byte("unused"))
	dir := t.TempDir()
	u, l := scheduled(rel, time.Hour, dir)
	start := time.Now()
	runFor(u, 500*time.Millisecond)
	if n := rel.checks(); n != 1 {
		t.Fatalf("%d checks in 500ms with a 1h interval and no state, want 1\n%s", n, l)
	}
	data, err := os.ReadFile(filepath.Join(dir, "update-check"))
	if err != nil {
		t.Fatalf("check time not saved: %v", err)
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil || last.Before(start.Add(-time.Second)) || last.After(time.Now()) {
		t.Fatalf("saved check time %q, %v; want the time of the check", data, err)
	}
}

// TestRunThrottledAcrossRestarts starts updaters as restarted daemons
// would, with the time of the previous daemon's check on disk.
func TestRunThrottledAcrossRestarts(t *testing.T) {
	rel := newRelease(t, "v1.0.0", []byte("unused"))
	for _, tc := range []struct {
		name string
		last time.Duration // before now; negative is in the future
		want int
	}{
		{"checked recently", 30 * time.Minute, 0},
		{"interval passed", 61 * time.Minute, 1},
		{"clock went back", -10 * time.Hour, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := rel.checks()
			dir := t.TempDir()
			writeState(t, dir, time.Now().Add(-tc.last))
			u, l := scheduled(rel, time.Hour, dir)
			runFor(u, 500*time.Millisecond)
			if n := rel.checks() - before; n != tc.want {
				t.Fatalf("%d checks, want %d\n%s", n, tc.want, l)
			}
		})
	}
	t.Run("unreadable state", func(t *testing.T) {
		before := rel.checks()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "update-check"), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		u, _ := scheduled(rel, time.Hour, dir)
		runFor(u, 500*time.Millisecond)
		if n := rel.checks() - before; n != 1 {
			t.Fatalf("%d checks, want 1", n)
		}
	})
}

func TestRunRepeatsEveryInterval(t *testing.T) {
	rel := newRelease(t, "v1.0.0", []byte("unused"))
	u, l := scheduled(rel, 100*time.Millisecond, t.TempDir())
	runFor(u, 1050*time.Millisecond)
	if n := rel.checks(); n < 3 || n > 12 {
		t.Fatalf("%d checks in about 1s at 100ms, want about 10\n%s", n, l)
	}
}

// TestRunCancelsHangingCheck ends Run while GitHub does not answer, as on
// daemon shutdown: Run must return promptly.
func TestRunCancelsHangingCheck(t *testing.T) {
	rel := newRelease(t, "v1.1.0", []byte("unused"))
	rel.mu.Lock()
	rel.block = make(chan struct{})
	rel.mu.Unlock()
	defer close(rel.block)
	u, l := scheduled(rel, time.Hour, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for rel.checks() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no check started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after cancellation\n%s", l)
	}
}

// TestRunReportsFailures checks that a failed check is logged and retried
// at the next interval rather than ending the updater.
func TestRunReportsFailures(t *testing.T) {
	rel := newRelease(t, "v1.1.0", []byte("unused"))
	rel.mu.Lock()
	rel.tag = "nightly" // not a release version
	rel.mu.Unlock()
	u, l := scheduled(rel, 100*time.Millisecond, t.TempDir())
	runFor(u, 450*time.Millisecond)
	if n := rel.checks(); n < 2 {
		t.Fatalf("%d checks, want retries after a failure\n%s", n, l)
	}
	if !strings.Contains(l.String(), "auto-update: check failed") {
		t.Fatalf("failure not logged:\n%s", l)
	}
}
