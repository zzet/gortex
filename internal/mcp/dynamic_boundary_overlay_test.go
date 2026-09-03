package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// TestDynamicBoundariesScanTheOverlayBuffer pins which text the
// dispatch scan slices: an overlay-modified file's node carries
// BUFFER coordinates, so slicing the stale on-disk content by those
// lines reads a different function's body and fabricates boundaries.
// The scan must substitute the session's buffer, exactly as the
// source-reading handlers do.
func TestDynamicBoundariesScanTheOverlayBuffer(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "svc.py")
	// Disk: the pre-edit file — short, no dispatch anywhere.
	require.NoError(t, os.WriteFile(fp, []byte("def old(self):\n    return 1\n"), 0o644))
	// Buffer: an edit inserted a line above, shifting route() down —
	// its reflection dispatch now lives at buffer line 3.
	overlay := strings.Join([]string{
		"# inserted comment",
		"def route(self, name, payload):",
		"    handler = getattr(self, name)",
	}, "\n")

	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	node := &graph.Node{
		ID: fp + "::route", Kind: graph.KindFunction, Name: "route",
		FilePath: fp, StartLine: 2, EndLine: 3,
	}

	const sessionID = "overlay-dispatch-scan"
	ctx := WithSessionID(context.Background(), sessionID)
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
		sessionID: sessionID,
		canonical: true,
		files:     []daemon.OverlayFile{{Path: fp, Content: overlay}},
	})

	got := srv.dynamicBoundariesForSymbol(ctx, srv.readerFor(ctx), node)
	require.NotEmpty(t, got, "the buffer's getattr dispatch at the node's buffer lines must be detected")
	require.Equal(t, fp+":3", got[0].Site,
		"the site is a buffer coordinate — the disk file has no dispatch at all")

	// Without the overlay, the same node coordinates slice the disk
	// file: no dispatch there, no boundaries — never fabricated ones.
	base := context.Background()
	require.Empty(t, srv.dynamicBoundariesForSymbol(base, srv.readerFor(base), node))
}
