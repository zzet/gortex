package indexer

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/resolver"
)

// The per-stack caches over a dirty chain.
//
// A working-tree edit stands on the dirty chain the previous edits published.
// The per-stack caches are keyed by the stack below the chain, and every read
// a delta serves from them composes the chain's layers (and the delta's own)
// over the kept answer. These tests hold every such read to the same read
// composed without any cache, over a routed chain the coordinator built
// through a rename, a changed signature, a removed declaration, an added file
// and a deleted file — at every depth, with the caches warmed by the other
// depths in both orders, and with and without the delta's own writes.
//
// The fixture's views compose the base corpus (generation zero), which a
// daemon does not key: the tests key them by their generations, and nothing
// writes generation zero while they run.

func chainOverlayTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/fx\n\ngo 1.22\n\nrequire example.com/base v1.0.0\n",
		"core/core.go": `package core

type Options struct{ N int }

func Compute(o Options, scale int) int {
	return Helper(o.N) * scale
}

func Helper(n int) int {
	return n + 1
}
`,
		"core/extra.go": `package core

func Gone() int {
	return Helper(2)
}

func Keep() int {
	return Helper(1)
}
`,
		"app/caller.go": `package app

import "example.com/fx/core"

func Run() int {
	return core.Compute(core.Options{N: 1}, 2)
}
`,
		"app/other.go": `package app

import "example.com/fx/core"

func Other() int {
	return core.Gone() + core.Keep()
}
`,
		"app/oldname.go": `package app

func Renamed() int {
	return Run()
}
`,
		// A literal two files emit: its one node is the smaller file's copy.
		"shared/x.go": "package shared\n\nimport \"errors\"\n\nfunc X() error {\n\treturn errors.New(\"boom\")\n}\n",
		"shared/y.go": "package shared\n\nimport \"errors\"\n\n// Y is the larger file.\nfunc Y() error {\n\tif true {\n\t\treturn errors.New(\"boom\")\n\t}\n\treturn nil\n}\n",
		// HTTP routes (contracts), one ID shared by two files.
		"api/routes.go": "package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc setupRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsers)\n\tr.POST(\"/api/items\", createItem)\n}\n\nfunc listUsers()  {}\nfunc createItem() {}\n",
		"api/more.go":   "package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc moreRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsersToo)\n}\n\nfunc listUsersToo() {}\n",
		"web/app.module.ts": `import { Module } from "@nestjs/common";
import { Logger, ConsoleLogger } from "./logger";

@Module({
  providers: [{ provide: Logger, useClass: ConsoleLogger }],
})
export class AppModule {}
`,
		"web/logger.ts": `export class Logger {}
export class ConsoleLogger extends Logger {}
export class FileLogger extends Logger {}
`,
	}
}

// chainOverlayEdit is one working-tree edit of the chain.
type chainOverlayEdit struct {
	name  string
	write map[string]string
	drop  []string
}

func chainOverlayEdits() []chainOverlayEdit {
	return []chainOverlayEdit{
		{name: "signature", write: map[string]string{"go.mod": "module example.com/fx\n\ngo 1.22\n\nrequire (\n\texample.com/base v1.0.0\n\texample.com/dep v1.2.3\n)\n", "core/core.go": `package core

type Options struct{ N int }

func Compute(o Options) int {
	return Helper(o.N)
}

func Helper(n int) int {
	return n + 1
}
`}},
		{name: "rename_symbol", write: map[string]string{"api/routes.go": "package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc setupRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsers)\n\tr.POST(\"/api/things\", createItem)\n}\n\nfunc listUsers()  {}\nfunc createItem() {}\n", "core/core.go": `package core

type Options struct{ N int }

func Compute(o Options) int {
	return Assist(o.N)
}

func Assist(n int) int {
	return n + 1
}
`}},
		{name: "remove_declaration", write: map[string]string{"core/extra.go": `package core

func Keep() int {
	return Assist(1)
}
`}},
		{name: "add_file", write: map[string]string{"app/added.go": `package app

import "example.com/fx/core"

func Added() int {
	return core.Keep() + Run()
}
`}},
		{name: "delete_file", drop: []string{"app/other.go", "api/more.go"}},
		{name: "rename_file", drop: []string{"app/oldname.go"}, write: map[string]string{"app/newname.go": `package app

func Renamed() int {
	return Run() + Added()
}
`}},
		{name: "remove_provider_target", write: map[string]string{"shared/x.go": "package shared\n\nfunc X() error {\n\treturn nil\n}\n", "web/logger.ts": `export class Logger {}
export class FileLogger extends Logger {}
`}},
		{name: "rebind_provider", write: map[string]string{"web/app.module.ts": `import { Module } from "@nestjs/common";
import { Logger, FileLogger } from "./logger";

@Module({
  providers: [{ provide: Logger, useClass: FileLogger }],
})
export class AppModule {}
`}},
	}
}

