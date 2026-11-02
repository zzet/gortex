package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Socket placement is selected before allocating a private run. The default
// preserves the existing short-path behavior on every supported host.
func privateRunSocketBase() (string, error) {
	if base := os.Getenv("GORTEX_LOAD_SOCKET_BASE"); base != "" {
		if !filepath.IsAbs(base) || filepath.Clean(base) != base {
			return "", fmt.Errorf("load socket base must be an absolute canonical directory")
		}
		info, err := os.Lstat(base)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("load socket base must be an existing non-symlink directory: %s", base)
		}
		real, err := filepath.EvalSymlinks(base)
		if err != nil || real != base {
			return "", fmt.Errorf("load socket base must not traverse directory aliases: %s", base)
		}
		// Darwin's sockaddr_un allows103 path bytes plus its terminatingNUL.
		// The MkdirTemp suffix uses at most10 decimal digits. Keep the
		// explicit override portable without changing default placement.
		if len(filepath.Join(base, "gl-1234567890", "d.sock")) >= 104 {
			return "", fmt.Errorf("load socket base exceeds the portable Unix socket path limit: %s", base)
		}
		return base, nil
	}
	if runtime.GOOS != "windows" {
		return "/tmp", nil
	}
	return os.TempDir(), nil
}
