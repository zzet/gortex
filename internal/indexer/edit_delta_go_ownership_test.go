package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// A delta's Go package ownership comes from the stack's Go file inventory,
// read once per stack and kept across deltas, with the delta's own change set
// overlaid as its view holds it: a file the delta adds is owned, a file it
// deletes is not, and a later delta over the same stack is not served the
// earlier delta's files. Every pass of one delta shares one prepared lookup.
func TestEditDeltaGoOwnershipKeepsTheStackInventoryAndOverlaysTheDelta(t *testing.T) {
	resetEditDeltaGoInventories()
	t.Cleanup(resetEditDeltaGoInventories)
	resetEditDeltaGoOwnershipLookups()
	t.Cleanup(resetEditDeltaGoOwnershipLookups)
	root := builderTempDir(t, "go-ownership-delta")
	builderWriteTree(t, root, map[string]string{
		"go.mod":   "module example.test/m\n\ngo 1.23\n",
		"a/a.go":   "package a\n",
		"b/b.go":   "package b\n",
		"c/c.go":   "package c\n",
		"x/doc.md": "x\n",
	})
	fileNode := func(rel string) *graph.Node {
		p := builderRepoPrefix + "/" + rel
		return &graph.Node{ID: p, Kind: graph.KindFile, Language: "go", FilePath: p, RepoPrefix: builderRepoPrefix}
	}
	view := func(rels ...string) *Indexer {
		g := graph.New()
		nodes := make([]*graph.Node, 0, len(rels))
		for _, rel := range rels {
			nodes = append(nodes, fileNode(rel))
		}
		g.AddBatch(nodes, nil)
		return &Indexer{repoPrefix: builderRepoPrefix, rootPath: root, graph: g, resolver: resolver.New(g)}
	}
	owned := func(o *editDeltaGoOwnership, importer, candidate string) resolver.GoPackageOwnershipResult {
		t.Helper()
		lookups, err := o.factory(context.Background(), []string{builderRepoPrefix}, nil)
		if err != nil {
			t.Fatal(err)
		}
		lookup := lookups[builderRepoPrefix]
		if lookup == nil {
			return resolver.GoPackageOwnershipUnknown
		}
		c := fileNode(candidate)
		return lookup(resolver.GoImportCandidate{
			ImportPath: "example.test/m/" + candidate[:1], ImporterRepoPrefix: builderRepoPrefix,
			ImporterFilePath: builderRepoPrefix + "/" + importer, CandidateRepoPrefix: builderRepoPrefix,
			CandidateID: c.ID, CandidateFilePath: c.FilePath,
		})
	}
	const key = "stack"

	// The first delta over the stack {a, b} adds c: the stack is read from
	// the view before the delta writes, then the delta's file is overlaid.
	first := view("a/a.go", "b/b.go")
	o1 := primeEditDeltaGoOwnership(first, key, []string{"c/c.go"})
	if o1 == nil || o1.baseHit || len(o1.base) != 2 {
		t.Fatalf("first delta: installed=%t cached=%t stack=%v", o1 != nil, o1 != nil && o1.baseHit, o1.base)
	}
	first.graph.AddBatch([]*graph.Node{fileNode("c/c.go")}, nil)
	if got := owned(o1, "c/c.go", "a/a.go"); got != resolver.GoPackageOwnershipExact {
		t.Fatalf("the delta's added file is not an owned importer: %v", got)
	}
	if got := owned(o1, "a/a.go", "b/b.go"); got != resolver.GoPackageOwnershipExact {
		t.Fatalf("the stack's file is not owned: %v", got)
	}
	if o1.calls != 2 || o1.prepares != 1 {
		t.Fatalf("one delta prepared %d lookups over %d passes; want one shared", o1.prepares, o1.calls)
	}

	// The second delta over the same stack deletes b. The stack's inventory
	// is served from the cache (its view no longer holds b, the stack does),
	// the first delta's c is not in it, and b is gone from this delta's view.
	second := view("a/a.go")
	o2 := primeEditDeltaGoOwnership(second, key, []string{"b/b.go"})
	if o2 == nil || !o2.baseHit || len(o2.base) != 2 {
		t.Fatalf("second delta: installed=%t cached=%t stack=%v", o2 != nil, o2 != nil && o2.baseHit, o2.base)
	}
	if got := owned(o2, "b/b.go", "a/a.go"); got != resolver.GoPackageOwnershipUnknown {
		t.Fatalf("the deleted file is still an owned importer: %v", got)
	}
	if got := owned(o2, "c/c.go", "a/a.go"); got != resolver.GoPackageOwnershipUnknown {
		t.Fatalf("an earlier delta's file leaked into a later delta: %v", got)
	}
	if got := owned(o2, "a/a.go", "a/a.go"); got != resolver.GoPackageOwnershipExact {
		t.Fatalf("the surviving file is not owned: %v", got)
	}

	// A request that also names another repository (a pending source with
	// no repository) answers the delta's own from the kept inventory and no
	// lookup for the other, as the indexer's own factory does.
	o2.prepares, o2.ready = 0, false
	resetEditDeltaGoOwnershipLookups()
	lookups, err := o2.factory(context.Background(), []string{"", builderRepoPrefix}, nil)
	if err != nil || lookups[builderRepoPrefix] == nil || lookups[""] != nil || o2.prepares != 1 {
		t.Fatalf("mixed request: lookups=%v err=%v prepares=%d", lookups, err, o2.prepares)
	}

	// A change set with no Go path keeps the resolver's own factory.
	if o := primeEditDeltaGoOwnership(view("a/a.go"), key, []string{"x/doc.md"}); o != nil {
		t.Fatal("a change set without Go files installed the delta factory")
	}
}

