// Package config resolves settings shared by the daemon and its clients.
// Settings come from flags, which default to GOCACHELRU_* environment
// variables. A client passes its resolved settings to the daemon it spawns.
package config

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juev/go-cache-lru/internal/cache"
)

// Environment variables.
const (
	EnvDir          = "GOCACHELRU_DIR"
	EnvMaxSize      = "GOCACHELRU_MAX_SIZE"
	EnvTargetSize   = "GOCACHELRU_TARGET_SIZE"
	EnvRemote       = "GOCACHELRU_REMOTE"
	EnvSocket       = "GOCACHELRU_SOCKET"
	EnvIdleTimeout  = "GOCACHELRU_IDLE_TIMEOUT"
	EnvDrainTimeout = "GOCACHELRU_SHUTDOWN_TIMEOUT"
)

// DefaultDrainTimeout is the default for --shutdown-timeout.
const DefaultDrainTimeout = time.Minute

// Config is the resolved configuration.
type Config struct {
	Dir          string        // cache directory
	GoCache      string        // the go command's GOCACHE, watched for "go clean -cache"
	MaxSize      int64         // eviction starts above this
	TargetSize   int64         // eviction shrinks to this
	Remote       string        // optional remote storage URL
	Socket       string        // daemon socket path
	IdleTimeout  time.Duration // daemon exits after this long without sessions; 0 never
	DrainTimeout time.Duration // how long shutdown waits for active sessions
}

// StateDir is where the daemon keeps its lock, log and statistics.
func (c Config) StateDir() string { return filepath.Join(c.Dir, cache.StateDirName) }

// LockPath is the daemon's exclusive lock.
func (c Config) LockPath() string { return filepath.Join(c.StateDir(), "daemon.lock") }

// SpawnLockPath serializes daemon autostart between clients.
func (c Config) SpawnLockPath() string { return filepath.Join(c.StateDir(), "spawn.lock") }

// LogPath is the autostarted daemon's log.
func (c Config) LogPath() string { return filepath.Join(c.StateDir(), "daemon.log") }

// Flags holds the raw command-line settings. Each flag defaults to its
// environment variable, so flags win over the environment.
type Flags struct {
	dir, maxSize, targetSize, remote, socket string
	idle, drain                              time.Duration
	envErr                                   error
}

// Register adds the configuration flags to fs.
func Register(fs *flag.FlagSet) *Flags {
	f := new(Flags)
	fs.StringVar(&f.dir, "dir", os.Getenv(EnvDir), "cache `directory` (default: GOCACHE) ["+EnvDir+"]")
	fs.StringVar(&f.maxSize, "max-size", envOr(EnvMaxSize, "30GiB"), "evict when the cache grows above this `size` ["+EnvMaxSize+"]")
	fs.StringVar(&f.targetSize, "target-size", envOr(EnvTargetSize, "25GiB"), "size to shrink the cache to on eviction ["+EnvTargetSize+"]")
	fs.StringVar(&f.remote, "remote", os.Getenv(EnvRemote), "optional remote storage `URL` (file://..., s3://...) ["+EnvRemote+"]")
	fs.StringVar(&f.socket, "socket", os.Getenv(EnvSocket), "daemon socket `path` (default: <dir>/gocacheprog/daemon.sock) ["+EnvSocket+"]")
	var err error
	f.idle, err = durationEnv(EnvIdleTimeout, 0)
	f.envErr = errors.Join(f.envErr, err)
	f.drain, err = durationEnv(EnvDrainTimeout, DefaultDrainTimeout)
	f.envErr = errors.Join(f.envErr, err)
	fs.DurationVar(&f.idle, "idle-timeout", f.idle, "stop the daemon after this long without go commands, 0 never ["+EnvIdleTimeout+"]")
	fs.DurationVar(&f.drain, "shutdown-timeout", f.drain, "how long shutdown waits for running go commands ["+EnvDrainTimeout+"]")
	return f
}

