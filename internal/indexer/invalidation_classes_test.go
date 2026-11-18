package indexer

import (
	"slices"
	"strings"
	"testing"
)

// The five invalidation classes of a latest edit over an accumulated dirty
// checkout: an exported signature change, an import change, the deletion of a
// dirty file, a rename that moves a package and updates its importer, and an
// undo of a dirty file to its committed bytes. See invalidation_fixture_test.go
// for what every case builds, asserts and records.
//
// The chain is a -> b -> c -> d: each package calls the next one down, and
// consumer imports a.

var invalidationChainFiles = []string{
	"chain/a/a.go", "chain/b/b.go", "chain/c/c.go", "chain/d/d.go", "consumer/consumer.go", "go.mod",
}

// invalidationWithout is the chain file list minus the given paths.
func invalidationWithout(extra []string, drop ...string) []string {
	var out []string
	for _, p := range append(slices.Clone(invalidationChainFiles), extra...) {
		if !slices.Contains(drop, p) {
			out = append(out, p)
		}
	}
	return out
}

// dirtyD is a body-only edit of d: one local binding, no signature change.
const invalidationDirtyD = `package d

// D is the bottom of the chain.
func D() int {
	v := 1
	return v
}
`

func TestInvalidationSignatureChangeKeepsDependentsBound(t *testing.T) {
	n := invalidationAccumulated(t)
	r := newInvalidationRun(t, "signature", n)

	// B gains a builtin-typed parameter: its identity is unchanged, its shape
	// (parameter node, typed_as edge onto the repository's builtin int) is not.
	// a — its one caller — keeps its call bound: the delta claims the edited
	// file and re-binds the caller's edge through the incoming leg, without
	// re-deriving a (the view must still equal a clean index).
	r.write("chain/b/b.go", `package b

import "`+accumulatedDirtyModule+`/chain/c"

// B calls down the chain.
func B(delta int) int {
	return c.C() + delta
}
`)
	id, _, rec := r.capture("signature", n)
	assertInvalidationFrontier(t, rec.Frontier,
		[]string{"chain/b/b.go"},
		invalidationWithout(nil))
	if mode := rec.Frontier.Masks["chain/b/b.go"]; mode == "" || mode == "delete" {
		t.Errorf("the edited file carries mask %q, want a replace claim", mode)
	}

	composed := r.composed(id)
	var param bool
	for _, node := range composed.GetFileNodes(builderRepoPrefix + "/chain/b/b.go") {
		if node != nil && node.Name == "delta" {
			param = true
		}
	}
	if !param {
		t.Error("the composed view does not carry B's new parameter")
	}
	var callsB bool
	for _, e := range composed.GetOutEdges(builderRepoPrefix + "/chain/a/a.go::A") {
		if e != nil && e.To == builderRepoPrefix+"/chain/b/b.go::B" {
			callsB = true
		}
	}
	if !callsB {
		t.Error("the dependent's call into the changed signature is gone from the composed view")
	}
}

func TestInvalidationImportChangeDropsOldImportFacts(t *testing.T) {
	n := invalidationAccumulated(t)
	r := newInvalidationRun(t, "import", n)

	// consumer switches its import from a to b and calls b.B instead of a.A.
	r.write("consumer/consumer.go", `package consumer

import "`+accumulatedDirtyModule+`/chain/b"

// Consume is the chain's one importing consumer.
func Consume() int {
	return b.B()
}
`)
	id, _, rec := r.capture("import", n)
	assertInvalidationFrontier(t, rec.Frontier,
		[]string{"consumer/consumer.go"},
		invalidationWithout(nil))

	composed := r.composed(id)
	consumer := builderRepoPrefix + "/consumer/consumer.go"
	var stale, callsB []string
	for _, e := range composed.AllEdges() {
		if e == nil || (e.From != consumer && !strings.HasPrefix(e.From, consumer+"::")) {
			continue
		}
		if strings.Contains(e.To, "chain/a") {
			stale = append(stale, e.From+" -"+string(e.Kind)+"-> "+e.To)
		}
		if e.To == builderRepoPrefix+"/chain/b/b.go::B" {
			callsB = append(callsB, string(e.Kind))
		}
	}
	if len(stale) > 0 {
		t.Errorf("the composed view keeps the removed import/call facts: %v", stale)
	}
	if len(callsB) == 0 {
		t.Error("the composed view has no edge from the consumer to its new callee b.B")
	}
	for _, e := range composed.GetInEdges(builderRepoPrefix + "/chain/a/a.go::A") {
		if e != nil && strings.HasPrefix(e.From, consumer) {
			t.Errorf("a.A still has an inbound edge from the consumer: %s -%s-> %s", e.From, e.Kind, e.To)
		}
	}
}

