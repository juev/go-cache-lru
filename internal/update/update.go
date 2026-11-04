// Package update keeps the daemon's binary up to date with the latest
// stable release on GitHub. It runs only in the daemon, off the path of
// cache requests; a failed check or installation leaves the running binary
// in place.
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// GitHub endpoints of the project. Only tests change them.
const (
	latestURL   = "https://api.github.com/repos/juev/go-cache-lru/releases/latest"
	downloadURL = "https://github.com/juev/go-cache-lru/releases/download/"
)

// attemptTimeout bounds one check, including the download and the
// installation.
const attemptTimeout = 10 * time.Minute

var (
	errUpToDate    = errors.New("no newer release")
	errReplaced    = errors.New("the binary was replaced since the daemon started")
	errUnsupported = errors.New("automatic replacement is not supported on " + runtime.GOOS)
)

// Options configure an Updater.
type Options struct {
	Executable string           // path of the running binary, from os.Executable
	Version    string           // version set at link time, if any
	BuildInfo  *debug.BuildInfo // build information of the running binary
	Interval   time.Duration    // time between checks
	StatePath  string           // file that keeps the time of the last check
	Logf       func(string, ...any)
}

// Updater checks for and installs new releases.
type Updater struct {
	exe       string      // executable as started
	target    string      // exe with symlinks resolved: the file replaced
	info      os.FileInfo // target when the updater was created
	current   semver
	interval  time.Duration
	statePath string
	logf      func(string, ...any)

	latestURL, downloadURL string
	client                 *http.Client
}

// New returns an updater for the running binary, or an error that says why
// it cannot update itself. New does not touch the network.
func New(o Options) (*Updater, error) {
	current, err := currentVersion(o.Version, o.BuildInfo)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		return nil, errUnsupported
	}
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return nil, fmt.Errorf("no release binaries for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if o.Interval <= 0 {
		return nil, errors.New("no update interval")
	}
	if o.Executable == "" {
		return nil, errors.New("executable path unknown")
	}
	target, err := filepath.EvalSymlinks(o.Executable)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Updater{
		exe:         o.Executable,
		target:      target,
		info:        info,
		current:     current,
		interval:    o.Interval,
		statePath:   o.StatePath,
		logf:        logf,
		latestURL:   latestURL,
		downloadURL: downloadURL,
		client:      newClient(newTransport(), githubHost),
	}, nil
}

// Run checks for updates every Interval until ctx is done or a new binary
// is in place. The time of the last check survives restarts, so a daemon
// that restarts often does not check more often.
func (u *Updater) Run(ctx context.Context) {
	last := u.lastCheck()
	wait := time.Duration(0)
	if now := time.Now(); !last.IsZero() && !last.After(now) {
		wait = max(last.Add(u.interval).Sub(now), 0)
	}
	next := "now"
	if wait > 0 {
		next = "in " + wait.Round(time.Second).String()
	}
	u.logf("auto-update: on for %v, checking every %v; next check %s", u.current, u.interval, next)
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if u.attempt(ctx) {
			return
		}
		t.Reset(u.interval)
	}
}

// attempt runs one check and logs its outcome. It reports whether the
// updater is done.
func (u *Updater) attempt(parent context.Context) bool {
	u.saveCheck(time.Now())
	ctx, cancel := context.WithTimeout(parent, attemptTimeout)
	defer cancel()
	err := u.check(ctx)
	switch {
	case err == nil:
		return true
	case parent.Err() != nil: // shutdown
		return true
	case errors.Is(err, errUpToDate):
		return false
	case errors.Is(err, errReplaced):
		u.logf("auto-update: %v; leaving it alone", err)
		return true
	}
	u.logf("auto-update: check failed, keeping %v: %v", u.current, err)
	return false
}

// check looks for a newer release once and installs it. It returns nil
// once the new binary is in place.
func (u *Updater) check(ctx context.Context) error {
	rel, err := u.latest(ctx)
	if err != nil {
		return err
	}
	if !rel.version.newer(u.current) {
		u.logf("auto-update: %v is up to date (latest release %s)", u.current, rel.tag)
		return errUpToDate
	}
	if err := u.unchanged(); err != nil {
		return err
	}
	if err := u.writable(); err != nil {
		return err
	}
	u.logf("auto-update: downloading %s", rel.tag)
	asset := fmt.Sprintf("%s_%s_%s_%s.tar.gz", binaryName, strings.TrimPrefix(rel.tag, "v"), runtime.GOOS, runtime.GOARCH)
	sums, err := u.get(ctx, u.downloadURL+rel.tag+"/checksums.txt", maxChecksums)
	if err != nil {
		return err
	}
	want, err := findChecksum(sums, asset)
	if err != nil {
		return err
	}
	archive, err := u.get(ctx, u.downloadURL+rel.tag+"/"+asset, maxArchive)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(archive); !bytes.Equal(sum[:], want) {
		return fmt.Errorf("%s: SHA-256 does not match checksums.txt", asset)
	}
	bin, err := extract(archive, binaryName, maxContents)
	if err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}
	if err := u.install(ctx, bin, rel.version); err != nil {
		return err
	}
	u.logf("auto-update: installed %s as %s; the daemon restarts with it once no go command is running", rel.tag, u.target)
	return nil
}

// unchanged checks that the executable is still the file the daemon was
// started from. Anything else is someone else's installation.
func (u *Updater) unchanged() error {
	target, err := filepath.EvalSymlinks(u.exe)
	if err != nil {
		return err
	}
	cur, err := os.Stat(target)
	if err != nil {
		return err
	}
	if target != u.target || !os.SameFile(u.info, cur) || cur.Size() != u.info.Size() || !cur.ModTime().Equal(u.info.ModTime()) {
		return fmt.Errorf("%s: %w", u.exe, errReplaced)
	}
	return nil
}

// lastCheck reads the time of the previous check, zero if unknown.
func (u *Updater) lastCheck() time.Time {
	data, err := os.ReadFile(u.statePath)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// saveCheck records the time of a check before it starts, so that a
// daemon that dies during a check does not retry right away.
func (u *Updater) saveCheck(t time.Time) {
	tmp := u.statePath + ".tmp"
	err := os.WriteFile(tmp, []byte(t.Format(time.RFC3339Nano)+"\n"), 0o600)
	if err == nil {
		err = os.Rename(tmp, u.statePath)
	}
	if err != nil {
		u.logf("auto-update: save check time: %v", err)
	}
}
