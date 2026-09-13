package graph

import (
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMutationReceiptCapturesExactResolutionDelta(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	g.AddBatch([]*Node{
		{ID: "repo/src/a.go::Caller", Kind: KindFunction, Name: "Caller", QualName: "pkg.Caller", FilePath: "src/a.go", RepoPrefix: "repo"},
		{ID: "repo/src/b.go::Load", Kind: KindFunction, Name: "Load", QualName: "pkg.Load", FilePath: "src/b.go", RepoPrefix: "repo"},
	}, []*Edge{{
		From: "repo/src/a.go::Caller", To: "repo::" + UnresolvedMarker + "Load", Kind: EdgeImports,
		FilePath: "src/a.go", Alias: "loader",
	}})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want complete resolution delta", receipt)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "Caller", "Load", "pkg.Caller", "pkg.Load")
	assertReceiptContains(t, "target ids", receipt.TargetIDs,
		"repo/src/a.go::Caller", "repo/src/b.go::Load", "repo::"+UnresolvedMarker+"Load")
	assertReceiptContains(t, "import candidates", receipt.ImportCandidates, "Load", "loader")
}

func TestMutationReceiptIdempotentWritesProduceNoResolutionDelta(t *testing.T) {
	g := New()
	n := &Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	e := &Edge{From: n.ID, To: "repo::" + UnresolvedMarker + "B", Kind: EdgeCalls, FilePath: "a.go"}
	g.AddNode(n)
	g.AddEdge(e)

	token := g.BeginMutationReceipt()
	g.AddNode(n)
	g.AddEdge(e)
	receipt := g.EndMutationReceipt(token)
	if !receipt.Complete {
		t.Fatalf("idempotent receipt unexpectedly incomplete: %+v", receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("idempotent writes produced resolution delta: %+v", receipt)
	}
}

func TestMutationReceiptsOverlapWithoutStealingEvents(t *testing.T) {
	g := New()
	outer := g.BeginMutationReceipt()
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	inner := g.BeginMutationReceipt()
	g.AddNode(&Node{ID: "repo/b.go::B", Kind: KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo"})
	outerReceipt := g.EndMutationReceipt(outer)
	g.AddNode(&Node{ID: "repo/c.go::C", Kind: KindFunction, Name: "C", FilePath: "c.go", RepoPrefix: "repo"})
	innerReceipt := g.EndMutationReceipt(inner)

	assertReceiptContains(t, "outer files", outerReceipt.ResolutionFiles(), "a.go", "b.go")
	if slices.Contains(outerReceipt.ResolutionFiles(), "c.go") {
		t.Fatalf("outer receipt observed mutation after it ended: %+v", outerReceipt)
	}
	assertReceiptContains(t, "inner files", innerReceipt.ResolutionFiles(), "b.go", "c.go")
	if slices.Contains(innerReceipt.ResolutionFiles(), "a.go") {
		t.Fatalf("inner receipt observed mutation before it began: %+v", innerReceipt)
	}
}

func TestMutationReceiptFailsClosedForUnsupportedMutationAndUnknownToken(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	g.RemoveEdge("missing", "missing", EdgeCalls)
	if receipt := g.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("unsupported mutation returned complete receipt: %+v", receipt)
	}
	if receipt := g.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("already-ended token returned complete receipt: %+v", receipt)
	}
}

func TestMutationReceiptsConcurrentOverlap(t *testing.T) {
	g := New()
	const workers = 12
	ready := sync.WaitGroup{}
	ready.Add(workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			token := g.BeginMutationReceipt()
			ready.Done()
			<-start
			id := string(rune('a' + i))
			g.AddNode(&Node{ID: "repo/" + id + ".go::" + id, Kind: KindFunction, Name: id, FilePath: id + ".go", RepoPrefix: "repo"})
			receipt := g.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant {
				t.Errorf("concurrent receipt %d = %+v", i, receipt)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

func TestMutationReceiptEndWaitsForInFlightMutationRecord(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	mutationStarted := make(chan struct{})
	releaseMutation := make(chan struct{})
	go func() {
		if !g.beginReceiptMutation() {
			t.Error("active receipt was not observed")
			return
		}
		defer g.endReceiptMutation()
		close(mutationStarted)
		<-releaseMutation
		g.recordAddedNodeForReceipts(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go"}, true, true)
	}()
	<-mutationStarted

	receiptCh := make(chan MutationReceipt, 1)
	go func() { receiptCh <- g.EndMutationReceipt(token) }()
	select {
	case receipt := <-receiptCh:
		t.Fatalf("EndMutationReceipt overtook in-flight mutation: %+v", receipt)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseMutation)
	select {
	case receipt := <-receiptCh:
		if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, "a.go") {
			t.Fatalf("receipt missed in-flight mutation: %+v", receipt)
		}
	case <-time.After(time.Second):
		t.Fatal("EndMutationReceipt did not complete after mutation drained")
	}
}

func assertReceiptContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("%s = %v, missing %q", label, got, value)
		}
	}
}

func TestMutationReceiptEvictFilesCapturesExactFrontier(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::B", Kind: KindType, Name: "B", FilePath: "b.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: EdgeCalls, FilePath: "keep.go", Line: 3}})

	// Mirror the reindex composition: the surviving caller's edge is parked
	// under an unresolved stub BEFORE the eviction, so no resolved incoming
	// edge from a surviving source is deleted and the receipt can stay exact.
	token := g.BeginMutationReceipt()
	in := g.GetInEdgesByNodeIDs([]string{"repo/a.go::A"})
	stub := in["repo/a.go::A"][0]
	oldTo := stub.To
	StashRestubProvenance(stub)
	stub.To = UnresolvedMarker + "A"
	g.ReindexEdges([]EdgeReindex{{Edge: stub, OldTo: oldTo}})
	nodes, edges := g.EvictFiles([]string{"a.go", "b.go"})
	receipt := g.EndMutationReceipt(token)

	if nodes != 2 || edges != 0 {
		t.Fatalf("EvictFiles removed nodes=%d edges=%d, want 2/0 (the restubbed edge no longer touches the doomed nodes)", nodes, edges)
	}
	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("EvictFiles receipt = %+v, want complete exact eviction frontier", receipt)
	}
	if want := []string{"a.go", "b.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
		t.Fatalf("definition files = %v, want %v", receipt.DefinitionFiles, want)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "A", "pkg.A", "B")
	assertReceiptContains(t, "target ids", receipt.TargetIDs, "repo/a.go::A", "repo/b.go::B")
	assertReceiptContains(t, "changed files", receipt.ChangedFiles, "a.go", "b.go")
	if slices.Contains(receipt.DefinitionFiles, "keep.go") {
		t.Fatalf("unrelated file leaked into definition frontier: %+v", receipt)
	}
}

