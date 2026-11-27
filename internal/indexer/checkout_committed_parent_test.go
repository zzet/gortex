package indexer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

func TestCommittedParentReuseBuildsOnlyTheNewBranchDelta(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	parent, parentRoot := f.pinDependent(t, "parent")
	for i := range 20 {
		body := fmt.Sprintf("package fixture\nfunc Branch%d() { Helper() }\n", i)
		if err := os.WriteFile(filepath.Join(parentRoot, fmt.Sprintf("branch%d.go", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	builderGit(t, parentRoot, "add", "-A")
	builderGit(t, parentRoot, "commit", "-m", "shared branch delta")
	if out := parent.reconcile(ctx); out.Err != nil || !out.CommitBuilt {
		t.Fatalf("build parent: %+v", out)
	}
	parentID := f.routeOf(t, parent.checkoutID).CommitGenerationID
	parentRow, found := f.generation(parentID)
	if !found {
		t.Fatal("parent generation missing")
	}
	anchor := parentRow.BaseGenerationID
	parentHead := builderGit(t, parentRoot, "rev-parse", "HEAD")

	newChild := func(name, symbol string) (*CheckoutCoordinator, string) {
		t.Helper()
		c, root := f.pinDependent(t, name)
		builderGit(t, root, "reset", "--hard", parentHead)
		if err := os.WriteFile(filepath.Join(root, name+"_only.go"), []byte("package fixture\nfunc "+symbol+"() { Branch0() }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		builderGit(t, root, "add", "-A")
		builderGit(t, root, "commit", "-m", "one change above shared branch")
		return c, root
	}
	child, childRoot := newChild("child", "ChildOnly")
	if out := child.reconcile(ctx); out.Err != nil || !out.CommitBuilt {
		t.Fatalf("build child: %+v", out)
	}
	childRoute := f.routeOf(t, child.checkoutID)
	row, found := f.generation(childRoute.CommitGenerationID)
	if !found || row.BaseGenerationID != parentID {
		t.Fatalf("target rebuilt over primary %d, want retained committed parent %d: %+v", anchor, parentID, row)
	}
	if child.PinnedBaseGeneration() != anchor {
		t.Fatalf("pin reports %d, want dedicated anchor %d", child.PinnedBaseGeneration(), anchor)
	}
	if got := f.store.AtGeneration(row.GenerationID).GetRepoNodes(builderRepoPrefix); len(got) >= len(f.store.AtGeneration(parentID).GetRepoNodes(builderRepoPrefix)) {
		t.Fatal("child repeated the shared branch payload")
	}
	assertViewIsItsOwnTree(t, f, child.checkoutID, childRoot)
	if out := child.reconcile(ctx); out.Err != nil || out.CommitBuilt || out.DirtyBuilt || out.Recomposed {
		t.Fatalf("unchanged child did not settle: %+v", out)
	}

	// Reuse never grows a chain of committed parents: another checkout uses
	// the same direct-over-dedicated parent, not the child just created.
	third, thirdRoot := newChild("third", "ThirdOnly")
	if out := third.reconcile(ctx); out.Err != nil {
		t.Fatal(out.Err)
	}
	thirdRow, _ := f.generation(f.routeOf(t, third.checkoutID).CommitGenerationID)
	if thirdRow.BaseGenerationID != parentID {
		t.Fatalf("committed parent depth grew: %+v", thirdRow)
	}
	assertViewIsItsOwnTree(t, f, third.checkoutID, thirdRoot)

	// The sweep still names the dedicated ancestor. Releasing it must
	// rebuild off-route over the family base and leave a coherent view.
	advanced, _ := f.advanceCommittedBase(t, "advanced.go", "Advanced")
	if out := third.reconcile(ctx); out.Err != nil || !out.BasePinned || out.CommitBuilt {
		t.Fatalf("immutable parent was lost on base advance: %+v", out)
	}
	if third.PinnedBaseGeneration() != anchor {
		t.Fatal("dedicated retirement anchor was lost")
	}
	if !third.RequestBaseRelease(anchor, "test releases committed parent anchor") {
		t.Fatal("release request refused")
	}
	if out := third.reconcile(ctx); out.Err != nil || !out.Recomposed || out.BasePinned {
		t.Fatalf("anchor release did not recompose: %+v", out)
	}
	row, _ = f.generation(f.routeOf(t, third.checkoutID).CommitGenerationID)
	if row.BaseGenerationID != advanced || third.PinnedBaseGeneration() != 0 {
		t.Fatalf("released parent remains pinned: %+v", row)
	}
	assertViewIsItsOwnTree(t, f, third.checkoutID, thirdRoot)
}

func committedParentReuseFixture(t *testing.T) (*committedBaseFixture, string, store_sqlite.ViewGeneration) {
	t.Helper()
	f := newCommittedBaseFixture(t)
	return committedParentReuseFixtureOver(t, f)
}

func committedParentReuseFixtureOver(t *testing.T, f *committedBaseFixture) (*committedBaseFixture, string, store_sqlite.ViewGeneration) {
	t.Helper()
	parent, root := f.pinDependent(t, "parent")
	for i := range 20 {
		builderWriteFile(t, root, fmt.Sprintf("branch%d.go", i), fmt.Sprintf("package fixture\nfunc Branch%d() { Helper() }\n", i))
	}
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "shared branch delta")
	if out := parent.reconcile(context.Background()); out.Err != nil || !out.CommitBuilt {
		t.Fatalf("build parent: %+v", out)
	}
	row, found := f.generation(f.routeOf(t, parent.checkoutID).CommitGenerationID)
	if !found {
		t.Fatal("parent generation missing")
	}
	return f, root, row
}

func committedParentChild(t *testing.T, f *committedBaseFixture, parentRoot, name string) (*CheckoutCoordinator, string) {
	t.Helper()
	c, root := f.pinDependent(t, name)
	builderGit(t, root, "reset", "--hard", builderGit(t, parentRoot, "rev-parse", "HEAD"))
	builderWriteFile(t, root, name+"_only.go", "package fixture\nfunc "+name+"Only() { Branch0() }\n")
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "one new branch change")
	return c, root
}

func TestCommittedParentReuseReturnsToPrimaryAfterHEADMoves(t *testing.T) {
	for _, modified := range []bool{false, true} {
		t.Run(fmt.Sprintf("modified=%t", modified), func(t *testing.T) {
			f, parentRoot, parent := committedParentReuseFixture(t)
			c, root := committedParentChild(t, f, parentRoot, "child")
			ctx := context.Background()
			if out := c.reconcile(ctx); out.Err != nil {
				t.Fatal(out.Err)
			}
			row, _ := f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
			if row.BaseGenerationID != parent.GenerationID {
				t.Fatal("fixture did not borrow the committed parent")
			}
			builderWriteFile(t, root, "child_only.go", "package fixture\nfunc childOnly() { Branch1() }\n")
			builderGit(t, root, "add", "-A")
			builderGit(t, root, "commit", "-m", "stay near the borrowed branch")
			if out := c.reconcile(ctx); out.Err != nil {
				t.Fatal(out.Err)
			}
			row, _ = f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
			if row.BaseGenerationID != parent.GenerationID {
				t.Fatal("nearby HEAD move lost the useful committed parent")
			}
			builderGit(t, root, "reset", "--hard", builderGit(t, f.primary, "rev-parse", "HEAD"))
			if modified {
				builderWriteFile(t, root, "helper.go", "package fixture\nfunc Helper() { Updated() }\nfunc Updated() {}\n")
				builderGit(t, root, "add", "-A")
				builderGit(t, root, "commit", "-m", "one change on primary")
			}
			if out := c.reconcile(ctx); out.Err != nil {
				t.Fatal(out.Err)
			}
			row, _ = f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
			if row.BaseGenerationID != parent.BaseGenerationID || c.PinnedBaseGeneration() != 0 {
				t.Fatalf("moved checkout retained an unnecessarily distant parent: %+v pin=%d", row, c.PinnedBaseGeneration())
			}
			assertViewIsItsOwnTree(t, f, c.checkoutID, root)
		})
	}
}

func TestCommittedParentReusePreservesSemanticQuality(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
	for _, baselineComplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("baseline_complete=%t", baselineComplete), func(t *testing.T) {
			f := newUnpublishedCommittedBaseFixture(t)
			if baselineComplete {
				manager := goTypesManager(t)
				f.newBuilder = func(store *store_sqlite.Store) *SparseGenerationBuilder {
					b := builderNewBuilder(store)
					b.Semantic = manager
					return b
				}
			}
			builderWriteFile(t, f.primary, "go.mod", "module example.invalid/semanticquality\n\ngo 1.24\n")
			builderGit(t, f.primary, "add", "-A")
			builderGit(t, f.primary, "commit", "-m", "module base")
			f.publishBase(t)
			// The parent cannot finish its compiler pass. With no manager in
			// either build, this must still permit ordinary structural reuse.
			f.newBuilder = nil
			_, parentRoot, _ := committedParentReuseFixtureOver(t, f)
			c, root := committedParentChild(t, f, parentRoot, "child")
			ctx := context.Background()
			base, err := c.primaryBase(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, release, ok := c.chooseCommittedParent(ctx, base, builderGit(t, root, "rev-parse", "HEAD^{tree}"))
			if ok {
				release()
			}
			if ok == baselineComplete {
				t.Fatalf("semantic quality admission=%t with complete baseline=%t", ok, baselineComplete)
			}
		})
	}
}

func TestCommittedParentReuseRefusesChangedInputs(t *testing.T) {
	f, parentRoot, _ := committedParentReuseFixture(t)
	c, root := committedParentChild(t, f, parentRoot, "child")
	ctx := context.Background()
	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"config", func() { c.configHash += "-changed" }},
		{"extractors", func() { c.extractors += "-changed" }},
		{"resolver", func() { c.resolverVersion += "-changed" }},
		{"dependency", func() { c.revisionMu.Lock(); c.revision += "-changed"; c.revisionMu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, extractors, resolver, revision := c.configHash, c.extractors, c.resolverVersion, c.dependencyRevision()
			tc.change()
			defer func() {
				c.configHash, c.extractors, c.resolverVersion = config, extractors, resolver
				c.revisionMu.Lock()
				c.revision = revision
				c.revisionMu.Unlock()
			}()
			if parent, release, ok := c.chooseCommittedParent(ctx, base, target); ok {
				release()
				t.Fatalf("changed %s reused parent %+v", tc.name, parent)
			}
		})
	}
}

func TestCommittedParentReuseRejectsUnavailableAncestry(t *testing.T) {
	for _, ancestor := range []string{"parent", "dedicated"} {
		t.Run(ancestor, func(t *testing.T) {
			f, parentRoot, row := committedParentReuseFixture(t)
			c, root := committedParentChild(t, f, parentRoot, "child")
			ctx := context.Background()
			base, err := c.primaryBase(ctx)
			if err != nil {
				t.Fatal(err)
			}
			target := builderGit(t, root, "rev-parse", "HEAD^{tree}")
			_, release, ok := c.chooseCommittedParent(ctx, base, target)
			if !ok {
				t.Fatal("complete parent was not reusable before withdrawal")
			}
			release()
			id := row.GenerationID
			if ancestor == "dedicated" {
				id = row.BaseGenerationID
			}
			if err := f.catalog.WithdrawProducer(ctx, id, string(graphview.CapIncomingEdges), "test missing incoming data"); err != nil {
				t.Fatal(err)
			}
			if _, release, ok := c.chooseCommittedParent(ctx, base, target); ok {
				release()
				t.Fatal("unavailable ancestor was reused")
			}
			if out := c.reconcile(ctx); out.Err != nil {
				t.Fatal(out.Err)
			}
			built, _ := f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
			if built.BaseGenerationID != base.generationID {
				t.Fatalf("fallback did not use primary: %+v", built)
			}
		})
	}
}

func TestCommittedParentReuseKeepsExactAdoptionFirst(t *testing.T) {
	f, parentRoot, _ := committedParentReuseFixture(t)
	first, firstRoot := committedParentChild(t, f, parentRoot, "first")
	// Force the existing direct path to establish an exact reusable row.
	base, err := first.primaryBase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first.RequestBaseRelease(base.generationID, "establish direct exact generation")
	if out := first.reconcile(context.Background()); out.Err != nil {
		t.Fatal(out.Err)
	}
	exact := f.routeOf(t, first.checkoutID).CommitGenerationID
	second, secondRoot := f.pinDependent(t, "second")
	builderGit(t, secondRoot, "reset", "--hard", builderGit(t, firstRoot, "rev-parse", "HEAD"))
	out := second.reconcile(context.Background())
	if out.Err != nil || out.CommitBuilt || !out.CommitReused || out.CommitGenerationID != exact {
		t.Fatalf("near-parent selection displaced exact adoption: %+v", out)
	}
}

func TestCommittedParentReuseReleaseDuringBuildIsNotPublished(t *testing.T) {
	f, parentRoot, row := committedParentReuseFixture(t)
	c, _ := committedParentChild(t, f, parentRoot, "child")
	core, _ := observer.New(zap.InfoLevel)
	var once sync.Once
	c.builder.Logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "indexer: full index started" {
			once.Do(func() { c.RequestBaseRelease(row.BaseGenerationID, "release during committed build") })
		}
		return nil
	}))
	out := c.reconcile(context.Background())
	if out.Err != nil || !out.Rescheduled || out.CommitBuilt {
		t.Fatalf("released ancestor was routed during build: %+v", out)
	}
	if route := f.routeOf(t, c.checkoutID); route.CommitGenerationID != 0 {
		t.Fatalf("released ancestor got a published route: %+v", route)
	}
	if out := c.reconcile(context.Background()); out.Err != nil || !out.CommitBuilt {
		t.Fatalf("release did not recover through direct build: %+v", out)
	}
	built, _ := f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
	if built.BaseGenerationID != row.BaseGenerationID {
		t.Fatalf("recovery retained committed parent: %+v", built)
	}
}