// Resolve validates the flags and fills in defaults.
func (f *Flags) Resolve() (Config, error) {
	var c Config
	if f.envErr != nil {
		return c, f.envErr
	}
	var err error
	c.GoCache, err = goCacheDir(f.dir == "")
	if err != nil {
		return c, err
	}
	c.Dir = f.dir
	if c.Dir == "" {
		c.Dir = c.GoCache
	}
	if c.Dir, err = filepath.Abs(expandHome(c.Dir)); err != nil {
		return c, err
	}
	if c.GoCache == "" {
		c.GoCache = c.Dir
	}
	if c.MaxSize, err = ParseSize(f.maxSize); err != nil {
		return c, fmt.Errorf("--max-size: %w", err)
	}
	if c.TargetSize, err = ParseSize(f.targetSize); err != nil {
		return c, fmt.Errorf("--target-size: %w", err)
	}
	if c.TargetSize <= 0 || c.TargetSize > c.MaxSize {
		return c, fmt.Errorf("--target-size (%s) must be positive and not above --max-size (%s)", f.targetSize, f.maxSize)
	}
	c.Remote = f.remote
	c.IdleTimeout = f.idle
	c.DrainTimeout = f.drain
	c.Socket = expandHome(f.socket)
	if c.Socket == "" {
		c.Socket = defaultSocket(c.StateDir())
	}
	return c, nil
}

// Args renders c as flags, for passing the configuration to a spawned
// daemon.
func (c Config) Args() []string {
	args := []string{
		"--dir=" + c.Dir,
		"--max-size=" + strconv.FormatInt(c.MaxSize, 10),
		"--target-size=" + strconv.FormatInt(c.TargetSize, 10),
		"--socket=" + c.Socket,
		"--idle-timeout=" + c.IdleTimeout.String(),
		"--shutdown-timeout=" + c.DrainTimeout.String(),
	}
	if c.Remote != "" {
		args = append(args, "--remote="+c.Remote)
	}
	return args
}

// ProgArgs returns the arguments that the go command's GOCACHEPROG passes
// to go-cache-lru, so that a control command run by hand finds the daemon
// the go commands use. It reads GOCACHEPROG from the environment, then from
// "go env", and returns nil unless it runs a binary named like self.
func ProgArgs(self string) []string {
	prog := os.Getenv("GOCACHEPROG")
	if prog == "" {
		out, err := exec.Command("go", "env", "GOCACHEPROG").Output()
		if err != nil {
			return nil
		}
		prog = strings.TrimSpace(string(out))
	}
	return progArgs(prog, self)
}

func progArgs(prog, self string) []string {
	fields, err := splitQuoted(prog)
	if err != nil || len(fields) == 0 || filepath.Base(fields[0]) != filepath.Base(self) {
		return nil
	}
	return fields[1:]
}

// splitQuoted splits s into fields the way the go command splits
// GOCACHEPROG: on white space, with single- or double-quoted fields.
func splitQuoted(s string) ([]string, error) {
	var fields []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			return fields, nil
		}
		if q := s[0]; q == '"' || q == '\'' {
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c string", q)
			}
			fields = append(fields, s[1:1+end])
			s = s[2+end:]
			continue
		}
		end := strings.IndexAny(s, " \t\n\r")
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// goCacheDir returns the go command's cache directory. The go command
// exports GOCACHE to the cache program; elsewhere "go env" resolves it.
// Without required, a failure to resolve it is not an error.
func goCacheDir(required bool) (string, error) {
	dir := os.Getenv("GOCACHE")
	if dir == "" {
		out, err := exec.Command("go", "env", "GOCACHE").Output()
		if err != nil {
			if !required {
				return "", nil
			}
			return "", fmt.Errorf("resolve GOCACHE: %w", err)
		}
		dir = strings.TrimSpace(string(out))
	}
	if dir == "" || dir == "off" {
		if !required {
			return "", nil
		}
		return "", errors.New("no cache directory: set --dir or GOCACHE")
	}
	return filepath.Abs(dir)
}

// defaultSocket keeps the socket in the state directory unless the path
// exceeds the sun_path limit (104 bytes on macOS), in which case it goes
// to the temp directory under a name derived from the state directory.
func defaultSocket(stateDir string) string {
	p := filepath.Join(stateDir, "daemon.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(stateDir))
	return filepath.Join(os.TempDir(), fmt.Sprintf("gocachelru-%d-%x.sock", os.Getuid(), sum[:6]))
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// ParseSize parses sizes like "30G", "25GiB", "500MB" or "1048576".
// Single-letter and *iB suffixes are binary, *B suffixes are decimal.
func ParseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, su := range sizeUnits {
		if strings.HasSuffix(u, su.suffix) {
			u = strings.TrimSpace(strings.TrimSuffix(u, su.suffix))
			mult = su.mult
			break
		}
	}
	f, err := strconv.ParseFloat(u, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatSize renders a byte count with a binary unit.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