// chainOverlayDepth is one published depth of the routed chain, its view held
// open.
type chainOverlayDepth struct {
	label string
	depth int
	view  *graphview.RepoView
	base  commitLayerBase
}

type chainOverlayRun struct {
	t      *testing.T
	f      *coordinatorFixture
	c      *CheckoutCoordinator
	depths []chainOverlayDepth
	// paths, ids and names are every path, identity and name any depth
	// served: the reads are asked for all of them.
	paths map[string]struct{}
	ids   map[string]struct{}
	names map[string]struct{}
}

// resetChainOverlayCaches empties every per-stack cache a delta keeps.
func resetChainOverlayCaches() {
	editDeltaBaseCaches.Lock()
	editDeltaBaseCaches.keys, editDeltaBaseCaches.caches = nil, nil
	editDeltaBaseCaches.Unlock()
	editDeltaBelowRows.Lock()
	editDeltaBelowRows.keys, editDeltaBelowRows.byKey = nil, nil
	editDeltaBelowRows.Unlock()
	resetEditDeltaPriorViews()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	resetEditDeltaParamIndexes()
	resetEditDeltaGoInventories()
	resetEditDeltaGoOwnershipLookups()
	resetEditDeltaPriorFingerprints()
	resetEditDeltaContractCache()
	resetEditDeltaChainLayers()
}

