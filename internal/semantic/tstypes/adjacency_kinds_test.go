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

const calleeSyntheticFile = "zz_hub/callees.syn"

// injectOutgoingCalls gives every callable fan outgoing calls edges (with
// meta) on its own first line, to synthetic callees no call fact names: the
// shape of a large repository's frontier members, whose bodies make many
// calls. Only a calls application reads them, and only for a caller in the
// applying file; the frontier walk loads every member of every supertype.
func injectOutgoingCalls(s graph.Store, fan int) {
	if fan <= 0 {
		return
	}
	var callables []*graph.Node
	for _, n := range s.AllNodes() {
		if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) &&
			n.FilePath != hubSyntheticFile && n.FilePath != calleeSyntheticFile {
			callables = append(callables, n)
		}
	}
	sort.Slice(callables, func(i, j int) bool { return callables[i].ID < callables[j].ID })
	callees := make([]*graph.Node, fan)
	for i := range callees {
		callees[i] = &graph.Node{
			ID: fmt.Sprintf("%s::callee%05d", calleeSyntheticFile, i), Name: fmt.Sprintf("callee%05d", i),
			Kind: graph.KindFunction, FilePath: calleeSyntheticFile, Language: "synthetic",
			StartLine: i + 1, EndLine: i + 1,
		}
	}
	var edges []*graph.Edge
	for _, from := range callables {
		for _, callee := range callees {
			edges = append(edges, &graph.Edge{From: from.ID, To: callee.ID, Kind: graph.EdgeCalls,
				FilePath: from.FilePath, Line: from.StartLine, Confidence: 0.8, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"semantic_source": "hub-lsp", "resolution": "name", "receiver": "hub"}})
		}
	}
	s.AddBatch(callees, edges)
}

