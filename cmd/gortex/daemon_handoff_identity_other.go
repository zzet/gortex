//go:build !linux && !darwin

package main

import "fmt"

func handoffProcessBirth(int) (string, error) {
	return "", fmt.Errorf("graceful PID-based daemon handoff is unsupported on this platform; use the matching binary to restart the daemon")
}
