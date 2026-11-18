package main

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newLegacyMCPCmd builds a throwaway cobra command carrying the same legacy
// flag surface `gortex mcp` exposes, so warnLegacyMCPFlags can be driven
// without mutating the real mcpCmd.
func newLegacyMCPCmd() *cobra.Command {
	var (
		index    string
		watch    bool
		proxy    bool
		noDaemon bool
		noCache  bool
	)
	cmd := &cobra.Command{Use: "mcp", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().StringVar(&index, "index", "", "repository path to index on startup")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep graph in sync with filesystem changes")
	cmd.Flags().BoolVar(&proxy, "proxy", false, "require a running daemon and proxy through it")
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "force embedded server")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "disable graph caching")
	return cmd
}

// captureLegacyStderr swaps os.Stderr and os.Stdout for pipes, runs fn,
// and returns whatever fn wrote to each. warnLegacyMCPFlags writes to
// os.Stderr directly (not cmd.ErrOrStderr), so the swap is the only way to
// observe its output.
func captureLegacyStderr(t *testing.T, fn func()) (stderr, stdout string) {
	t.Helper()
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = errR.Close()
		_ = errW.Close()
		t.Fatalf("stdout pipe: %v", err)
	}
	// Readers must run during fn: a full pipe blocks Write, and on Windows
	// even a small logger.Sync waits for the pipe reader to consume its bytes.
	var errBuf, outBuf bytes.Buffer
	errDone, outDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := errBuf.ReadFrom(errR); errDone <- err }()
	go func() { _, err := outBuf.ReadFrom(outR); outDone <- err }()
	origErr, origOut := os.Stderr, os.Stdout
	os.Stderr, os.Stdout = errW, outW
	defer func() {
		os.Stderr, os.Stdout = origErr, origOut
		_ = errW.Close()
		_ = outW.Close()
		if err := <-errDone; err != nil {
			t.Errorf("read captured stderr: %v", err)
		}
		if err := <-outDone; err != nil {
			t.Errorf("read captured stdout: %v", err)
		}
		_ = errR.Close()
		_ = outR.Close()
		stderr, stdout = errBuf.String(), outBuf.String()
	}()

	fn()
	return
}

// TestLegacyFlags_NoError asserts that the compatibility flags still parse
// (a stale editor config must not break), while only the retired flags emit a
// deprecation note. The active --proxy safety flag parses without a warning.
// Nothing may be written to stdout, which is the MCP JSON-RPC stream.
func TestLegacyFlags_NoError(t *testing.T) {
	// Every legacy flag set at once, plus its full combination, must parse
	// without a flag-parse error.
	allFlagsCmd := newLegacyMCPCmd()
	if err := allFlagsCmd.ParseFlags([]string{"--index", ".", "--watch", "--proxy", "--no-daemon", "--no-cache"}); err != nil {
		t.Fatalf("legacy flag combination must parse without error: %v", err)
	}

	// The real command must still accept every shimmed flag: an editor
	// mcp.json is never migrated, and cobra hard-errors on an unknown flag.
	for _, lf := range legacyMCPFlags {
		if mcpCmd.Flags().Lookup(lf.name) == nil {
			t.Fatalf("`gortex mcp` no longer registers the --%s compat shim; a stale editor config would fail to start", lf.name)
		}
	}

	cases := []struct {
		name      string
		argv      []string
		wantNotes []string // legacy flag names expected to produce a stderr note
	}{
		{"index only", []string{"--index", "."}, []string{"index"}},
		{"watch only", []string{"--watch"}, []string{"watch"}},
		{"proxy only", []string{"--proxy"}, nil},
		{"no-daemon only", []string{"--no-daemon"}, []string{"no-daemon"}},
		{"no-cache only", []string{"--no-cache"}, []string{"no-cache"}},
		{
			"all five",
			[]string{"--index", ".", "--watch", "--proxy", "--no-daemon", "--no-cache"},
			[]string{"index", "watch", "no-daemon", "no-cache"},
		},
		{"none set", []string{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newLegacyMCPCmd()
			if err := cmd.ParseFlags(tc.argv); err != nil {
				t.Fatalf("ParseFlags(%v) errored: %v", tc.argv, err)
			}

			// warnLegacyMCPFlags is guarded by a package-global so it only
			// warns once per process; reset it so each case is observed.
			legacyMCPFlagsWarned = false
			t.Cleanup(func() { legacyMCPFlagsWarned = false })

			stderr, stdout := captureLegacyStderr(t, func() {
				warnLegacyMCPFlags(cmd)
			})

			if stdout != "" {
				t.Fatalf("warnLegacyMCPFlags must write nothing to stdout (it is the JSON-RPC stream); got %q", stdout)
			}

			lines := nonEmptyLines(stderr)
			if len(lines) != len(tc.wantNotes) {
				t.Fatalf("want %d stderr note(s) %v, got %d: %q", len(tc.wantNotes), tc.wantNotes, len(lines), stderr)
			}
			for _, name := range tc.wantNotes {
				if !legacyNoteMentions(stderr, name) {
					t.Fatalf("expected a deprecation note for --%s, got: %q", name, stderr)
				}
			}
			// Every emitted note must name a deprecated/ignored flag.
			for _, ln := range lines {
				if !strings.Contains(ln, "deprecated") {
					t.Fatalf("stderr note is not a deprecation notice: %q", ln)
				}
			}
		})
	}
}

