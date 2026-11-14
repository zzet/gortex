//go:build !windows

package pathguard

import (
	"os"
	"path/filepath"
)

func isJunction(string, os.FileInfo) bool {
	return false
}

func evalLinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
