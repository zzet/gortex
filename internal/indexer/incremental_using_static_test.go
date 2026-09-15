package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

const csUsingStaticH1 = `namespace LibA {
    public static class H1 {
        public static int Clamp(int x) { return x; }
    }
}
`

const csUsingStaticH2 = `namespace LibB {
    public static class H2 {
        public static int Clamp(int x) { return x; }
    }
}
`

// clampCallEdge returns the EdgeCalls edge for the bare `Clamp(1)` leaving
// fromID, resolved or stubbed.
func clampCallEdge(t *testing.T, g graph.Store, fromID string) *graph.Edge {
	t.Helper()
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if n := graph.UnresolvedName(e.To); n == "Clamp" || n == "*.Clamp" {
				return e
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == "Clamp" {
			return e
		}
	}
	t.Fatalf("no Clamp call edge from %s", fromID)
	return nil
}

// TestIncrementalSave_GlobalUsingStaticEditReresolvesReceiverlessBinds: a
// receiverless call bound through `global using static LibA.H1;` is priced
// by unit-wide visibility exactly like an extension bind — swapping the
// directive's target must re-bind every dependent's bare calls, not leave
// the stale H1 bind in place.
func TestIncrementalSave_GlobalUsingStaticEditReresolvesReceiverlessBinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "H1.cs"), csUsingStaticH1)
	writeFile(t, filepath.Join(dir, "H2.cs"), csUsingStaticH2)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using static LibA.H1;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `namespace App {
    public class R {
        public int M() { return Clamp(1); }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, clampCallEdge(t, g, mID).To, "H1.Clamp",
		"baseline: the global using static binds the bare call to H1's member unit-wide")

	writeFile(t, usingsPath, "global using static LibB.H2;\n")
	require.NoError(t, idx.IndexFile(usingsPath))
	assert.Contains(t, clampCallEdge(t, g, mID).To, "H2.Clamp",
		"the swap changed every dependent's visibility — Caller.cs must re-bind to H2")
}

// TestReuseResolutionTag_CarriesOnlyTheVisibilityTags (PR #797 review
// P2): the reuse re-stamps exactly the two tags the visibility restub
// consumes. Every other resolver-authored tag (scope, import_closure,
// value_callee, useClass_binding, import_binding) is left to the fresh
// resolve, as before — this change is C#-visibility-only.
func TestReuseResolutionTag_CarriesOnlyTheVisibilityTags(t *testing.T) {
	for tag, want := range map[string]string{
		"extension_method": "extension_method",
		"using_static":     "using_static",
		"scope":            "",
		"import_closure":   "",
		"value_callee":     "",
		"useClass_binding": "",
		"import_binding":   "",
		"":                 "",
	} {
		e := &graph.Edge{Meta: map[string]any{"resolution": tag}}
		assert.Equal(t, want, reuseResolutionTag(e), "tag %q", tag)
	}
	assert.Equal(t, "", reuseResolutionTag(&graph.Edge{}), "no Meta, no tag")
	assert.Equal(t, "", reuseResolutionTag(nil), "nil edge, no tag")
}

// TestIncrementalSave_UnrelatedCallerEditKeepsUsingStaticTag: an edit to
// the caller that leaves the call untouched reuses the captured bind. The
// reuse must carry the resolution tag with it — a later directive swap
// finds the edges to re-price by that tag, so a bind that lost it would
// stay stale forever.
func TestIncrementalSave_UnrelatedCallerEditKeepsUsingStaticTag(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "H1.cs"), csUsingStaticH1)
	writeFile(t, filepath.Join(dir, "H2.cs"), csUsingStaticH2)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using static LibA.H1;\n")
	callerPath := filepath.Join(dir, "Caller.cs")
	caller := `namespace App {
    public class R {
        public int M() { return Clamp(1); }
    }
}
`
	writeFile(t, callerPath, caller)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	e := clampCallEdge(t, g, mID)
	require.Contains(t, e.To, "H1.Clamp")
	require.Equal(t, "using_static", e.Meta["resolution"], "baseline: the bind carries its tag")

	writeFile(t, callerPath, "// touched\n"+caller)
	require.NoError(t, idx.IndexFile(callerPath))
	e = clampCallEdge(t, g, mID)
	assert.Contains(t, e.To, "H1.Clamp", "the unrelated edit keeps the bind")
	assert.Equal(t, "using_static", e.Meta["resolution"], "and keeps the tag the restub path keys on")

	// The tag is what makes the later swap reach this edge.
	writeFile(t, usingsPath, "global using static LibB.H2;\n")
	require.NoError(t, idx.IndexFile(usingsPath))
	assert.Contains(t, clampCallEdge(t, g, mID).To, "H2.Clamp",
		"after the unrelated save the swap must still re-price the bind")
}
