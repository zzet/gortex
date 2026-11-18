//go:build unix

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestGetSymbolSourceExternalDeadlineNotBlockedByGoRootDiscovery covers
// the P2 finding: with GOROOT unset, the first external read discovers
// the Go root through a go env subprocess, and a slow toolchain must not
// hold the request past its deadline. The canceled request must also
// leave the process-wide cache clean, so the discovery that keeps
// running on its own bound still lands for the next caller.
func TestGetSymbolSourceExternalDeadlineNotBlockedByGoRootDiscovery(t *testing.T) {
	srv, _, _ := newIsolationServer(t)

	binDir := t.TempDir()
	fakeRoot := filepath.Join(t.TempDir(), "goroot")
	fakeGo := filepath.Join(binDir, "go")
	script := "#!/bin/sh\nsleep 2\necho " + fakeRoot + "\n"
	require.NoError(t, os.WriteFile(fakeGo, []byte(script), 0o755))

	fakeSrc := filepath.Join(fakeRoot, "src", "sync", "map.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(fakeSrc), 0o755))
	require.NoError(t, os.WriteFile(fakeSrc,
		[]byte("package sync\n\nfunc (m *Map) Load(key any) (value any) { return nil }\n"), 0o644))

	t.Setenv("GOROOT", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The lookup cache is process-wide: start this test from the
	// discovery path and leave the same clean state behind for the tests
	// that run after it.
	resetGoRootCache := func() {
		goRootMu.Lock()
		goRootOK, goRootCached, goRootActive = false, "", nil
		goRootMu.Unlock()
	}
	resetGoRootCache()
	t.Cleanup(func() {
		// Wait out any flight this test started before clearing, so a
		// late caching goroutine cannot leak the fake root into the
		// next tests.
		for {
			goRootMu.Lock()
			flight := goRootActive
			goRootMu.Unlock()
			if flight == nil {
				break
			}
			<-flight.done
		}
		resetGoRootCache()
	})

	node := &graph.Node{
		ID: "ext::go:sync::Map.Load", Kind: graph.KindMethod, Name: "Load",
		FilePath: "external::go:sync", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"module_kind": "stdlib",
			"source_file": fakeSrc,
			"source_line": 3,
		},
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := srv.externalSymbolSourceResult(ctx,
		makeReq("get_symbol_source", map[string]any{"id": node.ID, "format": "json"}), node)
	elapsed := time.Since(started)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second,
		"a 2s toolchain subprocess must not block a 50ms request")

	// The canceled request must not have cached an unavailable root: the
	// discovery keeps running on its own bound and lands for the next
	// caller, which also proves the child was reaped before caching.
	require.Eventually(t, func() bool {
		return goRoot(context.Background()) == fakeRoot
	}, 5*time.Second, 50*time.Millisecond,
		"discovery must still land for a later caller after the canceled one")
}