// injectSiblingStubs doubles every unresolved calls / extends / implements
// stub with a sibling of the other unresolved shape (`unresolved::*.M` and
// `unresolved::M`) on the same source, line and kind, inserted after it. The
// applier claims the FIRST claimable same-line stub whose trailing name
// matches (upgradeExistingCall, applySuper), so which of the two it retargets
// — and so the resulting graph — is decided by the within-kind list order: a
// preload read that orders a kind differently from the unfiltered read (an
// ORDER BY that drops or flips the insertion tiebreak) fails parity.
func injectSiblingStubs(s graph.Store) {
	type key struct {
		from, to string
		kind     graph.EdgeKind
	}
	existing := map[key]bool{}
	var stubs []*graph.Edge
	for _, e := range s.AllEdges() {
		existing[key{e.From, e.To, e.Kind}] = true
		switch e.Kind {
		case graph.EdgeCalls, graph.EdgeExtends, graph.EdgeImplements:
			if strings.HasPrefix(e.To, "unresolved::") {
				stubs = append(stubs, e)
			}
		}
	}
	sort.Slice(stubs, func(i, j int) bool {
		a, b := stubs[i], stubs[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.To < b.To
	})
	var edges []*graph.Edge
	for _, e := range stubs {
		name := strings.TrimPrefix(e.To, "unresolved::")
		sibling := "unresolved::*." + name
		if rest, ok := strings.CutPrefix(name, "*."); ok {
			sibling = "unresolved::" + rest
		}
		if existing[key{e.From, sibling, e.Kind}] {
			continue
		}
		existing[key{e.From, sibling, e.Kind}] = true
		meta := map[string]any{"sibling_stub": true}
		for k, v := range e.Meta {
			meta[k] = v
		}
		edges = append(edges, &graph.Edge{From: e.From, To: sibling, Kind: e.Kind, FilePath: e.FilePath,
			Line: e.Line, Confidence: e.Confidence, Origin: e.Origin, Meta: meta})
	}
	s.AddBatch(nil, edges)
}

// adjacencyIdentity is the part of an edge the coherence check compares.
type adjacencyIdentity struct {
	From, To, Kind, FilePath, Origin string
	Line                             int
	Confidence                       float64
}

// byKindIdentities groups a list per kind, keeping order within each kind —
// the order the kind-filtered reads promise to preserve.
func byKindIdentities(edges []*graph.Edge, kinds []graph.EdgeKind) string {
	groups := map[graph.EdgeKind][]*graph.Edge{}
	for _, e := range edges {
		if e != nil && (kinds == nil || edgeKindIn(e.Kind, kinds)) {
			groups[e.Kind] = append(groups[e.Kind], e)
		}
	}
	names := make([]string, 0, len(groups))
	for kind := range groups {
		names = append(names, string(kind))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, kind := range names {
		fmt.Fprintf(&b, "%s%v ", kind, adjacencyIdentities(groups[graph.EdgeKind(kind)]))
	}
	return b.String()
}

func adjacencyIdentities(edges []*graph.Edge) []adjacencyIdentity {
	out := make([]adjacencyIdentity, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			out = append(out, adjacencyIdentity{e.From, e.To, string(e.Kind), e.FilePath, e.Origin, e.Line, e.Confidence})
		}
	}
	return out
}

// adjacencyCoherence is a Provider.observePreload hook asserting that every
// list a page's applier starts from — whether the pass hot cache served it or
// the store did — holds, kind by kind and in order, exactly the edges the
// store's UNFILTERED adjacency read returns right now. An earlier page that
// mutated a list without dropping its cache entry (forgetOut / forgetIn)
// leaves a stale or reordered snapshot that fails here, in either preload
// mode; so does a kind-filtered read whose within-kind order departs from
// the unfiltered one (on the incoming side no reader is order-sensitive, so
// only this check sees it).
type adjacencyCoherence struct {
	mu       sync.Mutex
	failures []string
	pages    int
}

func (c *adjacencyCoherence) observe(ap *applier) {
	compare := func(dir string, have map[string][]*graph.Edge, loaded map[string]bool, kinds []graph.EdgeKind, read func([]string) map[string][]*graph.Edge) {
		ids := make([]string, 0, len(loaded))
		for id := range loaded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fresh := read(ids)
		for _, id := range ids {
			got, want := byKindIdentities(have[id], nil), byKindIdentities(fresh[id], kinds)
			if got != want {
				c.mu.Lock()
				if len(c.failures) < 5 {
					c.failures = append(c.failures, fmt.Sprintf("page %d %s %s:\n  served %v\n  store  %v", c.pages, dir, id, got, want))
				}
				c.mu.Unlock()
			}
		}
	}
	c.mu.Lock()
	c.pages++
	c.mu.Unlock()
	compare("out", ap.outByID, ap.outLoaded, ap.outKinds, ap.g.GetOutEdgesByNodeIDs)
	compare("in", ap.inByID, ap.inLoaded, ap.inKinds, ap.g.GetInEdgesByNodeIDs)
	compare("calls", ap.callsByID, ap.callsLoaded, adjacencyCallKinds, ap.g.GetOutEdgesByNodeIDs)
}

func (c *adjacencyCoherence) check(t testing.TB, label string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.failures) > 0 {
		t.Fatalf("%s: a page started from adjacency that differs from the store (stale hot-cache entry):\n%s",
			label, strings.Join(c.failures, "\n"))
	}
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
	// A base class in the last-sorted file whose members call each other:
	// earlier calls-phase pages load its members as frontier nodes only
	// (structural kinds, cached in the pass hot cache), the last page loads
	// the same nodes again as callers and must see their outgoing calls.
	late := map[string]string{
		"src/zz_base.ts": "export class Base {\n  run(): void {\n    this.stop(1);\n    this.stop(2);\n  }\n  stop(n: number): void {\n    this.run();\n  }\n}\n",
	}
	for i := 0; i < 70; i++ {
		late[fmt.Sprintf("src/c%03d.ts", i)] = fmt.Sprintf(`import { Base } from "./zz_base";

export class L%03d extends Base {
  go(): void {
    this.run();
    this.stop(%d);
  }
}
`, i, i)
	}
	// An interface chain across pages: each file's base list names the
	// previous file's interface and a shared root, so a supers page loads
	// (and may serve from the hot cache) the lists an earlier page just
	// retargeted, and the root's incoming list grows on every page.
	chain := map[string]string{
		"w/IRoot.cs": "namespace W { public interface IRoot { void Root(); } }\n",
		"w/I000.cs":  "namespace W { public interface I000 : IRoot { void M000(); } }\n",
	}
	for i := 1; i < 100; i++ {
		chain[fmt.Sprintf("w/I%03d.cs", i)] = fmt.Sprintf(
			"namespace W { public interface I%03d : I%03d, IRoot { void M%03d(); } }\n", i, i-1, i)
	}
	return []map[string]string{cs, ts, late, chain}
}