func TestMutationReceiptEvictFileCapturesExactFrontier(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})

	token := g.BeginMutationReceipt()
	nodes, _ := g.EvictFile("a.go")
	receipt := g.EndMutationReceipt(token)

	if nodes != 1 {
		t.Fatalf("EvictFile nodes = %d, want 1", nodes)
	}
	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("EvictFile receipt = %+v, want complete exact eviction frontier", receipt)
	}
	if want := []string{"a.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
		t.Fatalf("definition files = %v, want %v", receipt.DefinitionFiles, want)
	}

	noop := g.BeginMutationReceipt()
	g.EvictFile("missing.go")
	if receipt := g.EndMutationReceipt(noop); !receipt.Complete || receipt.ResolutionRelevant {
		t.Fatalf("no-op EvictFile receipt = %+v, want complete and neutral", receipt)
	}
}

func TestMutationReceiptEvictFilesNonreferenceableOnlyStaysNeutral(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "repo/doc::note", Kind: KindImport, Name: "note", FilePath: "doc.md", RepoPrefix: "repo"})

	token := g.BeginMutationReceipt()
	g.EvictFiles([]string{"doc.md"})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("non-referenceable eviction receipt incomplete (%q): %+v", receipt.IncompleteReason, receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("non-referenceable eviction entered the resolution frontier: %+v", receipt)
	}
	assertReceiptContains(t, "changed files", receipt.ChangedFiles, "doc.md")
}

