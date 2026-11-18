package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The #763 fixture: a one-line method whose body calls the same-named method
// on another receiver.
const nestedReceiverTS = `export class Leaf {
  serialize() { return { value: 1 }; }
}
export class Box {
  leaf: Leaf;
  constructor(leaf: Leaf) { this.leaf = leaf; }
  serialize() { return this.leaf.serialize(); }
}
`

// The #703 fixture: two members written on one line, the second one calling
// through a field.
const splitLineCS = `class SquashFlow {
    public void Tally() { } public Box Fetch() { return _box.Get(); }
}
interface ISlabBox {
    Box Get();
}
`

func nestedReceiverNodes() []*graph.Node {
	return []*graph.Node{
		{ID: "src/models.ts::Leaf", Kind: graph.KindType, Name: "Leaf", FilePath: "src/models.ts", StartLine: 1, EndLine: 3, Language: "typescript"},
		{ID: "src/models.ts::Leaf.serialize", Kind: graph.KindMethod, Name: "serialize", FilePath: "src/models.ts", StartLine: 2, EndLine: 2, Language: "typescript"},
		{ID: "src/models.ts::Box", Kind: graph.KindType, Name: "Box", FilePath: "src/models.ts", StartLine: 4, EndLine: 8, Language: "typescript"},
		{ID: "src/models.ts::Box.serialize", Kind: graph.KindMethod, Name: "serialize", FilePath: "src/models.ts", StartLine: 7, EndLine: 7, Language: "typescript"},
	}
}

func splitLineNodes() []*graph.Node {
	return []*graph.Node{
		{ID: "Squash.cs::SquashFlow", Kind: graph.KindType, Name: "SquashFlow", FilePath: "Squash.cs", StartLine: 1, EndLine: 3, Language: "csharp"},
		// Tally precedes Fetch in the file's node list, so a line-keyed
		// first-match owner lands on Tally.
		{ID: "Squash.cs::SquashFlow.Tally", Kind: graph.KindMethod, Name: "Tally", FilePath: "Squash.cs", StartLine: 2, EndLine: 2, Language: "csharp"},
		{ID: "Squash.cs::SquashFlow.Fetch", Kind: graph.KindMethod, Name: "Fetch", FilePath: "Squash.cs", StartLine: 2, EndLine: 2, Language: "csharp"},
		{ID: "Squash.cs::ISlabBox", Kind: graph.KindInterface, Name: "ISlabBox", FilePath: "Squash.cs", StartLine: 4, EndLine: 6, Language: "csharp"},
		{ID: "Squash.cs::ISlabBox.Get", Kind: graph.KindMethod, Name: "Get", FilePath: "Squash.cs", StartLine: 5, EndLine: 5, Language: "csharp"},
	}
}

// column returns the 0-based column of the nth (0-based) whole-identifier
// occurrence of name on the 1-based line of src.
func column(t *testing.T, src string, line int, name string, nth int) int {
	t.Helper()
	lines := strings.Split(src, "\n")
	require.Greater(t, len(lines), line-1)
	text, base := lines[line-1], 0
	for {
		idx := identifierIndex(text[base:], name)
		require.GreaterOrEqual(t, idx, 0, "occurrence %d of %q on line %d", nth, name, line)
		if nth == 0 {
			return base + idx
		}
		nth--
		base += idx + len(name)
	}
}

func nameRange(line, col int, name string) Range {
	return Range{Start: Position{Line: line, Character: col}, End: Position{Line: line, Character: col + len(name)}}
}

func outEdgesTo(g graph.Store, from string, kind graph.EdgeKind) map[string]*graph.Edge {
	out := map[string]*graph.Edge{}
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == kind {
			out[e.To] = e
		}
	}
	return out
}

