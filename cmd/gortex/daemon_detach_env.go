package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zzet/gortex/internal/serverstack"
)

func detachedDaemonEnv(parent []string, backendPath string) ([]string, error) {
	path, err := serverstack.ResolveStorePath(backendPath)
	if err != nil {
		return nil, fmt.Errorf("resolve detached SQLite temp directory: %w", err)
	}
	child := make([]string, 0, len(parent)+2)
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if key != "SQLITE_TMPDIR" && key != "GORTEX_DAEMON_CHILD" {
			child = append(child, entry)
		}
	}
	// SQLite/libc may cache the environment at first use. Set this before
	// exec, replacing inherited duplicates. General TMPDIR is deliberately
	// preserved: only SQLite's scratch placement changes.
	return append(child, "GORTEX_DAEMON_CHILD=1", "SQLITE_TMPDIR="+filepath.Dir(path)), nil
}
