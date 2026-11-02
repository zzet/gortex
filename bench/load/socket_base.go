package main

import (
	"os"
	"runtime"
)

// Socket placement is selected before allocating a private run. The default
// preserves the existing short-path behavior on every supported host.
func privateRunSocketBase() (string, error) {
	if runtime.GOOS != "windows" {
		return "/tmp", nil
	}
	return os.TempDir(), nil
}
