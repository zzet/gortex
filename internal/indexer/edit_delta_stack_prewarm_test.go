package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// A pre-warmed stack serves the first delta over it from its caches: the Go
// file inventory, the provides rows, the stack's directory index, its import
// adjacency and its package type index are loaded before any edit, so the
// first delta reads none of them from the layers again.
func TestPrewarmedStackServesTheFirstDelta(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	if err := os.WriteFile(filepath.Join(f.worktree, "a", "types.go"), []byte("package a\n\ntype Box struct{ n int }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
		t.Fatal("the edit built nothing")
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	// A synthetic immutable stack over the chain: the key the delta and the
	// pre-warm share.
	base := commitLayerBase{Reader: view.Reader, stack: []int64{1000001, 1000002}}
	key, ok := editDeltaBaseCacheKey(base, c.builder.Store)
	if !ok {
		t.Fatal("no stack key")
	}
	invKey, _ := editDeltaContractCacheKey(base, c.builder.Store, c.repoPrefix, "", "")

	var warmed []string
	editDeltaPrewarmDone = func(k string) { warmed = append(warmed, k) }
	t.Cleanup(func() { editDeltaPrewarmDone = nil })
	c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, nil)
	if len(warmed) != 1 || warmed[0] != key {
		t.Fatalf("pre-warm finished for %v, want the stack's key", warmed)
	}
	if _, ok := cachedEditDeltaProvides(key); !ok {
		t.Error("the provides rows were not pre-warmed")
	}
	if _, ok := cachedEditDeltaDeps(key + "\x00" + c.repoPrefix); !ok {
		t.Error("the dependency contracts were not pre-warmed")
	}
	inventory, ok := cachedEditDeltaGoInventory(invKey)
	if !ok || len(inventory) == 0 {
		t.Error("the Go file inventory was not pre-warmed")
	}

	cache := editDeltaBaseCache(key)
	_, importMisses := cache.StackStats()
	_, fileMisses := cache.StackFileStats()
	_, nodeMisses := cache.StackNodeStats()
	dw := graph.NewDeltaWriter(base, nil)
	dw.SetBaseProjectionCache(cache)
	var files []string
	for row := range dw.FileNodeIdentitiesSeq([]string{c.repoPrefix}) {
		files = append(files, row.FilePath)
	}
	dw.ProjectImportAdjacency(files)
	var typesFile []string
	for _, p := range files {
		if strings.HasSuffix(p, "/a/types.go") || strings.HasSuffix(p, "/a/a.go") {
			typesFile = append(typesFile, p)
		}
	}
	if nodes := dw.NodesInFilesByKind(typesFile, []graph.NodeKind{graph.KindType, graph.KindInterface}); len(nodes) == 0 {
		t.Fatal("fixture precondition: no type in package a")
	}
	if _, m := cache.StackFileStats(); m != fileMisses {
		t.Errorf("the directory index was read from the layers again (%d misses after %d)", m, fileMisses)
	}
	if _, m := cache.StackStats(); m != importMisses {
		t.Errorf("the import adjacency was read from the layers again (%d misses after %d)", m, importMisses)
	}
	if _, m := cache.StackNodeStats(); m != nodeMisses {
		t.Errorf("the package type index was read from the layers again (%d misses after %d)", m, nodeMisses)
	}
}

// The pre-warm never competes with an edit: while a per-file delta runs it
// makes no step; it resumes after. And it warms the likely-edited files'
// packages before the whole-repository provides scan and the rest of the
// repository: by the time it reaches them, the likely file's import adjacency
// is kept.
func TestPrewarmYieldsToEditsAndWarmsLikelyFilesFirst(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{2000001, 2000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	likelyPath := c.repoPrefix + "/b/b.go"

	var mu sync.Mutex
	var steps []string
	likelyWarmBeforeProvides := false
	editDeltaPrewarmStep = func(step string) {
		mu.Lock()
		steps = append(steps, step)
		mu.Unlock()
		if step == "likely_callee_params" {
			_, before := cache.StackStats()
			probe := graph.NewDeltaWriter(base, nil)
			probe.SetBaseProjectionCache(cache)
			probe.ProjectImportAdjacency([]string{likelyPath})
			_, after := cache.StackStats()
			likelyWarmBeforeProvides = after == before
		}
	}
	done := make(chan struct{})
	editDeltaPrewarmDone = func(string) { close(done) }
	t.Cleanup(func() { editDeltaPrewarmStep, editDeltaPrewarmDone = nil, nil })

	endEdit := editDeltaBegin()
	go c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"b/b.go"})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	during := len(steps)
	mu.Unlock()
	endEdit()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the pre-warm never finished after the edit ended")
	}
	if during != 0 {
		t.Fatalf("the pre-warm made %d steps while an edit ran", during)
	}
	order := strings.Join(steps, ",")
	if !strings.Contains(order, "likely_packages,likely_callee_params,provides,likely_names,repository_packages") {
		t.Fatalf("pre-warm order %s", order)
	}
	if !likelyWarmBeforeProvides {
		t.Fatal("the likely file's import adjacency was not kept before the provides scan")
	}
}

