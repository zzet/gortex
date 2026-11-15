package tstypes

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	sitterparser "github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

// The adjacency preload loads only the edge kinds the applier reads. These
// tests pin that the restriction is invisible: every fixture in this package,
// salted with hub nodes carrying thousands of edges of kinds the applier never
// reads, must produce exactly the graph the legacy all-kinds preload produces
// (Provider.fullAdjacency), on both backends.

const hubSyntheticFile = "zz_hub/callers.syn"

// injectHubAdjacency gives every symbol node fan incoming references,
// calls and typed_as edges (with meta) from synthetic callers, and every
// callable fan/4 outgoing references/reads/writes — the shape of a large C#
// repository's hub types, none of it read by the applier. Deterministic, so
// two stores built from one fixture receive identical rows.
func injectHubAdjacency(s graph.Store, fan int) {
	var targets []*graph.Node
	for _, n := range s.AllNodes() {
		switch n.Kind {
		case graph.KindType, graph.KindInterface, graph.KindMethod, graph.KindFunction,
			graph.KindField, graph.KindParam, graph.KindVariable, graph.KindPackage:
			if n.FilePath != hubSyntheticFile {
				targets = append(targets, n)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	callers := make([]*graph.Node, fan)
	for i := range callers {
		callers[i] = &graph.Node{
			ID: fmt.Sprintf("%s::caller%05d", hubSyntheticFile, i), Name: fmt.Sprintf("caller%05d", i),
			Kind: graph.KindFunction, FilePath: hubSyntheticFile, Language: "synthetic",
			StartLine: i + 1, EndLine: i + 1,
		}
	}
	var edges []*graph.Edge
	for _, target := range targets {
		for i, caller := range callers {
			edges = append(edges,
				&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: hubSyntheticFile, Line: i + 1,
					Confidence: 0.9, Origin: graph.OriginASTInferred, Meta: map[string]any{"ref_context": "type_position", "receiver": "hub"}},
				&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: hubSyntheticFile, Line: i + 1,
					Confidence: 0.8, Origin: graph.OriginASTInferred, Meta: map[string]any{"semantic_source": "hub-lsp", "resolution": "name"}},
				&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeTypedAs, FilePath: hubSyntheticFile, Line: i + 1},
			)
		}
		if target.Kind != graph.KindMethod && target.Kind != graph.KindFunction {
			continue
		}
		for i := 0; i < fan/4; i++ {
			to := callers[i].ID
			for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeReads, graph.EdgeWrites} {
				edges = append(edges, &graph.Edge{From: target.ID, To: to, Kind: kind, FilePath: target.FilePath,
					Line: target.StartLine, Meta: map[string]any{"access": string(kind)}})
			}
		}
	}
	s.AddBatch(callers, edges)
}

type parityEdge struct {
	From, To, Kind, FilePath, Origin, Tier, ConfidenceLabel string
	Line                                                    int
	Confidence                                              float64
	CrossRepo                                               bool
	Meta                                                    string
}

type paritySnapshot struct {
	Edges  []parityEdge
	Nodes  []string
	Result string
}

func takeParitySnapshot(t testing.TB, s graph.Store, res *semantic.EnrichResult) paritySnapshot {
	t.Helper()
	var snap paritySnapshot
	for _, e := range s.AllEdges() {
		meta, err := json.Marshal(e.Meta)
		if err != nil {
			t.Fatalf("marshal edge meta: %v", err)
		}
		snap.Edges = append(snap.Edges, parityEdge{
			From: e.From, To: e.To, Kind: string(e.Kind), FilePath: e.FilePath, Origin: e.Origin,
			Tier: e.Tier, ConfidenceLabel: e.ConfidenceLabel, Line: e.Line, Confidence: e.Confidence,
			CrossRepo: e.CrossRepo, Meta: string(meta),
		})
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		return fmt.Sprint(snap.Edges[i]) < fmt.Sprint(snap.Edges[j])
	})
	for _, n := range s.AllNodes() {
		meta, err := json.Marshal(n.Meta)
		if err != nil {
			t.Fatalf("marshal node meta: %v", err)
		}
		snap.Nodes = append(snap.Nodes, n.ID+" "+string(meta))
	}
	sort.Strings(snap.Nodes)
	if res != nil {
		r := *res
		r.DurationMs, r.LockWaitMs = 0, 0
		blob, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		snap.Result = string(blob)
	}
	return snap
}

