package config

import (
	"path/filepath"
	"strings"
)

// progName is the base name of a program path without ".exe", which
// GOCACHEPROG may leave out. Windows file names ignore case.
func progName(p string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(p)), ".exe")
}
