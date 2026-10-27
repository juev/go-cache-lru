package config

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"30GiB": 30 << 30, "30G": 30 << 30, "30gb": 30e9, "512MiB": 512 << 20,
		"1.5G": 3 << 29, "100": 100, "4 KiB": 4096, "10B": 10,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "-1G", "G"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): want error", in)
		}
	}
}

func parse(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f.Resolve()
}

func TestDefaults(t *testing.T) {
	gocache := t.TempDir()
	t.Setenv("GOCACHE", gocache)
	for _, k := range []string{EnvDir, EnvMaxSize, EnvTargetSize, EnvSocket, EnvRemote} {
		t.Setenv(k, "")
	}
	c, err := parse(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != gocache || c.GoCache != gocache || c.MaxSize != 30<<30 || c.TargetSize != 25<<30 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	gocache, dir := t.TempDir(), t.TempDir()
	t.Setenv("GOCACHE", gocache)
	t.Setenv(EnvDir, "/nonexistent")
	t.Setenv(EnvMaxSize, "1G")
	t.Setenv(EnvTargetSize, "")
	c, err := parse(t, "--dir="+dir, "--max-size=30GiB", "--target-size=25GiB")
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != dir || c.GoCache != gocache || c.MaxSize != 30<<30 || c.TargetSize != 25<<30 {
		t.Fatalf("got %+v", c)
	}
	if !strings.HasPrefix(c.Socket, filepath.Join(dir, "gocacheprog")) && !strings.Contains(c.Socket, "gocachelru-") {
		t.Fatalf("socket %s", c.Socket)
	}
}

func TestArgsRoundTrip(t *testing.T) {
	t.Setenv("GOCACHE", t.TempDir())
	c, err := parse(t, "--dir="+t.TempDir(), "--max-size=3G", "--target-size=2G", "--remote=file:///x", "--idle-timeout=1h")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := parse(t, c.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	if c != c2 {
		t.Fatalf("round trip:\n%+v\n%+v", c, c2)
	}
}

func TestRejectsTargetAboveMax(t *testing.T) {
	t.Setenv("GOCACHE", t.TempDir())
	if _, err := parse(t, "--max-size=1G", "--target-size=2G"); err == nil {
		t.Fatal("want error")
	}
}