// openFixed opens the same base for every unit of pre-warm work.
func openFixed(base LayerBase) func(context.Context) (LayerBase, func(), error) {
	return func(context.Context) (LayerBase, func(), error) { return base, func() {}, nil }
}

// The pre-warm holds at most one view of the stack at a time, and none
// between its units of work: every open is released before the next open.
func TestPrewarmHoldsAtMostOneViewAtATime(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{3000001, 3000002}}
	var mu sync.Mutex
	open, max, opens := 0, 0, 0
	held := func(context.Context) (LayerBase, func(), error) {
		mu.Lock()
		open++
		opens++
		if open > max {
			max = open
		}
		mu.Unlock()
		return base, func() {
			mu.Lock()
			open--
			mu.Unlock()
		}, nil
	}
	if !c.builder.PrewarmEditDeltaStack(context.Background(), held, c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	if max != 1 || open != 0 {
		t.Fatalf("the pre-warm held up to %d views at once, %d still open", max, open)
	}
	if opens < 5 {
		t.Fatalf("the pre-warm opened its view %d times: it holds it across its steps", opens)
	}
}

// The pre-warm makes the name reads an edit of a likely file makes: after it,
// the same reads (the file's references, re-resolved, and its referrers'
// pending ones) find every scope and name kept.
func TestPrewarmWarmsTheLikelyFilesNames(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{4000001, 4000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	editDeltaPrewarmLowPriorityPause = 0
	t.Cleanup(func() { editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond })
	if !c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	hits, misses := cache.StackNameStats()
	if misses == 0 {
		t.Fatal("the pre-warm read no name")
	}
	dw := graph.NewDeltaWriter(base, nil)
	dw.SetBaseProjectionCache(cache)
	warmLikelyNames(dw, c.repoPrefix, c.repoPrefix+"/a/a.go", time.Time{})
	hits2, misses2 := cache.StackNameStats()
	if misses2 != misses || hits2 == hits {
		t.Fatalf("an edit's name reads missed after the pre-warm: %d/%d hits/misses after %d/%d", hits2, misses2, hits, misses)
	}
}

// An edit of a likely file needs only what the warm loads before its
// low-priority tail: stopped right after the provides scan (the whole
// directory index and the other packages never warmed), the reads such an
// edit makes over the likely file's package all hit.
func TestAnEditOfALikelyFileNeedsOnlyTheLikelySet(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	resetEditDeltaParamIndexes()
	resetEditDeltaPriorViews()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	t.Cleanup(resetEditDeltaParamIndexes)
	t.Cleanup(resetEditDeltaPriorViews)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{5000001, 5000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var steps []string
	editDeltaPrewarmStep = func(step string) {
		steps = append(steps, step)
		if step == "likely_names" {
			cancel()
		}
	}
	t.Cleanup(func() { editDeltaPrewarmStep = nil })
	c.builder.PrewarmEditDeltaStack(ctx, openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"})
	for _, step := range steps {
		if step == "repository_packages" || step == "directory_index_all" {
			t.Fatalf("the tail ran after the stop: %v", steps)
		}
	}
	if _, ok := cachedEditDeltaProvides(key); !ok {
		t.Fatal("the provides rows are not kept before the tail")
	}
	_, imports := cache.StackStats()
	_, nodes := cache.StackNodeStats()
	_, names := cache.StackNameStats()
	loads := priorViewLoads.Load()
	file := c.repoPrefix + "/a/a.go"
	dw := graph.NewDeltaWriter(base, nil)
	dw.SetBaseProjectionCache(cache)
	var pkg []string
	for row := range dw.FileNodeIdentitiesSeq([]string{c.repoPrefix}) {
		if strings.HasPrefix(row.FilePath, c.repoPrefix+"/a/") {
			pkg = append(pkg, row.FilePath)
		}
	}
	dw.ProjectImportAdjacency(pkg)
	dw.NodesInFilesByKind(pkg, []graph.NodeKind{graph.KindType, graph.KindInterface})
	var ids []string
	for _, n := range dw.GetFileNodes(file) {
		ids = append(ids, n.ID)
	}
	editDeltaPriorEdges(dw, key)(ids)
	warmLikelyNames(dw, c.repoPrefix, file, time.Time{})
	if _, m := cache.StackStats(); m != imports {
		t.Errorf("the likely package's imports missed (%d after %d)", m, imports)
	}
	if _, m := cache.StackNodeStats(); m != nodes {
		t.Errorf("the likely package's type index missed (%d after %d)", m, nodes)
	}
	if _, m := cache.StackNameStats(); m != names {
		t.Errorf("the likely file's names missed (%d after %d)", m, names)
	}
	if priorViewLoads.Load() != loads {
		t.Error("the likely file's prior view was read again")
	}
}

// The likely-names step stops at its budget: a spent budget warms no name,
// so a long likely set cannot hold the warm for minutes.
func TestPrewarmLikelyNamesStopsAtItsBudget(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{6000001, 6000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	editDeltaPrewarmLowPriorityPause = 0
	editDeltaPrewarmNamesBudget = -time.Second
	t.Cleanup(func() {
		editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond
		editDeltaPrewarmNamesBudget = 60 * time.Second
	})
	if !c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	if _, misses := cache.StackNameStats(); misses != 0 {
		t.Fatalf("a spent budget still read %d names", misses)
	}
}

// Each likely file's name reads stop at the file's own deadline, inside the
// step's budget: a spent per-file allowance reads no name even with the step
// budget left.
func TestPrewarmLikelyNamesStopsAtEachFilesDeadline(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{7000001, 7000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	editDeltaPrewarmLowPriorityPause = 0
	editDeltaPrewarmNamesPerFile = -time.Second
	t.Cleanup(func() {
		editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond
		editDeltaPrewarmNamesPerFile = 10 * time.Second
	})
	if !c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	if _, misses := cache.StackNameStats(); misses != 0 {
		t.Fatalf("a spent per-file deadline still read %d names", misses)
	}
}

// referrerFactsStore serves fixed reference facts and records the names read.
type referrerFactsStore struct {
	*graph.Graph
	facts map[string][]graph.RefFact
	names map[string]int
	stubs map[string]int
}

func (s *referrerFactsStore) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	for _, id := range ids {
		s.stubs[id]++
	}
	return s.Graph.GetInEdgeIdentitiesByNodeIDs(ids)
}

func (s *referrerFactsStore) LoadRefFactsByTargets(_ string, _ []string) (map[string][]graph.RefFact, error) {
	return s.facts, nil
}

func (s *referrerFactsStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	for _, name := range names {
		s.names[name]++
	}
	return s.Graph.FindNodesByNames(names)
}

func (s *referrerFactsStore) LoadRefFactsByFiles(string, []string) ([]graph.RefFact, error) {
	return nil, nil
}

func (s *referrerFactsStore) FindNodesByName(name string) []*graph.Node {
	s.names[name]++
	return s.Graph.FindNodesByName(name)
}

func (s *referrerFactsStore) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*graph.Node {
	for _, name := range names {
		s.names[name]++
	}
	return s.Graph.FindNodesByNamesInRepo(names, repoPrefix)
}

// A likely file's reads past its own names are warmed: its declarations' stub
// keys' incoming identities, and for a file referencing it (by reference
// fact, as the affected-by planner finds it) that file's pending names and
// its callees' parameter tables. Past the deadline nothing is read.
func TestPrewarmWarmsALikelyFilesStubKeysAndReferrers(t *testing.T) {
	resetEditDeltaParamIndexes()
	t.Cleanup(resetEditDeltaParamIndexes)
	build := func() *referrerFactsStore {
		g := graph.New()
		node := func(id, file, name string, kind graph.NodeKind, meta map[string]any) {
			g.AddNode(&graph.Node{ID: id, Name: name, Kind: kind, FilePath: file, RepoPrefix: "repo", Language: "go", Meta: meta})
		}
		node("repo/a/a.go", "repo/a/a.go", "a.go", graph.KindFile, nil)
		node("repo/a/a.go::A", "repo/a/a.go", "A", graph.KindFunction, nil)
		node("repo/b/b.go", "repo/b/b.go", "b.go", graph.KindFile, nil)
		node("repo/b/b.go::B", "repo/b/b.go", "B", graph.KindFunction, nil)
		node("repo/c/c.go::C", "repo/c/c.go", "C", graph.KindFunction, nil)
		node("repo/c/c.go::C#param:y", "repo/c/c.go", "y", graph.KindParam, map[string]any{"position": 0})
		g.AddBatch(nil, []*graph.Edge{
			{From: "repo/b/b.go::B", To: "repo/a/a.go::A", Kind: graph.EdgeCalls, FilePath: "repo/b/b.go", Line: 3},
			{From: "repo/b/b.go::B", To: graph.UnresolvedMarker + "Foo", Kind: graph.EdgeCalls, FilePath: "repo/b/b.go", Line: 4},
			{From: "repo/b/b.go::B", To: "repo/c/c.go::C", Kind: graph.EdgeArgOf, FilePath: "repo/b/b.go", Line: 3,
				Meta: map[string]any{"arg_position": 0}},
			{From: "repo/c/c.go::C#param:y", To: "repo/c/c.go::C", Kind: graph.EdgeParamOf, FilePath: "repo/c/c.go"},
		})
		return &referrerFactsStore{Graph: g, names: map[string]int{}, stubs: map[string]int{}, facts: map[string][]graph.RefFact{
			"repo/b/b.go": {{RepoPrefix: "repo", FromID: "repo/b/b.go::B", ToID: "repo/a/a.go::A", Kind: "calls", RefName: "A", FilePath: "repo/b/b.go"}},
		}}
	}
	store := build()
	dw := graph.NewDeltaWriter(store, nil)
	params := newEditDeltaParamIndex("referrers", nil)
	warmLikelyReferrers(dw, "repo", "repo/a/a.go", time.Time{}, params)
	if store.stubs[graph.UnresolvedMarker+"A"] == 0 {
		t.Fatalf("the declaration's stub keys' identities were not read; read %v", store.stubs)
	}
	if store.names["Foo"] == 0 {
		t.Fatalf("the referrer's pending name was not read; names read %v", store.names)
	}
	if _, ok := stackParamIndex("referrers")["repo/c/c.go::C"]; !ok {
		t.Fatalf("the referrer's callee was not indexed: %v", stackParamIndex("referrers"))
	}

	late := build()
	warmLikelyReferrers(graph.NewDeltaWriter(late, nil), "repo", "repo/a/a.go", time.Now().Add(-time.Second), newEditDeltaParamIndex("late", nil))
	if len(late.names) != 0 || len(late.stubs) != 0 || len(stackParamIndex("late")) != 0 {
		t.Fatalf("a spent deadline still read names %v and params %v", late.names, stackParamIndex("late"))
	}
}

// The stack pre-warm makes, for a likely file, the catch-up's incoming
// admission read of its declarations' stub keys: after it, an edit's read of
// them finds every identity kept per stack.
func TestPrewarmReadsALikelyFilesStubKeyIdentities(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{9100001, 9100002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	editDeltaPrewarmLowPriorityPause = 0
	t.Cleanup(func() { editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond })
	if !c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	dw := graph.NewDeltaWriter(base, nil)
	dw.SetBaseProjectionCache(cache)
	var stubKeys []string
	for _, n := range dw.GetFileNodes(c.repoPrefix + "/a/a.go") {
		if n != nil && n.Name != "" && graph.IsReferenceableSymbol(n.Kind) {
			stubKeys = append(stubKeys, graph.UnresolvedNameCandidateIDs(n)...)
		}
	}
	if len(stubKeys) == 0 {
		t.Fatal("fixture precondition: a.go declares nothing referenceable")
	}
	_, before := cache.StackInIdentityStats()
	if before == 0 {
		t.Fatal("the pre-warm read no stub key's incoming identities")
	}
	graph.InEdgeIdentitiesByNodeIDs(dw, stubKeys)
	if _, after := cache.StackInIdentityStats(); after != before {
		t.Fatalf("an edit's stub-key read missed %d identities after the pre-warm", after-before)
	}
}

// A likely file whose prior rows carry no derived fingerprints gets its HEAD
// content's kept per stack by the pre-warm, under the key a delta over the
// stack reads them by; rows that carry them read nothing, nor does an unknown
// HEAD.
func TestPrewarmKeepsALikelyFilesPriorFingerprints(t *testing.T) {
	resetEditDeltaPriorFingerprints()
	t.Cleanup(resetEditDeltaPriorFingerprints)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{9200001, 9200002}}
	dw := graph.NewDeltaWriter(base, nil)
	idx := New(dw, c.builder.Registry, c.builder.Config, c.builder.Logger)
	defer idx.Close()
	idx.SetRepoPrefix(c.repoPrefix)
	idx.SetWorkspaceID(c.workspaceID)
	idx.SetProjectID(c.projectID)
	file := c.repoPrefix + "/a/a.go"
	rows := dw.GetFileNodes(file)
	if !storedDerivedFingerprints(rows).complete() {
		t.Fatal("fixture precondition: the rows carry derived fingerprints")
	}
	var unstamped []*graph.Node
	for _, n := range rows {
		c := *n
		if n.Kind == graph.KindFile {
			c.Meta = map[string]any{}
		}
		unstamped = append(unstamped, &c)
	}
	key, ok := editDeltaContractCacheKey(base, c.builder.Store, c.repoPrefix, "", "")
	if !ok {
		t.Fatal("no stack key")
	}
	cacheKey := key + "\x00a/a.go"
	head := prewarmHead{root: c.root, sha: gitHeadCommit(context.Background(), c.root)}
	if head.sha == "" {
		t.Fatal("fixture precondition: the checkout has a HEAD")
	}

	warmLikelyPriorFingerprints(idx, base, c.builder.Store, c.repoPrefix, file, rows, head)
	warmLikelyPriorFingerprints(idx, base, c.builder.Store, c.repoPrefix, file, unstamped, prewarmHead{root: c.root})
	if _, kept := cachedPriorFingerprints(cacheKey); kept {
		t.Fatal("fingerprints were read for stamped rows or an unknown HEAD")
	}
	elsewhere := New(dw, c.builder.Registry, c.builder.Config, c.builder.Logger)
	defer elsewhere.Close()
	elsewhere.SetRepoPrefix(c.repoPrefix)
	elsewhere.SetRootPath(t.TempDir())
	warmLikelyPriorFingerprints(elsewhere, base, c.builder.Store, c.repoPrefix, file, unstamped, head)
	if fp, kept := cachedPriorFingerprints(cacheKey); kept {
		t.Fatalf("an indexer rooted elsewhere kept %+v for the stack", fp)
	}
	warmLikelyPriorFingerprints(idx, base, c.builder.Store, c.repoPrefix, file, unstamped, head)
	fp, kept := cachedPriorFingerprints(cacheKey)
	if !kept || !fp.derived.complete() {
		t.Fatalf("the unstamped rows' HEAD fingerprints were not kept: %+v %t", fp, kept)
	}
	if fp.derived.declarations != storedDerivedFingerprints(rows).declarations {
		t.Fatalf("kept fingerprints %+v differ from the rows' own %+v", fp.derived, storedDerivedFingerprints(rows))
	}
}