// newChainOverlayRun routes the commit, then publishes one chained
// working-tree generation per edit, and holds every depth's view.
func newChainOverlayRun(t *testing.T) *chainOverlayRun {
	t.Helper()
	resetChainOverlayCaches()
	t.Cleanup(resetChainOverlayCaches)
	f := newCoordinatorFixtureWithTree(t, chainOverlayTree())
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	r := &chainOverlayRun{t: t, f: f, c: c,
		paths: map[string]struct{}{}, ids: map[string]struct{}{}, names: map[string]struct{}{}}
	first := coordinatorReconcile(t, c)
	if first.CommitGenerationID <= 0 {
		t.Fatal("no routed commit generation")
	}
	// The commit generation alone, then the routed checkout as the first
	// cycle left it.
	commitView, err := (&graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}).
		MaterializeRefView(context.Background(), f.graphID, first.CommitGenerationID)
	if err != nil {
		t.Fatalf("materialize the commit generation: %v", err)
	}
	r.holdView("commit", commitView)
	r.hold("routed")
	for i, edit := range chainOverlayEdits() {
		for rel, body := range edit.write {
			full := filepath.Join(f.worktree, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, f.worktree, rel, body)
		}
		for _, rel := range edit.drop {
			if err := os.Remove(filepath.Join(f.worktree, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
		}
		cycle := coordinatorReconcile(t, c)
		// The first edit is built direct (a one-path delta over the clean
		// layer is not smaller); every later one chains over it.
		if want := i + 1; !cycle.DirtyBuilt || cycle.DirtyChainDepth != want {
			t.Fatalf("%s: built=%t at depth %d, want depth %d (reason %q)",
				edit.name, cycle.DirtyBuilt, cycle.DirtyChainDepth, want, cycle.DirtyChainReason)
		}
		r.hold(edit.name)
		if got := r.depths[len(r.depths)-1].depth; got != cycle.DirtyChainDepth {
			t.Fatalf("%s: the view stacks %d chain generations, the cycle reports depth %d", edit.name, got, cycle.DirtyChainDepth)
		}
	}
	t.Cleanup(func() {
		for _, d := range r.depths {
			d.view.Close()
		}
	})
	return r
}

// hold opens the routed view.
func (r *chainOverlayRun) hold(label string) {
	r.t.Helper()
	r.holdView(label, chainMaterialize(r.t, r.f))
}

// holdView keys a view by its generations, the working-tree generations at
// its top the dirty chain (as a chained build's base marks them), and holds
// it.
func (r *chainOverlayRun) holdView(label string, view *graphview.RepoView) {
	r.t.Helper()
	stack := view.Generations()
	depth := 0
	for i := len(stack) - 1; i > 0; i-- {
		row, found, err := r.f.catalog.GetViewGeneration(context.Background(), stack[i])
		if err != nil || !found {
			r.t.Fatalf("%s: read generation %d: %v", label, stack[i], err)
		}
		if row.GenerationKind != DirtyLayerGenerationKind {
			break
		}
		depth++
	}
	base := commitLayerBase{Reader: view.Reader, facts: newAncestryRefFacts(r.f.store, view), stack: stack}
	if depth > 0 {
		base = withChainDepth(base, stack[len(stack)-1], depth).(commitLayerBase)
	}
	r.t.Logf("%s: stack %v, chain depth %d", label, stack, depth)
	r.depths = append(r.depths, chainOverlayDepth{label: label, depth: depth, view: view, base: base})
	for n := range view.Reader.NodesByKind(graph.KindFile) {
		r.paths[n.FilePath] = struct{}{}
	}
	for _, n := range view.Reader.AllNodes() {
		if n == nil {
			continue
		}
		r.ids[n.ID] = struct{}{}
		if n.FilePath != "" {
			r.paths[n.FilePath] = struct{}{}
		}
		if n.Name != "" {
			r.names[n.Name] = struct{}{}
		}
	}
}

func chainOverlayKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// chainOverlayPair is the cached delta and the uncached one over one depth.
type chainOverlayPair struct {
	cached, plain *graph.DeltaWriter
	key           string
	cacheBase     graph.Reader
}

func (r *chainOverlayRun) pair(d chainOverlayDepth) chainOverlayPair {
	r.t.Helper()
	cached := graph.NewDeltaWriter(d.base, nil)
	keyBase, cacheBase := installEditDeltaStackCache(cached, d.base, r.f.store)
	if cached.ChainLayers() != d.depth {
		r.t.Fatalf("%s: the delta composes %d chain layers, want %d", d.label, cached.ChainLayers(), d.depth)
	}
	key, ok := editDeltaBaseCacheKey(keyBase, r.f.store)
	if !ok {
		r.t.Fatalf("%s: no stack key", d.label)
	}
	return chainOverlayPair{cached: cached, plain: graph.NewDeltaWriter(d.base, nil), key: key, cacheBase: cacheBase}
}

// ownWrites re-derives app/caller.go in the delta's own layer and restates a
// caller in core/extra.go: the same writes on both deltas.
func chainOverlayOwnWrites(dw *graph.DeltaWriter) {
	p := builderRepoPrefix + "/app/caller.go"
	dw.EvictFile(p)
	dw.AddBatch([]*graph.Node{
		{ID: p, Kind: graph.KindFile, Name: "caller.go", FilePath: p, RepoPrefix: builderRepoPrefix, Language: "go"},
		{ID: p + "::RunOwn", Kind: graph.KindFunction, Name: "RunOwn", FilePath: p, RepoPrefix: builderRepoPrefix, Language: "go", StartLine: 5, EndLine: 7},
	}, []*graph.Edge{
		{From: p + "::RunOwn", To: builderRepoPrefix + "/core/extra.go::Keep", Kind: graph.EdgeCalls, FilePath: p, Line: 6},
		{From: p, To: builderRepoPrefix + "/core", Kind: graph.EdgeImports, FilePath: p, Line: 3},
	})
}

func renderChainNodes(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%d-%d", n.ID, n.Kind, n.Name, n.FilePath, n.RepoPrefix, n.StartLine, n.EndLine))
	}
	sort.Strings(out)
	return out
}

func renderChainEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s>%s|%s|%s:%d", e.From, e.To, e.Kind, e.FilePath, e.Line))
	}
	sort.Strings(out)
	return out
}

func renderChainNodeMap(m map[string][]*graph.Node) []string {
	var out []string
	for k, nodes := range m {
		for _, line := range renderChainNodes(nodes) {
			out = append(out, k+"="+line)
		}
	}
	sort.Strings(out)
	return out
}

func renderChainEdgeMap(m map[string][]*graph.Edge) []string {
	var out []string
	for k, edges := range m {
		for _, line := range renderChainEdges(edges) {
			out = append(out, k+"="+line)
		}
	}
	sort.Strings(out)
	return out
}

// chainOverlayRows counts, per comparison, the rows the uncached reads
// served over the whole run: a comparison of two empty answers proves
// nothing, so the run requires each read to have served rows somewhere.
var chainOverlayRows = map[string]int{}