// A restub write — a surviving caller's edge parked under an unresolved stub
// by the re-parse flow, carrying the stashed provenance — must not enter
// UnresolvedFiles: the forward file pass re-resolves without restoring the
// stash (demoting the tier), while the name/incoming frontier restores it.
// The stub still marks the receipt resolution-relevant and contributes its
// name so that frontier runs.
func TestMutationReceiptRestubWriteStaysOutOfUnresolvedFiles(t *testing.T) {
	g := New()
	def := &Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"}
	caller := &Node{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"}
	e := &Edge{From: caller.ID, To: def.ID, Kind: EdgeCalls, FilePath: "keep.go", Line: 3, Origin: OriginLSPResolved, Confidence: 1}
	g.AddBatch([]*Node{def, caller}, []*Edge{e})

	token := g.BeginMutationReceipt()
	oldTo := e.To
	StashRestubProvenance(e)
	e.To = UnresolvedMarker + "A"
	g.ReindexEdges([]EdgeReindex{{Edge: e, OldTo: oldTo}})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("restub write must not void the receipt: %+v", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("a parked stub still needs the name frontier: %+v", receipt)
	}
	if slices.Contains(receipt.UnresolvedFiles, "keep.go") {
		t.Fatalf("restubbed caller file leaked into UnresolvedFiles (forward pass would demote its tier): %+v", receipt)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "A")
}

// An eviction that deletes a RESOLVED incoming edge from a surviving source
// still describes its RESOLUTION delta exactly, on every active window. The
// caller's edge is destroyed rather than parked, and no resolution pass
// reconstructs a deleted edge -- not the exact frontier and not the
// whole-graph fallback, which retargets only edges that still exist under a
// stub. Failing the receipt closed here would force the larger pass to reach
// an identical graph. The destruction is asserted, not glossed: it is a real
// pre-existing defect, tracked on its own rather than as receipt exactness.
func TestMutationReceiptEvictSurvivingIncomingEdgeStaysExact(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: EdgeCalls, FilePath: "keep.go", Line: 3}})

	outer := g.BeginMutationReceipt()
	inner := g.BeginMutationReceipt()
	g.EvictFile("a.go")
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})
	innerReceipt := g.EndMutationReceipt(inner)
	outerReceipt := g.EndMutationReceipt(outer)

	if !innerReceipt.Complete || !innerReceipt.ResolutionRelevant {
		t.Fatalf("inner receipt = %+v, want a complete resolution-relevant delta", innerReceipt)
	}
	if !outerReceipt.Complete || !outerReceipt.ResolutionRelevant {
		t.Fatalf("outer receipt = %+v, want a complete resolution-relevant delta", outerReceipt)
	}
	assertReceiptContains(t, "evicted names", innerReceipt.EvictedNames, "A", "pkg.A")
	if edges := g.GetOutEdges("repo/keep.go::Keep"); len(edges) != 0 {
		t.Fatalf("surviving caller edges = %v, want the eviction to have destroyed it", edges)
	}
}

// The same for an import edge from a surviving file into a non-referenceable
// doomed node. The evicted package's own import stubs are what the receipt
// must describe, and they are: the edge's destruction is orthogonal.
func TestMutationReceiptEvictImportToNonreferenceableStaysExact(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::pkg", Kind: KindPackage, Name: "pkg", QualName: "example/pkg", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::pkg", Kind: EdgeImports, FilePath: "keep.go"}})

	token := g.BeginMutationReceipt()
	g.EvictFiles([]string{"a.go"})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
	}
	assertReceiptContains(t, "evicted names", receipt.EvictedNames, "import::example/pkg", "import::pkg")
	if edges := g.GetOutEdges("repo/keep.go::Keep"); len(edges) != 0 {
		t.Fatalf("surviving importer edges = %v, want the eviction to have destroyed it", edges)
	}
}

func TestMutationReceiptEvictAmbiguousImportPackageCandidateRecordsImportStub(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{
		ID: "one::pkg", Kind: KindPackage, Name: "pkg", QualName: "example/pkg",
		FilePath: "a/one.go", RepoPrefix: "one",
	}}, nil)

	token := g.BeginMutationReceipt()
	g.EvictFile("a/one.go")
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "import::example/pkg", "import::pkg")
	assertReceiptContains(t, "evicted names", receipt.EvictedNames, "import::example/pkg", "import::pkg")
	if want := []string{"a/one.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
}