func TestInvalidationDeleteOfDirtyFileLeavesNothingBehind(t *testing.T) {
	n := invalidationAccumulated(t)
	r := newInvalidationRun(t, "delete", n)

	// d becomes dirty in its own generation first, so the deletion removes a
	// path the previous generation carried payload for.
	r.write("chain/d/d.go", invalidationDirtyD)
	r.build()
	r.remove("chain/d/d.go")

	id, _, rec := r.capture("delete", n)
	// c is the inbound dependent: its import and call now name a package the
	// module no longer holds, so they rebind onto the dependency stub the
	// resolver mints for it — a pathless claim the generation must make too.
	assertInvalidationFrontier(t, rec.Frontier,
		[]string{"chain/d/d.go", "chain/c/c.go"},
		invalidationWithout([]string{"external-call::dep:" + accumulatedDirtyModule + "/chain/d"}))
	if mode := rec.Frontier.Masks["chain/d/d.go"]; mode != "delete" {
		t.Errorf("the deleted file carries mask %q, want delete", mode)
	}

	composed := r.composed(id)
	if got := invalidationNodesAt(composed, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("nodes survive at the deleted path: %v", got)
	}
	if got := invalidationEdgesTouching(composed, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("edges survive at or into the deleted path: %v", got)
	}
	if got := r.invalidationComposedDocsAt(id, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("search documents survive at the deleted path: %v", got)
	}
	for table, count := range r.invalidationGenerationRowsAt(id, "chain/d/d.go") {
		if count != 0 {
			t.Errorf("the generation stores %d %s rows at the path it deletes", count, table)
		}
	}
	for _, node := range composed.FindNodesByName("D") {
		if node != nil && node.FilePath != "" {
			t.Errorf("FindNodesByName(D) still answers %s at %s", node.ID, node.FilePath)
		}
	}
}

func TestInvalidationRenameHidesTheOldPath(t *testing.T) {
	n := invalidationAccumulated(t)
	r := newInvalidationRun(t, "rename", n)

	r.write("chain/d/d.go", invalidationDirtyD)
	r.build()

	// d moves to a new package path e, and c (its importer and caller)
	// follows it.
	r.remove("chain/d/d.go")
	r.write("chain/e/e.go", strings.Replace(invalidationDirtyD, "package d", "package e", 1))
	r.write("chain/c/c.go", `package c

import "`+accumulatedDirtyModule+`/chain/e"

// C calls down the chain.
func C() int {
	return e.D() + 1
}
`)
	id, _, rec := r.capture("rename", n)
	assertInvalidationFrontier(t, rec.Frontier,
		[]string{"chain/d/d.go", "chain/e/e.go", "chain/c/c.go"},
		invalidationWithout(nil))
	if mode := rec.Frontier.Masks["chain/d/d.go"]; mode != "delete" {
		t.Errorf("the old path carries mask %q, want delete", mode)
	}
	if mode := rec.Frontier.Masks["chain/e/e.go"]; mode == "" || mode == "delete" {
		t.Errorf("the new path carries mask %q, want a replace claim", mode)
	}

	composed := r.composed(id)
	if got := invalidationNodesAt(composed, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("the old path is still readable through the composed view: %v", got)
	}
	if got := composed.GetFileNodes(builderRepoPrefix + "/chain/d/d.go"); len(got) > 0 {
		t.Errorf("GetFileNodes still answers %d nodes for the old path", len(got))
	}
	if got := invalidationEdgesTouching(composed, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("edges survive at or into the old path: %v", got)
	}
	if got := r.invalidationComposedDocsAt(id, "chain/d/d.go"); len(got) > 0 {
		t.Errorf("search documents survive at the old path: %v", got)
	}
	for table, count := range r.invalidationGenerationRowsAt(id, "chain/d/d.go") {
		if count != 0 {
			t.Errorf("the generation stores %d %s rows at the old path", count, table)
		}
	}
	if got := invalidationNodesAt(composed, "chain/e/e.go"); len(got) == 0 {
		t.Error("the new path carries no nodes in the composed view")
	}
	var callsE bool
	for _, e := range composed.GetOutEdges(builderRepoPrefix + "/chain/c/c.go::C") {
		if e != nil && e.To == builderRepoPrefix+"/chain/e/e.go::D" {
			callsE = true
		}
	}
	if !callsE {
		t.Error("the updated caller does not reach the renamed callee")
	}
}