// parityRun is one enrichment of a salted fixture.
type parityRun struct {
	sqlite bool
	fan    int
	// full selects the legacy all-kinds preload.
	full bool
	// perFile drives EnrichFile over every fixture file (one applier running
	// all four phases per file) instead of the streamed EnrichRepo pass.
	perFile bool
	// compat hides the store's optional capabilities (compatOnlyStore).
	compat bool
}

// enrichSnapshot builds the fixture on the chosen backend, salts it (hub
// adjacency of unread kinds, outgoing calls on every callable, sibling
// stubs), runs every provider and snapshots the graph. Every page is
// checked for hot-cache coherence on the way.
func enrichSnapshot(t *testing.T, label string, fixture map[string]string, providers []*Provider, run parityRun) paritySnapshot {
	t.Helper()
	var s graph.Store
	var root string
	if run.sqlite {
		st, dir := buildSQLiteFixture(t, filepath.Join(t.TempDir(), "store.sqlite"), fixture)
		t.Cleanup(func() { _ = st.Close() })
		s, root = st, dir
	} else {
		g, dir := buildFixture(t, fixture)
		s, root = g, dir
	}
	injectHubAdjacency(s, run.fan)
	injectOutgoingCalls(s, run.fan/4)
	injectSiblingStubs(s)
	if run.compat {
		s = compatOnlyStore{s}
	}
	files := make([]string, 0, len(fixture))
	for rel := range fixture {
		files = append(files, rel)
	}
	sort.Strings(files)
	coherence := &adjacencyCoherence{}
	var results []string
	for _, base := range providers {
		// Drop the toolchain-fallback gate (the Go spec no-ops whenever
		// `go` is on PATH, as it always is under go test).
		spec := *base.spec
		spec.Suppressed = nil
		p := NewProvider(&spec, zap.NewNop())
		p.fullAdjacency = run.full
		p.observePreload = coherence.observe
		if run.perFile {
			for _, rel := range files {
				res, err := p.EnrichFile(s, root, rel)
				if err != nil {
					t.Fatalf("%s: enrich %s %s: %v", label, p.Name(), rel, err)
				}
				results = append(results, takeParitySnapshot(t, s, res).Result)
			}
			continue
		}
		res, err := p.EnrichRepo(s, "", root)
		if err != nil {
			t.Fatalf("%s: enrich %s: %v", label, p.Name(), err)
		}
		results = append(results, takeParitySnapshot(t, s, res).Result)
	}
	coherence.check(t, fmt.Sprintf("%s (full=%v)", label, run.full))
	snap := takeParitySnapshot(t, s, nil)
	snap.Result = strings.Join(results, "\n")
	return snap
}