// chainOverlaySame fails when two renders differ, naming the first rows
// either side has alone.
func chainOverlaySame(t *testing.T, what string, cached, plain []string) {
	t.Helper()
	name := t.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	chainOverlayRows[name] += len(plain)
	if strings.Join(cached, "\n") == strings.Join(plain, "\n") {
		return
	}
	have := make(map[string]int)
	for _, line := range plain {
		have[line]++
	}
	var extra, missing []string
	for _, line := range cached {
		if have[line] > 0 {
			have[line]--
			continue
		}
		extra = append(extra, line)
	}
	for line, n := range have {
		for ; n > 0; n-- {
			missing = append(missing, line)
		}
	}
	sort.Strings(missing)
	if len(extra) > 5 {
		extra = extra[:5]
	}
	if len(missing) > 5 {
		missing = missing[:5]
	}
	t.Fatalf("%s: the cached read differs from the uncached one (%d rows against %d)\nonly cached: %v\nonly uncached: %v",
		what, len(cached), len(plain), extra, missing)
}

// TestChainOverlayCachedReadsMatchUncached holds every per-stack cache's read
// over a routed dirty chain to the uncached read, depth by depth.
func TestChainOverlayCachedReadsMatchUncached(t *testing.T) {
	r := newChainOverlayRun(t)
	// Forward: each depth reads the caches the shallower depths warmed.
	var firstKey string
	for _, d := range r.depths {
		p := r.pair(d)
		if firstKey == "" {
			firstKey = p.key
		} else if p.key != firstKey {
			t.Fatalf("%s: the chain moved the stack key\n%q\n%q", d.label, p.key, firstKey)
		}
		r.check(t, "forward/"+d.label, d, p)
	}
	// Every kept answer was served again to a deeper chain.
	cache := editDeltaBaseCache(firstKey)
	for name, hits := range map[string]func() (int, int){
		"path_nodes":      cache.StackFileNodeStats,
		"nodes_by_kind":   cache.StackNodeStats,
		"file_identities": cache.StackFileStats,
		"names":           cache.StackNameStats,
		"placements":      cache.StackPlacementStats,
		"in_identities":   cache.StackInIdentityStats,
		"import_adjacency": func() (int, int) {
			return cache.StackStats()
		},
		"recorded_edges":  cache.StackRecordedEdgeStats,
		"layer_adjacency": cache.StackLayerAdjacencyStats,
	} {
		if h, m := hits(); h == 0 {
			t.Errorf("%s: the stack's cache served no read over the chain (%d misses)", name, m)
		}
	}
	// Backward, from empty caches: the deepest chain warms them, and every
	// shallower depth (the commit alone last) reads what it kept.
	resetChainOverlayCaches()
	for i := len(r.depths) - 1; i >= 0; i-- {
		d := r.depths[i]
		r.check(t, "backward/"+d.label, d, r.pair(d))
	}
	// Some chain layer carries a node outside the paths it covers (the
	// moved literal's kept copy), so the kept layers' detached rows are read.
	detached := 0
	for _, d := range r.depths {
		for _, source := range d.view.GenerationSources() {
			if layer, ok := source.Layer.(graph.OverlayDetachedSummaryReader); ok {
				for n := range layer.DetachedNodeSummaries() {
					detached++
					if os.Getenv("GX_CHAIN_OVERLAY_DEBUG") != "" {
						t.Logf("%s: generation %d carries %s at %s", d.label, source.Generation, n.ID, n.FilePath)
					}
				}
			}
		}
	}
	if detached == 0 {
		t.Error("fixture precondition: no chain layer carries a node outside its covered paths")
	}
	if layers, rows, hits, loads := editDeltaChainLayerRowsFor(r.f.store).Stats(); hits == 0 || loads == 0 {
		t.Errorf("the chain's kept layers served nothing (%d layers, %d rows, %d hits, %d loads)", layers, rows, hits, loads)
	} else {
		t.Logf("kept chain layers: %d layers, %d rows, %d hits, %d loads", layers, rows, hits, loads)
	}
	for _, name := range []string{"path_nodes", "file_identities", "nodes_by_kind", "names", "placements",
		"in_identities", "layer_adjacency", "import_adjacency", "recorded_edges", "below_rows", "provides",
		"param_index", "prior_edges", "go_inventory", "deps", "deps_declined"} {
		if chainOverlayRows[name] == 0 {
			t.Errorf("%s: the uncached reads served no rows over the whole run", name)
		}
	}
	t.Logf("rows compared per read: %v", chainOverlayRows)
}

