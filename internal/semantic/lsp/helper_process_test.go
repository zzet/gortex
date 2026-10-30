package lsp

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"

	"go.uber.org/zap"
)

// The transport tests need a subprocess with scripted stdout/stderr
// behaviour. They used to spawn /bin/sh and /bin/cat, which do not exist on
// Windows (and a `.sh` fixture is not executable there either), so the stand-
// in is this test binary re-exec'd against TestSubprocessHelper — the same
// helper-process pattern internal/semantic/scip and internal/indexer use.
//
// The behaviour selector travels in the environment rather than in argv so a
// transport's Command / Args stay exactly what the test wants to assert on.
const (
	helperModeEnv  = "GORTEX_LSP_SUBPROCESS_HELPER"
	helperLinesEnv = "GORTEX_LSP_SUBPROCESS_HELPER_LINES"

	// helperCat copies stdin to stdout and exits when stdin closes — the
	// blocking, well-behaved child /bin/cat used to provide.
	helperCat = "cat"
	// helperStderrLines writes helperLinesEnv "err<N>" lines to stderr and
	// then one line to stdout.
	helperStderrLines    = "stderr-lines"
	helperLSPInitBarrier = "lsp-initialize-barrier"
	helperInitEnteredEnv = "GORTEX_LSP_HELPER_INITIALIZE_ENTERED"
	helperInitReleaseEnv = "GORTEX_LSP_HELPER_INITIALIZE_RELEASE"
	helperLSPGraceful    = "lsp-graceful"
	helperLSPIgnore      = "lsp-ignore-shutdown"
	helperLSPNoRead      = "lsp-no-read"
	helperExitClean      = "exit-clean"
	helperTranscriptEnv  = "GORTEX_LSP_SUBPROCESS_TRANSCRIPT"
	helperReadyEnv       = "GORTEX_LSP_SUBPROCESS_READY"
)

// helperArgs is the argv that re-runs this binary as the helper process.
// The trailing "--" keeps any further arguments away from the test flags.
var helperArgs = []string{"-test.run=^TestSubprocessHelper$", "--"}

// helperCommand returns the path to this test binary, the command every
// helper-process spawn uses.
func helperCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return exe
}

// useHelper arms the environment the re-exec'd child reads. It must be called
// from the test goroutine (t.Setenv), so no test using it may run in parallel.
func useHelper(t *testing.T, mode string, lines int) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)
	t.Setenv(helperLinesEnv, strconv.Itoa(lines))
}

// helperTransport builds a SpawnTransport whose subprocess is this test
// binary running the scripted helper behaviour.
func helperTransport(t *testing.T, mode string, lines int, logger *zap.Logger) *SpawnTransport {
	t.Helper()
	useHelper(t, mode, lines)
	return &SpawnTransport{
		Command: helperCommand(t),
		Args:    helperArgs,
		// Keep race detection enabled, but remove its artificial process-exit
		// sleep from actual child-reap tests (the Go runtime's own subprocess
		// tests use the same option). This changes only the test fixture.
		Env:    []string{"GORACE=" + os.Getenv("GORACE") + " atexit_sleep_ms=0"},
		Logger: logger,
	}
}

// TestSubprocessHelper is not a test. It is the child process the transport
// tests spawn; with helperModeEnv unset (an ordinary suite run) it returns
// immediately and passes. It exits the process itself so the test framework's
// own "PASS" summary never lands on the stdout pipe the parent is reading.
func TestSubprocessHelper(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case helperLSPGraceful, helperLSPIgnore, helperLSPNoRead, helperLSPInitBarrier:
		runShutdownHelper(t, mode)
	case helperExitClean:
		// Immediate success exercises EOF before the caller begins shutdown.
	case helperCat:
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case helperStderrLines:
		n, err := strconv.Atoi(os.Getenv(helperLinesEnv))
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "helper: bad line count %q\n", os.Getenv(helperLinesEnv))
			os.Exit(2)
		}
		for i := 1; i <= n; i++ {
			fmt.Fprintf(os.Stderr, "err%d\n", i)
		}
		fmt.Fprintln(os.Stdout, "stdout-line")
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}
