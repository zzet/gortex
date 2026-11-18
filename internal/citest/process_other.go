//go:build !unix && !windows

package citest

import "os/exec"

func configureProcess(_ *exec.Cmd) {}