// #763: the definition-rebind pass positioned its request at the FIRST
// `serialize` on line 7 — Box.serialize's own declaration token — so the
// server answered Box.serialize and the edge was confirmed as a false
// self-recursion at the lsp tier. The use `this.leaf.serialize()` resolves to
// Leaf.serialize, and that is where the edge must land.
func TestLSP_DefinitionRebind_SameLineNestedReceiver(t *testing.T) {
	for _, seed := range []string{"src/models.ts::Box.serialize", "src/models.ts::Leaf.serialize"} {
		t.Run(seed, func(t *testing.T) {
			repoRoot := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, "src"), 0o755))
			uri := pathToURI(filepath.Join(repoRoot, "src", "models.ts"))
			require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "src", "models.ts"), []byte(nestedReceiverTS), 0o644))

			declCol := column(t, nestedReceiverTS, 7, "serialize", 0)
			useCol := column(t, nestedReceiverTS, 7, "serialize", 1)
			leafCol := column(t, nestedReceiverTS, 2, "serialize", 0)

			server := newFakeLSPServer()
			server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
			// No reference ties the site to the seeded target, so the
			// definition-rebind fallback adjudicates it.
			server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) { return []Location{}, nil })
			server.handle("textDocument/definition", func(raw json.RawMessage) (any, *jsonRPCError) {
				var params TextDocumentPositionParams
				_ = json.Unmarshal(raw, &params)
				if params.Position.Line != 6 {
					return nil, nil
				}
				switch params.Position.Character {
				case declCol: // the declaration token answers itself
					return []Location{{URI: uri, Range: nameRange(6, declCol, "serialize")}}, nil
				case useCol: // this.leaf.serialize() → Leaf.serialize
					return []Location{{URI: uri, Range: nameRange(1, leafCol, "serialize")}}, nil
				}
				return nil, nil
			})

			p, cleanup := providerWithFakeServer(t, server, []string{"typescript"})
			defer cleanup()
			p.caps = ServerCapabilities{ReferencesProvider: true, DefinitionProvider: true, HoverProvider: true}

			g := graph.New()
			for _, n := range nestedReceiverNodes() {
				g.AddNode(n)
			}
			g.AddEdge(&graph.Edge{
				From: "src/models.ts::Box.serialize", To: seed, Kind: graph.EdgeCalls,
				FilePath: "src/models.ts", Line: 7,
				Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
			})

			require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

			calls := outEdgesTo(g, "src/models.ts::Box.serialize", graph.EdgeCalls)
			assert.NotContains(t, calls, "src/models.ts::Box.serialize", "no false self-recursive edge")
			leaf := calls["src/models.ts::Leaf.serialize"]
			require.NotNil(t, leaf, "Box.serialize must call Leaf.serialize")
			assert.Equal(t, graph.OriginLSPResolved, leaf.Origin)
			assert.Equal(t, 7, leaf.Line)
		})
	}
}

// #703 (call hierarchy): the incoming hop for ISlabBox.Get names its caller
// Fetch, but Tally shares Fetch's line and was matched first by line alone,
// minting Tally→Get at the lsp tier.
func TestLSP_CallHierarchy_TwoMembersOnOneLine(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot := t.TempDir()
	uri := pathToURI(filepath.Join(repoRoot, "Squash.cs"))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "Squash.cs"), []byte(splitLineCS), 0o644))

	fetchCol := column(t, splitLineCS, 2, "Fetch", 0)
	callCol := column(t, splitLineCS, 2, "Get", 0)
	getItem := CallHierarchyItem{Name: "Get", URI: uri, SelectionRange: nameRange(4, column(t, splitLineCS, 5, "Get", 0), "Get")}

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareCallHierarchy", func(raw json.RawMessage) (any, *jsonRPCError) {
		var params TextDocumentPositionParams
		_ = json.Unmarshal(raw, &params)
		if params.Position.Line != 4 {
			return []CallHierarchyItem{}, nil
		}
		return []CallHierarchyItem{getItem}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{{
			From:       CallHierarchyItem{Name: "Fetch() : Box", URI: uri, SelectionRange: nameRange(1, fetchCol, "Fetch")},
			FromRanges: []Range{nameRange(1, callCol, "Get")},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"csharp"})
	defer cleanup()

	g := graph.New()
	for _, n := range splitLineNodes() {
		g.AddNode(n)
	}

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Empty(t, outEdgesTo(g, "Squash.cs::SquashFlow.Tally", graph.EdgeCalls), "Tally owns no call")
	got := outEdgesTo(g, "Squash.cs::SquashFlow.Fetch", graph.EdgeCalls)["Squash.cs::ISlabBox.Get"]
	require.NotNil(t, got, "Fetch must call ISlabBox.Get")
	assert.Equal(t, graph.OriginLSPResolved, got.Origin)
	assert.Equal(t, 2, got.Line)
}