func runParity(t *testing.T, label string, fixture map[string]string, providers []*Provider, sqlite bool, fan int) {
	t.Helper()
	legacy := enrichSnapshot(t, label, fixture, providers, parityRun{sqlite: sqlite, fan: fan, full: true})
	filtered := enrichSnapshot(t, label, fixture, providers, parityRun{sqlite: sqlite, fan: fan})
	diffParity(t, label, legacy, filtered)
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
// entries mutated by an earlier page, and run four phases whose appliers
// reload the same nodes: a page-file caller of the calls phase was a seed or
// frontier node of every earlier phase (and of earlier calls pages, whose
// cache entries hold no calls). A tiny budget forces generation rotation, so
// cache residency differs between the modes. Every run — legacy and
// filtered, on every budget — must equal the filtered run with the hot cache
// off, and every page must start from adjacency equal to the store's.
func TestAdjacencyKindFilterParityMultiPage(t *testing.T) {
	for i, fixture := range multiPageFixtures() {
		providers := fixtureProviders(t, fixture)
		for _, sqlite := range []bool{false, true} {
			backend := map[bool]string{false: "memory", true: "sqlite"}[sqlite]
			t.Run(fmt.Sprintf("%s/fixture%d", backend, i), func(t *testing.T) {
				var reference paritySnapshot
				t.Run("cache=off", func(t *testing.T) {
					t.Setenv("GORTEX_TSTYPES_HOTCACHE", "off")
					reference = enrichSnapshot(t, "cache off", fixture, providers, parityRun{sqlite: sqlite, fan: 60})
				})
				for _, budget := range []string{"", "1"} {
					t.Run("budget="+budget, func(t *testing.T) {
						if budget != "" {
							t.Setenv("GORTEX_TSTYPES_HOTCACHE_MB", budget)
						}
						for _, full := range []bool{true, false} {
							label := fmt.Sprintf("%s multipage %d budget=%q full=%v", backend, i, budget, full)
							got := enrichSnapshot(t, label, fixture, providers, parityRun{sqlite: sqlite, fan: 60, full: full})
							diffParity(t, label+" vs cache off", reference, got)
						}
					})
				}
			})
		}
	}
}

// The single-file pass runs all four phases on one applier: adjacency the
// supers phase loaded (structural kinds only) is what the calls phase reads,
// with the file's callers' calls loaded beside it.
func TestAdjacencyKindFilterParityEnrichFile(t *testing.T) {
	fixtures := append(multiPageFixtures(), goldenFixtureFiles())
	for i, fixture := range fixtures {
		providers := fixtureProviders(t, fixture)
		for _, sqlite := range []bool{false, true} {
			label := fmt.Sprintf("enrichfile fixture%d sqlite=%v", i, sqlite)
			legacy := enrichSnapshot(t, label, fixture, providers, parityRun{sqlite: sqlite, fan: 20, full: true, perFile: true})
			filtered := enrichSnapshot(t, label, fixture, providers, parityRun{sqlite: sqlite, fan: 20, perFile: true})
			diffParity(t, label, legacy, filtered)
		}
	}
}

// adjacencyLoadStore counts what the preload pulls through either read path,
// in edges and an approximation of decoded bytes, and the caller-calls reads
// (adjacencyCallKinds) by query and by requested node ID.
type adjacencyLoadStore struct {
	graph.Store
	edges, bytes int
	kinds        map[graph.EdgeKind]int
	callQueries  int
	callIDs      map[string]int
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
	if slicesEqualKinds(kinds, adjacencyCallKinds) {
		s.callQueries++
		if s.callIDs == nil {
			s.callIDs = map[string]int{}
		}
		for _, id := range ids {
			s.callIDs[id]++
		}
	}
	return s.count(graph.OutEdgesByNodeIDsOfKinds(s.Store, ids, kinds))
}

func (s *adjacencyLoadStore) GetInEdgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	return s.count(graph.InEdgesByNodeIDsOfKinds(s.Store, ids, kinds))
}

// hubPreloadFixture is the C# golden fixture with every symbol turned into a
// hub of fan unread edges and, when outCalls > 0, every callable making
// outCalls outgoing calls.
func hubPreloadFixture(t testing.TB, fan, outCalls int) (graph.Store, string) {
	t.Helper()
	st, root := buildSQLiteFixture(t, filepath.Join(t.TempDir(), "store.sqlite"), goldenFixtureFiles())
	t.Cleanup(func() { _ = st.Close() })
	injectHubAdjacency(st, fan)
	injectOutgoingCalls(st, outCalls)
	return st, root
}

// phaseLoads is the calls rows the preload read during one page.
type phaseLoads struct {
	class factClass
	calls int
}

func preloadAllPhases(t testing.TB, s graph.Store, root string, full bool) (pages []factPageStats, loads *adjacencyLoadStore) {
	pages, loads, _ = preloadAllPhasesByPage(t, s, root, full)
	return pages, loads
}

func preloadAllPhasesByPage(t testing.TB, s graph.Store, root string, full bool) (pages []factPageStats, loads *adjacencyLoadStore, perPage []phaseLoads) {
	t.Helper()
	loads = &adjacencyLoadStore{Store: s, kinds: map[graph.EdgeKind]int{}}
	p := NewProvider(CSharpSpec(), zap.NewNop())
	p.fullAdjacency = full
	seen := 0
	p.observePage = func(stats factPageStats) {
		pages = append(pages, stats)
		perPage = append(perPage, phaseLoads{class: stats.Class, calls: loads.kinds[graph.EdgeCalls] - seen})
		seen = loads.kinds[graph.EdgeCalls]
	}
	if _, err := p.EnrichRepo(loads, "", root); err != nil {
		t.Fatal(err)
	}
	return pages, loads, perPage
}

