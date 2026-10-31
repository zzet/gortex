package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticBufferKeepsBoundedPrefixAndFatalTail(t *testing.T) {
	for _, chunkSize := range []int{1, 3000, 100000} {
		input := "initial context\n" + strings.Repeat("x", 50000) + "\nfatal: useful final detail"
		var output diagnosticBuffer
		for start := 0; start < len(input); start += chunkSize {
			chunk := input[start:min(start+chunkSize, len(input))]
			if count, err := output.Write([]byte(chunk)); err != nil || count != len(chunk) {
				t.Fatalf("writer failed: %d %v", count, err)
			}
		}
		got := output.String()
		if !strings.HasPrefix(got, "initial context\n") || !strings.HasSuffix(got, "fatal: useful final detail") || !strings.Contains(got, "diagnostic bytes omitted") || len(got) > 2*diagnosticHalf+100 || len(output.head) > diagnosticHalf || len(output.tail) > diagnosticHalf {
			t.Fatalf("stderr not bounded or lost context/fatal tail: bytes%d", len(got))
		}
	}
	var small diagnosticBuffer
	if _, err := small.Write([]byte("fatal: concise")); err != nil || small.String() != "fatal: concise" {
		t.Fatal("small stderr changed")
	}
}

func TestPrivateGitFailurePreservesOperationStderrAndExitStatus(t *testing.T) {
	corpus := gitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	private, err := newPrivateRun(ctx, t.TempDir(), "/unused", []Corpus{corpus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = private.removeState()
		_ = os.RemoveAll(private.SocketDir)
	})
	const missing = "refs/heads/definitely-not-present-996"
	_, err = private.git(ctx, private.Repos[0].Path, "rev-parse", "--verify", missing)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || !strings.Contains(err.Error(), "git rev-parse in "+private.Repos[0].Path) || !strings.Contains(err.Error(), "stderr:") || !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("Git failure provenance lost: %v", err)
	}
}
