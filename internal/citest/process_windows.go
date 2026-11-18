package citest

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// taskkill's tree is rooted at this still-owned process handle's PID.
		err := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		if err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