// Evicting a file whose nodes map to no stub names still changes resolution.
// A file node is an import candidate - relative imports, Lua requires, Godot
// res:// paths all bind to one - so its removal can collapse an ambiguity for
// a pending import elsewhere. ReceiptNamesForEvictedSymbol has no stub key to
// offer for it, but "no name to add" is not "no resolution work": the file
// still has to reach the definition frontier, and the receipt must not certify
// itself resolution-irrelevant, which is the one verdict that skips the
// catch-up pass entirely.
func TestMutationReceiptEvictNamelessNodeStaysResolutionRelevant(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/b.go", Kind: KindFile, Name: "b.go", FilePath: "b.go", RepoPrefix: "repo"},
	}, nil)

	token := g.BeginMutationReceipt()
	g.EvictFile("b.go")
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("receipt = %+v, want complete", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want resolution-relevant: an evicted file node can rebind a pending import", receipt)
	}
	if files := receipt.ResolutionFiles(); !slices.Contains(files, "b.go") {
		t.Fatalf("resolution files = %v, want the evicted file in the frontier", files)
	}
}

// EvictedNames is the name frontier's input, and it must carry only names that
// vanished. An added definition is reached through its own file: that file is
// in DefinitionFiles, and the file frontier enumerates the stub forms of every
// name the file declares. Handing the name pass the whole target set is what
// made it scale with batch size instead of with the evictions that motivated
// it, since TargetNames holds every added node's Name and QualName and each
// entry costs four stub forms per repository prefix in the pass's probe.
func TestMutationReceiptEvictedNamesCarryOnlyVanishedDefinitions(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{
		ID: "repo/gone.go::Gone", Kind: KindFunction, Name: "Gone", QualName: "pkg.Gone",
		FilePath: "gone.go", RepoPrefix: "repo",
	}}, nil)

	token := g.BeginMutationReceipt()
	g.AddNode(&Node{
		ID: "repo/new.go::Added", Kind: KindFunction, Name: "Added", QualName: "pkg.Added",
		FilePath: "new.go", RepoPrefix: "repo",
	})
	g.EvictFile("gone.go")
	receipt := g.EndMutationReceipt(token)

	assertReceiptContains(t, "target names", receipt.TargetNames, "Added", "pkg.Added", "Gone", "pkg.Gone")
	assertReceiptContains(t, "evicted names", receipt.EvictedNames, "Gone", "pkg.Gone")
	for _, added := range []string{"Added", "pkg.Added"} {
		if slices.Contains(receipt.EvictedNames, added) {
			t.Fatalf("evicted names %v include the added definition %q", receipt.EvictedNames, added)
		}
	}
}

func TestMutationReceiptEvictUnmappedImportCandidateKindFailsClosed(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{
		ID: "one::mod", Kind: KindModule, Name: "mod", QualName: "example/pkg",
		FilePath: "m/mod.go", RepoPrefix: "one",
	}}, nil)

	token := g.BeginMutationReceipt()
	g.EvictFile("m/mod.go")
	receipt := g.EndMutationReceipt(token)

	if receipt.Complete {
		t.Fatalf("receipt = %+v, want incomplete: the kind is a qualified-name import candidate without an exact stub mapping", receipt)
	}
}

// ---------------------------------------------------------------------------
// Bounded-derived-pass completeness (FanoutTruncations)
// ---------------------------------------------------------------------------
//
// A bounded derived pass (the incremental affected-by re-resolution) knowingly
// leaves dependants holding edges and persisted reference facts derived against
// the pre-mutation shape. Until this axis existed the fact stopped at a Warn
// line and a hook whose only setters are tests, so nothing a caller, an API or
// a receipt consumer reads could tell a truncated fan-out from a complete one.

