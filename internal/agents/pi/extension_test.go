package pi

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The extension is TypeScript, so its behaviour is covered by a node:test
// suite in extension/. This drives that suite, so a failure there fails
// `go test ./...` too.
//
// Node 24 is the floor: the suite imports index.ts directly under type
// stripping, with no build step and no package.json in the tree.
const nodeMajorFloor = 24

// requireNodeEnv makes a missing or too-old toolchain a hard failure.
// CI sets it: a runner that quietly skipped would drop the only coverage
// the extension has, which is the outcome this suite exists to prevent.
const requireNodeEnv = "GORTEX_REQUIRE_NODE"

// node --test exits 0 when it discovers no test files, so the count is what
// separates "the suite passed" from "the suite never ran". A file renamed off
// node's default glob would otherwise leave CI green with no coverage.
var nodeTestCount = regexp.MustCompile(`(?m)^\W*tests\s+(\d+)\s*$`)

func TestPiExtensionHarness(t *testing.T) {
	node, reason := usableNode()
	if reason != "" {
		if envRequiresNode() {
			t.Fatalf("%s is set and the Pi extension suite cannot run: %s", requireNodeEnv, reason)
		}
		t.Skipf("skipping the Pi extension suite: %s (set %s=1 to make this a failure)", reason, requireNodeEnv)
	}

	// The barrier parks a turn on a promise, so a regression can hang. Bound
	// it well above the suite's ~3s.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// node --test resolves a positional argument as a file, so the directory
	// it discovers *_test.mjs in has to be the cwd.
	cmd := exec.CommandContext(ctx, node, "--test")
	cmd.Dir = "extension"
	// Killing node leaves one grandchild per suite file holding the output
	// pipe open; without this, Wait outlives the context and the effective
	// bound becomes the whole-run -timeout.
	cmd.WaitDelay = 10 * time.Second

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the Pi extension suite timed out after 2m (%s):\n%s", describeOutput(out), out)
	}
	if err != nil {
		t.Fatalf("the Pi extension suite failed (%v):\n%s", err, out)
	}

	ran, ok := testsRun(out)
	if !ok {
		t.Fatalf("could not read a test count out of the node:test output — the reporter format changed:\n%s", out)
	}
	if ran == 0 {
		t.Fatalf("the Pi extension suite discovered no test files, and node --test exits 0 for that:\n%s", out)
	}
}

// envRequiresNode reports whether requireNodeEnv demands a usable toolchain.
// Any non-empty value other than an explicit false counts: someone exporting
// GORTEX_REQUIRE_NODE=yes means to arm the gate, and ParseBool alone reads
// that as off, restoring the silent skip the variable exists to abolish.
func envRequiresNode() bool {
	v := strings.TrimSpace(os.Getenv(requireNodeEnv))
	if v == "" {
		return false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return true
}

// testsRun reports how many tests node:test executed.
func testsRun(out []byte) (int, bool) {
	m := nodeTestCount.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return n, true
}

func describeOutput(out []byte) string {
	if len(out) == 0 {
		return "no output captured"
	}
	return strconv.Itoa(len(out)) + " bytes captured"
}

// usableNode returns the node binary to run the suite with, or a reason it
// cannot be run.
func usableNode() (path, reason string) {
	node, err := exec.LookPath("node")
	if err != nil {
		return "", "no `node` on PATH"
	}

	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		return "", "`node --version` failed: " + err.Error()
	}

	major, ok := nodeMajor(string(out))
	if !ok {
		return "", "could not parse a version out of `node --version` (" + strings.TrimSpace(string(out)) + ")"
	}
	if major < nodeMajorFloor {
		return "", "node " + strconv.Itoa(major) + " is older than the v" + strconv.Itoa(nodeMajorFloor) + " needed to run index.ts under type stripping"
	}
	return node, ""
}

// nodeMajor parses the major version out of `node --version` ("v24.16.0").
func nodeMajor(version string) (int, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, false
	}
	return n, true
}

func TestTestsRunReadsNodeReporterCounts(t *testing.T) {
	// Verbatim tails from `node --test`: a populated run, and the empty
	// directory case that exits 0.
	populated := "✔ a turn arriving mid-handshake (641ms)\nℹ tests 26\nℹ suites 6\nℹ pass 26\nℹ fail 0\n"
	empty := "ℹ tests 0\nℹ suites 0\nℹ pass 0\nℹ fail 0\n"

	if n, ok := testsRun([]byte(populated)); !ok || n != 26 {
		t.Errorf("populated run: got (%d, %v), want (26, true)", n, ok)
	}
	if n, ok := testsRun([]byte(empty)); !ok || n != 0 {
		t.Errorf("empty run: got (%d, %v), want (0, true)", n, ok)
	}
	if _, ok := testsRun([]byte("some unrelated output\n")); ok {
		t.Error("output with no count line: got ok=true, want false")
	}
}

func TestEnvRequiresNode(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		// Unparseable but set: honour the intent rather than silently skipping.
		{"yes", true},
		{"on", true},
		{"Y", true},
	} {
		t.Setenv(requireNodeEnv, tc.value)
		if got := envRequiresNode(); got != tc.want {
			t.Errorf("%s=%q: got %v, want %v", requireNodeEnv, tc.value, got, tc.want)
		}
	}
}
