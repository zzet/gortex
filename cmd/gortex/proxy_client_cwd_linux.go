//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// parentProcessCWD reads the current working directory of the process that
// spawned this proxy — the MCP host — from procfs. It returns "" when there
// is no meaningful parent (orphaned to init) or the link cannot be read.
func parentProcessCWD() string {
	ppid := os.Getppid()
	if ppid <= 1 {
		return ""
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(ppid) + "/cwd")
	if err != nil || strings.HasSuffix(cwd, " (deleted)") {
		return ""
	}
	return cwd
}
