package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureOwnedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func killOwnedProcess(cmd *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	var output diagnosticBuffer
	kill.Stdout, kill.Stderr = &output, &output
	if err := kill.Run(); err != nil {
		return fmt.Errorf("taskkill owned PID %d: %w; output: %s", cmd.Process.Pid, err, output.String())
	}
	return nil
}

// Called only after Wait completed (the daemon completion channel or Git Output). taskkill
// cannot target a PID that Windows has already reaped (it returns exit128),
// and targeting a reused PID could affect an unrelated process. Live forced
// cancellation still uses taskkill /T above; never suppress its failures.
func cleanupExitedOwnedProcess(_ *exec.Cmd) error { return nil }
