package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

// staleDerivationTreeA is the committed state the base corpus indexes: a
// receiver type whose methods mutate its fields, callers of them in another
// file, and an environment read.
func staleDerivationTreeA() map[string]string {
	tree := capabilityWriteSetTree()
	tree["pkg/env.go"] = `package pkg

import "os"

func Home() string { return os.Getenv("HOME") }
`
	return tree
}

// staleDerivationTreeB is the commit the layer describes: Touch stops calling
// Bump (so Outer and Far, its receiver callers in the same file, lose their
// indirect mutation), and a new file writes a field from the environment.
func staleDerivationTreeB() map[string]string {
	tree := staleDerivationTreeA()
	tree["pkg/use.go"] = strings.Replace(tree["pkg/use.go"], "func (s *Store) Touch() { s.Bump() }", "func (s *Store) Touch() { _ = s }", 1)
	tree["pkg/more.go"] = `package pkg

import "os"

func (s *Store) Configure() {
	s.n = len(os.Getenv("GORTEX_N"))
}
`
	return tree
}

type staleDerivationFixture struct {
	store      *store_sqlite.Store
	generation int64
	repoDir    string
	commit     string
}

// newStaleDerivationFixture builds a commit layer the way an older daemon
// did: no derivation stamps, no capability rows, and file rows without the
// derived fingerprints.
func newStaleDerivationFixture(t *testing.T) staleDerivationFixture {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "stale-repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, staleDerivationTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	baseDir := builderTempDir(t, "stale-base")
	builderWriteTree(t, baseDir, staleDerivationTreeA())
	builderWriteTree(t, repoDir, staleDerivationTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "stale-base")
	builderIndex(t, store, baseDir)
	ctx := context.Background()
	changes, err := diffTreeChanges(ctx, repoDir, treeA, treeB)
	if err != nil {
		t.Fatalf("diff trees: %v", err)
	}
	target, err := source.NewGitTreeSource(ctx, repoDir, treeB)
	if err != nil {
		t.Fatalf("open target tree: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	saved := derivationStampsForBuild
	derivationStampsForBuild = func() map[string]int { return nil }
	defer func() { derivationStampsForBuild = saved }()
	generationID, _, err := builderNewBuilder(store).Build(ctx, BuildRequest{
		Identity: GenerationIdentity{
			OwnerKind: "dedicated_graph", GraphID: "graph-stale", LayerID: "layer-" + treeB,
			CheckoutID: "checkout-stale", GenerationKind: CommitLayerGenerationKind,
			TreeOID: treeB, ProvenanceCommitOID: commitB, CreatedAt: time.Now().Unix(),
		},
		Base: store, Target: target, Changes: changes, RootPath: baseDir,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		PrePublish: func(ctx context.Context, generationID int64) error {
			return makeGenerationStale(store.AtGeneration(generationID))
		},
	})
	if err != nil {
		t.Fatalf("build the commit layer: %v", err)
	}
	return staleDerivationFixture{store: store, generation: generationID, repoDir: repoDir, commit: commitB}
}

// makeGenerationStale removes a building generation's capability rows and
// its file rows' derived fingerprints.
func makeGenerationStale(handle *store_sqlite.Store) error {
	for _, kind := range capabilityDerivedEdgeKinds {
		var rows []*graph.Edge
		for edge := range handle.EdgesByKind(kind) {
			rows = append(rows, edge)
		}
		for _, edge := range rows {
			handle.RemoveEdge(edge.From, edge.To, edge.Kind)
		}
	}
	var files []*graph.Node
	for node := range handle.NodesByKind(graph.KindFile) {
		c := *node
		c.Meta = make(map[string]any, len(node.Meta))
		for k, v := range node.Meta {
			if !strings.HasPrefix(k, "source_derived_") {
				c.Meta[k] = v
			}
		}
		files = append(files, &c)
	}
	if len(files) == 0 {
		return errors.New("fixture: the layer has no file rows")
	}
	handle.AddBatch(files, nil)
	return nil
}

func (f staleDerivationFixture) env(t *testing.T) derivationEnv {
	return derivationEnv{
		view: func(context.Context) (graph.Reader, func(), error) {
			return builderComposed(t, f.store, f.generation), func() {}, nil
		},
		parser: func() *Indexer {
			idx := New(graph.New(), builderRegistry(), config.Default().Index, zap.NewNop())
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.rootPath = f.repoDir
			return idx
		},
		content: gitBlobContent(f.repoDir, f.commit),
	}
}

func readerCapabilityRows(r graph.Reader) []string {
	var rows []string
	for _, kind := range capabilityDerivedEdgeKinds {
		for edge := range r.EdgesByKind(kind) {
			meta, _ := json.Marshal(edge.Meta)
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s", edge.Kind, edge.From, edge.To, edge.FilePath, edge.Line, edge.Origin, meta))
		}
	}
	sort.Strings(rows)
	return rows
}

func fileFingerprints(nodes []*graph.Node) derivedFingerprints {
	return storedDerivedFingerprints(nodes)
}

// A base generation an older derivation wrote — no stamps, no capability rows,
// file rows without fingerprints — is corrected once at startup: afterwards its
// composed capability rows and its file rows' fingerprints are a whole index's
// of the same tree, its stamps are the running versions, and a second run
// finds nothing stale. A delta over it then plans from the fingerprints
// instead of falling back to every derived family, and reuses capability rows.
func TestStartupCorrectionMakesAStaleBaseAWholeIndex(t *testing.T) {
	f := newStaleDerivationFixture(t)
	ctx := context.Background()
	for _, pass := range []string{derivationPassCapability, derivationPassFileFingerprints} {
		stale, err := f.store.GenerationsWithStaleDerivation(ctx, pass, currentDerivationStamps()[pass])
		if err != nil {
			t.Fatal(err)
		}
		if len(stale) != 1 || stale[0].GenerationID != f.generation {
			t.Fatalf("fixture: stale %s generations = %+v, want only %d", pass, stale, f.generation)
		}
	}
	cleanDir := builderTempDir(t, "stale-clean")
	builderWriteTree(t, cleanDir, staleDerivationTreeB())
	clean := builderOpenStore(t, "stale-clean")
	builderIndex(t, clean, cleanDir)
	whole := capabilityRows(t, clean)
	if len(whole) == 0 {
		t.Fatal("fixture: the whole index derived no capability rows")
	}
	if got := readerCapabilityRows(builderComposed(t, f.store, f.generation)); len(diffOnly(got, whole)) == 0 {
		t.Fatal("fixture: the stale layer already composes the whole index's capability rows")
	}

	stack := commitLayerBase{Reader: builderComposed(t, f.store, f.generation), stack: []int64{f.generation}}
	keyBefore, ok := editDeltaBaseCacheKey(stack, f.store)
	if !ok {
		t.Fatal("fixture: the layer's stack has no cache key")
	}
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	lifecycle.derivationEnvHook = func(store_sqlite.ViewGeneration) (derivationEnv, bool) { return f.env(t), true }
	report, err := lifecycle.CorrectStaleDerivations(ctx)
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if report.Corrected != 2 || report.Unmatched != 0 {
		t.Fatalf("correction report = %+v; want both passes of the one generation corrected, every file matched", report)
	}
	stamps, err := f.store.AtGeneration(f.generation).DerivationStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for pass, version := range currentDerivationStamps() {
		if stamps[pass] != version {
			t.Fatalf("stamp %s = %d, want %d", pass, stamps[pass], version)
		}
	}
	got := readerCapabilityRows(builderComposed(t, f.store, f.generation))
	if onlyGot, onlyWhole := diffRows(got, whole); len(onlyGot) > 0 || len(onlyWhole) > 0 {
		t.Fatalf("corrected capability rows differ from a whole index\ncorrected only:\n  %s\nwhole only:\n  %s",
			strings.Join(onlyGot, "\n  "), strings.Join(onlyWhole, "\n  "))
	}
	own := f.store.AtGeneration(f.generation)
	checked := 0
	for node := range own.NodesByKind(graph.KindFile) {
		want := fileFingerprints(clean.GetFileNodes(node.FilePath))
		if have := fileFingerprints([]*graph.Node{node}); !have.complete() || have != want {
			t.Fatalf("%s fingerprints = %+v, want the whole index's %+v", node.FilePath, have, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no file row checked")
	}
	// Every per-stack cache of the corrected layer's rows is a new entry.
	if keyAfter, _ := editDeltaBaseCacheKey(stack, f.store); keyAfter == keyBefore {
		t.Fatal("the stack's cache key did not move with the correction")
	}
	again, err := lifecycle.CorrectStaleDerivations(ctx)
	if err != nil || again.Stale != 0 {
		t.Fatalf("second run = %+v, %v; want nothing stale", again, err)
	}

	// A save over the corrected layer: a body line in use.go.
	core, logs := observer.New(zap.InfoLevel)
	// This is a real IndexFile mutation, not a read-only derived scratch.
	// Give it an isolated accepted file/constant sidecar like the builder does.
	dw := graph.NewDeltaWriter(builderComposed(t, f.store, f.generation), graph.New())
	idx := New(dw, builderRegistry(), config.Default().Index, zap.New(core))
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	idx.rootPath = f.repoDir
	path := filepath.Join(f.repoDir, "pkg", "use.go")
	edited := strings.Replace(staleDerivationTreeB()["pkg/use.go"], "func (s *Store) Touch()", "// Touch does nothing.\nfunc (s *Store) Touch()", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexFile(path); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	seen := false
	for _, entry := range logs.All() {
		if entry.Message != "incremental derived passes complete" {
			continue
		}
		seen = true
		fields := entry.ContextMap()
		if fallback, _ := fields["legacy_fallback"].(bool); fallback {
			t.Fatal("a delta over the corrected layer still fell back to every derived family")
		}
		if reused, _ := fields["capability_sources_reused"].(int64); reused == 0 {
			t.Fatalf("a delta over the corrected layer reused no capability source (%v)", fields)
		}
	}
	if !seen {
		t.Fatal("no derived pass ran for the save")
	}
}

func diffOnly(left, right []string) []string {
	onlyLeft, onlyRight := diffRows(left, right)
	return append(onlyLeft, onlyRight...)
}

// A build stamps the running versions on every generation it publishes, so a
// fresh generation is never found stale.
func TestBuiltGenerationsCarryTheRunningDerivationStamps(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	generationID, _, err := fixture.builder.Build(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	stamps, err := fixture.store.AtGeneration(generationID).DerivationStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for pass, version := range currentDerivationStamps() {
		if stamps[pass] != version {
			t.Fatalf("stamp %s = %d, want %d", pass, stamps[pass], version)
		}
		stale, err := fixture.store.GenerationsWithStaleDerivation(ctx, pass, version)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range stale {
			if g.GenerationID == generationID {
				t.Fatalf("a freshly built generation is stale for %s", pass)
			}
		}
	}
}

// An edit arriving while the correction writes gets the writer within 100 ms:
// the chunk in flight is cancelled, the correction waits for the edit, and
// then corrects the generation from the start.
func TestStartupCorrectionGivesAnEditTheWriterWithin100ms(t *testing.T) {
	f := newStaleDerivationFixture(t)
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	var editCycle atomic.Bool
	lifecycle.analysisEditCycle = editCycle.Load
	lifecycle.interactiveDemand = func() bool { return false }
	inView := make(chan struct{}, 1)
	release := make(chan struct{})
	var blocked atomic.Bool
	lifecycle.derivationEnvHook = func(store_sqlite.ViewGeneration) (derivationEnv, bool) {
		env := f.env(t)
		inner := env.view
		env.view = func(ctx context.Context) (graph.Reader, func(), error) {
			// The first derivation holds, as a long chunk would, until the
			// correction's context ends.
			if blocked.CompareAndSwap(false, true) {
				inView <- struct{}{}
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-release:
				}
			}
			return inner(ctx)
		}
		return env, true
	}
	defer close(release)
	done := make(chan DerivedCorrectionReport, 1)
	go func() {
		report, err := lifecycle.CorrectStaleDerivations(context.Background())
		if err != nil {
			t.Errorf("correct: %v", err)
		}
		done <- report
	}()
	select {
	case <-inView:
	case <-time.After(20 * time.Second):
		t.Fatal("the capability correction never started")
	}
	cycleFrom := time.Now()
	editCycle.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for lifecycle.correctionPreemptions() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the running correction kept going through an edit cycle")
		}
		time.Sleep(time.Millisecond)
	}
	if waited := time.Since(cycleFrom); waited > 100*time.Millisecond {
		t.Fatalf("the edit waited %s for the correction, want <= 100ms", waited)
	}
	editCycle.Store(false)
	var report DerivedCorrectionReport
	select {
	case report = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the correction did not finish after the edit")
	}
	if report.Corrected != 2 || report.Preempted == 0 {
		t.Fatalf("report = %+v; want both passes corrected after a preemption", report)
	}
}

// A folded generation carries, per pass, the oldest stamp of the chain it
// folds: a pass one member was not stamped for stays unstamped, and the
// correction re-derives it.
func TestFoldedGenerationTakesTheChainsOldestStamps(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	current, _, err := fixture.builder.Build(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	saved := derivationStampsForBuild
	derivationStampsForBuild = func() map[string]int { return map[string]int{derivationPassCapability: capabilityDerivationVersion} }
	older := fixture.request
	older.Identity.LayerID += "-older"
	olderID, _, err := fixture.builder.Build(ctx, older)
	derivationStampsForBuild = saved
	if err != nil {
		t.Fatal(err)
	}
	folded := fixture.request.Identity
	id, handle, _, err := fixture.store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: folded.OwnerKind, GraphID: folded.GraphID, LayerID: folded.LayerID + "-folded",
		CheckoutID: folded.CheckoutID, GenerationKind: DirtyLayerGenerationKind, BaseGenerationID: current,
		TreeOID: folded.TreeOID, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stampFoldedGeneration(ctx, fixture.store, []int64{current, olderID}, handle); err != nil {
		t.Fatal(err)
	}
	stamps, err := fixture.store.AtGeneration(id).DerivationStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stamps[derivationPassCapability] != capabilityDerivationVersion || stamps[derivationPassFileFingerprints] != 0 {
		t.Fatalf("folded stamps = %v; want capability only", stamps)
	}
}

// A commit layer of a checkout whose coordinator has not started (lazy
// activation) is corrected through a read-only view of its own: the
// correction needs no coordinator and no test environment, stamps both
// passes, and its capability rows are a whole index's of the commit.
func TestStartupCorrectionReachesLazilyActivatedCheckouts(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, staleDerivationTreeA())
	builderWriteTree(t, f.worktree, staleDerivationTreeB())
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "B")
	saved := derivationStampsForBuild
	derivationStampsForBuild = func() map[string]int { return nil }
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	cycle := coordinatorReconcile(t, c)
	derivationStampsForBuild = saved
	if cycle.CommitGenerationID <= 0 {
		t.Fatalf("fixture: no commit layer (%+v)", cycle)
	}
	ctx := context.Background()
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	lifecycle.leases = f.leases
	lifecycle.mi = &MultiIndexer{registry: builderRegistry(), indexers: map[string]*Indexer{}}
	// No coordinator is registered: the checkout is not activated.
	core, logs := observer.New(zap.InfoLevel)
	lifecycle.logger = zap.New(core)
	report, err := lifecycle.CorrectStaleDerivations(ctx)
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	stamps, err := f.store.AtGeneration(cycle.CommitGenerationID).DerivationStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for pass, version := range currentDerivationStamps() {
		if stamps[pass] != version {
			var skips []string
			for _, entry := range logs.FilterMessage("indexer: stale derivation not corrected").All() {
				skips = append(skips, fmt.Sprint(entry.ContextMap()))
			}
			t.Fatalf("commit layer %d stamp %s = %d, want %d (report %+v, skips %v)",
				cycle.CommitGenerationID, pass, stamps[pass], version, report, skips)
		}
	}
	cleanDir := builderTempDir(t, "lazy-clean")
	builderWriteTree(t, cleanDir, staleDerivationTreeB())
	clean := builderOpenStore(t, "lazy-clean")
	builderIndex(t, clean, cleanDir)
	materializer := graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases, Logger: zap.NewNop()}
	row, _, err := f.catalog.GetViewGeneration(ctx, cycle.CommitGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := materializer.MaterializeRefView(ctx, row.GraphID, cycle.CommitGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if onlyGot, onlyWhole := diffRows(readerCapabilityRows(view.Reader), capabilityRows(t, clean)); len(onlyGot) > 0 || len(onlyWhole) > 0 {
		t.Fatalf("corrected commit layer's capability rows differ from a whole index\ncorrected only:\n  %s\nwhole only:\n  %s",
			strings.Join(onlyGot, "\n  "), strings.Join(onlyWhole, "\n  "))
	}
}

// A generation whose rows a current build wrote but that carries no stamp is
// corrected without writing a row: the correction moves the stamp only, and
// the per-stack caches keyed on the correction epoch stay warm.
func TestCorrectingRightRowsWritesNothingAndKeepsTheEpoch(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, staleDerivationTreeA())
	builderWriteTree(t, f.worktree, staleDerivationTreeB())
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "B")
	saved := derivationStampsForBuild
	derivationStampsForBuild = func() map[string]int { return nil }
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	cycle := coordinatorReconcile(t, c)
	derivationStampsForBuild = saved
	lifecycle := newGenerationRetirementLifecycle(f.store, time.Now())
	lifecycle.leases = f.leases
	lifecycle.mi = &MultiIndexer{registry: builderRegistry(), indexers: map[string]*Indexer{}}
	epoch := f.store.GenerationCorrectionEpoch(cycle.CommitGenerationID)
	report, err := lifecycle.CorrectStaleDerivations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.RowsChanged != 0 || report.NoOps == 0 || report.EdgesMoved != 0 || report.Nodes != 0 {
		t.Fatalf("report = %+v; want every correction a no-op", report)
	}
	if got := f.store.GenerationCorrectionEpoch(cycle.CommitGenerationID); got != epoch {
		t.Fatalf("a correction that changed no row moved the epoch %d -> %d", epoch, got)
	}
	if report.Elapsed <= 0 {
		t.Fatal("the report's elapsed was not set")
	}
}

// The batch reader serves several files of one commit from one git process
// and reports a missing path as absent.
func TestGitBatchContentReadsFilesAtACommit(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "batch-repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, map[string]string{"a.go": "package a\n", "b/c.go": "package c\n\nfunc C() {}\n"})
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "one")
	commit := builderGit(t, dir, "rev-parse", "HEAD")
	ctx := context.Background()
	read, closeBatch, err := gitBatchContent(dir, commit)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBatch()
	for rel, want := range map[string]string{"a.go": "package a\n", "b/c.go": "package c\n\nfunc C() {}\n"} {
		got, ok := read(ctx, rel)
		if !ok || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", rel, got, ok, want)
		}
	}
	if _, ok := read(ctx, "missing.go"); ok {
		t.Fatal("a missing path was served")
	}
	if got, ok := read(ctx, "a.go"); !ok || string(got) != "package a\n" {
		t.Fatalf("a read after a missing path = %q, %v", got, ok)
	}
}