// check runs every cache's comparison at one depth: first with the delta's
// own layer empty (the reads a delta makes before it writes), then after the
// same own writes on both deltas.
func (r *chainOverlayRun) check(t *testing.T, label string, d chainOverlayDepth, p chainOverlayPair) {
	t.Helper()
	paths := chainOverlayKeys(r.paths)
	ids := chainOverlayKeys(r.ids)
	names := chainOverlayKeys(r.names)

	t.Run(label+"/prior_edges", func(t *testing.T) {
		source := editDeltaPriorEdges(p.cached, p.key)
		in, out, ok := source(ids)
		if !ok {
			t.Fatal("the prior-edge source declined an untouched delta")
		}
		chainOverlaySame(t, "in-edges", renderChainEdgeMap(in), renderChainEdgeMap(p.plain.GetInEdgesByNodeIDs(ids)))
		chainOverlaySame(t, "out-edges", renderChainEdgeMap(out), renderChainEdgeMap(p.plain.GetOutEdgesByNodeIDs(ids)))
	})
	t.Run(label+"/go_inventory", func(t *testing.T) {
		idx := &Indexer{repoPrefix: builderRepoPrefix, graph: p.cached, resolver: resolver.New(p.cached)}
		key, _ := editDeltaContractCacheKey(d.base, r.f.store, builderRepoPrefix, "", "")
		o := primeEditDeltaGoOwnership(idx, key, []string{"core/core.go"})
		if o == nil {
			t.Fatal("no ownership factory")
		}
		var got []string
		for n := range o.currentFiles() {
			got = append(got, n.FilePath)
		}
		sort.Strings(got)
		want := goInventoryOf(graph.NodesInScopeSeq(p.plain, []string{builderRepoPrefix}, nil, graph.KindFile))
		chainOverlaySame(t, "go files", got, want)
	})
	t.Run(label+"/deps", func(t *testing.T) {
		idx := &Indexer{repoPrefix: builderRepoPrefix, graph: p.cached, resolver: resolver.New(p.cached)}
		installEditDeltaDeps(idx, p.key, []string{"core/core.go"}, p.cached.ChainTouchedPaths())
		source, ok := editDeltaDepSources.Load(idx.resolver)
		want := depContractRows(p.plain, builderRepoPrefix)
		if !ok {
			// A chain that speaks for a manifest reads its own view.
			chainOverlayRows["deps_declined"]++
			return
		}
		var got []graph.RepoNodeIdentity
		for row := range source.(func([]string) iter.Seq[graph.RepoNodeIdentity])([]string{builderRepoPrefix}) {
			got = append(got, row)
		}
		render := func(rows []graph.RepoNodeIdentity) []string {
			out := make([]string, 0, len(rows))
			for _, row := range rows {
				out = append(out, fmt.Sprint(row))
			}
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "dep contracts", render(got), render(want))
	})
	r.checkReads(t, label+"/pristine", d, p, paths, ids, names)
	chainOverlayOwnWrites(p.cached)
	chainOverlayOwnWrites(p.plain)
	r.checkReads(t, label+"/own", d, p, paths, ids, names)
}