func diffParity(t testing.TB, label string, legacy, filtered paritySnapshot) {
	t.Helper()
	if legacy.Result != filtered.Result {
		t.Fatalf("%s: enrich result differs\nlegacy:   %s\nfiltered: %s", label, legacy.Result, filtered.Result)
	}
	if len(legacy.Edges) != len(filtered.Edges) {
		t.Fatalf("%s: edge count legacy=%d filtered=%d", label, len(legacy.Edges), len(filtered.Edges))
	}
	for i := range legacy.Edges {
		if legacy.Edges[i] != filtered.Edges[i] {
			t.Fatalf("%s: edge %d differs\nlegacy:   %+v\nfiltered: %+v", label, i, legacy.Edges[i], filtered.Edges[i])
		}
	}
	for i := range legacy.Nodes {
		if legacy.Nodes[i] != filtered.Nodes[i] {
			t.Fatalf("%s: node %d differs\nlegacy:   %s\nfiltered: %s", label, i, legacy.Nodes[i], filtered.Nodes[i])
		}
	}
}

// harvestPackageFixtures collects every literal map[string]string fixture in
// this package's tests (values may be string literals or package string
// constants), so the parity check covers the shapes every per-language test
// already exercises without restating them.
func harvestPackageFixtures(t *testing.T) []map[string]string {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	consts := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								consts[name.Name] = v
							}
						}
					}
				}
			}
		}
	}
	var locals map[string]string
	str := func(expr ast.Expr) (string, bool) {
		switch v := expr.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				return s, err == nil
			}
		case *ast.Ident:
			if s, ok := locals[v.Name]; ok {
				return s, true
			}
			s, ok := consts[v.Name]
			return s, ok
		}
		return "", false
	}
	// collectLocals records a function's `name := "literal"` sources (the
	// Go spec tests declare their fixture bodies that way).
	collectLocals := func(fn *ast.FuncDecl) {
		locals = map[string]string{}
		ast.Inspect(fn, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			id, okID := as.Lhs[0].(*ast.Ident)
			lit, okLit := as.Rhs[0].(*ast.BasicLit)
			if okID && okLit && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					locals[id.Name] = v
				}
			}
			return true
		})
	}
	seen := map[[32]byte]bool{}
	var out []map[string]string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				collectLocals(fn)
				return true
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok || fmt.Sprint(mt.Key) != "string" || fmt.Sprint(mt.Value) != "string" || len(lit.Elts) == 0 {
				return true
			}
			fixture := map[string]string{}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				k, okK := str(kv.Key)
				v, okV := str(kv.Value)
				if !okK || !okV || !strings.Contains(k, ".") {
					return true
				}
				fixture[k] = v
			}
			keys := make([]string, 0, len(fixture))
			for k := range fixture {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			h := sha256.New()
			for _, k := range keys {
				fmt.Fprintf(h, "%q=%q;", k, fixture[k])
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			if !seen[sum] {
				seen[sum] = true
				out = append(out, fixture)
			}
			return true
		})
	}
	return out
}

// fixtureRegistry is built once: registering every extractor costs more
// than running a small fixture.
var fixtureRegistry = sync.OnceValue(func() *sitterparser.Registry {
	reg := sitterparser.NewRegistry()
	languages.RegisterAll(reg)
	return reg
})

