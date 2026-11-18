//go:build !unix

package main

import "os/exec"

// The collector enables this policy only on Linux.
func ownQueryMicroProcess(_ *exec.Cmd) func() error {
	return func() error { return nil }
}
