package config

import (
	"strings"
	"testing"
)

// TestProgArgsWithoutExe checks that GOCACHEPROG may leave out ".exe" and
// differ in case from the running binary.
func TestProgArgsWithoutExe(t *testing.T) {
	got := progArgs(`C:\go\bin\go-cache-lru --dir=D:\c`, `C:\go\bin\Go-Cache-LRU.exe`)
	if strings.Join(got, "|") != `--dir=D:\c` {
		t.Errorf("progArgs = %q, want [--dir=D:\\c]", got)
	}
}
