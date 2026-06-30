// Package naming generates default session names from the working directory,
// the enclosing git repository, or the current time.
package naming

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Generate returns a default session name for cwd. It prefers the enclosing git
// repository's path, then cwd, and finally a timestamp. Directory-derived names
// keep the last two path elements (e.g. "/a/b/c/d" -> "c/d") so sibling repos
// checked out under different parents stay distinguishable.
func Generate(cwd string, now time.Time) string {
	if root, ok := gitRoot(cwd); ok {
		return lastTwo(root)
	}

	if cwd != "" {
		base := filepath.Base(cwd)
		if base != "." && base != string(filepath.Separator) && base != "" {
			return lastTwo(cwd)
		}
	}

	return now.Format("2006-01-02-150405")
}

// lastTwo returns the final two elements of path joined ("/a/b/c/d" -> "c/d"), or
// just the final element when the path has only one ("/a" -> "a").
func lastTwo(path string) string {
	base := filepath.Base(path)
	parent := filepath.Base(filepath.Dir(path))

	if parent == "." || parent == string(filepath.Separator) {
		return base
	}

	return filepath.Join(parent, base)
}

// Unique returns base if it is free, otherwise base with the smallest "-N"
// suffix (starting at 2) that is not present in taken.
func Unique(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}

	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// gitRoot walks up from dir looking for a directory containing a ".git" entry.
func gitRoot(dir string) (string, bool) {
	for dir != "" {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	return "", false
}