// The receipt a truncating window returns must NAME the hole: which pass, how
// big the union was, what the cap was, and exactly which files were left stale.
// A window with no cut must return a receipt that says so by carrying nothing.
func TestMutationReceiptCarriesBoundedFanoutTruncation(t *testing.T) {
	g := New()

	token := g.BeginMutationReceipt()
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Cap: 4, Considered: 6,
		Dropped: 2, DroppedFiles: []string{"src/z.go", "src/y.go"},
	})
	receipt := g.EndMutationReceipt(token)

	if receipt.DerivedFanoutComplete() {
		t.Fatalf("truncated window reports a complete fan-out: %+v", receipt)
	}
	if got := receipt.DroppedFanoutFiles(); got != 2 {
		t.Fatalf("dropped files = %d, want 2 (%+v)", got, receipt.FanoutTruncations)
	}
	fact, ok := receipt.FanoutTruncationFor("affected_by")
	if !ok {
		t.Fatalf("receipt does not name the affected_by pass: %+v", receipt.FanoutTruncations)
	}
	if fact.Cap != 4 || fact.Considered != 6 || fact.Dropped != 2 {
		t.Fatalf("fact = %+v, want cap 4 / considered 6 / dropped 2", fact)
	}
	if want := []string{"src/y.go", "src/z.go"}; !slices.Equal(fact.DroppedFiles, want) {
		t.Fatalf("dropped files = %v, want %v (sorted)", fact.DroppedFiles, want)
	}

	// The fan-out axis is additive: it must not void the delta description,
	// because an incomplete receipt forces the conservative whole-frontier
	// fallback the bound exists to avoid.
	if !receipt.Complete || receipt.IncompleteReason != "" {
		t.Fatalf("a bounded fan-out voided the delta receipt: %+v", receipt)
	}

	clean := g.BeginMutationReceipt()
	cleanReceipt := g.EndMutationReceipt(clean)
	if !cleanReceipt.DerivedFanoutComplete() || len(cleanReceipt.FanoutTruncations) != 0 {
		t.Fatalf("untruncated window carries a fact: %+v", cleanReceipt.FanoutTruncations)
	}
}

// A deferred batch commits in chunks and re-applies the whole-batch bound on
// every merge, so the SAME file can be rejected by two chunks. Summing the
// per-cut counts would report it twice and overstate the hole; the receipt
// merges on the union of names instead.
func TestMutationReceiptFanoutMergesOnTheUnionNotTheSum(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Cap: 2, Considered: 4,
		Dropped: 2, DroppedFiles: []string{"src/c.go", "src/d.go"},
	})
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Cap: 2, Considered: 6,
		Dropped: 2, DroppedFiles: []string{"src/d.go", "src/e.go"},
	})
	receipt := g.EndMutationReceipt(token)

	if n := len(receipt.FanoutTruncations); n != 1 {
		t.Fatalf("two cuts by one pass produced %d facts, want 1: %+v", n, receipt.FanoutTruncations)
	}
	fact := receipt.FanoutTruncations[0]
	if want := []string{"src/c.go", "src/d.go", "src/e.go"}; !slices.Equal(fact.DroppedFiles, want) {
		t.Fatalf("dropped files = %v, want %v", fact.DroppedFiles, want)
	}
	if fact.Dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (the union, not the 2+2 sum)", fact.Dropped)
	}
	if fact.Considered != 6 {
		t.Fatalf("considered = %d, want the largest union the pass saw (6)", fact.Considered)
	}
}