func TestCommittedParentProducersRequireExplicitCompleteness(t *testing.T) {
	complete := []store_sqlite.ProducerCompleteness{
		{Producer: string(graphview.CapResolutionLocal), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapIncomingEdges), State: store_sqlite.ProducerStateComplete},
	}
	if !completeCommittedParentProducers(complete) {
		t.Fatal("complete pair refused")
	}
	if completeCommittedParentProducers(nil) || completeCommittedParentProducers(complete[:1]) {
		t.Fatal("unknown completeness accepted")
	}
	for _, state := range []store_sqlite.ProducerState{store_sqlite.ProducerStateIncomplete, store_sqlite.ProducerStateBuilding, store_sqlite.ProducerStateUnavailable, store_sqlite.ProducerStateDisabledByConfig} {
		for i := range complete {
			rows := append([]store_sqlite.ProducerCompleteness(nil), complete...)
			rows[i].State = state
			if completeCommittedParentProducers(rows) {
				t.Fatalf("accepted %s for %s", state, rows[i].Producer)
			}
		}
	}
}

func TestCommittedParentReusePreservesCompilerBindingsAndPathMasks(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
	f := newUnpublishedCommittedBaseFixture(t)
	manager := goTypesManager(t)
	f.newBuilder = func(store *store_sqlite.Store) *SparseGenerationBuilder {
		b := builderNewBuilder(store)
		b.Semantic = manager
		return b
	}
	builderWriteFile(t, f.primary, "go.mod", "module example.invalid/parentreuse\n\ngo 1.24\n")
	types := func(result string) string {
		return "package fixture\ntype First struct{}\ntype Second struct{}\ntype Third struct{}\nfunc Make() " + result + " { return " + result + "{} }\n"
	}
	builderWriteFile(t, f.primary, "types.go", types("First"))
	builderWriteFile(t, f.primary, "binding.go", "package fixture\nfunc BindingConsumer() {\n value := Make()\n _ = value\n}\n")
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-m", "typed base")
	f.publishBase(t)
	parent, parentRoot := f.pinDependent(t, "parent")
	for i := range 20 {
		builderWriteFile(t, parentRoot, fmt.Sprintf("branch%d.go", i), fmt.Sprintf("package fixture\nfunc Branch%d() { Helper() }\n", i))
	}
	builderWriteFile(t, parentRoot, "types.go", types("Second"))
	builderWriteFile(t, parentRoot, "inherited.go", "package fixture\nfunc ParentBinding() {\n inherited := Second{}\n _ = inherited\n}\n")
	builderWriteFile(t, parentRoot, "oldname.go", "package fixture\nfunc Renamed() {\n oldValue := Options{}\n _ = oldValue\n}\n")
	builderWriteFile(t, parentRoot, "gone.go", "package fixture\nfunc Gone() {\n goneValue := Options{}\n _ = goneValue\n}\n")
	builderGit(t, parentRoot, "add", "-A")
	builderGit(t, parentRoot, "commit", "-m", "typed shared branch")
	if out := parent.reconcile(context.Background()); out.Err != nil {
		t.Fatal(out.Err)
	}
	parentID := f.routeOf(t, parent.checkoutID).CommitGenerationID
	inherited := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/inherited.go", Line: 3, Name: "inherited"}
	old := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/oldname.go", Line: 3, Name: "oldValue"}
	gone := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/gone.go", Line: 3, Name: "goneValue"}
	parentBindings, err := f.store.AtGeneration(parentID).SemanticBindingTypes([]graph.SemanticBindingSite{inherited, old, gone})
	if err != nil || parentBindings[inherited] != "Second" || parentBindings[old] != "Options" || parentBindings[gone] != "Options" {
		t.Fatalf("parent binding fixture is empty or wrong: %v err %v", parentBindings, err)
	}
	c, root := f.pinDependent(t, "child")
	builderGit(t, root, "reset", "--hard", builderGit(t, parentRoot, "rev-parse", "HEAD"))
	builderWriteFile(t, root, "types.go", types("Third"))
	builderGit(t, root, "mv", "oldname.go", "newname.go")
	builderGit(t, root, "rm", "gone.go")
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "typed child with rename and deletion")
	if out := c.reconcile(context.Background()); out.Err != nil {
		t.Fatal(out.Err)
	}
	row, _ := f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
	if row.BaseGenerationID != parentID {
		t.Fatalf("typed case did not exercise parent reuse: %+v", row)
	}
	masks, err := f.store.AtGeneration(row.GenerationID).FileMasks()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range masks {
		if mask.FilePath == inherited.FilePath && mask.Mode != store_sqlite.OwnershipContext {
			t.Fatal("child owns the parent-only binding fixture; inheritance check would be vacuous")
		}
	}
	m := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := m.MaterializeCheckout(context.Background(), c.checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	site := graph.SemanticBindingSite{RepoPrefix: builderRepoPrefix, FilePath: builderRepoPrefix + "/binding.go", Line: 3, Name: "value"}
	bindings, err := view.Reader.(graph.SemanticBindingTypeReader).SemanticBindingTypes([]graph.SemanticBindingSite{site})
	if err != nil || bindings[site] != "Third" {
		t.Fatalf("child compiler binding is stale: %v, %v", bindings, err)
	}
	if len(view.Reader.GetFileNodes(builderRepoPrefix+"/oldname.go")) != 0 || len(view.Reader.GetFileNodes(builderRepoPrefix+"/gone.go")) != 0 || len(view.Reader.GetFileNodes(builderRepoPrefix+"/newname.go")) == 0 {
		t.Fatal("rename/deletion masks did not survive parent reuse")
	}
	renamed := old
	renamed.FilePath = builderRepoPrefix + "/newname.go"
	carried, err := view.Reader.(graph.SemanticBindingTypeReader).SemanticBindingTypes([]graph.SemanticBindingSite{inherited, old, gone, renamed})
	if err != nil || carried[inherited] != "Second" || carried[renamed] != "Options" {
		t.Fatalf("parent inheritance/renamed binding is wrong: %v err %v", carried, err)
	}
	if _, found := carried[old]; found {
		t.Fatal("old-path binding survived rename")
	}
	if _, found := carried[gone]; found {
		t.Fatal("deleted-path binding survived deletion")
	}
	flat := builderOpenStore(t, "typed-parent-oracle")
	builderIndexSemantic(manager)(t, flat, root)
	builderAssertReadersAgree(t, view.Reader, flat)
	// A normal full index does not persist its in-memory compiler binding
	// projection. Use an independent direct whole-module compiler pass as
	// the durable binding oracle, as the semantic-chain suite does.
	bindingOracle := builderOpenStore(t, "typed-parent-bindings")
	provider := goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, zap.NewNop())
	defer provider.Close()
	if _, err := provider.EnrichRepo(bindingOracle, builderRepoPrefix, root); err != nil {
		t.Fatal(err)
	}
	expected, err := bindingOracle.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	if err != nil || bindings[site] != expected[site] {
		t.Fatalf("binding differs from fresh compiler index: got %v want %v err %v", bindings, expected, err)
	}
	files := []string{builderRepoPrefix + "/oldname.go", builderRepoPrefix + "/gone.go"}
	seen := map[string]bool{}
	for _, node := range flat.AllNodes() {
		if node.FilePath != "" && !seen[node.FilePath] {
			seen[node.FilePath] = true
			files = append(files, node.FilePath)
		}
	}
	allBindings, err := c.ancestryLayerBase(view).(graph.SemanticBindingFileReader).SemanticBindingTypesForFiles(builderRepoPrefix, files)
	if err != nil {
		t.Fatal(err)
	}
	allExpected, err := bindingOracle.SemanticBindingTypesForFiles(builderRepoPrefix, files)
	if err != nil || !maps.Equal(allBindings, allExpected) {
		t.Fatalf("full compiler binding projection differs: got %v want %v err %v", allBindings, allExpected, err)
	}
}