// fixtureProviders returns the default providers serving any of the
// fixture's languages.
func fixtureProviders(t *testing.T, fixture map[string]string) []*Provider {
	t.Helper()
	langs := map[string]bool{}
	for rel := range fixture {
		if lang, ok := fixtureRegistry().DetectLanguage(rel); ok {
			langs[lang] = true
		}
	}
	var out []*Provider
	for _, p := range DefaultProviders(zap.NewNop()) {
		for _, lang := range p.Languages() {
			if langs[lang] {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func detectable(fixture map[string]string) bool {
	for rel := range fixture {
		if _, ok := fixtureRegistry().DetectLanguage(rel); !ok {
			return false
		}
	}
	return true
}

// multiPageFixtures spans several 32-file pages with cross-page adjacency
// mutations: a C# partial class whose base list and member calls live in
// different pages, and a TypeScript hierarchy whose subclasses call
// inherited methods twice on different lines.
func multiPageFixtures() []map[string]string {
	cs := map[string]string{
		"w/Base.cs":  "namespace W { public class WidgetBase { public void Render() { } public void Paint(int x) { } } public interface IShape { void Area(); } }\n",
		"w/Big00.cs": "namespace W { public partial class Big : WidgetBase, IShape { public void Area() { } } }\n",
	}
	for i := 1; i < 75; i++ {
		cs[fmt.Sprintf("w/Big%02d.cs", i)] = fmt.Sprintf(`namespace W {
    public partial class Big {
        public void M%02d() {
            this.Render();
            this.Paint(1);
            this.Render();
            var b = new Big();
            b.Area();
        }
    }
}
`, i)
	}
	ts := map[string]string{
		"src/base.ts": "export class Base {\n  run(): void {}\n  stop(n: number): void {}\n}\n",
	}
	for i := 0; i < 70; i++ {
		ts[fmt.Sprintf("src/c%03d.ts", i)] = fmt.Sprintf(`import { Base } from "./base";

export class C%03d extends Base {
  go(): void {
    this.run();
    this.stop(1);
    this.run();
  }
}
`, i)
	}
	return []map[string]string{cs, ts}
}

func runParity(t *testing.T, label string, fixture map[string]string, providers []*Provider, sqlite bool, fan int) {
	t.Helper()
	enrich := func(full bool) paritySnapshot {
		var s graph.Store
		var root string
		if sqlite {
			st, dir := buildSQLiteFixture(t, filepath.Join(t.TempDir(), "store.sqlite"), fixture)
			t.Cleanup(func() { _ = st.Close() })
			s, root = st, dir
		} else {
			g, dir := buildFixture(t, fixture)
			s, root = g, dir
		}
		injectHubAdjacency(s, fan)
		var results []string
		for _, base := range providers {
			// Drop the toolchain-fallback gate (the Go spec no-ops whenever
			// `go` is on PATH, as it always is under go test).
			spec := *base.spec
			spec.Suppressed = nil
			p := NewProvider(&spec, zap.NewNop())
			p.fullAdjacency = full
			res, err := p.EnrichRepo(s, "", root)
			if err != nil {
				t.Fatalf("%s: enrich %s: %v", label, p.Name(), err)
			}
			results = append(results, takeParitySnapshot(t, s, res).Result)
		}
		snap := takeParitySnapshot(t, s, nil)
		snap.Result = strings.Join(results, "\n")
		return snap
	}
	diffParity(t, label, enrich(true), enrich(false))
}

type parityCase struct {
	index     int
	fixture   map[string]string
	providers []*Provider
}

// parityCorpus selects every harvested fixture some default provider serves,
// and fails when a default provider is left unexercised. The whole corpus is
// deliberate: a sampled one missed a dropped param_of kind (overload arity
// disambiguation lives in a few Java/Kotlin/PHP fixtures only).
func parityCorpus(t *testing.T) []parityCase {
	t.Helper()
	fixtures := harvestPackageFixtures(t)
	if len(fixtures) < 100 {
		t.Fatalf("harvested %d fixtures, want the package's per-language corpus (>=100)", len(fixtures))
	}
	used := map[string]int{}
	var cases []parityCase
	for i, fixture := range fixtures {
		if !detectable(fixture) {
			continue
		}
		providers := fixtureProviders(t, fixture)
		for _, p := range providers {
			used[p.Name()]++
		}
		if len(providers) > 0 {
			cases = append(cases, parityCase{index: i, fixture: fixture, providers: providers})
		}
	}
	for _, p := range DefaultProviders(zap.NewNop()) {
		if used[p.Name()] == 0 {
			t.Errorf("no parity fixture exercised %s", p.Name())
		}
	}
	t.Logf("parity fixtures per provider: %v", used)
	return cases
}

func TestAdjacencyKindFilterParityAcrossPackageFixtures(t *testing.T) {
	for _, tc := range parityCorpus(t) {
		t.Run(fmt.Sprintf("fixture%03d", tc.index), func(t *testing.T) {
			t.Parallel()
			runParity(t, "memory", tc.fixture, tc.providers, false, 8)
		})
	}
}

// The SQLite backend is where the filter lives in SQL and where the row order
// is a plan property, so every engine runs there too.
func TestAdjacencyKindFilterParitySQLite(t *testing.T) {
	for _, tc := range parityCorpus(t) {
		t.Run(fmt.Sprintf("fixture%03d", tc.index), func(t *testing.T) {
			t.Parallel()
			runParity(t, "sqlite", tc.fixture, tc.providers, true, 20)
		})
	}
}

// Multi-page passes share adjacency through the pass hot cache, including
// entries mutated by an earlier page. A tiny budget forces generation
// rotation, so cache residency differs between the two modes; the result must
// not.
func TestAdjacencyKindFilterParityMultiPage(t *testing.T) {
	for _, budget := range []string{"", "1"} {
		t.Run("budget="+budget, func(t *testing.T) {
			if budget != "" {
				t.Setenv("GORTEX_TSTYPES_HOTCACHE_MB", budget)
			}
			for i, fixture := range multiPageFixtures() {
				providers := fixtureProviders(t, fixture)
				runParity(t, fmt.Sprintf("memory multipage %d", i), fixture, providers, false, 60)
				runParity(t, fmt.Sprintf("sqlite multipage %d", i), fixture, providers, true, 60)
			}
		})
	}
}

// adjacencyLoadStore counts what the preload pulls through either read path,
// in edges and an approximation of decoded bytes.
type adjacencyLoadStore struct {
	graph.Store
	edges, bytes int
	kinds        map[graph.EdgeKind]int
}

func (s *adjacencyLoadStore) count(m map[string][]*graph.Edge) map[string][]*graph.Edge {
	for _, edges := range m {
		for _, e := range edges {
			s.edges++
			s.kinds[e.Kind]++
			s.bytes += len(e.From) + len(e.To) + len(e.FilePath) + len(e.Kind) + len(e.Origin) + len(e.ConfidenceLabel)
			for k, v := range e.Meta {
				s.bytes += len(k) + len(fmt.Sprint(v))
			}
		}
	}
	return m
}

func (s *adjacencyLoadStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.count(s.Store.GetOutEdgesByNodeIDs(ids))
}

func (s *adjacencyLoadStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.count(s.Store.GetInEdgesByNodeIDs(ids))
}

func (s *adjacencyLoadStore) GetOutEdgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	return s.count(graph.OutEdgesByNodeIDsOfKinds(s.Store, ids, kinds))
}

func (s *adjacencyLoadStore) GetInEdgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	return s.count(graph.InEdgesByNodeIDsOfKinds(s.Store, ids, kinds))
}

// hubPreloadFixture is the C# golden fixture with every symbol turned into a
// hub of fan unread edges.
func hubPreloadFixture(t testing.TB, fan int) (graph.Store, string) {
	t.Helper()
	st, root := buildSQLiteFixture(t, filepath.Join(t.TempDir(), "store.sqlite"), goldenFixtureFiles())
	t.Cleanup(func() { _ = st.Close() })
	injectHubAdjacency(st, fan)
	return st, root
}

func preloadAllPhases(t testing.TB, s graph.Store, root string, full bool) (pages []factPageStats, loads *adjacencyLoadStore) {
	t.Helper()
	loads = &adjacencyLoadStore{Store: s, kinds: map[graph.EdgeKind]int{}}
	p := NewProvider(CSharpSpec(), zap.NewNop())
	p.fullAdjacency = full
	p.observePage = func(stats factPageStats) { pages = append(pages, stats) }
	if _, err := p.EnrichRepo(loads, "", root); err != nil {
		t.Fatal(err)
	}
	return pages, loads
}

// The hub fixture's adjacency preload must load only the kinds the applier
// reads: no references/typed_as/reads/writes, and no incoming calls.
func TestAdjacencyPreloadSkipsUnreadHubKinds(t *testing.T) {
	const fan = 2000
	s, root := hubPreloadFixture(t, fan)
	legacyPages, legacy := preloadAllPhases(t, s, root, true)

	s2, root2 := hubPreloadFixture(t, fan)
	pages, filtered := preloadAllPhases(t, s2, root2, false)

	sum := func(stats []factPageStats) (edges int) {
		for _, st := range stats {
			edges += st.CacheEdges
		}
		return edges
	}
	t.Logf("adjacency rows loaded: legacy=%d (%d B) filtered=%d (%d B); page-cache edges legacy=%d filtered=%d",
		legacy.edges, legacy.bytes, filtered.edges, filtered.bytes, sum(legacyPages), sum(pages))
	t.Logf("filtered kinds: %v", filtered.kinds)
	for kind := range filtered.kinds {
		switch kind {
		case graph.EdgeCalls, graph.EdgeMemberOf, graph.EdgeParamOf, graph.EdgeExtends, graph.EdgeImplements:
		default:
			t.Errorf("preload loaded %d %s edges the applier never reads", filtered.kinds[kind], kind)
		}
	}
	if filtered.edges*20 > legacy.edges {
		t.Fatalf("filtered preload loaded %d edges, want < 1/20 of legacy %d", filtered.edges, legacy.edges)
	}
	if sum(pages)*20 > sum(legacyPages) {
		t.Fatalf("filtered page caches held %d edges, want < 1/20 of legacy %d", sum(pages), sum(legacyPages))
	}
}

// BenchmarkHubAdjacencyPreload measures one full enrich pass over the hub
// fixture on SQLite, legacy all-kinds preload versus the kind-filtered one.
func BenchmarkHubAdjacencyPreload(b *testing.B) {
	for _, mode := range []struct {
		name string
		full bool
	}{{"legacy_all_kinds", true}, {"kind_filtered", false}} {
		b.Run(mode.name, func(b *testing.B) {
			s, root := hubPreloadFixture(b, 2000)
			b.ReportAllocs()
			b.ResetTimer()
			var rows int
			for i := 0; i < b.N; i++ {
				_, loads := preloadAllPhases(b, s, root, mode.full)
				rows += loads.edges
			}
			b.ReportMetric(float64(rows)/float64(b.N), "adj_rows/op")
		})
	}
}

// TestAdjacencyKindsCoverEveryReader guards the kind restriction against a
// future reader or spec: every edge-kind constant the applier source names
// must be loaded for some direction, and every kind a spec can hand the
// applier (SuperRef kinds, inheritance walk kinds) must be loaded outgoing.
func TestAdjacencyKindsCoverEveryReader(t *testing.T) {
	kindConsts := map[string]graph.EdgeKind{}
	graphFiles, err := filepath.Glob(filepath.Join("..", "..", "graph", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range graphFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "EdgeKind" {
					continue
				}
				for i, name := range vs.Names {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
						v, _ := strconv.Unquote(lit.Value)
						kindConsts[name.Name] = graph.EdgeKind(v)
					}
				}
			}
		}
	}
	if kindConsts["EdgeCalls"] != graph.EdgeCalls || kindConsts["EdgeParamOf"] != graph.EdgeParamOf {
		t.Fatalf("edge kind constants not discovered: %v", kindConsts)
	}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "graph" {
				return true
			}
			kind, ok := kindConsts[sel.Sel.Name]
			if !ok {
				return true
			}
			// engine.go holds every adjacency reader: whatever it names must
			// be loaded in some direction. Everywhere else (the specs) a kind
			// can only reach the applier as a super relation or an
			// inheritance walk kind, which must be loaded outgoing.
			allowed := edgeKindIn(kind, superEdgeKinds)
			if path == "engine.go" {
				allowed = edgeKindIn(kind, adjacencyOutKinds(nil)) || edgeKindIn(kind, adjacencyInKinds)
			}
			if !allowed {
				t.Errorf("%s names %s (%s), which the adjacency preload does not load: extend adjacencyOutKinds/adjacencyInKinds (and its parity tests) first",
					fset.Position(sel.Pos()), sel.Sel.Name, kind)
			}
			return true
		})
	}

	for _, p := range DefaultProviders(zap.NewNop()) {
		out := adjacencyOutKinds(p.spec)
		for _, kind := range append(append([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeMemberOf}, superEdgeKinds...), p.spec.inheritEdgeKinds()...) {
			if !edgeKindIn(kind, out) {
				t.Errorf("%s: outgoing preload kinds %v miss %s", p.Name(), out, kind)
			}
		}
		ap := p.newApplier(graph.New())
		if !slicesEqualKinds(ap.outKinds, out) || !slicesEqualKinds(ap.inKinds, adjacencyInKinds) {
			t.Errorf("%s: applier kinds out=%v in=%v", p.Name(), ap.outKinds, ap.inKinds)
		}
	}
}