// cloneReceiptFanoutTruncations is the detachment primitive, and it is pinned
// HERE rather than through a receipt pair.
//
// The earlier version of this pin mutated one receipt and asserted on another
// window's receipt — two accumulators that are never aliased to each other
// under any implementation, so replacing the whole clone with `return facts`
// left the package green. Calling the primitive directly is the only way to
// observe the aliasing it prevents: EndMutationReceipt retires an accumulator
// before receipt() runs, so the live-accumulator alias is unreachable through
// the public API and a receipt-level test cannot bind.
//
// Revert-red: replace the body of cloneReceiptFanoutTruncations with
// `return facts` and both the slice-identity and the element-slice assertions
// below fail.
func TestCloneReceiptFanoutTruncationsDetachesEveryDroppedFileSlice(t *testing.T) {
	if got := cloneReceiptFanoutTruncations(nil); got != nil {
		t.Fatalf("clone of nothing = %v, want nil", got)
	}

	source := []ReceiptFanoutTruncation{{
		Pass: "affected_by", Cap: 1, Considered: 3, Dropped: 2,
		DroppedFiles: []string{"src/a.go", "src/b.go"},
	}}
	clone := cloneReceiptFanoutTruncations(source)

	if &clone[0] == &source[0] {
		t.Fatalf("the clone shares the fact array with the accumulator")
	}
	if len(clone[0].DroppedFiles) > 0 && &clone[0].DroppedFiles[0] == &source[0].DroppedFiles[0] {
		t.Fatalf("the clone shares the DroppedFiles array with the accumulator")
	}

	clone[0].DroppedFiles[0] = "MUTATED"
	clone[0].Dropped = 99
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(source[0].DroppedFiles, want) {
		t.Fatalf("accumulator files = %v, want %v — a consumer reached the live accumulator",
			source[0].DroppedFiles, want)
	}
	if source[0].Dropped != 2 {
		t.Fatalf("accumulator dropped = %d, want 2", source[0].Dropped)
	}
}

// The facts a receipt hands out must be detached from the accumulator: a
// consumer that sorts or appends to DroppedFiles must not be able to reach
// back into a store that is still serving other windows.
func TestMutationReceiptFanoutFactsAreDetachedAndScopedToTheirWindow(t *testing.T) {
	g := New()
	outer := g.BeginMutationReceipt()
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Cap: 1, Dropped: 1, DroppedFiles: []string{"src/a.go"},
	})
	inner := g.BeginMutationReceipt()
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Cap: 1, Dropped: 1, DroppedFiles: []string{"src/b.go"},
	})
	outerReceipt := g.EndMutationReceipt(outer)

	// The outer window saw both cuts, the inner only the second.
	if got := outerReceipt.DroppedFanoutFiles(); got != 2 {
		t.Fatalf("outer window dropped = %d, want 2: %+v", got, outerReceipt.FanoutTruncations)
	}
	outerReceipt.FanoutTruncations[0].DroppedFiles[0] = "MUTATED"

	innerReceipt := g.EndMutationReceipt(inner)
	if got := innerReceipt.DroppedFanoutFiles(); got != 1 {
		t.Fatalf("inner window dropped = %d, want 1: %+v", got, innerReceipt.FanoutTruncations)
	}
	if want := []string{"src/b.go"}; !slices.Equal(innerReceipt.FanoutTruncations[0].DroppedFiles, want) {
		t.Fatalf("inner files = %v, want %v — a consumer mutated the accumulator",
			innerReceipt.FanoutTruncations[0].DroppedFiles, want)
	}

	// A fact with no window open is discarded, never buffered onto the next.
	g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
		Pass: "affected_by", Dropped: 1, DroppedFiles: []string{"src/stray.go"},
	})
	later := g.EndMutationReceipt(g.BeginMutationReceipt())
	if !later.DerivedFanoutComplete() {
		t.Fatalf("a fact recorded with no window open leaked onto a later receipt: %+v", later)
	}
}

// receiptOnlyStore bears receipts but cannot carry a fan-out fact — the shape
// every backend has before it implements the sink.
type receiptOnlyStore struct{ ended MutationReceipt }

func (s *receiptOnlyStore) BeginMutationReceipt() MutationReceiptToken { return 1 }
func (s *receiptOnlyStore) EndMutationReceipt(MutationReceiptToken) MutationReceipt {
	return s.ended
}

