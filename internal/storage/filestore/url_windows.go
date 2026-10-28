package filestore

import "path/filepath"

// urlPath turns the path of a file URL into a file path:
// file:///C:/share/gocache has the path /C:/share/gocache.
func urlPath(p string) string {
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}