// checkReads compares the reads a delta serves from the caches at any time.
func (r *chainOverlayRun) checkReads(t *testing.T, label string, d chainOverlayDepth, p chainOverlayPair, paths, ids, names []string) {
	t.Helper()
	repos := []string{builderRepoPrefix}
	t.Run(label+"/path_nodes", func(t *testing.T) {
		chainOverlaySame(t, "file nodes", renderChainNodeMap(p.cached.GetFileNodesByPaths(paths)), renderChainNodeMap(p.plain.GetFileNodesByPaths(paths)))
	})
	t.Run(label+"/file_identities", func(t *testing.T) {
		render := func(dw *graph.DeltaWriter, repos []string) []string {
			var out []string
			for row := range dw.FileNodeIdentitiesSeq(repos) {
				out = append(out, fmt.Sprintf("%s|%s|%s", row.ID, row.FilePath, row.RepoPrefix))
			}
			// The global read's order is the composed view's, which the
			// uncached read does not fix.
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "repository", render(p.cached, repos), render(p.plain, repos))
		chainOverlaySame(t, "global", render(p.cached, nil), render(p.plain, nil))
	})
	t.Run(label+"/nodes_by_kind", func(t *testing.T) {
		for _, kinds := range [][]graph.NodeKind{
			{graph.KindFunction},
			{graph.KindType, graph.KindInterface},
			{graph.KindParam},
			{graph.KindFile, graph.KindFunction, graph.KindMethod},
		} {
			chainOverlaySame(t, fmt.Sprint(kinds), renderChainNodes(p.cached.NodesInFilesByKind(paths, kinds)), renderChainNodes(p.plain.NodesInFilesByKind(paths, kinds)))
		}
	})
	t.Run(label+"/names", func(t *testing.T) {
		scopes := []graph.ResolverNameScope{
			{RepoPrefix: builderRepoPrefix, Names: names},
			{RepoPrefix: builderRepoPrefix, Names: names, Languages: []string{"go"}},
			{AllRepos: true, Names: names},
			{AllRepos: true, Names: names, Languages: []string{"typescript"}},
		}
		cached, err := p.cached.FindNodesByResolverNameScopes(scopes)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := p.plain.FindNodesByResolverNameScopes(scopes)
		if err != nil {
			t.Fatal(err)
		}
		for i := range scopes {
			chainOverlaySame(t, fmt.Sprintf("scope %d", i), renderChainNodeMap(cached[i]), renderChainNodeMap(plain[i]))
		}
		chainOverlaySame(t, "in repository", renderChainNodeMap(p.cached.FindNodesByNamesInRepo(names, builderRepoPrefix)),
			renderChainNodeMap(p.plain.FindNodesByNamesInRepo(names, builderRepoPrefix)))
	})
	t.Run(label+"/ref_facts", func(t *testing.T) {
		render := func(m map[string][]graph.RefFact) []string {
			var out []string
			for file, facts := range m {
				for _, f := range facts {
					out = append(out, fmt.Sprintf("%s=%s>%s|%s|%s:%d", file, f.FromID, f.ToID, f.Kind, f.RefName, f.Line))
				}
			}
			sort.Strings(out)
			return out
		}
		cached, err := p.cached.LoadRefFactsByTargets(builderRepoPrefix, ids)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := p.plain.LoadRefFactsByTargets(builderRepoPrefix, ids)
		if err != nil {
			t.Fatal(err)
		}
		chainOverlaySame(t, "facts", render(cached), render(plain))
	})
	t.Run(label+"/placements", func(t *testing.T) {
		render := func(m map[string]graph.NodePlacement) []string {
			var out []string
			for id, pl := range m {
				out = append(out, fmt.Sprintf("%s|%s|%s|%s", id, pl.Kind, pl.FilePath, pl.RepoPrefix))
			}
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "placements", render(p.cached.NodePlacementsByIDs(ids)), render(p.plain.NodePlacementsByIDs(ids)))
	})
	t.Run(label+"/in_identities", func(t *testing.T) {
		render := func(m map[string][]graph.EdgeIdentity) []string {
			var out []string
			for id, rows := range m {
				for _, e := range rows {
					out = append(out, fmt.Sprintf("%s=%s>%s|%s|%s:%d", id, e.From, e.To, e.Kind, e.FilePath, e.Line))
				}
			}
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "incoming identities", render(p.cached.GetInEdgeIdentitiesByNodeIDs(ids)), render(p.plain.GetInEdgeIdentitiesByNodeIDs(ids)))
	})
	t.Run(label+"/layer_adjacency", func(t *testing.T) {
		chainOverlaySame(t, "in-edges", renderChainEdgeMap(p.cached.GetInEdgesByNodeIDs(ids)), renderChainEdgeMap(p.plain.GetInEdgesByNodeIDs(ids)))
		chainOverlaySame(t, "out-edges", renderChainEdgeMap(p.cached.GetOutEdgesByNodeIDs(ids)), renderChainEdgeMap(p.plain.GetOutEdgesByNodeIDs(ids)))
	})
	t.Run(label+"/import_adjacency", func(t *testing.T) {
		cached, ok := p.cached.ProjectImportAdjacency(paths)
		if !ok {
			t.Fatal("cached import adjacency declined")
		}
		plain, ok := p.plain.ProjectImportAdjacency(paths)
		if !ok {
			t.Fatal("uncached import adjacency declined")
		}
		render := func(m map[string][]string) []string {
			var out []string
			for from, targets := range m {
				for _, to := range targets {
					out = append(out, from+">"+to)
				}
			}
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "imports", render(cached), render(plain))
	})
	t.Run(label+"/recorded_edges", func(t *testing.T) {
		cached, ok := graph.RecordedEdgesOf(p.cached)
		if !ok {
			t.Fatal("no cached recorded-edge reader")
		}
		plain, ok := graph.RecordedEdgesOf(p.plain.View())
		if !ok {
			t.Fatal("no uncached recorded-edge reader")
		}
		chainOverlaySame(t, "recorded", renderChainEdges(cached.RecordedEdgesAt(paths)), renderChainEdges(plain.RecordedEdgesAt(paths)))
	})
	t.Run(label+"/below_rows", func(t *testing.T) {
		source := editDeltaBelowRowsSource{key: p.key, base: p.cacheBase}
		if p.cached.ChainLayers() > 0 {
			source.chain = p.cached
		}
		below := p.plain.Below()
		recorded, ok := graph.RecordedEdgesOf(below)
		if !ok {
			t.Fatal("no recorded-edge reader below")
		}
		for _, path := range paths {
			nodes, edges, ok := source.BelowFileRows(path)
			if !ok {
				t.Fatalf("%s: no rows", path)
			}
			chainOverlaySame(t, path+" nodes", renderChainNodes(nodes), renderChainNodes(below.GetFileNodes(path)))
			chainOverlaySame(t, path+" recorded", renderChainEdges(edges), renderChainEdges(recorded.RecordedEdgesAt([]string{path})))
		}
	})
	t.Run(label+"/provides", func(t *testing.T) {
		idx := &Indexer{repoPrefix: builderRepoPrefix, graph: p.cached, resolver: resolver.New(p.cached)}
		o := installEditDeltaProvides(idx, p.cacheBase, p.key, []string{"app/caller.go"})
		if o == nil {
			t.Fatal("no provides source")
		}
		render := func(byRepo map[string][]*graph.Edge) []string {
			var out []string
			for repo, edges := range byRepo {
				for _, line := range renderChainEdges(edges) {
					out = append(out, repo+"="+line)
				}
			}
			sort.Strings(out)
			return out
		}
		want := make(map[string][]*graph.Edge)
		for _, row := range scanProvidesRows(p.plain) {
			want[row.repo] = append(want[row.repo], row.edge)
		}
		chainOverlaySame(t, "provides", render(o.rows()), render(want))
	})
	t.Run(label+"/param_index", func(t *testing.T) {
		changed := []string{builderRepoPrefix + "/app/caller.go"}
		callees := make(map[string]struct{})
		for _, id := range ids {
			if strings.Contains(id, "::") && !strings.Contains(id, "#") {
				callees[id] = struct{}{}
			}
		}
		index := newEditDeltaParamIndexOver(p.cached, p.key, changed)
		render := func(m map[string]map[int]string) []string {
			var out []string
			for id, table := range m {
				for pos, param := range table {
					out = append(out, fmt.Sprintf("%s#%d=%s", id, pos, param))
				}
			}
			sort.Strings(out)
			return out
		}
		chainOverlaySame(t, "parameters", render(index.index(p.cached, callees)), render(paramPositionIndexByFile(p.plain, callees)))
	})
}

