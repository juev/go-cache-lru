package cache

import (
	"runtime"
	"strings"
	"testing"
)

func TestIndexDSN(t *testing.T) {
	cases := map[string]string{
		"/home/me/.cache/go-lru/gocacheprog/index.db": "file:/home/me/.cache/go-lru/gocacheprog/index.db",
		"/tmp/a b?c#d%e/index.db":                     "file:/tmp/a%20b%3Fc%23d%25e/index.db",
		"C:/Users/me/AppData/Local/go-build/index.db": "file:/C:/Users/me/AppData/Local/go-build/index.db",
	}
	if runtime.GOOS == "windows" {
		cases[`C:\Users\me\go lru\index.db`] = "file:/C:/Users/me/go%20lru/index.db"
	}
	for path, want := range cases {
		dsn := indexDSN(path)
		got, query, _ := strings.Cut(dsn, "?")
		if got != want {
			t.Errorf("indexDSN(%q) = %q, want %q", path, got, want)
		}
		if !strings.Contains(query, "journal_mode%28WAL%29") {
			t.Errorf("indexDSN(%q) lost the pragmas: %q", path, dsn)
		}
	}
}
