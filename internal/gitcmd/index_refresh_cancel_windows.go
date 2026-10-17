//go:build windows

package gitcmd

import "os/exec"

// Console-less Windows children have no equivalent SIGTERM cleanup protocol.
// Preserve their existing cancellation behavior rather than claim lock cleanup.
func configureIndexRefreshCancellation(_ *exec.Cmd) {}