func TestInvalidationUndoToBaseRevealsTheCommittedFile(t *testing.T) {
	n := invalidationAccumulated(t)
	r := newInvalidationRun(t, "undo", n)

	// b becomes dirty in its own generation, then goes back to its committed
	// bytes exactly.
	r.write("chain/b/b.go", `package b

import "`+accumulatedDirtyModule+`/chain/c"

// B calls down the chain.
func B() int {
	v := c.C() + 1
	return v
}
`)
	dirtyID, _ := r.build()
	dirtyComposed := r.composed(dirtyID)
	baseB := builderRenderNodes(r.store.AtGeneration(0).GetFileNodes(builderRepoPrefix + "/chain/b/b.go"))
	if slices.Equal(builderRenderNodes(dirtyComposed.GetFileNodes(builderRepoPrefix+"/chain/b/b.go")), baseB) {
		t.Fatal("the dirty edit of b is not visible in its own generation — the undo would prove nothing")
	}

	r.write("chain/b/b.go", invalidationCommitted(t, "chain/b/b.go"))
	id, _, rec := r.capture("undo", n)
	// Nothing outside the accumulated set is required: the file is no longer
	// dirty. A build that plans against the previous dirty generation must
	// re-claim b with its committed payload, so b is allowed.
	assertInvalidationFrontier(t, rec.Frontier, nil, invalidationWithout(nil))

	composed := r.composed(id)
	if got := builderRenderNodes(composed.GetFileNodes(builderRepoPrefix + "/chain/b/b.go")); !slices.Equal(got, baseB) {
		t.Errorf("after undo the composed view serves b as\n  %v\nthe committed base serves\n  %v", got, baseB)
	}
	for _, node := range composed.GetFileNodes(builderRepoPrefix + "/chain/b/b.go") {
		if node != nil && node.Name == "v" {
			t.Errorf("the undone edit's local binding %s is still served", node.ID)
		}
	}
	// Physical check: the generation stores nothing at b unless it claims b,
	// and when it claims it, it stores exactly the committed payload.
	rows := r.invalidationGenerationRowsAt(id, "chain/b/b.go")
	if _, claimed := rec.Frontier.Masks["chain/b/b.go"]; !claimed {
		for table, count := range rows {
			if count != 0 {
				t.Errorf("the generation stores %d %s rows at b without claiming it", count, table)
			}
		}
	} else if got := builderRenderNodes(r.store.AtGeneration(id).GetFileNodes(builderRepoPrefix + "/chain/b/b.go")); !slices.Equal(got, baseB) {
		t.Errorf("the generation re-claims b with\n  %v\nwant the committed payload\n  %v", got, baseB)
	}
	var baseDocs []string
	for _, doc := range parityFTSRows(t, parityOpenRaw(t, r.store), 0) {
		if nodeID, _, _ := strings.Cut(doc, "\t"); strings.HasPrefix(nodeID, builderRepoPrefix+"/chain/b/b.go") {
			baseDocs = append(baseDocs, doc)
		}
	}
	if got := r.invalidationComposedDocsAt(id, "chain/b/b.go"); !slices.Equal(got, baseDocs) {
		t.Errorf("after undo the composed search documents at b are\n  %v\nthe base holds\n  %v", got, baseDocs)
	}
}
