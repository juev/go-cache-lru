//go:build !windows

package main

import "testing"

// exeName is the plugin binary's file name.
const exeName = "go-cache-lru"

// moveAside prepares bin to be replaced by a rename; nothing to do here.
func moveAside(*testing.T, string) {}