// The hub fixture's adjacency preload must load only the kinds the applier
// reads: no references/typed_as/reads/writes, and no incoming calls.
func TestAdjacencyPreloadSkipsUnreadHubKinds(t *testing.T) {
	const fan = 2000
	s, root := hubPreloadFixture(t, fan, 0)
	legacyPages, legacy := preloadAllPhases(t, s, root, true)

	s2, root2 := hubPreloadFixture(t, fan, 0)
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

// Outgoing calls are loaded in the calls phase only, and only for the nodes
// of call-bearing files: frontier members — every member of every supertype,
// each with a body full of calls — never load theirs, and the supers, metas
// and aliases phases load none at all.
func TestAdjacencyPreloadLoadsCallsOnlyForCallers(t *testing.T) {
	const fan, outCalls = 200, 400
	s, root := hubPreloadFixture(t, fan, outCalls)
	_, legacy, legacyPages := preloadAllPhasesByPage(t, s, root, true)
	s2, root2 := hubPreloadFixture(t, fan, outCalls)
	_, filtered, pages := preloadAllPhasesByPage(t, s2, root2, false)

	var legacyEarly, early, calls int
	for _, page := range legacyPages {
		if page.class != classCalls {
			legacyEarly += page.calls
		}
	}
	for _, page := range pages {
		if page.class != classCalls {
			early += page.calls
		} else {
			calls += page.calls
		}
	}
	t.Logf("calls rows: legacy=%d (%d outside the calls phase) filtered=%d (%d outside)",
		legacy.kinds[graph.EdgeCalls], legacyEarly, filtered.kinds[graph.EdgeCalls], early)
	if legacyEarly == 0 {
		t.Fatal("fixture does not exercise frontier calls: the legacy preload loaded none outside the calls phase")
	}
	if early != 0 {
		t.Fatalf("supers/metas/aliases pages loaded %d outgoing calls rows, want 0", early)
	}

	// Exactly the callers of call-bearing files: every non-file node of a
	// file with call facts, whose outgoing calls the store holds.
	callBearing := map[string]bool{}
	for rel := range goldenFixtureFiles() {
		for _, n := range s2.GetFileNodes(rel) {
			if n.Kind != graph.KindFile {
				continue
			}
			facts, err := analyzeFile(CSharpSpec(), fileRef{node: n, absPath: filepath.Join(root2, filepath.FromSlash(rel))})
			if err != nil {
				t.Fatal(err)
			}
			if facts != nil && len(facts.calls) > 0 {
				callBearing[rel] = true
			}
		}
	}
	if len(callBearing) == 0 || len(callBearing) == len(goldenFixtureFiles()) {
		t.Fatalf("call-bearing files %v: want a strict, non-empty subset of the fixture", callBearing)
	}
	want := 0
	for _, n := range s2.AllNodes() {
		if n.Kind == graph.KindFile || !callBearing[n.FilePath] {
			continue
		}
		for _, e := range s2.GetOutEdges(n.ID) {
			if e.Kind == graph.EdgeCalls {
				want++
			}
		}
	}
	if calls != want {
		t.Fatalf("calls phase loaded %d outgoing calls rows, want %d (the call-bearing files' callers)", calls, want)
	}
}

// A hot-cache out entry holds the structural kinds only. A later page that
// reads the same node as a caller must still get its outgoing calls.
func TestCallsNeverServedFromStructuralHotEntry(t *testing.T) {
	g := graph.New()
	caller := &graph.Node{ID: "repo/a.cs::A.M", Kind: graph.KindMethod, Name: "M", FilePath: "repo/a.cs", Language: "csharp", StartLine: 1, EndLine: 3}
	owner := &graph.Node{ID: "repo/a.cs::A", Kind: graph.KindType, Name: "A", FilePath: "repo/a.cs", Language: "csharp", StartLine: 1, EndLine: 4}
	g.AddBatch([]*graph.Node{caller, owner}, []*graph.Edge{
		{From: caller.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: "repo/a.cs", Line: 1},
		{From: caller.ID, To: "unresolved::*.Run", Kind: graph.EdgeCalls, FilePath: "repo/a.cs", Line: 2},
	})
	hot := newApplyHotCache(defaultApplyHotCacheBytes)
	frontier := NewProvider(CSharpSpec(), zap.NewNop()).newApplier(g).withHotCache(hot)
	frontier.loadAdjacency([]string{caller.ID})
	if cached, ok := hot.getOut(caller.ID); !ok || len(cached) != 1 || cached[0].Kind != graph.EdgeMemberOf {
		t.Fatalf("hot out entry = %v (cached=%v), want the member_of edge only", adjacencyIdentities(cached), ok)
	}

	page := NewProvider(CSharpSpec(), zap.NewNop()).newApplier(g).withHotCache(hot)
	page.rememberNode(caller)
	page.rememberNode(owner)
	page.fileLoaded["repo/a.cs"] = true
	page.loadAdjacency([]string{caller.ID, owner.ID})
	page.loadCallerCalls([]*fileFacts{{file: "repo/a.cs", calls: []callFact{{line: 2, method: "Run"}}}})
	calls := page.outCalls(caller.ID)
	if len(calls) != 1 || calls[0].To != "unresolved::*.Run" {
		t.Fatalf("caller's outgoing calls = %v, want the stub", adjacencyIdentities(calls))
	}
	for _, e := range page.outEdges(caller.ID) {
		if e.Kind == graph.EdgeCalls {
			t.Fatalf("structural out list holds %s", e.Kind)
		}
	}
	// A loaded node missed by the batch still reads its calls.
	lazy := NewProvider(CSharpSpec(), zap.NewNop()).newApplier(g).withHotCache(hot)
	lazy.loadAdjacency([]string{caller.ID})
	if got := lazy.outCalls(caller.ID); len(got) != 1 {
		t.Fatalf("lazy calls read = %v, want the stub", adjacencyIdentities(got))
	}
}

// BenchmarkHubAdjacencyPreload measures one full enrich pass over the hub
// fixture on SQLite, legacy all-kinds preload versus the kind-filtered one,
// without and with an outgoing-calls fan on every callable.
func BenchmarkHubAdjacencyPreload(b *testing.B) {
	for _, fixture := range []struct {
		name     string
		outCalls int
	}{{"hub", 0}, {"hub_out_calls", 500}} {
		for _, mode := range []struct {
			name string
			full bool
		}{{"legacy_all_kinds", true}, {"kind_filtered", false}} {
			b.Run(fixture.name+"/"+mode.name, func(b *testing.B) {
				s, root := hubPreloadFixture(b, 2000, fixture.outCalls)
				b.ReportAllocs()
				b.ResetTimer()
				var rows, calls int
				for i := 0; i < b.N; i++ {
					_, loads := preloadAllPhases(b, s, root, mode.full)
					rows += loads.edges
					calls += loads.kinds[graph.EdgeCalls]
				}
				b.ReportMetric(float64(rows)/float64(b.N), "adj_rows/op")
				b.ReportMetric(float64(calls)/float64(b.N), "calls_rows/op")
			})
		}
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
				allowed = edgeKindIn(kind, adjacencyOutKinds(nil)) || edgeKindIn(kind, adjacencyInKinds) ||
					edgeKindIn(kind, adjacencyCallKinds)
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
		for _, kind := range append(append([]graph.EdgeKind{graph.EdgeMemberOf}, superEdgeKinds...), p.spec.inheritEdgeKinds()...) {
			if !edgeKindIn(kind, out) {
				t.Errorf("%s: outgoing preload kinds %v miss %s", p.Name(), out, kind)
			}
		}
		for _, kind := range adjacencyCallKinds {
			// Calls are loaded for callers only, never with the frontier.
			if edgeKindIn(kind, out) {
				t.Errorf("%s: outgoing frontier kinds %v load %s", p.Name(), out, kind)
			}
		}
		ap := p.newApplier(graph.New())
		if !slicesEqualKinds(ap.outKinds, out) || !slicesEqualKinds(ap.inKinds, adjacencyInKinds) || !ap.splitCalls {
			t.Errorf("%s: applier kinds out=%v in=%v splitCalls=%v", p.Name(), ap.outKinds, ap.inKinds, ap.splitCalls)
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