// #703 (references-add): the reference to ISlabBox.Get sits inside Fetch; its
// column, not its line, decides the owner.
func TestLSP_ReferencesAdd_TwoMembersOnOneLine(t *testing.T) {
	repoRoot := t.TempDir()
	uri := pathToURI(filepath.Join(repoRoot, "Squash.cs"))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "Squash.cs"), []byte(splitLineCS), 0o644))
	callCol := column(t, splitLineCS, 2, "Get", 0)

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(raw json.RawMessage) (any, *jsonRPCError) {
		var params TextDocumentPositionParams
		_ = json.Unmarshal(raw, &params)
		if params.Position.Line != 4 { // only ISlabBox.Get is referenced
			return []Location{}, nil
		}
		return []Location{{URI: uri, Range: nameRange(1, callCol, "Get")}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"csharp"})
	defer cleanup()
	p.caps = ServerCapabilities{ReferencesProvider: true, HoverProvider: true}

	g := graph.New()
	for _, n := range splitLineNodes() {
		g.AddNode(n)
	}

	require.NoError(t, runEnrich(t, p, g, repoRoot, 5*time.Second))

	assert.Empty(t, outEdgesTo(g, "Squash.cs::SquashFlow.Tally", graph.EdgeCalls), "Tally owns no call")
	got := outEdgesTo(g, "Squash.cs::SquashFlow.Fetch", graph.EdgeCalls)["Squash.cs::ISlabBox.Get"]
	require.NotNil(t, got, "Fetch must call ISlabBox.Get")
	assert.Equal(t, graph.OriginLSPResolved, got.Origin)
}

func TestMatchCallableAt_SameLineMembers(t *testing.T) {
	src := []byte(splitLineCS)
	view := newLSPGraphView(splitLineNodes(), nil)
	load := func() []byte { return src }
	fetchCol := column(t, splitLineCS, 2, "Fetch", 0)
	tallyCol := column(t, splitLineCS, 2, "Tally", 0)
	callCol := column(t, splitLineCS, 2, "Get", 0)

	for _, test := range []struct {
		name string
		site lspSite
		want string
	}{
		{"line alone cannot choose", lineSite(2), ""},
		{"item name picks Fetch", lspSite{line: 2, col: -1, name: "Fetch"}, "Squash.cs::SquashFlow.Fetch"},
		{"decorated item name", lspSite{line: 2, col: -1, name: "SquashFlow.Tally()"}, "Squash.cs::SquashFlow.Tally"},
		{"item name matching neither", lspSite{line: 2, col: -1, name: "Other"}, ""},
		{"call column inside Fetch", lspSite{line: 2, col: callCol, src: load}, "Squash.cs::SquashFlow.Fetch"},
		{"column inside Tally", lspSite{line: 2, col: tallyCol + 3, src: load}, "Squash.cs::SquashFlow.Tally"},
		{"Fetch name column", lspSite{line: 2, col: fetchCol, src: load}, "Squash.cs::SquashFlow.Fetch"},
		{"column before every member", lspSite{line: 2, col: 0, src: load}, ""},
		{"column without source", lspSite{line: 2, col: callCol}, ""},
		{"unshared line unaffected", lineSite(5), "Squash.cs::ISlabBox.Get"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := view.matchCallableAt("Squash.cs", test.site)
			if test.want == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, test.want, got.ID)
		})
	}
}