func TestCommittedParentReuseReleasesDisplacedCandidate(t *testing.T) {
	f, parentRoot, better := committedParentReuseFixture(t)
	worse, worseRoot := committedParentChild(t, f, parentRoot, "worse")
	worse.RequestBaseRelease(better.BaseGenerationID, "build an independent direct candidate")
	if out := worse.reconcile(context.Background()); out.Err != nil {
		t.Fatal(out.Err)
	}
	worseRow, _ := f.generation(f.routeOf(t, worse.checkoutID).CommitGenerationID)
	if worseRow.BaseGenerationID != better.BaseGenerationID {
		t.Fatal("newer candidate is not direct over dedicated")
	}
	// The child of the older branch differs by one path; the newer, worse
	// candidate differs by two. Newest-first scanning must replace its lease.
	c, root := committedParentChild(t, f, parentRoot, "child")
	base, err := c.primaryBase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := f.leases.Held()
	parent, release, ok := c.chooseCommittedParent(context.Background(), base, builderGit(t, root, "rev-parse", "HEAD^{tree}"))
	if !ok {
		t.Fatal("no parent selected")
	}
	if parent.generationID != better.GenerationID || f.leases.InUse(worseRow.GenerationID) {
		release()
		t.Fatalf("displaced candidate remains selected or leased: selected %d better %d worse %d", parent.generationID, better.GenerationID, worseRow.GenerationID)
	}
	release()
	if f.leases.Held() != before {
		t.Fatal("replacing candidate leaked its ancestry lease")
	}
	_ = worseRoot
}