// A cut that reaches no carrier must be REPORTED as uncarried, never silently
// swallowed: the caller has to be able to say so through its own channel
// instead of letting a truncated pass read as a complete one.
func TestNoteMutationFanoutTruncationReportsAMissingCarrier(t *testing.T) {
	fact := ReceiptFanoutTruncation{Pass: "affected_by", Dropped: 1, DroppedFiles: []string{"src/a.go"}}

	var bare MutationReceiptStore = &receiptOnlyStore{}
	if ReceiptFanoutCarrier(bare) {
		t.Fatalf("a store without the sink advertises itself as a carrier")
	}
	if got := NoteMutationFanoutTruncation(bare, fact); got != MutationFanoutNoteNoCarrier {
		t.Fatalf("note on a sinkless store = %v, want MutationFanoutNoteNoCarrier", got)
	}

	g := New()
	if !ReceiptFanoutCarrier(g) {
		t.Fatalf("the in-memory backend does not advertise the sink it implements")
	}
	if got := NoteMutationFanoutTruncation(g, fact); got != MutationFanoutNoteRecorded {
		t.Fatalf("note on a carrier = %v, want MutationFanoutNoteRecorded", got)
	}
	// An empty fact is not a fact, and refusing it must NOT read as a missing
	// carrier: the caller announces "this store drops the fact on the floor"
	// only on MutationFanoutNoteNoCarrier, so a producer that reports a size
	// without names must never be able to provoke that message.
	if got := NoteMutationFanoutTruncation(g, ReceiptFanoutTruncation{Pass: "affected_by"}); got != MutationFanoutNoteEmpty {
		t.Fatalf("note on an empty cut = %v, want MutationFanoutNoteEmpty", got)
	}
	if got := NoteMutationFanoutTruncation(bare, ReceiptFanoutTruncation{Pass: "affected_by"}); got != MutationFanoutNoteEmpty {
		t.Fatalf("an empty cut on a sinkless store = %v, want MutationFanoutNoteEmpty "+
			"(nothing to carry is not a carrier gap)", got)
	}
	// A count without names is a fact: a producer that knows the size of the
	// hole but not its members must still reach the carrier.
	if got := NoteMutationFanoutTruncation(g, ReceiptFanoutTruncation{Pass: "affected_by", Dropped: 4}); got != MutationFanoutNoteRecorded {
		t.Fatalf("note on a count-only cut = %v, want MutationFanoutNoteRecorded", got)
	}
}

// DroppedFanoutFiles answers "how many DISTINCT files did this window leave
// stale". Two bounded passes over one mutation window routinely reject the
// same dependant; summing their counts reports it twice — the exact
// double-count MergeReceiptFanoutTruncation exists to prevent WITHIN a pass.
//
// Revert-red: sum fact.Dropped across passes and the shared file is counted
// twice (5 instead of 4).
func TestDroppedFanoutFilesUnionsAcrossPasses(t *testing.T) {
	receipt := MutationReceipt{
		Complete: true,
		FanoutTruncations: []ReceiptFanoutTruncation{
			{Pass: "affected_by", Dropped: 2, DroppedFiles: []string{"src/a.go", "src/shared.go"}},
			{Pass: "ref_facts", Dropped: 3, DroppedFiles: []string{"src/shared.go", "src/b.go", "src/c.go"}},
		},
	}
	if got := receipt.DroppedFanoutFiles(); got != 4 {
		t.Fatalf("dropped = %d, want 4 distinct files (a, b, c, shared counted once)", got)
	}
	if receipt.DerivedFanoutComplete() {
		t.Fatalf("a receipt naming two cuts reports a complete fan-out")
	}

	// A producer that reported a size without names cannot be unioned with
	// anything, so its count is added whole rather than dropped.
	countOnly := MutationReceipt{
		Complete: true,
		FanoutTruncations: []ReceiptFanoutTruncation{
			{Pass: "affected_by", Dropped: 2, DroppedFiles: []string{"src/a.go", "src/b.go"}},
			{Pass: "ref_facts", Dropped: 3},
		},
	}
	if got := countOnly.DroppedFanoutFiles(); got != 5 {
		t.Fatalf("dropped = %d, want 5 (2 named + 3 unnamed)", got)
	}
}

// Concurrent recorders and concurrent windows must not race or lose facts.
func TestMutationReceiptFanoutRecordingIsConcurrencySafe(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g.RecordMutationFanoutTruncation(ReceiptFanoutTruncation{
				Pass: "affected_by", Cap: 1, Considered: 2, Dropped: 1,
				DroppedFiles: []string{"src/f" + strconv.Itoa(i) + ".go"},
			})
		}(i)
	}
	wg.Wait()
	receipt := g.EndMutationReceipt(token)
	if got := receipt.DroppedFanoutFiles(); got != 8 {
		t.Fatalf("dropped = %d, want 8 concurrent cuts merged: %+v", got, receipt.FanoutTruncations)
	}
}