// Same-named members on one line (Leaf.serialize and Box.serialize written
// side by side) cannot be told apart by line, name, or a name column — every
// position-keyed lookup must refuse rather than pick the first.
func TestSameLineNamesakesRefuse(t *testing.T) {
	const src = "class Leaf { serialize() { return 1; } } class Box { serialize() { return this.leaf.serialize(); } }\n"
	nodes := []*graph.Node{
		{ID: "m.ts::Leaf", Kind: graph.KindType, Name: "Leaf", FilePath: "m.ts", StartLine: 1, EndLine: 1},
		{ID: "m.ts::Leaf.serialize", Kind: graph.KindMethod, Name: "serialize", FilePath: "m.ts", StartLine: 1, EndLine: 1},
		{ID: "m.ts::Box", Kind: graph.KindType, Name: "Box", FilePath: "m.ts", StartLine: 1, EndLine: 1},
		{ID: "m.ts::Box.serialize", Kind: graph.KindMethod, Name: "serialize", FilePath: "m.ts", StartLine: 1, EndLine: 1},
	}
	view := newLSPGraphView(nodes, nil)

	assert.Nil(t, view.findDeclarationNode("m.ts", 1, "serialize"), "definition target is ambiguous")
	assert.Equal(t, "m.ts::Leaf", view.findDeclarationNode("m.ts", 1, "Leaf").ID, "a unique name still matches")

	site := lspSite{line: 1, col: column(t, src, 1, "serialize", 2), name: "serialize", src: func() []byte { return []byte(src) }}
	assert.Nil(t, view.matchCallableAt("m.ts", site), "hierarchy target is ambiguous")

	shared := sameLineNamesakes(nodes)
	assert.True(t, shared["m.ts::Leaf.serialize"])
	assert.True(t, shared["m.ts::Box.serialize"])
	assert.False(t, shared["m.ts::Leaf"])
}

func TestResolveSiteDefinition(t *testing.T) {
	absRoot := filepath.Join(t.TempDir(), "repo")
	uri := pathToURI(filepath.Join(absRoot, "src", "models.ts"))
	require.Equal(t, "src/models.ts", uriToPath(uri, absRoot))
	view := newLSPGraphView(nestedReceiverNodes(), nil)
	declCol := column(t, nestedReceiverTS, 7, "serialize", 0)
	useCol := column(t, nestedReceiverTS, 7, "serialize", 1)
	self := siteDefinitionAnswer{col: declCol, locs: []Location{{URI: uri, Range: nameRange(6, declCol, "serialize")}}}
	leaf := siteDefinitionAnswer{col: useCol, locs: []Location{{URI: uri, Range: nameRange(1, 2, "serialize")}}}
	box := siteDefinitionAnswer{col: useCol, locs: []Location{{URI: uri, Range: nameRange(6, declCol, "serialize")}}}
	external := siteDefinitionAnswer{col: useCol, locs: []Location{{URI: pathToURI(filepath.Join(filepath.Dir(absRoot), "elsewhere", "lib.d.ts"))}}}
	resolve := func(want string, answers ...siteDefinitionAnswer) string {
		n := resolveSiteDefinition(view, "", absRoot, "src/models.ts", 7, "serialize", want, answers)
		if n == nil {
			return ""
		}
		return n.ID
	}

	assert.Equal(t, "src/models.ts::Leaf.serialize", resolve("src/models.ts::Box.serialize", self, leaf),
		"the declaration token is skipped; the use rebinds")
	assert.Equal(t, "src/models.ts::Leaf.serialize", resolve("src/models.ts::Leaf.serialize", self, leaf))
	assert.Empty(t, resolve("src/models.ts::Leaf.serialize", self), "a declaration token alone is no verdict")
	// Genuine recursion: a use (not the declaration token) resolving to the
	// enclosing method still confirms it.
	box.col = useCol
	assert.Equal(t, "src/models.ts::Box.serialize", resolve("src/models.ts::Box.serialize", self, box))
	assert.Empty(t, resolve("src/models.ts::Other", leaf, box), "uses that disagree leave no verdict")
	assert.Empty(t, resolve("src/models.ts::Other", leaf, external), "an unanchored use blocks a rebind")
	assert.Equal(t, "src/models.ts::Leaf.serialize", resolve("src/models.ts::Leaf.serialize", leaf, external),
		"the edge's own target among the uses still confirms")
}

func TestHierarchyItemIdent(t *testing.T) {
	for in, want := range map[string]string{
		"serialize":           "serialize",
		"Fetch() : Box":       "Fetch",
		"Fetch(int)":          "Fetch",
		"Box.serialize":       "serialize",
		"Box::serialize(int)": "serialize",
		"List<T>":             "List",
		"":                    "",
	} {
		assert.Equal(t, want, hierarchyItemIdent(in), in)
	}
}