// A prepared ownership lookup is kept across deltas over the same stack and
// the same Go file set: the next delta reuses it instead of reading every
// directory's manifest candidates again, and answers the same. A different
// file set prepares its own; a change set holding a module or workspace
// manifest neither reuses nor keeps one.
func TestEditDeltaGoOwnershipLookupIsReusedAcrossDeltas(t *testing.T) {
	resetEditDeltaGoInventories()
	t.Cleanup(resetEditDeltaGoInventories)
	resetEditDeltaGoOwnershipLookups()
	t.Cleanup(resetEditDeltaGoOwnershipLookups)
	root := builderTempDir(t, "go-ownership-reuse")
	builderWriteTree(t, root, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.23\n",
		"a/a.go": "package a\n",
		"b/b.go": "package b\n",
	})
	fileNode := func(rel string) *graph.Node {
		p := builderRepoPrefix + "/" + rel
		return &graph.Node{ID: p, Kind: graph.KindFile, Language: "go", FilePath: p, RepoPrefix: builderRepoPrefix}
	}
	view := func(rels ...string) *Indexer {
		g := graph.New()
		nodes := make([]*graph.Node, 0, len(rels))
		for _, rel := range rels {
			nodes = append(nodes, fileNode(rel))
		}
		g.AddBatch(nodes, nil)
		return &Indexer{repoPrefix: builderRepoPrefix, rootPath: root, graph: g, resolver: resolver.New(g)}
	}
	answer := func(o *editDeltaGoOwnership) resolver.GoPackageOwnershipResult {
		t.Helper()
		lookups, err := o.factory(context.Background(), []string{builderRepoPrefix}, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := fileNode("b/b.go")
		return lookups[builderRepoPrefix](resolver.GoImportCandidate{
			ImportPath: "example.test/m/b", ImporterRepoPrefix: builderRepoPrefix,
			ImporterFilePath: builderRepoPrefix + "/a/a.go", CandidateRepoPrefix: builderRepoPrefix,
			CandidateID: c.ID, CandidateFilePath: c.FilePath,
		})
	}
	const key = "stack"
	first := primeEditDeltaGoOwnership(view("a/a.go", "b/b.go"), key, []string{"a/a.go"})
	if got := answer(first); got != resolver.GoPackageOwnershipExact || first.prepares != 1 || first.reused {
		t.Fatalf("first delta: %v, prepares %d, reused %v", got, first.prepares, first.reused)
	}
	second := primeEditDeltaGoOwnership(view("a/a.go", "b/b.go"), key, []string{"b/b.go"})
	if got := answer(second); got != resolver.GoPackageOwnershipExact || second.prepares != 0 || !second.reused {
		t.Fatalf("second delta over the same stack and files: %v, prepares %d, reused %v", got, second.prepares, second.reused)
	}
	// A delta that adds a file has another file set: prepared afresh.
	third := primeEditDeltaGoOwnership(view("a/a.go", "b/b.go", "c/c.go"), key, []string{"c/c.go"})
	if answer(third); third.prepares != 1 || third.reused {
		t.Fatalf("a different file set reused a lookup: prepares %d, reused %v", third.prepares, third.reused)
	}
	// A change set with a manifest neither reuses nor keeps a lookup.
	manifest := primeEditDeltaGoOwnership(view("a/a.go", "b/b.go"), key, []string{"a/a.go", "go.mod"})
	if answer(manifest); manifest.prepares != 1 || manifest.reused {
		t.Fatalf("a manifest change reused a lookup: prepares %d, reused %v", manifest.prepares, manifest.reused)
	}
	after := primeEditDeltaGoOwnership(view("a/a.go", "b/b.go"), key, []string{"a/a.go"})
	if answer(after); after.prepares != 0 || !after.reused {
		t.Fatalf("the manifest delta disturbed the kept lookup: prepares %d, reused %v", after.prepares, after.reused)
	}
	// Consecutive edits stack generations (another stack key) but stand on
	// one commit: with the build's HEAD known the lookup is reused across
	// stacks; another commit prepares its own.
	onCommit := func(stack, commit string) *editDeltaGoOwnership {
		idx := view("a/a.go", "b/b.go")
		idx.headProvenance = &repoHeadProvenance{sha: commit}
		return primeEditDeltaGoOwnership(idx, stack, []string{"a/a.go"})
	}
	chainA := onCommit("stack-1", "c0ffee")
	if answer(chainA); chainA.prepares != 1 {
		t.Fatalf("first delta on the commit: prepares %d", chainA.prepares)
	}
	chainB := onCommit("stack-2", "c0ffee")
	if answer(chainB); chainB.prepares != 0 || !chainB.reused {
		t.Fatalf("a deeper stack on the same commit did not reuse: prepares %d, reused %v", chainB.prepares, chainB.reused)
	}
	moved := onCommit("stack-2", "beef00")
	if answer(moved); moved.prepares != 1 || moved.reused {
		t.Fatalf("another commit reused a lookup: prepares %d, reused %v", moved.prepares, moved.reused)
	}
}