// nonEmptyLines splits s into its non-blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			out = append(out, sc.Text())
		}
	}
	return out
}

// legacyNoteMentions reports whether stderr carries a note for the given
// legacy flag name (matched as the `--<name>` token).
func legacyNoteMentions(stderr, name string) bool {
	return strings.Contains(stderr, "--"+name+" ")
}

// TestRunMCP_TTYvsPipe_Identical proves the `gortex mcp` mode decision is
// independent of whether stdin is a TTY or a pipe. The decision is made by
// resolveDaemonDecision -> ensureDaemonReady from daemon-presence plus
// GORTEX_AUTOSTART, never from a stdin char-device probe (the old
// shouldTryProxy heuristic is gone).
//
// Two things are asserted:
//  1. For a fixed daemon state, the resolved decision is byte-identical
//     regardless of what os.Stdin points at (a pipe / non-TTY vs a regular
//     file). If any stdin char-device check leaked into the decision path,
//     swapping stdin would change the result.
//  2. The decision tracks daemon presence + autostart only — a live daemon
//     yields daemonReady under any stdin; autostart-off with no daemon
//     yields daemonUnavailable under any stdin.
func TestRunMCP_TTYvsPipe_Identical(t *testing.T) {
	defer restoreSeams()

	// A regular file stands in for "not a char device" and a pipe stands in
	// for the piped-stdin case an editor uses; neither is a TTY in CI. The
	// point is that the decision must not vary across these.
	regularFile, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer regularFile.Close()
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pipeR.Close()
	defer pipeW.Close()

	stdins := map[string]*os.File{
		"regular-file": regularFile,
		"pipe":         pipeR,
	}

	// Pin a fixed daemon state per scenario via the injectable seams, then
	// resolve the decision under each stdin and assert agreement.
	scenarios := []struct {
		name      string
		running   bool
		autostart bool
		want      daemonDecision
	}{
		{"daemon live", true, true, daemonReady},
		{"daemon live, autostart off", true, false, daemonReady},
		{"no daemon, autostart off", false, false, daemonUnavailable},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			isDaemonRunning = func() bool { return sc.running }
			spawnDaemon = func() error { t.Fatal("no spawn expected in this scenario"); return nil }
			t.Cleanup(restoreSeams)

			origStdin := os.Stdin
			defer func() { os.Stdin = origStdin }()

			var first daemonDecision
			var firstName string
			i := 0
			for name, f := range stdins {
				os.Stdin = f
				got := ensureDaemonReady(sc.autostart)
				if got != sc.want {
					t.Fatalf("stdin=%s: ensureDaemonReady(%v) = %d, want %d", name, sc.autostart, got, sc.want)
				}
				if i == 0 {
					first, firstName = got, name
				} else if got != first {
					t.Fatalf("decision differs by stdin: stdin=%s -> %d but stdin=%s -> %d (decision leaked a stdin probe)",
						firstName, first, name, got)
				}
				i++
			}
		})
	}

	// resolveDaemonDecision is the production entry point; confirm it too is
	// stdin-agnostic by pinning autostart off and a dead daemon, then
	// re-resolving under both stdins.
	t.Run("resolveDaemonDecision stdin-agnostic", func(t *testing.T) {
		t.Setenv("GORTEX_AUTOSTART", "0")
		isDaemonRunning = func() bool { return false }
		spawnDaemon = func() error { t.Fatal("autostart off must not spawn"); return nil }
		t.Cleanup(restoreSeams)

		origStdin := os.Stdin
		defer func() { os.Stdin = origStdin }()

		os.Stdin = regularFile
		fromFile := resolveDaemonDecision()
		os.Stdin = pipeR
		fromPipe := resolveDaemonDecision()
		if fromFile != fromPipe {
			t.Fatalf("resolveDaemonDecision varied by stdin: file=%d pipe=%d", fromFile, fromPipe)
		}
		if fromFile != daemonUnavailable {
			t.Fatalf("autostart off + dead daemon must be daemonUnavailable, got %d", fromFile)
		}
	})
}