func slicesEqualKinds(a, b []graph.EdgeKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A page that mutates a node's adjacency must not leave a pre-mutation
// snapshot in the pass hot cache: the next page sharing the cache has to see
// the store's current rows, whatever the cache's residency.
func TestHotCacheDropsAdjacencyMutatedByEarlierPage(t *testing.T) {
	g := graph.New()
	typeNode := &graph.Node{ID: "repo/a.cs::A", Kind: graph.KindType, Name: "A", FilePath: "repo/a.cs", Language: "csharp"}
	base := &graph.Node{ID: "repo/b.cs::B", Kind: graph.KindType, Name: "B", FilePath: "repo/b.cs", Language: "csharp"}
	method := &graph.Node{ID: "repo/a.cs::A.M", Kind: graph.KindMethod, Name: "M", FilePath: "repo/a.cs", Language: "csharp"}
	g.AddBatch([]*graph.Node{typeNode, base, method}, []*graph.Edge{
		{From: method.ID, To: typeNode.ID, Kind: graph.EdgeMemberOf, FilePath: "repo/a.cs", Line: 2},
		{From: typeNode.ID, To: "unresolved::Other", Kind: graph.EdgeExtends, FilePath: "repo/a.cs", Line: 1},
	})
	hot := newApplyHotCache(defaultApplyHotCacheBytes)
	page := func() *applier {
		ap := NewProvider(CSharpSpec(), zap.NewNop()).newApplier(g).withHotCache(hot)
		ap.loadAdjacency([]string{typeNode.ID, base.ID})
		return ap
	}
	extendsTargets := func(ap *applier) []string {
		var out []string
		for _, e := range ap.outEdges(typeNode.ID) {
			if e.Kind == graph.EdgeExtends {
				out = append(out, e.To)
			}
		}
		sort.Strings(out)
		return out
	}

	first := page()
	added := first.addASTEdge(typeNode.ID, base.ID, graph.EdgeExtends, "repo/a.cs", 1, strategyDirect, astConfidence)
	if got := extendsTargets(page()); len(got) != 2 || got[0] != base.ID {
		t.Fatalf("page after an add sees extends targets %v, want the added %s too", got, base.ID)
	}

	second := page()
	second.removeEdge(added)
	if got := extendsTargets(page()); len(got) != 1 || got[0] != "unresolved::Other" {
		t.Fatalf("page after a remove sees extends targets %v, want only the stub", got)
	}

	third := page()
	var stub *graph.Edge
	for _, e := range third.outEdges(typeNode.ID) {
		if e.Kind == graph.EdgeExtends {
			stub = e
		}
	}
	oldTo := stub.To
	stub.To = base.ID
	third.reindexEdge(stub, oldTo)
	if got := extendsTargets(page()); len(got) != 1 || got[0] != base.ID {
		t.Fatalf("page after a retarget sees extends targets %v, want %s", got, base.ID)
	}
	// The incoming lists hold only member_of/param_of, which no phase
	// mutates: an extends edge is never appended to the target's in-list.
	for _, e := range page().inEdges(base.ID) {
		if e.Kind == graph.EdgeExtends {
			t.Fatalf("in-list of %s holds unloaded kind %s", base.ID, e.Kind)
		}
	}
}
