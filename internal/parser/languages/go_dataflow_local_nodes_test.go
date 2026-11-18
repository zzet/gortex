package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestGoDataflow_LocalsMaterialiseAsKindLocal is the regression for
// the design change that lifted intra-function bindings from
// edge-endpoint-only IDs to first-class KindLocal nodes. Storage
// backends that enforce edge-endpoint foreign keys (the disk backend)
// had to auto-stub empty Node rows for every local-binding edge endpoint —
// 51k+ stubs on the gortex codebase. Materialising as KindLocal
// converges every backend's node count and gives locals a proper
// home in the graph via EdgeMemberOf to the enclosing function.
func TestGoDataflow_LocalsMaterialiseAsKindLocal(t *testing.T) {
	src := `package foo

func Handler(x int) int {
	y := x
	z := y
	return z
}
`
	fix := runGoExtract(t, src)
	owner := "pkg/foo.go::Handler"

	locals := fix.nodesByKind[graph.KindLocal]
	require.NotEmpty(t, locals, "extractor should emit KindLocal nodes for short_var_decl bindings")

	names := map[string]*graph.Node{}
	for _, n := range locals {
		names[n.Name] = n
	}
	for _, want := range []string{"y", "z"} {
		n, ok := names[want]
		require.Truef(t, ok, "missing KindLocal for %q; got: %v", want, names)
		assert.Equal(t, graph.KindLocal, n.Kind)
		assert.Equal(t, "pkg/foo.go", n.FilePath, "local %q should carry the file it lives in", want)
		assert.Equal(t, "go", n.Language, "local %q should carry language", want)
		assert.Greater(t, n.StartLine, 0, "local %q should carry a source line", want)
		// The node ID must be exactly the same string the dataflow
		// edges target — they're keyed by edge endpoint, so a
		// mismatch silently breaks flow_between BFS.
		assert.True(t, strings.HasPrefix(n.ID, owner+"#local:"+want+"@+"),
			"local node ID must follow the function-relative offset convention, got %q", n.ID)
	}

	// Every materialised local must have an EdgeMemberOf edge to the
	// enclosing function — that's what makes the local discoverable
	// as a member of its owner via get_callers / class_hierarchy.
	memberEdges := fix.edgesByKind[graph.EdgeMemberOf]
	memberOwners := map[string]string{}
	for _, e := range memberEdges {
		memberOwners[e.From] = e.To
	}
	for _, n := range locals {
		owner, ok := memberOwners[n.ID]
		assert.Truef(t, ok, "local %q must have an EdgeMemberOf edge", n.Name)
		assert.Equalf(t, "pkg/foo.go::Handler", owner,
			"local %q's EdgeMemberOf target must be the enclosing function", n.Name)
	}
}

// TestGoDataflow_LocalsDedupedAcrossWalks guards against duplicate
// KindLocal node emissions if the same binding is visited through
// more than one walk path (e.g., short_var + a subsequent reference
// in the same scope). The walker's emittedLocals set must collapse
// repeat visits to one node row.
func TestGoDataflow_LocalsDedupedAcrossWalks(t *testing.T) {
	src := `package foo

func Multi() {
	y := 1
	_ = y
	_ = y
	_ = y
}
`
	fix := runGoExtract(t, src)
	ys := []string{}
	for _, n := range fix.nodesByKind[graph.KindLocal] {
		if n.Name == "y" {
			ys = append(ys, n.ID)
		}
	}
	assert.Lenf(t, ys, 1, "exactly one KindLocal row per (function, binding) — got: %v", ys)
}

// TestGoDataflow_RangeClauseEmitsKindLocal covers the second binding
// site (the range-clause path) — confirms the materialisation isn't
// limited to short_var_decl / var_spec.
func TestGoDataflow_RangeClauseEmitsKindLocal(t *testing.T) {
	src := `package foo

func Iter(xs []int) int {
	total := 0
	for i, v := range xs {
		_ = i
		total += v
	}
	return total
}
`
	fix := runGoExtract(t, src)
	names := map[string]bool{}
	for _, n := range fix.nodesByKind[graph.KindLocal] {
		names[n.Name] = true
	}
	for _, want := range []string{"total", "i", "v"} {
		assert.Truef(t, names[want], "missing KindLocal for range binding %q; got %v", want, names)
	}
}

func TestGoDataflowZeroInitializedLocalsOwnTheirInitialReads(t *testing.T) {
	fix := runGoExtract(t, `package foo
func Consume(...int) {}
func Run() {
 var total int
 var left, right int
 Consume(total, left, right)
 total += 1
 Consume(total)
}
`)
	owner := "pkg/foo.go::Run"
	initial := map[string]string{}
	for _, n := range fix.nodesByKind[graph.KindLocal] {
		if n.StartLine == 4 || n.StartLine == 5 {
			initial[n.Name] = n.ID
		}
	}
	for _, name := range []string{"total", "left", "right"} {
		require.NotEmpty(t, initial[name], "zero-value declaration must own %s before an assignment", name)
		assert.True(t, strings.HasPrefix(initial[name], owner+"#local:"+name+"@+"))
		found := false
		for _, e := range fix.edgesByKind[graph.EdgeArgOf] {
			if e.Line == 6 && e.From == initial[name] {
				found = true
			}
		}
		assert.True(t, found, "initial argument must use %s declaration identity", name)
	}
	later := false
	for _, e := range fix.edgesByKind[graph.EdgeArgOf] {
		if e.Line == 8 {
			later = true
			assert.NotEqual(t, initial["total"], e.From, "later assignment retains its own existing flow identity")
			assert.True(t, strings.HasPrefix(e.From, owner+"#local:total@+"))
		}
	}
	require.True(t, later, "later argument flow must be present")
	blank := runGoExtract(t, `package foo
type Count int
func Named() {
 var number, _ Count
 _ = number
}
`)
	var named []*graph.Node
	for _, n := range blank.nodesByKind[graph.KindLocal] {
		require.NotEqual(t, "_", n.Name, "blank identifiers do not introduce bindings")
		require.NotEqual(t, "Count", n.Name, "the named type is not a declared identifier")
		if n.Name == "number" {
			named = append(named, n)
		}
	}
	require.Len(t, named, 1)
	require.Equal(t, 4, named[0].StartLine)

}
