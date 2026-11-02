package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A bounded helper process keeps the old capture implementation's deadlock
// from consuming the package timeout. Both streams must drain while fn runs;
// on Windows even a small stderr logger.Sync waits for its reader to consume.
func TestLegacyCaptureDrainsBothStreamsBeforeCallbackReturns(t *testing.T) {
	if os.Getenv("GORTEX_TEST_LEGACY_CAPTURE_DRAIN") == "1" {
		wantErr := strings.Repeat("stderr-payload\n", 1<<16)
		wantOut := strings.Repeat("stdout-payload\n", 1<<16)
		gotErr, gotOut := captureLegacyStderr(t, func() {
			_, _ = fmt.Fprint(os.Stderr, wantErr)
			_ = os.Stderr.Sync()
			_, _ = fmt.Fprint(os.Stdout, wantOut)
			_ = os.Stdout.Sync()
		})
		if gotErr != wantErr || gotOut != wantOut {
			t.Fatalf("capture lost bytes: stderr=%d/%d stdout=%d/%d", len(gotErr), len(wantErr), len(gotOut), len(wantOut))
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestLegacyCaptureDrainsBothStreamsBeforeCallbackReturns$")
	child.Env = append(os.Environ(), "GORTEX_TEST_LEGACY_CAPTURE_DRAIN=1")
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("capture callback did not finish while both streams drained: %v (context: %v)\n%s", err, ctx.Err(), output)
	}
}
