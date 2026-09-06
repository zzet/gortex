package indexer

import (
	"fmt"
	"os"
	"runtime"
)

// checkoutRootFileInfo captures physical directory identity before the caller
// waits or publishes a ticket. On Windows, os.Stat may defer loading the file
// ID until os.SameFile, reopening a pathname that could already be replaced.
// File.Stat loads identity from the open handle instead. Close that handle
// before returning so a retained identity never locks a worktree against moves.
// Other platforms already capture identity in Stat, without opening a handle.
func checkoutRootFileInfo(root string) (os.FileInfo, error) {
	var info os.FileInfo
	var err error
	if runtime.GOOS == "windows" {
		var file *os.File
		file, err = os.Open(root)
		if err != nil {
			return nil, err
		}
		info, err = file.Stat()
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	} else {
		info, err = os.Stat(root)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("checkout root is not a directory: %q", root)
	}
	return info, nil
}
