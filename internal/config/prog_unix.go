//go:build !windows

package config

import "path/filepath"

// progName is the base name of a program path.
func progName(p string) string { return filepath.Base(p) }