func TestCommittedParentReuseKeepsOldOwnExactAfterInterruptedPublication(t *testing.T) {
	f, parentRoot, parentRow := committedParentReuseFixture(t)
	c, root := committedParentChild(t, f, parentRoot, "child")
	ctx := context.Background()
	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ensureRoute(ctx, base); err != nil {
		t.Fatal(err)
	}
	target := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	// Match a crash between durable commit-layer publication and the first
	// route flip: no process cache or route slot owns this completed build.
	exact, reused, err := c.resolveCommitLayer(ctx, base, target)
	if err != nil || reused {
		t.Fatalf("establish exact orphan: id %d reused %v err %v", exact, reused, err)
	}
	adopter, adopterRoot := f.pinDependent(t, "adopter")
	builderGit(t, adopterRoot, "reset", "--hard", builderGit(t, root, "rev-parse", "HEAD"))
	if out := adopter.reconcile(ctx); out.Err != nil || !out.CommitReused || out.CommitGenerationID != exact {
		t.Fatalf("sibling did not protect exact published row: %+v", out)
	}
	for i := range maxSharedCommitLayerCandidates + 1 {
		id, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
			OwnerKind: checkoutLayerOwnerKind, GraphID: f.graphID,
			LayerID: fmt.Sprintf("noise-%d", i), CheckoutID: c.checkoutID,
			GenerationKind: CommitLayerGenerationKind, TreeOID: fmt.Sprintf("%040x", i+1),
			ConfigHash: "unrelated", State: store_sqlite.ViewGenerationBuilding, CreatedAt: int64(i + 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.catalog.PublishViewGeneration(ctx, id, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	// Place a genuinely reusable near parent inside the bounded scan, while
	// the exact own row is now outside it. Neither row changes its identity.
	recent, recentRoot := f.pinDependent(t, "recent")
	builderGit(t, recentRoot, "reset", "--hard", builderGit(t, parentRoot, "rev-parse", "HEAD"))
	near, _, err := recent.resolveCommitLayer(ctx, base, parentRow.TreeOID)
	if err != nil || near <= exact {
		t.Fatalf("establish recent near parent: %d err %v", near, err)
	}
	if route := f.routeOf(t, c.checkoutID); route.CommitGenerationID != 0 {
		t.Fatal("interrupted-publication fixture already has a commit route")
	}
	out := c.reconcile(ctx)
	if out.Err != nil || out.CommitBuilt || !out.CommitReused || out.CommitGenerationID != exact {
		t.Fatalf("older exact own publication lost to near-parent build: %+v", out)
	}
}

func TestCommittedParentReuseLeasesAndCancellation(t *testing.T) {
	f, parentRoot, row := committedParentReuseFixture(t)
	c, root := committedParentChild(t, f, parentRoot, "child")
	base, err := c.primaryBase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	before := f.leases.Held()
	_, release, ok := c.chooseCommittedParent(context.Background(), base, target)
	if !ok {
		t.Fatal("complete parent was not selected")
	}
	if !f.leases.InUse(row.GenerationID) || !f.leases.InUse(row.BaseGenerationID) {
		release()
		t.Fatal("selected parent ancestry was not leased")
	}
	release()
	if f.leases.Held() != before {
		t.Fatal("parent selection leaked its ancestry lease")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, release, ok := c.chooseCommittedParent(ctx, base, target); ok {
		release()
		t.Fatal("cancelled selection accepted a parent")
	}
	if f.leases.Held() != before {
		t.Fatal("cancelled selection leaked a lease")
	}
}

func TestBorrowedCommittedParentCollectsAfterLastChildRetires(t *testing.T) {
	f, parentRoot, parent := committedParentReuseFixture(t)
	child, childRoot := committedParentChild(t, f, parentRoot, "child")
	ctx := t.Context()
	if out := child.reconcile(ctx); out.Err != nil {
		t.Fatal(out.Err)
	}
	borrowed := f.routeOf(t, child.checkoutID)
	row, _ := f.generation(borrowed.CommitGenerationID)
	if row.BaseGenerationID != parent.GenerationID {
		t.Fatal("child did not borrow parent")
	}
	// The originating checkout goes away; its empty dirty layer has to retire
	// before only the borrower's immutable ancestry is retaining the parent.
	ownerRoute := f.routeOf(t, parent.CheckoutID)
	if err := f.catalog.DeleteCheckoutRoute(ctx, parent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetirePayloadGeneration(ctx, ownerRoute.DirtyGenerationID, f.leases.InUse); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetirePayloadGeneration(ctx, parent.GenerationID, f.leases.InUse); !errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced) {
		t.Fatalf("borrowed parent lost protection: %v", err)
	}
	assertViewIsItsOwnTree(t, f, child.checkoutID, childRoot)
	// Moving the borrower back to primary removes it from the live stack,
	// but the branch-switch cache's superseded child still references it.
	builderGit(t, childRoot, "reset", "--hard", builderGit(t, f.primary, "rev-parse", "HEAD"))
	if out := child.reconcile(ctx); out.Err != nil {
		t.Fatal(out.Err)
	}
	if err := f.store.RetirePayloadGeneration(ctx, borrowed.DirtyGenerationID, f.leases.InUse); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetirePayloadGeneration(ctx, parent.GenerationID, f.leases.InUse); !errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced) {
		t.Fatalf("cached child no longer protects parent: %v", err)
	}
	// Once cache retention ends, collecting the superseded child releases
	// its ancestry and the borrowed layer is physically collectable.
	child.forgetRetained(borrowed.CommitGenerationID)
	if err := f.store.RetirePayloadGeneration(ctx, borrowed.CommitGenerationID, f.leases.InUse); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RetirePayloadGeneration(ctx, parent.GenerationID, f.leases.InUse); err != nil {
		t.Fatal(err)
	}
	if _, found := f.generation(parent.GenerationID); found {
		t.Fatal("borrowed parent remained after last child retired")
	}
}
