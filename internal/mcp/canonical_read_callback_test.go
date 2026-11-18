package mcp

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// canonicalReadWithOwner runs fixture callbacks on the test's goroutine. A
// fatal fixture assertion must terminate the test, not Goexit the handler and
// leave the production deadline firewall waiting for an outcome it cannot get.
// The handler still uses the real contexts, deadlines, middleware and leases.
func canonicalReadWithOwner(t *testing.T, invoke func(func(func()) bool) (*mcplib.CallToolResult, error)) (*mcplib.CallToolResult, error) {
	t.Helper()
	type callback struct {
		run  func()
		done chan struct{}
	}
	type reply struct {
		result *mcplib.CallToolResult
		err    error
	}
	callbacks := make(chan callback)
	stopped := make(chan struct{})
	finished := make(chan reply, 1)
	joined := false
	defer func() {
		close(stopped)
		if !joined {
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Error("owned fixture call did not join after callback assertion failed")
			}
		}
	}()
	go func() {
		result, err := invoke(func(run func()) bool {
			call := callback{run: run, done: make(chan struct{})}
			select {
			case callbacks <- call:
			case <-stopped:
				return false
			}
			select {
			case <-call.done:
				return true
			case <-stopped:
				return false
			}
		})
		finished <- reply{result, err}
	}()
	for {
		select {
		case call := <-callbacks:
			call.run()
			close(call.done)
		case got := <-finished:
			joined = true
			return got.result, got.err
		}
	}
}

func TestCanonicalReadCallbackFatalDoesNotStrandHandler(t *testing.T) {
	const markerEnv = "GORTEX_CANONICAL_CALLBACK_JOIN_TEST"
	if marker := os.Getenv(markerEnv); marker != "" {
		_, _ = canonicalReadWithOwner(t, func(onOwner func(func()) bool) (*mcplib.CallToolResult, error) {
			if onOwner(func() { t.Fatal("controlled owning-test callback failure") }) {
				return mcplib.NewToolResultError("fatal callback returned"), nil
			}
			// This runs only after the owner's fatal assertion stopped the callback.
			// The helper must join this completion before test-process cleanup exits.
			if err := os.WriteFile(marker, []byte("handler joined"), 0600); err != nil {
				return nil, err
			}
			return mcplib.NewToolResultError("fixture callback aborted"), nil
		})
		return
	}
	marker := filepath.Join(t.TempDir(), "handler-joined")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCanonicalReadCallbackFatalDoesNotStrandHandler$")
	child.Env = append(os.Environ(), markerEnv+"="+marker)
	output, err := child.CombinedOutput()
	require.NoError(t, ctx.Err(), "fatal fixture callback must not wait for the production firewall: %s", output)
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "the child's controlled test assertion must fail: %s", output)
	require.Contains(t, string(output), "controlled owning-test callback failure")
	joined, err := os.ReadFile(marker)
	require.NoError(t, err, "the aborted callback's handler must finish before the failed test exits")
	require.Equal(t, "handler joined", string(joined))
}