// Prior fingerprints of a path the chain speaks for are neither read from
// nor kept in the cache kept for the stack below the chain: its prior rows
// are the chain's, which a stack-wide answer would not describe.
func TestChainOverlayPriorFingerprintsSkipTheChainsPaths(t *testing.T) {
	var asked []string
	unkept := func(rel string) bool {
		asked = append(asked, rel)
		return rel == "core/core.go"
	}
	resetEditDeltaPriorFingerprints()
	t.Cleanup(resetEditDeltaPriorFingerprints)
	storePriorFingerprints("stack\x00core/core.go", priorFingerprints{})
	storePriorFingerprints("stack\x00core/extra.go", priorFingerprints{})
	idx := &Indexer{}
	root := t.TempDir()
	source := headPriorFingerprintsExcept(idx, root, "HEAD", "stack", unkept)
	// A kept path is answered from the cache (an empty refusal here): no read.
	if _, _, ok := source(filepath.Join(root, "core/extra.go"), nil); ok {
		t.Fatal("the kept refusal was not served")
	}
	// The chain's path is read afresh: the tree has no git, so the read
	// fails, and nothing is kept for it.
	if _, _, ok := source(filepath.Join(root, "core/core.go"), nil); ok {
		t.Fatal("the chain's path was served")
	}
	if fp, ok := cachedPriorFingerprints("stack\x00core/core.go"); !ok || fp.derived.complete() {
		t.Fatal("the stack's kept entry for the chain's path changed")
	}
	if strings.Join(asked, ",") != "core/extra.go,core/core.go" {
		t.Fatalf("asked %v", asked)
	}
}

// The contract registry is not composed per read: over a chain it stays keyed
// by the whole stack, so a registry loaded at one depth is never served at
// another.
func TestChainOverlayContractRegistryKeepsTheWholeStackKey(t *testing.T) {
	base := commitLayerBase{stack: []int64{11, 21, 22}, chainDepth: 2}
	shallower := commitLayerBase{stack: []int64{11, 21}, chainDepth: 1}
	store := &struct{}{}
	whole, ok := editDeltaRegistryKey(base, store, "repo", "w", "p")
	if !ok {
		t.Fatal("no registry key")
	}
	other, _ := editDeltaRegistryKey(shallower, store, "repo", "w", "p")
	if whole == other {
		t.Fatal("two chain depths share the registry key")
	}
	commit, _ := editDeltaContractCacheKey(base, store, "repo", "w", "p")
	commit2, _ := editDeltaContractCacheKey(shallower, store, "repo", "w", "p")
	if commit != commit2 || commit == whole {
		t.Fatalf("the per-stack key moved with the chain: %q %q (whole %q)", commit, commit2, whole)
	}
}

