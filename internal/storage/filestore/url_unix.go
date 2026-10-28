//go:build !windows

package filestore

// urlPath turns the path of a file URL into a file path.
func urlPath(p string) string { return p }
