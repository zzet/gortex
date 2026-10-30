// Command release-gate runs the strict release lane from a reviewed configuration.
// Invoke from the repository root: go run ./bench/release-gate --config FILE --out NEW-DIR.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	// Keep one implementation of orchestration, policy parsing and evidence
	// refusal. The supplied config must itself select the release lane.
	args := append([]string{"run", "./bench/check-budgets", "collect"}, os.Args[1:]...)
	args = append(args, "--require-lane", "release")
	command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