// A chain layer is kept per generation and correction epoch: a correction of
// a chain generation is served at once, and a generation that stops being
// servable leaves the keeper through the materializer's forget observer.
func TestChainOverlayKeptLayersFollowCorrectionsAndForgets(t *testing.T) {
	r := newChainOverlayRun(t)
	deepest := r.depths[len(r.depths)-1]
	p := r.pair(deepest)
	source := "rewarm_test.go::Source"
	read := func(p chainOverlayPair) ([]string, []string) {
		cached, _ := graph.RecordedEdgesOf(p.cached)
		plain, _ := graph.RecordedEdgesOf(p.plain.View())
		return append(renderChainEdgeMap(p.cached.GetOutEdgesByNodeIDs([]string{source})), renderChainEdges(cached.RecordedEdgesAt([]string{"rewarm_test.go"}))...),
			append(renderChainEdgeMap(p.plain.GetOutEdgesByNodeIDs([]string{source})), renderChainEdges(plain.RecordedEdgesAt([]string{"rewarm_test.go"}))...)
	}
	// Warm the keeper over every chain layer.
	r.checkReads(t, "warm", deepest, p, chainOverlayKeys(r.paths), chainOverlayKeys(r.ids), chainOverlayKeys(r.names))
	chain := deepest.base.chainGenerations()
	corrected := chain[len(chain)/2]
	keeper := editDeltaChainLayerRowsFor(r.f.store)
	if !keeper.Holds(corrected, r.f.store.GenerationCorrectionEpoch(corrected)) {
		t.Fatalf("generation %d was not kept", corrected)
	}
	correctOneRow(t, r.f.store, corrected)
	r.hold("corrected")
	after := r.depths[len(r.depths)-1]
	cached, plain := read(r.pair(after))
	if len(plain) == 0 {
		t.Fatal("fixture precondition: the correction added no row")
	}
	chainOverlaySame(t, "the corrected layer", cached, plain)

	// The coordinator's base materializer forgets a generation it finds no
	// longer servable; the keeper drops it with it.
	epoch := r.f.store.GenerationCorrectionEpoch(corrected)
	if !keeper.Holds(corrected, epoch) {
		t.Fatalf("generation %d at epoch %d was not kept", corrected, epoch)
	}
	r.c.baseViewMaterializer().ForgetGeneration(corrected)
	if keeper.Holds(corrected, epoch) {
		t.Fatal("a forgotten generation stayed kept")
	}
}

// The keeper holds at most its bound of layers and rows, least recently used
// out first, and never keeps a layer past the per-layer bound.
func TestChainLayerRowsBound(t *testing.T) {
	r := newChainOverlayRun(t)
	deepest := r.depths[len(r.depths)-1]
	chain := deepest.base.chainGenerations()
	// Room for two layers: a read over the whole chain keeps the last two.
	keeper := graph.NewChainLayerRows(2, 1_000_000)
	editDeltaChainLayers.Lock()
	editDeltaChainLayers.byStore = map[any]*graph.ChainLayerRows{r.f.store: keeper}
	editDeltaChainLayers.Unlock()
	p := r.pair(deepest)
	p.cached.GetInEdgesByNodeIDs(chainOverlayKeys(r.ids))
	if layers, _, _, _ := keeper.Stats(); layers != 2 {
		t.Fatalf("the keeper holds %d layers, want 2", layers)
	}
	// A rows bound below one layer's size keeps none, and reads still match.
	small := graph.NewChainLayerRows(64, 1)
	editDeltaChainLayers.Lock()
	editDeltaChainLayers.byStore = map[any]*graph.ChainLayerRows{r.f.store: small}
	editDeltaChainLayers.Unlock()
	p = r.pair(deepest)
	chainOverlaySame(t, "unkept layers", renderChainEdgeMap(p.cached.GetInEdgesByNodeIDs(chainOverlayKeys(r.ids))),
		renderChainEdgeMap(p.plain.GetInEdgesByNodeIDs(chainOverlayKeys(r.ids))))
	// Only a layer with no rows at all fits a bound of one row.
	if layers, rows, _, _ := small.Stats(); rows != 0 {
		t.Fatalf("a keeper bounded to one row holds %d layers, %d rows", layers, rows)
	}
	_ = chain
}
