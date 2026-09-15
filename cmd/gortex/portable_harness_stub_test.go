package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"testing"
)

// The harness unit tests execute this test binary as a native child process.
// The selector is installed only in that child's environment, so ordinary
// test discovery and real opt-in daemon binaries never enter this dispatcher.
const portableHarnessModeEnv = "GX_TEST_PORTABLE_HARNESS_MODE"

func init() {
	mode := os.Getenv(portableHarnessModeEnv)
	if mode == "" {
		return
	}
	if err := runPortableHarnessStub(mode); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func portableHarnessStubBinary(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

func portableHarnessAssertExecutableBit(t *testing.T, mode fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not expose the POSIX executable permission bit")
	}
	if mode.Perm()&0o100 == 0 {
		t.Fatalf("the executable bit was lost: %v", mode)
	}
}

func portableHarnessStubTick() (int, error) {
	path := os.Getenv("GX_SUSTAINED_IO_STUB_COUNT")
	if path == "" {
		return 0, fmt.Errorf("stub counter path is required")
	}
	n := 0
	raw, err := os.ReadFile(path)
	if err == nil {
		n, err = strconv.Atoi(string(raw))
	}
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read stub counter: %w", err)
	}
	n++
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600); err != nil {
		return 0, err
	}
	return n, nil
}

func runPortableHarnessStub(mode string) error {
	var response any
	switch mode {
	case "argv":
		for _, arg := range os.Args[1:] {
			if _, err := fmt.Fprintln(os.Stdout, arg); err != nil {
				return err
			}
		}
		return nil
	case "noop":
		return nil
	case "search":
		response = map[string]any{"exact": true, "results": []any{
			map[string]any{"name": sustainedIOPrimaryMarker,
				"absolute_file_path": os.Getenv("GX_SUSTAINED_IO_STUB_MARKER"), "repo_prefix": "issue767"},
		}}
	case "counter":
		n, err := portableHarnessStubTick()
		if err != nil {
			return err
		}
		response = map[string]any{"views": map[string]any{"counters": map[string]int{
			"views_generation_published_total": 100 + n,
		}}}
	case "inexact-then-empty":
		n, err := portableHarnessStubTick()
		if err != nil {
			return err
		}
		if n <= 2 {
			response = map[string]any{"exact": false, "fallback_reason": "base_changed"}
		} else {
			response = map[string]any{"exact": true, "results": []any{}}
		}
	case "settled":
		response = map[string]any{"views": map[string]any{"counters": map[string]int{
			"views_dedicated_base_publication_total{outcome=skipped}": 1,
		}}}
	default:
		return fmt.Errorf("unknown portable harness mode %q", mode)
	}
	return json.NewEncoder(os.Stdout).Encode(response)
}
