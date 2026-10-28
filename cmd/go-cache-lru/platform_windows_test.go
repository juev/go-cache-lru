package main

import (
	"os"
	"testing"
)

// exeName is the plugin binary's file name; Windows runs only files
// ending in ".exe".
const exeName = "go-cache-lru.exe"

// moveAside renames bin away: Windows does not replace a running
// executable, but lets it be renamed.
func moveAside(t *testing.T, bin string) {
	t.Helper()
	if err := os.Rename(bin, bin+".old"); err != nil {
		t.Fatal(err)
	}
}
