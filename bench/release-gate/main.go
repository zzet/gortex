// Command release-gate runs the strict release lane from a reviewed configuration.
// Build the entry binary first, then invoke it from the repository root with
// --config FILE --out NEW-DIR so a supervisor can signal the entry PID directly.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type collectorProgram struct {
	binary  string
	prefix  []string
	cleanup func() error
}

type prepareCollector func(context.Context, io.Writer, io.Writer) (collectorProgram, error)

func main() { os.Exit(releaseEntry(os.Args[1:], os.Stdout, os.Stderr, buildCollector)) }

func releaseEntry(args []string, stdout, stderr io.Writer, prepare prepareCollector) (code int) {
	started := time.Now().UTC()
	defer func() { fmt.Fprintf(stderr, "release entry wall time: %.3fs\n", time.Since(started).Seconds()) }()
	signals, cancelSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignals()
	// Includes compilation. The direct collector receives the same start time
	// and keeps three minutes for independent owned-daemon cleanup/receipts.
	ctx, cancel := context.WithDeadline(signals, started.Add(8*time.Hour-3*time.Minute))
	defer cancel()
	program, err := prepare(ctx, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() {
		if err := program.cleanup(); err != nil {
			fmt.Fprintln(stderr, err)
			code = 1
		}
	}()
	// Do not keep a go-run intermediary while any daemon can exist. Signals
	// directed only at this wrapper reach the actual collector and Run joins it.
	collectorArgs := append([]string{"collect"}, args...)
	collectorArgs = append(collectorArgs, "--require-lane", "release", "--release-started", started.Format(time.RFC3339Nano))
	commandArgs := append(append([]string{}, program.prefix...), collectorArgs...)
	if err := forwardAndJoin(ctx, program.binary, commandArgs, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		return 1
	}
	if time.Since(started) > 8*time.Hour {
		fmt.Fprintln(stderr, "release entry exceeded eight hours")
		return 1
	}
	return 0
}

func forwardAndJoin(ctx context.Context, binary string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// The load collector handles Interrupt, waits for its signal-aware load
	// child, and retains stop receipts. Never hard-kill that cleanup path.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	return cmd.Run()
}

func buildCollector(ctx context.Context, stdout, stderr io.Writer) (collectorProgram, error) {
	dir, err := os.MkdirTemp("", "release-gate-bin-")
	if err != nil {
		return collectorProgram{}, err
	}
	binary := filepath.Join(dir, "check-budgets")
	err = forwardAndJoin(ctx, "go", []string{"build", "-o", binary, "./bench/check-budgets"}, stdout, stderr)
	if err != nil {
		return collectorProgram{}, errors.Join(err, os.RemoveAll(dir))
	}
	return collectorProgram{binary: binary, cleanup: func() error { return os.RemoveAll(dir) }}, nil
}
