package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The leak fence.
//
// One database is seeded twice: the base corpus at generation 0 and a divergent
// copy at generation 1. Shared identities carry different payloads, and each
// generation owns rows the other does not. Every read the package exposes is
// then driven through both handles and its output rendered to text.
//
// Two assertions hold for every probe:
//
//   - the base handle's output never mentions generation 1, and the derived
//     handle's output never mentions generation 0. Every divergent value ends
//     in genZeroMark / genOneMark, so a single substring test catches a leaked
//     row, a leaked payload column, and a leaked aggregate alike;
//   - the two outputs differ. A read that forgot its generation predicate
//     usually returns the union, which trips the first assertion — but a read
//     that returns nothing at all, or the same thing for both handles, proves
//     nothing, and this catches that.
//
// The capability checklist below then requires every optional interface the
// package asserts *Store satisfies to name a probe or state why it has none.
// The set of asserted interfaces is read back out of the package source, so a
// new capability cannot be added without deciding what happens to it here.

const (
	genZeroMark = "GenZero"
	genOneMark  = "GenOne"

	genReadRepo      = "repo"
	genReadOtherRepo = "other"
	genReadFileA     = "repo::pkg/a.go"
	genReadOtherFile = "other::pkg/c.go"

	genReadShared = "repo::pkg/a.go::Shared"
	genReadType   = "repo::pkg/a.go::Kind"
	genReadIface  = "repo::pkg/a.go::Iface"
	genReadMethod = "repo::pkg/a.go::Kind.Method"
	genReadOther  = "other::pkg/c.go::Other"
)

func genOnlyID(mark string) string       { return "repo::pkg/a.go::Only" + mark }
func genImportFile(mark string) string   { return "repo::pkg/b" + mark + ".go" }
func genUnresolved(mark string) string   { return "unresolved::Missing" + mark }
func genFnValue(mark string) string      { return "unresolved::fnvalue::Fn" + mark }
func genValueRef(mark string) string     { return "unresolved::valueref::V" + mark }
func genExternal(mark string) string     { return "dep::ext" + mark }
func genErrTarget(mark string) string    { return "repo::pkg/a.go::Err" + mark }
func genExtraID(mark string) string      { return "extra::pkg/x.go::Extra" + mark }
func genSecondNodeID(mark string) string { return "repo::pkg/a.go::Second" + mark }
func genSecondIfaceID(m string) string   { return "repo::pkg/a.go::Iface2" + m }
func genContentNodeID(m string) string   { return "repo::pkg/a.go::Doc" + m }

func oppositeMark(mark string) string {
	if mark == genZeroMark {
		return genOneMark
	}
	return genZeroMark
}

// genReadNodes builds one generation's node population. Shared identities keep
// their IDs across generations and diverge only in payload; the Only… and
// Extra… rows exist in exactly one generation. The Extra row also owns a
// repository prefix generation 0 never sees, so the per-repo aggregates differ
// as well as the per-node ones.
//
// The generation-1-only rows also make the marker-free projections asymmetric.
// A projection that renders no payload column — an inheritance topology, a
// per-repository-pair count, a corpus size — cannot be caught by the leak
// assertion, because a generation-blind read doubles it identically through
// BOTH handles and the two outputs still match. Only a population that differs
// between the generations makes those probes fail when the predicate is
// dropped, so generation 1 owns a second interface, a second cross-repository
// pair, an extra unresolved call site, and a third indexed symbol.
func genReadNodes(mark string) []*graph.Node {
	nodes := []*graph.Node{
		{ID: genReadFileA, Kind: graph.KindFile, Name: "a.go", FilePath: genReadFileA, RepoPrefix: genReadRepo, Language: "go"},
		{ID: genImportFile(mark), Kind: graph.KindFile, Name: "b" + mark + ".go", FilePath: genImportFile(mark), RepoPrefix: genReadRepo, Language: "go"},
		{
			ID: genReadShared, Kind: graph.KindFunction, Name: "Shared" + mark,
			QualName: "pkg.Shared" + mark, FilePath: genReadFileA, RepoPrefix: genReadRepo,
			Language: "go", StartLine: 10, EndLine: 40,
		},
		{
			ID: genReadType, Kind: graph.KindType, Name: "Kind" + mark,
			QualName: "pkg.Kind" + mark, FilePath: genReadFileA, RepoPrefix: genReadRepo,
			Language: "go", StartLine: 41, EndLine: 45,
		},
		{
			ID: genReadIface, Kind: graph.KindInterface, Name: "Iface" + mark,
			QualName: "pkg.Iface" + mark, FilePath: genReadFileA, RepoPrefix: genReadRepo,
			Language: "go", StartLine: 46, EndLine: 49,
			Meta: map[string]any{"methods": []string{"Method" + mark}},
		},
		{
			ID: genReadMethod, Kind: graph.KindMethod, Name: "Method" + mark,
			QualName: "pkg.Kind.Method" + mark, FilePath: genReadFileA, RepoPrefix: genReadRepo,
			Language: "go", StartLine: 50, EndLine: 80,
		},
		{
			ID: genOnlyID(mark), Kind: graph.KindFunction, Name: "Only" + mark,
			QualName: "pkg.Only" + mark, FilePath: genReadFileA, RepoPrefix: genReadRepo,
			Language: "go", StartLine: 90, EndLine: 120,
		},
		{
			ID: genErrTarget(mark), Kind: graph.KindString, Name: "boom " + mark,
			FilePath: genReadFileA, RepoPrefix: genReadRepo, Language: "go", StartLine: 121,
			Meta: map[string]any{"context": "error_msg"},
		},
		{
			ID: genContentNodeID(mark), Kind: graph.KindDoc, Name: "Doc" + mark,
			FilePath: genReadFileA, RepoPrefix: genReadRepo, Language: "markdown",
			Meta: map[string]any{"data_class": "content"},
		},
		{
			ID: genReadOther, Kind: graph.KindFunction, Name: "Other" + mark,
			QualName: "pkg.Other" + mark, FilePath: genReadOtherFile,
			RepoPrefix: genReadOtherRepo, Language: "go", StartLine: 5, EndLine: 9,
		},
	}
	if mark == genOneMark {
		// Three extra rows, in two different repositories, so the per-repo
		// aggregates diverge as well as the whole-graph ones. The second
		// interface is what gives the inheritance projections an asymmetric
		// topology rather than merely asymmetric payload columns.
		nodes = append(nodes,
			&graph.Node{
				ID: genExtraID(mark), Kind: graph.KindFunction, Name: "Extra" + mark,
				QualName: "pkg.Extra" + mark, FilePath: "extra::pkg/x.go",
				RepoPrefix: "extra", Language: "go", StartLine: 1, EndLine: 3,
			},
			&graph.Node{
				ID: genSecondNodeID(mark), Kind: graph.KindFunction, Name: "Second" + mark,
				QualName: "pkg.Second" + mark, FilePath: genReadFileA,
				RepoPrefix: genReadRepo, Language: "go", StartLine: 130, EndLine: 140,
			},
			&graph.Node{
				ID: genSecondIfaceID(mark), Kind: graph.KindInterface, Name: "Iface2" + mark,
				QualName: "pkg.Iface2" + mark, FilePath: genReadFileA,
				RepoPrefix: genReadRepo, Language: "go", StartLine: 141, EndLine: 143,
				Meta: map[string]any{"methods": []string{"Method" + mark}},
			},
		)
	}
	return nodes
}

// genReadEdges is the matching edge population. The references edge keeps one
// identity across generations and diverges only in Origin, so a read that
// fetches the right row but the wrong generation's columns is still caught.
//
// The last shared edge stays a calls edge: the cursor fence requires the base
// corpus's largest edge id to be a calls row.
func genReadEdges(mark string) []*graph.Edge {
	edges := []*graph.Edge{
		{From: genReadShared, To: genOnlyID(mark), Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 11, Confidence: 1},
		{From: genReadShared, To: genReadType, Kind: graph.EdgeReferences, FilePath: genReadFileA, Line: 12, Origin: "origin-" + mark, Confidence: 1},
		{From: genReadType, To: genReadIface, Kind: graph.EdgeImplements, FilePath: genReadFileA, Line: 13, Confidence: 1},
		{From: genReadMethod, To: genReadType, Kind: graph.EdgeMemberOf, FilePath: genReadFileA, Line: 14, Confidence: 1},
		{From: genReadShared, To: genUnresolved(mark), Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 15, Confidence: 0.4},
		{From: genReadShared, To: genFnValue(mark), Kind: graph.EdgeReferences, FilePath: genReadFileA, Line: 16, Confidence: 0.4},
		{From: genReadShared, To: genValueRef(mark), Kind: graph.EdgeReads, FilePath: genReadFileA, Line: 17, Confidence: 0.4},
		{From: genReadShared, To: genExternal(mark), Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 18, Confidence: 0.9},
		{From: genReadShared, To: genImportFile(mark), Kind: graph.EdgeImports, FilePath: genReadFileA, Line: 19, Confidence: 1},
		{From: genReadShared, To: genErrTarget(mark), Kind: graph.EdgeThrows, FilePath: genReadFileA, Line: 20, Confidence: 1},
		{From: genReadShared, To: genErrTarget(mark), Kind: graph.EdgeEmits, FilePath: genReadFileA, Line: 24, Confidence: 1},
		{From: genReadShared, To: genReadMethod, Kind: graph.EdgeWrites, FilePath: genReadFileA, Line: 21, Confidence: 1},
		{From: genReadShared, To: genReadOther, Kind: graph.EdgeCrossRepoCalls, FilePath: genReadFileA, Line: 22, Origin: "xrepo-" + mark, Confidence: 1},
		{From: genReadShared, To: genReadOther, Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 23, Origin: "xrepo-" + mark, Confidence: 1},
	}
	if mark == genOneMark {
		edges = append(edges,
			&graph.Edge{
				From: genExtraID(mark), To: genReadShared, Kind: graph.EdgeCalls,
				FilePath: "extra::pkg/x.go", Line: 2, Confidence: 1,
			},
			&graph.Edge{
				From: genSecondNodeID(mark), To: genReadShared, Kind: graph.EdgeCalls,
				FilePath: genReadFileA, Line: 25, Confidence: 1,
			},
			// A second (extra -> other) repository pair, a second inheritance
			// parent for the shared type, and a second unresolved call site:
			// the three populations whose projections carry no marker.
			&graph.Edge{
				From: genExtraID(mark), To: genReadOther, Kind: graph.EdgeCrossRepoCalls,
				FilePath: "extra::pkg/x.go", Line: 3, Confidence: 1,
			},
			&graph.Edge{
				From: genReadType, To: genSecondIfaceID(mark), Kind: graph.EdgeImplements,
				FilePath: genReadFileA, Line: 26, Confidence: 1,
			},
			&graph.Edge{
				From: genSecondNodeID(mark), To: genUnresolved(mark), Kind: graph.EdgeCalls,
				FilePath: genReadFileA, Line: 27, Confidence: 0.4,
			},
		)
	}
	return edges
}

// openGenerationReadPair seeds one database with two divergent corpora and
// returns the base handle plus a handle pinned to generation 1.
func openGenerationReadPair(t *testing.T) (base, derived *Store) {
	t.Helper()
	base, err := Open(filepath.Join(t.TempDir(), "generation_read.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := base.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	derived = base.AtGeneration(1)
	if derived == nil {
		t.Fatal("AtGeneration(1) returned nil")
	}
	for _, pair := range []struct {
		store *Store
		mark  string
	}{{base, genZeroMark}, {derived, genOneMark}} {
		pair.store.AddBatch(genReadNodes(pair.mark), genReadEdges(pair.mark))
		seedGenerationFTS(t, pair.store, pair.mark)
	}
	return base, derived
}

// seedGenerationFTS fills both full-text corpora for one generation. Symbol and
// content rows live in a single shared virtual table, so their per-generation
// ownership comes from the rowid sidecars — which is exactly what the search
// probes below exercise.
//
// Generation 1 indexes one symbol more than the base corpus, so the corpus-size
// read differs between the handles instead of merely doubling in both.
func seedGenerationFTS(t *testing.T, s *Store, mark string) {
	t.Helper()
	items := []graph.SymbolFTSItem{
		{NodeID: genReadShared, Tokens: "Shared" + mark},
		{NodeID: genOnlyID(mark), Tokens: "Only" + mark},
	}
	if mark == genOneMark {
		items = append(items, graph.SymbolFTSItem{NodeID: genSecondNodeID(mark), Tokens: "Second" + mark})
	}
	if err := s.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatalf("seed symbol fts (%s): %v", mark, err)
	}
	content := []graph.ContentFTSItem{
		{NodeID: genContentNodeID(mark), FilePath: genReadFileA, Ordinal: 0, Body: "documented body " + mark},
	}
	if err := s.AppendContent(genReadRepo, content); err != nil {
		t.Fatalf("seed content fts (%s): %v", mark, err)
	}
}

// genProbe is one read rendered to text. identicalOK states why a probe may
// legitimately produce the same output for both handles. It is only ever
// admissible for a projection the fixture cannot populate at all: for a
// projection that returns rows, it disables the sole assertion with teeth,
// because a generation-blind read yields the same doubled union through both
// handles and the marker test alone cannot see that.
type genProbe struct {
	name        string
	run         func(t *testing.T, s *Store) []string
	identicalOK string
}

func nodeTokens(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		out = append(out, fmt.Sprintf("node %s|%s|%s|%s", n.ID, n.Name, n.QualName, n.RepoPrefix))
	}
	return out
}

func edgeTokens(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		out = append(out, fmt.Sprintf("edge %s->%s|%s|%d|%s", e.From, e.To, e.Kind, e.Line, e.Origin))
	}
	return out
}

func nodeMapTokens(m map[string]*graph.Node) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, "key "+k)
		out = append(out, nodeTokens([]*graph.Node{v})...)
	}
	return out
}

func nodeSliceMapTokens(m map[string][]*graph.Node) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, "key "+k)
		out = append(out, nodeTokens(v)...)
	}
	return out
}

func edgeSliceMapTokens(m map[string][]*graph.Edge) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, "key "+k)
		out = append(out, edgeTokens(v)...)
	}
	return out
}

// genReadProbeIDs is the identity frontier handed to every batch read: both
// generations' exclusive rows plus the shared ones. A generation-blind batch
// read answers for identities it should not be able to see.
func genReadProbeIDs() []string {
	return []string{
		genReadShared, genReadType, genReadIface, genReadMethod,
		genOnlyID(genZeroMark), genOnlyID(genOneMark),
		genExtraID(genOneMark), genReadOther,
		genUnresolved(genZeroMark), genUnresolved(genOneMark),
	}
}

func genReadProbeNames() []string {
	return []string{
		"Shared" + genZeroMark, "Shared" + genOneMark,
		"Only" + genZeroMark, "Only" + genOneMark,
		"Extra" + genOneMark, "Kind" + genZeroMark, "Kind" + genOneMark,
	}
}

func genReadProbeQualNames() []string {
	return []string{
		"pkg.Shared" + genZeroMark, "pkg.Shared" + genOneMark,
		"pkg.Only" + genZeroMark, "pkg.Only" + genOneMark,
	}
}

// Four graph.Store methods deliberately have no probe:
//
//   - ProxyNodeCountAtLeast — proxy nodes are never persisted (AddNode returns
//     early and the AddBatch insert filters them), so the durable store has no
//     population for it to observe in any generation;
//   - EdgeIdentityRevisions and VerifyEdgeIdentities — an in-process counter
//     and a no-op, neither of which reads a row;
//   - ResolveMutex — returns the core's lock, which every handle shares by
//     design.
//
//nolint:gocyclo // one entry per read; the length is the coverage.
func generationReadProbes() []genProbe {
	return []genProbe{
		{name: "GetNode", run: func(t *testing.T, s *Store) []string {
			return nodeTokens([]*graph.Node{s.GetNode(genReadShared), s.GetNode(genOnlyID(genZeroMark)), s.GetNode(genOnlyID(genOneMark))})
		}},
		{name: "GetNodeContext", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for _, id := range genReadProbeIDs() {
				n, err := s.GetNodeContext(context.Background(), id)
				if err != nil {
					t.Fatalf("GetNodeContext: %v", err)
				}
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "GetNodeByQualName", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for _, q := range genReadProbeQualNames() {
				out = append(out, s.GetNodeByQualName(q))
			}
			return nodeTokens(out)
		}},
		{name: "GetNodesByQualNames", run: func(t *testing.T, s *Store) []string {
			return nodeSliceMapTokens(s.GetNodesByQualNames(genReadProbeQualNames()))
		}},
		{name: "FindNodesByName", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for _, n := range genReadProbeNames() {
				out = append(out, s.FindNodesByName(n)...)
			}
			return nodeTokens(out)
		}},
		{name: "FindNodesByNameInRepo", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for _, n := range genReadProbeNames() {
				out = append(out, s.FindNodesByNameInRepo(n, genReadRepo)...)
			}
			return nodeTokens(out)
		}},
		{name: "FindNodesByNameContaining", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.FindNodesByNameContaining("Gen", 0))
		}},
		{name: "FindNodesByNames", run: func(t *testing.T, s *Store) []string {
			return nodeSliceMapTokens(s.FindNodesByNames(genReadProbeNames()))
		}},
		{name: "GetFileNodes", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetFileNodes(genReadFileA))
		}},
		{name: "GetFileNodesContext", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetFileNodesContext(context.Background(), genReadFileA))
		}},
		{name: "GetFileNodesByPaths", run: func(t *testing.T, s *Store) []string {
			return nodeSliceMapTokens(s.GetFileNodesByPaths([]string{genReadFileA, genImportFile(genZeroMark), genImportFile(genOneMark)}))
		}},
		{name: "GetRepoNodes", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoNodes(genReadRepo))
		}},
		{name: "GetRepoNodesByLanguage", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoNodesByLanguage(genReadRepo, "go"))
		}},
		{name: "GetNodesByLanguage", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetNodesByLanguage("go"))
		}},
		{name: "GetRepoNonContentNodes", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoNonContentNodes(genReadRepo))
		}},
		{name: "GetRepoContentNodes", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoContentNodes(genReadRepo))
		}},
		{name: "GetRepoNodeSummariesByLanguage", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoNodeSummariesByLanguage(genReadRepo, "go"))
		}},
		{name: "GetRepoNodesLight", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.GetRepoNodesLight(genReadRepo))
		}},
		{name: "RepoNodesLight", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.RepoNodesLight([]string{genReadRepo, "extra"}))
		}},
		{name: "AllNodes", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.AllNodes())
		}},
		{name: "AllNodesLight", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.AllNodesLight())
		}},
		{name: "NodesLightSeq", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesLightSeq() {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "GetNodesByIDs", run: func(t *testing.T, s *Store) []string {
			return nodeMapTokens(s.GetNodesByIDs(genReadProbeIDs()))
		}},
		{name: "GetNodesByIDsContext", run: func(t *testing.T, s *Store) []string {
			m, err := s.GetNodesByIDsContext(context.Background(), genReadProbeIDs())
			if err != nil {
				t.Fatalf("GetNodesByIDsContext: %v", err)
			}
			return nodeMapTokens(m)
		}},
		{name: "ExistingNodeIDs", run: func(t *testing.T, s *Store) []string {
			out := make([]string, 0)
			for id := range s.ExistingNodeIDs(genReadProbeIDs()) {
				out = append(out, "id "+id)
			}
			return out
		}},
		{name: "CountNodesByNameClass", run: func(t *testing.T, s *Store) []string {
			counts := s.CountNodesByNameClass(genReadProbeNames(), []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType})
			out := make([]string, 0, len(counts))
			for name, c := range counts {
				out = append(out, fmt.Sprintf("class %s real=%d stub=%d", name, c.Real, c.Stub))
			}
			return out
		}},
		{name: "NodesByKind", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesByKind(graph.KindFunction) {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "NodesByKinds", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.NodesByKinds([]graph.NodeKind{graph.KindFunction, graph.KindType}))
		}},
		{name: "NodesByKindsSeq", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesByKindsSeq(graph.KindFunction, graph.KindType) {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "NodesByKindLang", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesByKindLang(graph.KindFunction, "go") {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "NodeIDsByKinds", run: func(t *testing.T, s *Store) []string {
			return prefixed("id ", s.NodeIDsByKinds([]graph.NodeKind{graph.KindFunction}))
		}},
		{name: "NodesInFilesByKind", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.NodesInFilesByKind([]string{genReadFileA}, []graph.NodeKind{graph.KindFunction, graph.KindMethod}))
		}},
		{name: "NodeIDNamesByKindsSeq", run: func(t *testing.T, s *Store) []string {
			var out []string
			for row := range s.NodeIDNamesByKindsSeq(genReadRepo, graph.KindFunction) {
				out = append(out, "idname "+row.ID+"|"+row.Name)
			}
			return out
		}},
		{name: "FileSymbolNamesByPaths", run: func(t *testing.T, s *Store) []string {
			rows := s.FileSymbolNamesByPaths([]string{genReadFileA}, []graph.NodeKind{graph.KindFunction})
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, "filesym "+r.FilePath+"|"+r.Name)
			}
			return out
		}},
		{name: "ScanNodeSearchKeys", run: func(t *testing.T, s *Store) []string {
			var out []string
			err := s.ScanNodeSearchKeys(context.Background(), 4, func(page []graph.NodeSearchKey) bool {
				for _, k := range page {
					out = append(out, "searchkey "+k.ID+"|"+k.Name)
				}
				return true
			})
			if err != nil {
				t.Fatalf("ScanNodeSearchKeys: %v", err)
			}
			return out
		}},
		{name: "FindNodesByNamesInRepo", run: func(t *testing.T, s *Store) []string {
			return nodeSliceMapTokens(s.FindNodesByNamesInRepo(genReadProbeNames(), genReadRepo))
		}},
		{name: "FindNodesByNamesInRepoLanguages", run: func(t *testing.T, s *Store) []string {
			return nodeSliceMapTokens(s.FindNodesByNamesInRepoLanguages(genReadProbeNames(), genReadRepo, []string{"go"}))
		}},
		{name: "FindNodesByResolverNameScopes", run: func(t *testing.T, s *Store) []string {
			scopes := []graph.ResolverNameScope{
				{RepoPrefix: genReadRepo, Languages: []string{"go"}, Names: genReadProbeNames()},
				{AllRepos: true, Languages: []string{"go"}, Names: genReadProbeNames()},
				{RepoPrefix: genReadRepo, Names: genReadProbeNames()},
				{AllRepos: true, Names: genReadProbeNames()},
			}
			got, err := s.FindNodesByResolverNameScopes(scopes)
			if err != nil {
				t.Fatalf("FindNodesByResolverNameScopes: %v", err)
			}
			var out []string
			for i, byName := range got {
				out = append(out, fmt.Sprintf("scope %d", i))
				out = append(out, nodeSliceMapTokens(byName)...)
			}
			return out
		}},
		{name: "CountRepoLanguageSymbols", run: func(t *testing.T, s *Store) []string {
			return []string{fmt.Sprintf("symbols=%d", s.CountRepoLanguageSymbols(genReadRepo, []string{"go"}))}
		}},
		{name: "FindNodesByNameBounded", run: func(t *testing.T, s *Store) []string {
			var out []string
			for _, name := range genReadProbeNames() {
				got, err := s.FindNodesByNameBounded(context.Background(), name, graph.LocalizationNodeScope{}, 8)
				if err != nil {
					t.Fatalf("FindNodesByNameBounded: %v", err)
				}
				out = append(out, nodeTokens(got.Nodes)...)
			}
			return out
		}},
		{name: "FindFileNodesBounded", run: func(t *testing.T, s *Store) []string {
			got, err := s.FindFileNodesBounded(context.Background(), genReadFileA, graph.LocalizationNodeScope{}, 32)
			if err != nil {
				t.Fatalf("FindNodesInFileBounded: %v", err)
			}
			return nodeTokens(got.Nodes)
		}},

		{name: "GetOutEdges", run: func(t *testing.T, s *Store) []string {
			return edgeTokens(s.GetOutEdges(genReadShared))
		}},
		{name: "GetInEdges", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			out = append(out, s.GetInEdges(genReadType)...)
			out = append(out, s.GetInEdges(genOnlyID(genZeroMark))...)
			out = append(out, s.GetInEdges(genOnlyID(genOneMark))...)
			return edgeTokens(out)
		}},
		{name: "GetOutEdgesForNodes", run: func(t *testing.T, s *Store) []string {
			return edgeSliceMapTokens(s.GetOutEdgesForNodes(genReadProbeIDs()))
		}},
		{name: "GetOutEdgesByNodeIDs", run: func(t *testing.T, s *Store) []string {
			return edgeSliceMapTokens(s.GetOutEdgesByNodeIDs(genReadProbeIDs()))
		}},
		{name: "GetInEdgesByNodeIDs", run: func(t *testing.T, s *Store) []string {
			return edgeSliceMapTokens(s.GetInEdgesByNodeIDs(genReadProbeIDs()))
		}},
		{name: "GetOutEdgesByNodeIDsContext", run: func(t *testing.T, s *Store) []string {
			m, _, err := s.GetOutEdgesByNodeIDsContext(context.Background(), genReadProbeIDs(), 64)
			if err != nil {
				t.Fatalf("GetOutEdgesByNodeIDsContext: %v", err)
			}
			return edgeSliceMapTokens(m)
		}},
		{name: "GetInEdgesByNodeIDsContext", run: func(t *testing.T, s *Store) []string {
			m, _, err := s.GetInEdgesByNodeIDsContext(context.Background(), genReadProbeIDs(), 64)
			if err != nil {
				t.Fatalf("GetInEdgesByNodeIDsContext: %v", err)
			}
			return edgeSliceMapTokens(m)
		}},
		{name: "GetRepoEdges", run: func(t *testing.T, s *Store) []string {
			return edgeTokens(s.GetRepoEdges(genReadRepo))
		}},
		{name: "AllEdges", run: func(t *testing.T, s *Store) []string {
			return edgeTokens(s.AllEdges())
		}},
		{name: "AllEdgesLight", run: func(t *testing.T, s *Store) []string {
			return edgeTokens(s.AllEdgesLight())
		}},
		{name: "EdgesLightSeq", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.EdgesLightSeq(graph.EdgeCalls, graph.EdgeReferences) {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "EdgesByKind", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.EdgesByKind(graph.EdgeCalls) {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "EdgesByKinds", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.EdgesByKinds([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}) {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "ScanEdgesByKindsBatched", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			s.ScanEdgesByKindsBatched([]graph.EdgeKind{graph.EdgeCalls}, 4, func(page []*graph.Edge) bool {
				out = append(out, page...)
				return true
			})
			return edgeTokens(out)
		}},
		// The labels here are deliberately marker-free: the probe reports which
		// generation's edge each handle can see, so embedding the marker in the
		// label would trip the leak assertion on a correct answer.
		{name: "EdgeExists", run: func(t *testing.T, s *Store) []string {
			return []string{fmt.Sprintf("exists zero=%t one=%t",
				s.EdgeExists(genReadShared, genOnlyID(genZeroMark), graph.EdgeCalls, genReadFileA, 11),
				s.EdgeExists(genReadShared, genOnlyID(genOneMark), graph.EdgeCalls, genReadFileA, 11))}
		}},
		{name: "GetEdgeCandidates", run: func(t *testing.T, s *Store) []string {
			set := s.GetEdgeCandidates(
				[]graph.EdgeEndpoint{
					{From: genReadShared, To: genOnlyID(genZeroMark)},
					{From: genReadShared, To: genOnlyID(genOneMark)},
					{From: genReadShared, To: genReadType},
				},
				[]graph.EdgeSite{
					{From: genReadShared, Line: 12, Kind: graph.EdgeReferences},
					{From: genReadShared, Line: 11},
				},
			)
			var out []string
			for _, mark := range []string{genZeroMark, genOneMark} {
				out = append(out, edgeTokens([]*graph.Edge{set.Endpoint(genReadShared, genOnlyID(mark))})...)
			}
			out = append(out, edgeTokens([]*graph.Edge{set.Endpoint(genReadShared, genReadType)})...)
			out = append(out, edgeTokens(set.Site(genReadShared, 12, graph.EdgeReferences))...)
			return out
		}},
		{name: "FindEdgesByIdentities", run: func(t *testing.T, s *Store) []string {
			ids := []graph.EdgeIdentity{
				{From: genReadShared, To: genOnlyID(genZeroMark), Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 11},
				{From: genReadShared, To: genOnlyID(genOneMark), Kind: graph.EdgeCalls, FilePath: genReadFileA, Line: 11},
				{From: genReadShared, To: genReadType, Kind: graph.EdgeReferences, FilePath: genReadFileA, Line: 12},
			}
			var out []string
			for _, e := range s.FindEdgesByIdentities(ids) {
				out = append(out, edgeTokens([]*graph.Edge{e})...)
			}
			return out
		}},
		{name: "GetInEdgeIdentitiesByNodeIDs", run: func(t *testing.T, s *Store) []string {
			m := s.GetInEdgeIdentitiesByNodeIDs(genReadProbeIDs())
			var out []string
			for k, v := range m {
				for _, id := range v {
					out = append(out, fmt.Sprintf("inid %s %s->%s|%s", k, id.From, id.To, id.Kind))
				}
			}
			return out
		}},
		{name: "NodePlacementsByIDs", run: func(t *testing.T, s *Store) []string {
			var out []string
			for id, p := range s.NodePlacementsByIDs(genReadProbeIDs()) {
				out = append(out, fmt.Sprintf("place %s|%s|%s|%s", id, p.Kind, p.FilePath, p.RepoPrefix))
			}
			return out
		}},
		{name: "FindOutgoingEdgeIdentitiesBounded", run: func(t *testing.T, s *Store) []string {
			p, err := s.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{genReadShared}, []graph.EdgeKind{graph.EdgeCalls}, 32)
			if err != nil {
				t.Fatalf("FindOutgoingEdgeIdentitiesBounded: %v", err)
			}
			return identityProjectionTokens(p)
		}},
		{name: "FindIncomingEdgeIdentitiesBounded", run: func(t *testing.T, s *Store) []string {
			p, err := s.FindIncomingEdgeIdentitiesBounded(context.Background(),
				[]string{genOnlyID(genZeroMark), genOnlyID(genOneMark), genReadType},
				[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, 32)
			if err != nil {
				t.Fatalf("FindIncomingEdgeIdentitiesBounded: %v", err)
			}
			return identityProjectionTokens(p)
		}},
		{name: "FindOutgoingSiteEdgeIdentitiesBounded", run: func(t *testing.T, s *Store) []string {
			p, err := s.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(),
				[]graph.EdgeSourceSite{{From: genReadShared, Line: 11}, {From: genReadShared, Line: 12}},
				[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, 32)
			if err != nil {
				t.Fatalf("FindOutgoingSiteEdgeIdentitiesBounded: %v", err)
			}
			var out []string
			for site, ids := range p.BySite {
				for _, id := range ids {
					out = append(out, fmt.Sprintf("site %s:%d %s->%s", site.From, site.Line, id.From, id.To))
				}
			}
			return out
		}},
		{name: "FindExistingEdgeEndpoints", run: func(t *testing.T, s *Store) []string {
			found, err := s.FindExistingEdgeEndpoints(context.Background(), []graph.TypedEdgeEndpoint{
				{From: genReadShared, To: genOnlyID(genZeroMark), Kind: graph.EdgeCalls},
				{From: genReadShared, To: genOnlyID(genOneMark), Kind: graph.EdgeCalls},
			}, 16)
			if err != nil {
				t.Fatalf("FindExistingEdgeEndpoints: %v", err)
			}
			var out []string
			for k := range found {
				out = append(out, fmt.Sprintf("endpoint %s->%s|%s", k.From, k.To, k.Kind))
			}
			return out
		}},
		{name: "FindIncomingSourcesBounded", run: func(t *testing.T, s *Store) []string {
			p, err := s.FindIncomingSourcesBounded(context.Background(),
				[]string{genOnlyID(genZeroMark), genOnlyID(genOneMark), genReadShared}, graph.EdgeCalls, 32)
			if err != nil {
				t.Fatalf("FindIncomingSourcesBounded: %v", err)
			}
			var out []string
			for target, sources := range p.Sources {
				for _, src := range sources {
					out = append(out, "insrc "+target+"<-"+src)
				}
			}
			return out
		}},
		{name: "EdgesWithUnresolvedTarget", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.EdgesWithUnresolvedTarget() {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "UnresolvedEdgePages", run: func(t *testing.T, s *Store) []string {
			scan, err := s.BeginUnresolvedEdgeScan(context.Background())
			if err != nil {
				t.Fatalf("BeginUnresolvedEdgeScan: %v", err)
			}
			page, err := s.ReadUnresolvedEdgePage(context.Background(), scan, 0, 64, 1<<20)
			if err != nil {
				t.Fatalf("ReadUnresolvedEdgePage: %v", err)
			}
			return edgeTokens(page.Edges)
		}},
		{name: "ScanUnresolvedEdgeIdentitiesBatched", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanUnresolvedEdgeIdentitiesBatched([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeReads}, 8,
				func(page []graph.EdgeIdentity) bool {
					for _, id := range page {
						out = append(out, fmt.Sprintf("unresolved %s->%s|%s", id.From, id.To, id.Kind))
					}
					return true
				})
			return out
		}},
		{name: "CountUnresolvedFrontier", run: func(t *testing.T, s *Store) []string {
			stats, err := s.CountUnresolvedFrontier()
			if err != nil {
				t.Fatalf("CountUnresolvedFrontier: %v", err)
			}
			out := []string{fmt.Sprintf("pending=%d groups=%d", stats.Pending, stats.GroupCount)}
			for _, b := range stats.Buckets {
				out = append(out, fmt.Sprintf("bucket %s|%s|%d", b.Kind, b.TargetClass, b.Count))
			}
			return out
		}},
		{name: "FnValuePlaceholderEdges", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.FnValuePlaceholderEdges() {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "ValueRefPlaceholderEdges", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Edge
			for e := range s.ValueRefPlaceholderEdges() {
				out = append(out, e)
			}
			return edgeTokens(out)
		}},
		{name: "ExternalCallCandidateEdges", run: func(t *testing.T, s *Store) []string {
			return edgeTokens(s.ExternalCallCandidateEdges())
		}},
		{name: "DistinctExternalTargets", run: func(t *testing.T, s *Store) []string {
			return prefixed("external ", s.DistinctExternalTargets([]graph.EdgeKind{graph.EdgeCalls}))
		}},

		{name: "NodeCount", run: func(t *testing.T, s *Store) []string {
			return []string{fmt.Sprintf("nodes=%d", s.NodeCount())}
		}},
		{name: "EdgeCount", run: func(t *testing.T, s *Store) []string {
			return []string{fmt.Sprintf("edges=%d", s.EdgeCount())}
		}},
		{name: "Stats", run: func(t *testing.T, s *Store) []string {
			st := s.Stats()
			out := []string{fmt.Sprintf("total=%d/%d", st.TotalNodes, st.TotalEdges)}
			return append(out, sortedCountTokens("kind", st.ByKind)...)
		}},
		{name: "RepoStats", run: func(t *testing.T, s *Store) []string {
			var out []string
			for repo, st := range s.RepoStats() {
				out = append(out, fmt.Sprintf("repo %s nodes=%d edges=%d", repo, st.TotalNodes, st.TotalEdges))
			}
			return out
		}},
		{name: "RepoPrefixes", run: func(t *testing.T, s *Store) []string {
			return prefixed("prefix ", s.RepoPrefixes())
		}},
		{name: "RepoMemoryEstimate", run: func(t *testing.T, s *Store) []string {
			e := s.RepoMemoryEstimate(genReadRepo)
			return []string{fmt.Sprintf("estimate nodes=%d edges=%d", e.NodeCount, e.EdgeCount)}
		}},
		{name: "ScanRepoMemoryEstimates", run: func(t *testing.T, s *Store) []string {
			m, err := s.ScanRepoMemoryEstimates(context.Background())
			if err != nil {
				t.Fatalf("ScanRepoMemoryEstimates: %v", err)
			}
			var out []string
			for repo, e := range m {
				out = append(out, fmt.Sprintf("scan %s nodes=%d edges=%d", repo, e.NodeCount, e.EdgeCount))
			}
			return out
		}},
		{name: "InEdgeCountsByKind", run: func(t *testing.T, s *Store) []string {
			return sortedCountTokens("incount", s.InEdgeCountsByKind([]graph.EdgeKind{graph.EdgeCalls}))
		}},
		{name: "EdgeKindCounts", run: func(t *testing.T, s *Store) []string {
			counts := s.EdgeKindCounts()
			out := make([]string, 0, len(counts))
			for k, n := range counts {
				out = append(out, fmt.Sprintf("kindcount %s=%d", k, n))
			}
			sort.Strings(out)
			return out
		}},
		{name: "InDegreeForNodes", run: func(t *testing.T, s *Store) []string {
			return sortedCountTokens("indegree", s.InDegreeForNodes(genReadProbeIDs()))
		}},
		{name: "NodeDegreeByKinds", run: func(t *testing.T, s *Store) []string {
			rows := s.NodeDegreeByKinds([]graph.NodeKind{graph.KindFunction}, "")
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("degree %s in=%d out=%d", r.NodeID, r.InCount, r.OutCount))
			}
			return out
		}},
		{name: "NodeDegreeCounts", run: func(t *testing.T, s *Store) []string {
			rows := s.NodeDegreeCounts(genReadProbeIDs(), []graph.EdgeKind{graph.EdgeCalls})
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("counts %s in=%d out=%d usage=%d", r.NodeID, r.InCount, r.OutCount, r.UsageInCount))
			}
			return out
		}},
		{name: "NodeFanCounts", run: func(t *testing.T, s *Store) []string {
			rows := s.NodeFanCounts(genReadProbeIDs(), []graph.EdgeKind{graph.EdgeCalls}, []graph.EdgeKind{graph.EdgeCalls})
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("fan %s in=%d out=%d", r.NodeID, r.FanIn, r.FanOut))
			}
			return out
		}},
		{name: "EdgeAdjacencyForKinds", run: func(t *testing.T, s *Store) []string {
			var out []string
			for pair := range s.EdgeAdjacencyForKinds([]graph.EdgeKind{graph.EdgeCalls}, []graph.NodeKind{graph.KindFunction}) {
				out = append(out, "adj "+pair[0]+"->"+pair[1])
			}
			return out
		}},
		{name: "CommunityCrossingsByKind", run: func(t *testing.T, s *Store) []string {
			comm := map[string]string{
				genReadShared: "a", genOnlyID(genZeroMark): "b", genOnlyID(genOneMark): "b",
				genReadOther: "c", genExtraID(genOneMark): "d",
			}
			return sortedCountTokens("crossing", s.CommunityCrossingsByKind([]graph.EdgeKind{graph.EdgeCalls}, comm))
		}},
		{name: "FileImportCounts", run: func(t *testing.T, s *Store) []string {
			rows := s.FileImportCounts(nil)
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("import %s=%d", r.FilePath, r.Count))
			}
			sort.Strings(out)
			return out
		}},
		{name: "FileImporters", run: func(t *testing.T, s *Store) []string {
			var out []string
			for _, mark := range []string{genZeroMark, genOneMark} {
				for _, r := range s.FileImporters(genImportFile(mark)) {
					out = append(out, fmt.Sprintf("importer %s %s|%s", mark, r.FromID, r.FromName))
				}
			}
			return out
		}},
		{name: "CrossRepoEdgeCounts", run: func(t *testing.T, s *Store) []string {
			rows := s.CrossRepoEdgeCounts()
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("xrepo %s %s->%s=%d", r.Kind, r.FromRepo, r.ToRepo, r.Count))
			}
			sort.Strings(out)
			return out
		}},
		{name: "CrossRepoCandidates", run: func(t *testing.T, s *Store) []string {
			return crossRepoTokens(s.CrossRepoCandidates([]graph.EdgeKind{graph.EdgeCalls}))
		}},
		{name: "CrossRepoCandidatesForRepos", run: func(t *testing.T, s *Store) []string {
			return crossRepoTokens(s.CrossRepoCandidatesForRepos([]graph.EdgeKind{graph.EdgeCalls}, []string{genReadRepo, genReadOtherRepo}))
		}},
		{name: "CrossRepoCandidatesForFiles", run: func(t *testing.T, s *Store) []string {
			return crossRepoTokens(s.CrossRepoCandidatesForFiles([]graph.EdgeKind{graph.EdgeCalls}, []string{genReadFileA}))
		}},
		{name: "CrossRepoCandidatesForMutation", run: func(t *testing.T, s *Store) []string {
			return crossRepoTokens(s.CrossRepoCandidatesForMutation([]graph.EdgeKind{graph.EdgeCalls}, []string{genReadFileA}, []string{genReadFileA}))
		}},

		{name: "DeadCodeCandidates", run: func(t *testing.T, s *Store) []string {
			return nodeTokens(s.DeadCodeCandidates(
				[]graph.NodeKind{graph.KindFunction},
				map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}},
			))
		}},
		{name: "IfaceImplementsRows", run: func(t *testing.T, s *Store) []string {
			rows := s.IfaceImplementsRows()
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("impl %s->%s methods=%v", r.TypeID, r.IfaceID, r.IfaceMeta["methods"]))
			}
			return out
		}},
		{name: "MemberMethodsByType", run: func(t *testing.T, s *Store) []string {
			var out []string
			for typeID, methods := range s.MemberMethodsByType() {
				for _, m := range methods {
					out = append(out, fmt.Sprintf("member %s %s|%s", typeID, m.MethodID, m.Name))
				}
			}
			return out
		}},
		{name: "StructuralParentEdges", run: func(t *testing.T, s *Store) []string {
			rows := s.StructuralParentEdges()
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("parent %s->%s|%s|%s", r.FromID, r.ToID, r.FromKind, r.ToKind))
			}
			return out
		}},
		{name: "ExtractCandidates", run: func(t *testing.T, s *Store) []string {
			rows := s.ExtractCandidates([]graph.EdgeKind{graph.EdgeCalls}, 1, 0, 0, "")
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("extract %s|%s callers=%d fanout=%d", r.NodeID, r.Name, r.CallerCount, r.FanOut))
			}
			return out
		}},
		{name: "ThrowerErrorSurface", run: func(t *testing.T, s *Store) []string {
			rows := s.ThrowerErrorSurface("")
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("thrower %s targets=%v msgs=%v", r.ThrowerID, r.ErrorTargets, r.ErrorMsgs))
			}
			return out
		}},
		{name: "BFS", run: func(t *testing.T, s *Store) []string {
			hops, err := s.BFS([]string{genReadShared}, graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, 3, 0)
			if err != nil {
				t.Fatalf("BFS: %v", err)
			}
			out := make([]string, 0, len(hops))
			for _, h := range hops {
				out = append(out, fmt.Sprintf("hop %s depth=%d parent=%s", h.NodeID, h.Depth, h.ParentID))
			}
			return out
		}},
		{name: "ReachableForwardByKinds", run: func(t *testing.T, s *Store) []string {
			var out []string
			for id := range s.ReachableForwardByKinds([]string{genReadShared}, []graph.EdgeKind{graph.EdgeCalls}) {
				out = append(out, "reach "+id)
			}
			return out
		}},
		{name: "ClassHierarchyTraverse", run: func(t *testing.T, s *Store) []string {
			rows := s.ClassHierarchyTraverse(genReadType, "up", []graph.EdgeKind{graph.EdgeImplements}, 3)
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, fmt.Sprintf("hier %v|%v", r.Path, r.EdgeKinds))
			}
			return out
		}},
		{name: "ExpandFrontier", run: func(t *testing.T, s *Store) []string {
			hops := s.ExpandFrontier([]string{genReadShared}, true, []graph.EdgeKind{graph.EdgeCalls}, 32)
			var out []string
			for _, h := range hops {
				out = append(out, edgeTokens([]*graph.Edge{h.Edge})...)
				out = append(out, nodeTokens([]*graph.Node{h.Neighbor})...)
			}
			return out
		}},
		{name: "FileEditingContext", run: func(t *testing.T, s *Store) []string {
			res := s.FileEditingContext(genReadFileA, []graph.NodeKind{graph.KindFunction, graph.KindMethod})
			if res == nil {
				return nil
			}
			out := nodeTokens(append([]*graph.Node{res.FileNode}, res.Defines...))
			out = append(out, edgeTokens(res.Imports)...)
			out = append(out, nodeTokens(res.CalledBy)...)
			return append(out, nodeTokens(res.Calls)...)
		}},
		{name: "GetFileSubGraph", run: func(t *testing.T, s *Store) []string {
			nodes, edges := s.GetFileSubGraph(genReadFileA)
			return append(nodeTokens(nodes), edgeTokens(edges)...)
		}},
		{name: "GetFileSubGraphCounts", run: func(t *testing.T, s *Store) []string {
			nodes, edgeCount := s.GetFileSubGraphCounts(genReadFileA)
			return append(nodeTokens(nodes), fmt.Sprintf("subgraph edges=%d", edgeCount))
		}},

		{name: "NodesInScopeSeq", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesInScopeSeq([]string{genReadRepo}, nil, graph.KindFunction) {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "NodesLightInScopeSeq", run: func(t *testing.T, s *Store) []string {
			var out []*graph.Node
			for n := range s.NodesLightInScopeSeq([]string{genReadRepo}, nil) {
				out = append(out, n)
			}
			return nodeTokens(out)
		}},
		{name: "EdgesInScopeSeq", run: func(t *testing.T, s *Store) []string {
			var out []string
			for row := range s.EdgesInScopeSeq([]string{genReadRepo}, nil, graph.EdgeCalls) {
				out = append(out, edgeTokens([]*graph.Edge{row.Edge})...)
				out = append(out, nodeTokens([]*graph.Node{row.Source, row.Target})...)
			}
			return out
		}},
		{name: "ScanRepoCapabilityEdges", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanRepoCapabilityEdges([]string{genReadRepo}, 8, func(page []graph.RepoCapabilityEdge) bool {
				for _, row := range page {
					out = append(out, fmt.Sprintf("cap %s %s->%s|%s", row.RepoPrefix, row.Identity.From, row.Identity.To, row.Identity.Kind))
				}
				return true
			})
			return out
		}},
		{name: "ProjectImportAdjacency", run: func(t *testing.T, s *Store) []string {
			m, complete := s.ProjectImportAdjacency([]string{genReadFileA})
			out := []string{fmt.Sprintf("complete=%t", complete)}
			for file, targets := range m {
				for _, target := range targets {
					out = append(out, "adjimport "+file+"->"+target)
				}
			}
			return out
		}},
		{name: "FrameworkCensusEdgesSeq", run: func(t *testing.T, s *Store) []string {
			var out []string
			for row := range s.FrameworkCensusEdgesSeq(graph.EdgeCalls, graph.EdgeReferences) {
				out = append(out, fmt.Sprintf("census %s->%s|%s", row.From, row.To, row.Kind))
			}
			return out
		}},
		{name: "ScanOverrideDispatchCalls", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanOverrideDispatchCalls([]string{genReadRepo}, 8, func(page []graph.OverrideDispatchCall) bool {
				for _, row := range page {
					out = append(out, fmt.Sprintf("dispatch %s->%s", row.From, row.To))
				}
				return true
			})
			return out
		}, identicalOK: "the fixture holds no Java/PHP caller, so both handles project nothing. The probe is retained so the query keeps compiling and running against a generation-scoped store; its isolation rests on the same predicate the other edge projections assert."},
		{name: "ScanTestProjections", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanTestNodeProjections([]graph.NodeKind{graph.KindFunction}, 8, func(page []graph.TestNodeProjection) bool {
				for _, row := range page {
					out = append(out, "testnode "+row.ID+"|"+row.Name)
				}
				return true
			})
			s.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 8, func(page []graph.TestEdgeProjection) bool {
				for _, row := range page {
					out = append(out, "testedge "+row.From+"->"+row.To)
				}
				return true
			})
			return out
		}},
		{name: "ScanTestCallProjections", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanTestCallProjections([]string{genReadShared}, 8, 8, func(page []graph.TestEdgeProjection) bool {
				for _, row := range page {
					out = append(out, "testcall "+row.From+"->"+row.To)
				}
				return true
			})
			return out
		}},
		{name: "ScanReceiverMutation", run: func(t *testing.T, s *Store) []string {
			var out []string
			s.ScanReceiverMutationMethods(8, func(page []graph.ReceiverMutationMethod) bool {
				for _, row := range page {
					out = append(out, "recvmethod "+row.ID)
				}
				return true
			})
			s.ScanReceiverMutationWrites(8, func(page []graph.ReceiverMutationWrite) bool {
				for _, row := range page {
					out = append(out, "recvwrite "+row.From+"->"+row.To)
				}
				return true
			})
			return out
		}, identicalOK: "receiver-mutation rows require receiver metadata the fixture deliberately does not carry, so both handles project nothing. The keyset walk itself is generation-bound; the leak assertion is what this entry preserves."},
		{name: "GetDataflowEdges", run: func(t *testing.T, s *Store) []string {
			out := edgeSliceMapTokens(s.GetDataflowCallEdgesByCallerIDs(genReadProbeIDs()))
			return append(out, edgeSliceMapTokens(s.GetDataflowParamEdgesByOwnerIDs(genReadProbeIDs()))...)
		}, identicalOK: "the fixture carries no dataflow param/call edge kinds, so both handles project nothing; the entry keeps the generation-bound keyset query exercised."},
		{name: "LSPRepoProjections", run: func(t *testing.T, s *Store) []string {
			totals, unstamped := s.LSPRepoFileCounts(genReadRepo, []string{"go"})
			out := append(sortedCountTokens("lsptotal", totals), sortedCountTokens("lsppending", unstamped)...)
			out = append(out, nodeTokens(s.LSPRepoNodesByFiles(genReadRepo, []string{"go"}, []string{genReadFileA}, false))...)
			out = append(out, edgeTokens(s.LSPRepoConfirmableEdgesByFiles(genReadRepo, []string{"go"}, []string{genReadFileA}, false))...)
			out = append(out, edgeTokens(s.LSPRepoEdgesByFilesAndKinds(genReadRepo, []string{"go"}, []string{genReadFileA}, []graph.EdgeKind{graph.EdgeCalls}))...)
			// Only non-zero fan-in rows are rendered: the query returns one row
			// per REQUESTED id, so a zero row would echo the caller's own
			// argument back into the token set rather than report a read.
			for _, token := range sortedCountTokens("lspfanin", s.LSPNodeFanInCounts(genReadProbeIDs())) {
				if !strings.HasSuffix(token, "=0") {
					out = append(out, token)
				}
			}
			out = append(out, edgeTokens(s.LSPInEdgesByNodeIDsAndKinds(genReadProbeIDs(), []graph.EdgeKind{graph.EdgeCalls}))...)
			return out
		}},
		{name: "RepoProjections", run: func(t *testing.T, s *Store) []string {
			var out []string
			for _, row := range s.RepoLanguageFileCounts([]string{genReadRepo}) {
				out = append(out, fmt.Sprintf("langfile %s|%s|%s=%d", row.RepoPrefix, row.FilePath, row.Language, row.Count))
			}
			for repo, byLang := range s.RepoLanguageCounts([]string{genReadRepo, "extra"}) {
				out = append(out, sortedCountTokens("langcount "+repo, byLang)...)
			}
			out = append(out, prefixed("repoid ", s.RepoNodeIDsByKinds([]string{genReadRepo}, []graph.NodeKind{graph.KindFunction}))...)
			out = append(out, prefixed("repofile ", s.RepoFilePaths(genReadRepo, "", []string{"go"}, nil))...)
			out = append(out, nodeTokens(s.RepoNodesByKindsWithMetaKey(genReadRepo, "", []graph.NodeKind{graph.KindInterface}, "methods"))...)
			for _, row := range s.RepoEdgesByKinds([]string{genReadRepo}, []graph.EdgeKind{graph.EdgeCalls}) {
				out = append(out, "repoedge "+row.RepoPrefix+" ")
				out = append(out, edgeTokens([]*graph.Edge{row.Edge})...)
			}
			return out
		}},
		{name: "PrefixDiagnostics", run: func(t *testing.T, s *Store) []string {
			d := s.PrefixDiagnostics(8)
			return []string{fmt.Sprintf("prefixdiag scanned=%d owned=%d unowned=%d misprefixed=%d",
				d.Scanned, d.OwnedCodeNodes, d.UnownedCodeNodes, d.MisprefixedNodes)}
		}},
		{name: "HasLanguage", run: func(t *testing.T, s *Store) []string {
			return []string{fmt.Sprintf("go=%t markdown=%t", s.HasLanguage("go"), s.HasLanguage("markdown"))}
		}, identicalOK: "both generations carry both languages; the probe exists so the recursive repo-prefix walk is exercised under a pinned generation."},
		{name: "AuditStructuralIntegrity", run: func(t *testing.T, s *Store) []string {
			audit, err := s.AuditStructuralIntegrity(context.Background(), graph.StructuralIntegrityAuditOptions{SampleLimit: 8})
			if err != nil {
				t.Fatalf("AuditStructuralIntegrity: %v", err)
			}
			return []string{fmt.Sprintf("audit rows=%d groups=%d", audit.TotalRows, len(audit.Groups))}
		}, identicalOK: "the fixture holds no structurally invalid edge, so the audit is empty in both generations. It is here because the LEFT JOIN it drives had to gain a generation pairing to keep a dangling edge dangling."},

		// The query strings carry a marker of their own, so the tokens label
		// them by position instead of echoing them: the interesting fact is
		// which query each handle can answer, not what was asked.
		{name: "SearchSymbols", run: func(t *testing.T, s *Store) []string {
			var out []string
			for i, q := range []string{"Shared" + genZeroMark, "Shared" + genOneMark, "Only" + genZeroMark, "Only" + genOneMark} {
				hits, err := s.SearchSymbols(q, 16)
				if err != nil {
					t.Fatalf("SearchSymbols: %v", err)
				}
				for _, h := range hits {
					out = append(out, fmt.Sprintf("hit q%d -> %s", i, h.NodeID))
				}
			}
			return out
		}},
		{name: "SearchSymbolBundles", run: func(t *testing.T, s *Store) []string {
			var out []string
			for _, q := range []string{"Shared" + genZeroMark, "Shared" + genOneMark} {
				bundles, err := s.SearchSymbolBundles(q, 16)
				if err != nil {
					t.Fatalf("SearchSymbolBundles: %v", err)
				}
				for _, b := range bundles {
					out = append(out, nodeTokens([]*graph.Node{b.Node})...)
					out = append(out, edgeTokens(b.OutEdges)...)
					out = append(out, edgeTokens(b.InEdges)...)
				}
			}
			return out
		}},
		{name: "SearchContent", run: func(t *testing.T, s *Store) []string {
			var out []string
			for i, q := range []string{"documented", genZeroMark, genOneMark} {
				hits, err := s.SearchContent(q, "", 16)
				if err != nil {
					t.Fatalf("SearchContent: %v", err)
				}
				for _, h := range hits {
					out = append(out, fmt.Sprintf("content q%d -> %s", i, h.NodeID))
				}
			}
			return out
		}},
		{name: "ScanContent", run: func(t *testing.T, s *Store) []string {
			var out []string
			err := s.ScanContent("", func(nodeID, filePath, body string) bool {
				out = append(out, "scan "+nodeID+"|"+body)
				return true
			})
			if err != nil {
				t.Fatalf("ScanContent: %v", err)
			}
			return out
		}},
		{name: "SymbolFTSCount", run: func(t *testing.T, s *Store) []string {
			n, err := s.SymbolFTSCount()
			if err != nil {
				t.Fatalf("SymbolFTSCount: %v", err)
			}
			return []string{fmt.Sprintf("ftscount=%d", n)}
		}},
	}
}

func prefixed(prefix string, values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, prefix+v)
	}
	return out
}

func sortedCountTokens[K ~string](label string, m map[K]int) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s %s=%d", label, string(k), v))
	}
	sort.Strings(out)
	return out
}

func identityProjectionTokens(p graph.BoundedEdgeIdentityProjection) []string {
	var out []string
	for endpoint, ids := range p.ByEndpoint {
		for _, id := range ids {
			out = append(out, fmt.Sprintf("bounded %s %s->%s|%s", endpoint, id.From, id.To, id.Kind))
		}
	}
	return out
}

func crossRepoTokens(rows []graph.CrossRepoCandidateRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("xcand %s->%s|%s|%s|%s", r.Edge.From, r.Edge.To, r.Edge.Kind, r.Edge.Origin, r.FromRepo+"/"+r.ToRepo))
	}
	sort.Strings(out)
	return out
}

// TestGenerationReadIsolation is the fence itself.
func TestGenerationReadIsolation(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	for _, probe := range generationReadProbes() {
		t.Run(probe.name, func(t *testing.T) {
			zero := renderProbe(t, base, probe)
			one := renderProbe(t, derived, probe)
			if strings.Contains(zero, genOneMark) {
				t.Fatalf("base handle leaked generation 1:\n%s", zero)
			}
			if strings.Contains(one, genZeroMark) {
				t.Fatalf("generation-1 handle leaked the base corpus:\n%s", one)
			}
			if zero == one {
				if probe.identicalOK != "" {
					return
				}
				t.Fatalf("both handles produced identical output, so the probe proves nothing:\n%s", zero)
			}
		})
	}
}

func renderProbe(t *testing.T, s *Store, probe genProbe) string {
	t.Helper()
	tokens := probe.run(t, s)
	sorted := append([]string(nil), tokens...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n")
}

// TestGenerationCursorIsolation pins the high-water and keyset cursors
// separately. They are the reads most likely to look correct while being
// wrong: a cursor that freezes a global boundary walks the right rows for the
// wrong corpus, and the paged output can still be non-empty.
func TestGenerationCursorIsolation(t *testing.T) {
	base, derived := openGenerationReadPair(t)

	t.Run("edge_kind_high_water", func(t *testing.T) {
		zero, ok := base.edgeKindHighWater([]string{string(graph.EdgeCalls)})
		if !ok {
			t.Fatal("base high water missing")
		}
		one, ok := derived.edgeKindHighWater([]string{string(graph.EdgeCalls)})
		if !ok {
			t.Fatal("derived high water missing")
		}
		if zero >= one {
			t.Fatalf("generation-1 rows are written after generation 0's, so its high water must be larger: base=%d derived=%d", zero, one)
		}
		maxZero := maxEdgeIDAtGeneration(t, base, baseViewGeneration)
		if zero != maxZero {
			t.Fatalf("base high water = %d, want the largest generation-0 edge id %d", zero, maxZero)
		}
	})

	t.Run("unresolved_scan_high_water", func(t *testing.T) {
		zero, err := base.BeginUnresolvedEdgeScan(context.Background())
		if err != nil {
			t.Fatalf("base scan: %v", err)
		}
		one, err := derived.BeginUnresolvedEdgeScan(context.Background())
		if err != nil {
			t.Fatalf("derived scan: %v", err)
		}
		if zero.HighWaterID >= one.HighWaterID {
			t.Fatalf("unresolved high water did not follow the generation: base=%d derived=%d", zero.HighWaterID, one.HighWaterID)
		}
	})

	t.Run("node_search_key_keyset", func(t *testing.T) {
		for _, tc := range []struct {
			store *Store
			mark  string
		}{{base, genZeroMark}, {derived, genOneMark}} {
			var seen []string
			err := tc.store.ScanNodeSearchKeys(context.Background(), 2, func(page []graph.NodeSearchKey) bool {
				for _, k := range page {
					seen = append(seen, k.ID+"|"+k.Name)
				}
				return true
			})
			if err != nil {
				t.Fatalf("scan keys (%s): %v", tc.mark, err)
			}
			joined := strings.Join(seen, "\n")
			if strings.Contains(joined, oppositeMark(tc.mark)) {
				t.Fatalf("keyset walk at %s leaked the other generation:\n%s", tc.mark, joined)
			}
			if !strings.Contains(joined, tc.mark) {
				t.Fatalf("keyset walk at %s returned none of its own rows:\n%s", tc.mark, joined)
			}
		}
	})

	t.Run("scoped_edge_max_id", func(t *testing.T) {
		for _, tc := range []struct {
			store *Store
			mark  string
		}{{base, genZeroMark}, {derived, genOneMark}} {
			var seen []string
			for row := range tc.store.EdgesInScopeSeq([]string{genReadRepo}, nil, graph.EdgeCalls) {
				seen = append(seen, row.Edge.From+"->"+row.Edge.To)
			}
			joined := strings.Join(seen, "\n")
			if strings.Contains(joined, oppositeMark(tc.mark)) {
				t.Fatalf("scoped edge cursor at %s leaked the other generation:\n%s", tc.mark, joined)
			}
			if !strings.Contains(joined, tc.mark) {
				t.Fatalf("scoped edge cursor at %s returned none of its own rows:\n%s", tc.mark, joined)
			}
		}
	})
}

func maxEdgeIDAtGeneration(t *testing.T, s *Store, gen int64) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ?`, gen).Scan(&id); err != nil {
		t.Fatalf("max edge id at generation %d: %v", gen, err)
	}
	return id
}

// TestGenerationFTSIsolation asserts both directions explicitly. The two FTS5
// virtual tables are shared across generations — a docid names one row of one
// table no matter which generation wrote it — so the only thing separating the
// corpora is the rowid map the MATCH joins back through.
func TestGenerationFTSIsolation(t *testing.T) {
	base, derived := openGenerationReadPair(t)

	for _, tc := range []struct {
		name  string
		store *Store
		mark  string
	}{
		{"base", base, genZeroMark},
		{"derived", derived, genOneMark},
	} {
		// Both the own-generation and the other generation's query text are
		// driven through each handle. The FTS matcher is prefix-OR over
		// camelCase-split tokens, so "OnlyGenOne" shares its leading tokens
		// with "OnlyGenZero" and a foreign query legitimately still matches
		// this generation's row — what must never happen is a hit that names
		// the other generation's node.
		t.Run(tc.name+"_symbols", func(t *testing.T) {
			for _, query := range []string{"Only" + tc.mark, "Only" + oppositeMark(tc.mark), "Only"} {
				hits, err := tc.store.SearchSymbols(query, 16)
				if err != nil {
					t.Fatalf("search %q: %v", query, err)
				}
				for _, hit := range hits {
					if strings.Contains(hit.NodeID, oppositeMark(tc.mark)) {
						t.Fatalf("symbol search %q leaked %s: %+v", query, oppositeMark(tc.mark), hit)
					}
				}
			}
			own, err := tc.store.SearchSymbols("Only"+tc.mark, 16)
			if err != nil {
				t.Fatalf("search own: %v", err)
			}
			if len(own) != 1 || !strings.Contains(own[0].NodeID, tc.mark) {
				t.Fatalf("search must return exactly this generation's own symbol, got %+v", own)
			}
		})

		t.Run(tc.name+"_content", func(t *testing.T) {
			for _, query := range []string{tc.mark, oppositeMark(tc.mark), "documented"} {
				hits, err := tc.store.SearchContent(query, "", 16)
				if err != nil {
					t.Fatalf("content search %q: %v", query, err)
				}
				for _, hit := range hits {
					if strings.Contains(hit.NodeID, oppositeMark(tc.mark)) || strings.Contains(hit.Snippet, oppositeMark(tc.mark)) {
						t.Fatalf("content search %q leaked %s: %+v", query, oppositeMark(tc.mark), hit)
					}
				}
			}
			own, err := tc.store.SearchContent("documented", "", 16)
			if err != nil {
				t.Fatalf("content search own: %v", err)
			}
			if len(own) != 1 || !strings.Contains(own[0].NodeID, tc.mark) {
				t.Fatalf("content search must return exactly this generation's own section, got %+v", own)
			}
		})
	}

	t.Run("bundle_cache_key", func(t *testing.T) {
		// Warm the base handle's bundle cache, then ask the derived handle for
		// the same node id. A cache keyed by id alone answers with the base
		// bundle; keyed by generation it misses and recomputes.
		base.SetBundleFingerprints(map[string]uint64{bundlePackageKey(genReadFileA): 1})
		derived.SetBundleFingerprints(map[string]uint64{bundlePackageKey(genReadFileA): 1})
		if _, err := base.SearchSymbolBundles("Shared"+genZeroMark, 4); err != nil {
			t.Fatalf("warm base bundles: %v", err)
		}
		bundles, err := derived.SearchSymbolBundles("Shared"+genOneMark, 4)
		if err != nil {
			t.Fatalf("derived bundles: %v", err)
		}
		if len(bundles) == 0 {
			t.Fatal("derived bundle search returned nothing")
		}
		for _, b := range bundles {
			if b.Node == nil {
				continue
			}
			if strings.Contains(b.Node.Name, genZeroMark) {
				t.Fatalf("bundle cache served the base corpus to a derived handle: %+v", b.Node)
			}
		}
	})
}

// TestGenerationMaskIsolation extends the fence to the ownership masks. They
// are not payload, so the probe table above cannot carry them: the base
// generation is not merely empty of masks, it REFUSES to hold any, and the
// symmetric half of the fence therefore runs between two derived generations.
func TestGenerationMaskIsolation(t *testing.T) {
	base, one := openGenerationReadPair(t)
	two := base.AtGeneration(2)
	if two == nil {
		t.Fatal("AtGeneration(2) returned nil")
	}

	// Each derived generation claims one shared path plus a set of rows marked
	// with its own name, so a read that dropped its generation predicate
	// surfaces the other generation's mark.
	masked := []struct {
		store *Store
		mark  string
		other string
	}{
		{one, "MaskGenOne", "MaskGenTwo"},
		{two, "MaskGenTwo", "MaskGenOne"},
	}
	for _, tc := range masked {
		if err := tc.store.SetFileMasks([]FileMask{
			{RepoPrefix: genReadRepo, FilePath: genReadFileA, Mode: OwnershipReplace},
			{RepoPrefix: genReadRepo, FilePath: "repo::pkg/" + tc.mark + ".go", Mode: OwnershipDelete},
		}); err != nil {
			t.Fatalf("SetFileMasks at generation %d: %v", tc.store.ViewGeneration(), err)
		}
		if err := tc.store.SetNodeTombstones([]string{"repo::pkg/a.go::" + tc.mark}); err != nil {
			t.Fatalf("SetNodeTombstones at generation %d: %v", tc.store.ViewGeneration(), err)
		}
		if err := tc.store.SetEdgeSourceMasks([]EdgeSourceMask{
			{SourceID: "repo::pkg/a.go::Source" + tc.mark, Mode: OwnershipReplace},
		}); err != nil {
			t.Fatalf("SetEdgeSourceMasks at generation %d: %v", tc.store.ViewGeneration(), err)
		}
		if err := tc.store.SetProducerState(ProducerCompleteness{
			Producer: "resolver", State: ProducerStateIncomplete, Reason: "stopped at " + tc.mark,
		}); err != nil {
			t.Fatalf("SetProducerState at generation %d: %v", tc.store.ViewGeneration(), err)
		}
	}

	// Neither derived generation may see the other's claims, in either
	// direction — enumerated or point-read.
	for _, tc := range masked {
		rendered := renderMaskState(t, tc.store)
		if strings.Contains(rendered, tc.other) {
			t.Fatalf("generation %d leaked %s masks:\n%s", tc.store.ViewGeneration(), tc.other, rendered)
		}
		if !strings.Contains(rendered, tc.mark) {
			t.Fatalf("generation %d cannot read its own masks:\n%s", tc.store.ViewGeneration(), rendered)
		}
		mode, ok, err := tc.store.FileMaskFor(genReadRepo, "repo::pkg/"+tc.other+".go")
		if err != nil || ok {
			t.Fatalf("generation %d point-read the other generation's mask: %q, %v, %v",
				tc.store.ViewGeneration(), mode, ok, err)
		}
	}

	// The base generation sees none of it and cannot write any of its own.
	if rendered := renderMaskState(t, base); rendered != "" {
		t.Fatalf("base handle sees derived masks:\n%s", rendered)
	}
	if err := base.SetFileMasks([]FileMask{
		{RepoPrefix: genReadRepo, FilePath: genReadFileA, Mode: OwnershipReplace},
	}); !errors.Is(err, ErrMasksAtBaseGeneration) {
		t.Fatalf("base handle mask write = %v, want ErrMasksAtBaseGeneration", err)
	}
}

// renderMaskState renders every mask a handle can read to sorted text, so one
// substring test covers a leaked row, a leaked key and a leaked payload column
// alike — the same shape the probe renderer above uses.
func renderMaskState(t *testing.T, s *Store) string {
	t.Helper()
	var tokens []string
	masks, err := s.FileMasks()
	if err != nil {
		t.Fatalf("FileMasks: %v", err)
	}
	for _, mask := range masks {
		tokens = append(tokens, fmt.Sprintf("file %s|%s|%s", mask.RepoPrefix, mask.FilePath, mask.Mode))
	}
	tombstones, err := s.NodeTombstones()
	if err != nil {
		t.Fatalf("NodeTombstones: %v", err)
	}
	for _, id := range tombstones {
		tokens = append(tokens, "tombstone "+id)
	}
	sources, err := s.EdgeSourceMasks()
	if err != nil {
		t.Fatalf("EdgeSourceMasks: %v", err)
	}
	for _, source := range sources {
		tokens = append(tokens, fmt.Sprintf("source %s|%s", source.SourceID, source.Mode))
	}
	states, err := s.ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	for _, state := range states {
		tokens = append(tokens, fmt.Sprintf("producer %s|%s|%s", state.Producer, state.State, state.Reason))
	}
	sort.Strings(tokens)
	return strings.Join(tokens, "\n")
}

// --- capability checklist -------------------------------------------------

// capabilityCase pairs one optional interface the package asserts *Store
// satisfies with either the probe that exercises it or the reason it has none.
type capabilityCase struct {
	iface any    // (*graph.X)(nil) — the compile-time proof the name is real
	probe string // a probe name from generationReadProbes
	skip  string // required when probe is empty
	// writeFence names the write-side test standing in for a probe. It is
	// required by (and only by) skipWrite, and is checked against
	// generationWriteFenceNames so a skip cannot cite a test nothing runs.
	writeFence string
}

const (
	skipWrite    = "mutating surface: its generation scoping is proved by the write-side fence in store_generation_write_test.go, not by a read"
	skipSidecar  = "reads a v15 generation-keyed payload sidecar rather than nodes/edges; the leading view_gen primary key is its isolation and the sidecar schema fences assert it"
	skipAdmin    = "repository administration or store lifecycle, generation-unscoped by design (see EvictRepoAllGenerations / PurgeRepo)"
	skipInMemory = "answers from in-process state, not from a SQL read"
)

// writerFamilyFence names one row of the write fence's writer-family table.
func writerFamilyFence(caseName string) string {
	return "TestGenerationScopedWriterFamilies/" + caseName
}

func generationCapabilityChecklist() []capabilityCase {
	return []capabilityCase{
		// graph.Store's read methods each have their own probe above; the entry
		// names one so the checklist stays uniform.
		{iface: (*graph.Store)(nil), probe: "AllNodes"},
		{iface: (*graph.AllGenerationsRepoEvicter)(nil), skip: skipAdmin},
		{iface: (*graph.AnalysisGenerationStore)(nil), skip: skipSidecar},
		{iface: (*graph.AnalysisQueryStore)(nil), skip: skipSidecar},
		{iface: (*graph.AtomicVectorCorpusInstaller)(nil), skip: skipSidecar},
		{iface: (*graph.BFSCapable)(nil), probe: "BFS"},
		{iface: (*graph.BlameEnrichmentReader)(nil), skip: skipSidecar},
		{iface: (*graph.BlameEnrichmentWriter)(nil), skip: skipSidecar},
		{iface: (*graph.BoundedEdgeExistenceReader)(nil), probe: "FindExistingEdgeEndpoints"},
		{iface: (*graph.BoundedExactNameReader)(nil), probe: "FindNodesByNameBounded"},
		{iface: (*graph.BoundedFileNodeReader)(nil), probe: "FindFileNodesBounded"},
		{iface: (*graph.BoundedIncomingEdgeIdentityReader)(nil), probe: "FindIncomingEdgeIdentitiesBounded"},
		{iface: (*graph.BoundedIncomingSourceReader)(nil), probe: "FindIncomingSourcesBounded"},
		{iface: (*graph.BoundedOutgoingEdgeIdentityReader)(nil), probe: "FindOutgoingEdgeIdentitiesBounded"},
		{iface: (*graph.BoundedOutgoingSiteEdgeIdentityReader)(nil), probe: "FindOutgoingSiteEdgeIdentitiesBounded"},
		{iface: (*graph.BulkLoader)(nil), skip: "the cold-load bracket engages only on a provably empty store; its generation-0 emptiness gate is asserted by TestColdGraphStoreEmptyIgnoresDerivedGenerations"},
		{iface: (*graph.BundleFingerprintSink)(nil), skip: skipInMemory},
		{iface: (*graph.CallableBindingNodeSequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.ChurnEnrichmentReader)(nil), skip: skipSidecar},
		{iface: (*graph.ChurnEnrichmentWriter)(nil), skip: skipSidecar},
		{iface: (*graph.ClassHierarchyTraverser)(nil), probe: "ClassHierarchyTraverse"},
		{iface: (*graph.CloneCorpusInitialization)(nil), skip: skipSidecar},
		{iface: (*graph.CloneCorpusPager)(nil), skip: skipSidecar},
		{iface: (*graph.CloneCorpusRepoReplacer)(nil), skip: skipSidecar},
		{iface: (*graph.CloneCorpusSignatureWriter)(nil), skip: skipWrite, writeFence: writerFamilyFence("clone_signatures")},
		{iface: (*graph.CloneCorpusWriter)(nil), skip: skipSidecar},
		{iface: (*graph.CloneShingleReader)(nil), skip: skipSidecar},
		{iface: (*graph.CloneShingleWriter)(nil), skip: skipSidecar},
		{iface: (*graph.ConfigNodeBatchEvicter)(nil), skip: skipWrite, writeFence: writerFamilyFence("config_node_evict")},
		{iface: (*graph.ConstantValueReader)(nil), skip: skipSidecar},
		{iface: (*graph.ConstantValueRepoReplacer)(nil), skip: skipSidecar},
		{iface: (*graph.ConstantValueWriter)(nil), skip: skipSidecar},
		{iface: (*graph.ContentFTSBatchReplacer)(nil), skip: skipWrite, writeFence: writerFamilyFence("content_fts_replace")},
		{iface: (*graph.ContentNodeReader)(nil), probe: "GetRepoContentNodes"},
		{iface: (*graph.ContentSearcher)(nil), probe: "SearchContent"},
		{iface: (*graph.ContractNodeBatchEvicter)(nil), skip: skipWrite, writeFence: writerFamilyFence("contract_node_evict")},
		{iface: (*graph.ContractOwnerReplacer)(nil), skip: skipWrite, writeFence: writerFamilyFence("contract_owner_replace")},
		{iface: (*graph.ContractStateStore)(nil), skip: skipSidecar},
		{iface: (*graph.CoverageEnrichmentReader)(nil), skip: skipSidecar},
		{iface: (*graph.CoverageEnrichmentWriter)(nil), skip: skipSidecar},
		{iface: (*graph.CrossRepoCandidates)(nil), probe: "CrossRepoCandidates"},
		{iface: (*graph.CrossRepoEdgeAggregator)(nil), probe: "CrossRepoEdgeCounts"},
		{iface: (*graph.CrossRepoFlagMarker)(nil), skip: skipWrite, writeFence: writerFamilyFence("cross_repo_flags")},
		{iface: (*graph.CurrentGenerationRepoEvicter)(nil), skip: skipWrite, writeFence: "TestGenerationScopedFileEvict"},
		{iface: (*graph.DeadCodeCandidator)(nil), probe: "DeadCodeCandidates"},
		{iface: (*graph.DerivedContractReplacer)(nil), skip: skipWrite, writeFence: writerFamilyFence("derived_contract_replace")},
		{iface: (*graph.EdgeAdjacencyForKinds)(nil), probe: "EdgeAdjacencyForKinds"},
		{iface: (*graph.EdgeIdentityBatchFinder)(nil), probe: "FindEdgesByIdentities"},
		{iface: (*graph.EdgeKindCounter)(nil), probe: "EdgeKindCounts"},
		{iface: (*graph.EdgeKindEvicter)(nil), skip: skipWrite, writeFence: writerFamilyFence("edge_kind_evict")},
		{iface: (*graph.EdgeMetaBatchPersister)(nil), skip: skipWrite, writeFence: "TestGenerationScopedEdgeAttributeUpdate"},
		{iface: (*graph.EdgesByKindsBatchScanner)(nil), probe: "ScanEdgesByKindsBatched"},
		{iface: (*graph.EdgesByKindsScanner)(nil), probe: "EdgesByKinds"},
		{iface: (*graph.EdgeTerminalStampPersister)(nil), skip: skipWrite, writeFence: writerFamilyFence("edge_terminal_stamps")},
		{iface: (*graph.EnrichmentStateStore)(nil), skip: skipSidecar},
		{iface: (*graph.ExactEdgeBatchRemover)(nil), skip: skipWrite, writeFence: "TestGenerationScopedExactEdgeDelete"},
		{iface: (*graph.ExistingNodeIDFinder)(nil), probe: "ExistingNodeIDs"},
		{iface: (*graph.ExtractCandidatesScanner)(nil), probe: "ExtractCandidates"},
		{iface: (*graph.FileBatchEvicter)(nil), skip: skipWrite, writeFence: "TestGenerationScopedFileEvict"},
		{iface: (*graph.FileEditingContext)(nil), probe: "FileEditingContext"},
		{iface: (*graph.FileImportAggregator)(nil), probe: "FileImportCounts"},
		{iface: (*graph.FileImporters)(nil), probe: "FileImporters"},
		{iface: (*graph.FileLanguageNodeSequencer)(nil), probe: "NodesLightSeq"},
		{iface: (*graph.FileMetaPathReader)(nil), skip: skipSidecar},
		{iface: (*graph.FileMetaReader)(nil), skip: skipSidecar},
		{iface: (*graph.FileMetaRepoReplacer)(nil), skip: skipSidecar},
		{iface: (*graph.FileMetaWriter)(nil), skip: skipSidecar},
		{iface: (*graph.FileMtimeDeleter)(nil), skip: skipSidecar},
		{iface: (*graph.FileMtimeReader)(nil), skip: skipSidecar},
		{iface: (*graph.FileMtimeReplacer)(nil), skip: skipSidecar},
		{iface: (*graph.FileMtimeWriter)(nil), skip: skipSidecar},
		{iface: (*graph.FileNodeIdentitySequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.FileReceiptPager)(nil), skip: skipSidecar},
		{iface: (*graph.FileSubGraphCountReader)(nil), probe: "GetFileSubGraphCounts"},
		{iface: (*graph.FileSubGraphReader)(nil), probe: "GetFileSubGraph"},
		{iface: (*graph.FileSymbolNamesByPaths)(nil), probe: "FileSymbolNamesByPaths"},
		{iface: (*graph.FnValuePlaceholderScanner)(nil), probe: "FnValuePlaceholderEdges"},
		{iface: (*graph.FrameworkCensusEdgeSequencer)(nil), probe: "FrameworkCensusEdgesSeq"},
		{iface: (*graph.FrontierExpander)(nil), probe: "ExpandFrontier"},
		{iface: (*graph.GoMethodReceiverBatchRebinder)(nil), skip: skipWrite, writeFence: "TestGenerationScopedReceiverRebind"},
		{iface: (*graph.GoMethodReceiverRebinder)(nil), skip: skipWrite, writeFence: "TestGenerationScopedReceiverRebind"},
		{iface: (*graph.IfaceImplementsScanner)(nil), probe: "IfaceImplementsRows"},
		{iface: (*graph.ImportAdjacencyProjector)(nil), probe: "ProjectImportAdjacency"},
		{iface: (*graph.InDegreeForNodes)(nil), probe: "InDegreeForNodes"},
		{iface: (*graph.InEdgeCounter)(nil), probe: "InEdgeCountsByKind"},
		{iface: (*graph.InEdgeIdentityBatchReader)(nil), probe: "GetInEdgeIdentitiesByNodeIDs"},
		{iface: (*graph.LightEdgeScanner)(nil), probe: "AllEdgesLight"},
		{iface: (*graph.LightEdgeSequencer)(nil), probe: "EdgesLightSeq"},
		{iface: (*graph.MemberMethodsByType)(nil), probe: "MemberMethodsByType"},
		{iface: (*graph.MutationReceiptStore)(nil), skip: skipInMemory},
		{iface: (*graph.MutationScopedCrossRepoCandidates)(nil), probe: "CrossRepoCandidatesForMutation"},
		{iface: (*graph.NamedLanguageNodeSequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.NodeDegreeAggregator)(nil), probe: "NodeDegreeCounts"},
		{iface: (*graph.NodeDegreeByKinds)(nil), probe: "NodeDegreeByKinds"},
		{iface: (*graph.NodeFanAggregator)(nil), probe: "NodeFanCounts"},
		{iface: (*graph.NodeIDNamesByKindsSequencer)(nil), probe: "NodeIDNamesByKindsSeq"},
		{iface: (*graph.NodeIDsByKinds)(nil), probe: "NodeIDsByKinds"},
		{iface: (*graph.NodeLightSequencer)(nil), probe: "NodesLightSeq"},
		{iface: (*graph.NodeNameClassCounter)(nil), probe: "CountNodesByNameClass"},
		{iface: (*graph.NodePlacementBatchReader)(nil), probe: "NodePlacementsByIDs"},
		{iface: (*graph.NodeSearchKeyScanner)(nil), probe: "ScanNodeSearchKeys"},
		{iface: (*graph.NodesByKindsScanner)(nil), probe: "NodesByKinds"},
		{iface: (*graph.NodesByKindsSequencer)(nil), probe: "NodesByKindsSeq"},
		{iface: (*graph.NodesInFilesByKindFinder)(nil), probe: "NodesInFilesByKind"},
		{iface: (*graph.OverrideDispatchCallBatchScanner)(nil), probe: "ScanOverrideDispatchCalls"},
		{iface: (*graph.QualifiedNodeIdentitySequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.ReachableForwardByKinds)(nil), probe: "ReachableForwardByKinds"},
		{iface: (*graph.ReceiverMutationScanner)(nil), probe: "ScanReceiverMutation"},
		{iface: (*graph.RefFactsReader)(nil), skip: skipSidecar},
		{iface: (*graph.RefFactsRebuilder)(nil), skip: skipWrite, writeFence: writerFamilyFence("ref_facts_rebuild")},
		{iface: (*graph.RefFactsWriter)(nil), skip: skipSidecar},
		{iface: (*graph.ReleaseEnrichmentReader)(nil), skip: skipSidecar},
		{iface: (*graph.ReleaseEnrichmentWriter)(nil), skip: skipSidecar},
		{iface: (*graph.RepoCapabilityEdgeScanner)(nil), probe: "ScanRepoCapabilityEdges"},
		{iface: (*graph.RepoEdgeKindReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.RepoFilePathReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.RepoLanguageCountReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.RepoLanguageFileCountReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.RepoLanguageNodeSummaryReader)(nil), probe: "GetRepoNodeSummariesByLanguage"},
		{iface: (*graph.RepoLanguageSymbolCounter)(nil), probe: "CountRepoLanguageSymbols"},
		{iface: (*graph.RepoLightNodeReader)(nil), probe: "RepoNodesLight"},
		{iface: (*graph.RepoMemoryEstimateScanner)(nil), probe: "ScanRepoMemoryEstimates"},
		{iface: (*graph.RepoMetaNodeReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.RepoNamesNodeFinder)(nil), probe: "FindNodesByNamesInRepo"},
		{iface: (*graph.RepoNodeIdentitySequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.RepoNodeKindIDReader)(nil), probe: "RepoProjections"},
		{iface: (*graph.ResolverNameScopeFinder)(nil), probe: "FindNodesByResolverNameScopes"},
		{iface: (*graph.ScopeBindingNodeSequencer)(nil), probe: "NodesInScopeSeq"},
		{iface: (*graph.ScopedCrossRepoCandidates)(nil), probe: "CrossRepoCandidatesForRepos"},
		{iface: (*graph.ScopedEdgeKindEvicter)(nil), skip: skipWrite, writeFence: writerFamilyFence("scoped_edge_kind_evict")},
		{iface: (*graph.ScopedProjectionSequencer)(nil), probe: "EdgesInScopeSeq"},
		{iface: (*graph.ScopedSymbolBundleSearcher)(nil), probe: "SearchSymbolBundles"},
		{iface: (*graph.SemanticBindingTypeStore)(nil), skip: skipSidecar},
		{iface: (*graph.SemanticNodeStampWriter)(nil), skip: skipWrite, writeFence: writerFamilyFence("semantic_node_stamps")},
		{iface: (*graph.StructuralIntegrityAuditor)(nil), probe: "AuditStructuralIntegrity"},
		{iface: (*graph.StructuralIntegrityEventRecorder)(nil), skip: skipInMemory},
		{iface: (*graph.StructuralIntegritySnapshotter)(nil), skip: skipInMemory},
		{iface: (*graph.StructuralParentEdges)(nil), probe: "StructuralParentEdges"},
		{iface: (*graph.SymbolBundleSearcher)(nil), probe: "SearchSymbolBundles"},
		{iface: (*graph.SymbolFTSBatchDeleter)(nil), skip: skipWrite, writeFence: writerFamilyFence("symbol_fts_batch_delete")},
		{iface: (*graph.SymbolFTSBatchUpserter)(nil), skip: skipWrite, writeFence: writerFamilyFence("symbol_fts_batch_upsert")},
		{iface: (*graph.SymbolFTSNormalizationState)(nil), skip: "symbol_fts_state is keyed by repository only; normalization is a property of the index build, not of a payload view"},
		{iface: (*graph.SymbolFTSRepoReplacer)(nil), skip: skipWrite, writeFence: writerFamilyFence("symbol_fts_repo_replace")},
		{iface: (*graph.SymbolFTSRepoResetter)(nil), skip: skipWrite, writeFence: writerFamilyFence("symbol_fts_repo_reset")},
		{iface: (*graph.SymbolSearcher)(nil), probe: "SearchSymbols"},
		{iface: (*graph.TestCallProjectionScanner)(nil), probe: "ScanTestCallProjections"},
		{iface: (*graph.TestProjectionScanner)(nil), probe: "ScanTestProjections"},
		{iface: (*graph.ThrowerErrorSurfacer)(nil), probe: "ThrowerErrorSurface"},
		{iface: (*graph.UnresolvedEdgeIdentityBatchScanner)(nil), probe: "ScanUnresolvedEdgeIdentitiesBatched"},
		{iface: (*graph.UnresolvedEdgePager)(nil), probe: "UnresolvedEdgePages"},
		{iface: (*graph.UnresolvedEdgeTargetBatchReindexer)(nil), skip: skipWrite, writeFence: writerFamilyFence("unresolved_target_reindex")},
		{iface: (*graph.UnresolvedFrontierCounter)(nil), probe: "CountUnresolvedFrontier"},
		{iface: (*graph.ValueRefPlaceholderScanner)(nil), probe: "ValueRefPlaceholderEdges"},
		{iface: (*graph.VectorSearcher)(nil), skip: skipSidecar},
		{iface: (*graph.WorkspaceSlugBackfiller)(nil), skip: skipWrite, writeFence: writerFamilyFence("workspace_slug_backfill")},
		{iface: (*graph.WorkspaceSlugImpactBackfiller)(nil), skip: skipWrite, writeFence: writerFamilyFence("workspace_slug_backfill_with_impact")},
	}
}

// TestGenerationCapabilityChecklistIsComplete reads every
// `var _ graph.X = (*Store)(nil)` assertion back out of the package source and
// requires the checklist to account for each one. Adding a capability without
// deciding what generation isolation means for it fails here.
func TestGenerationCapabilityChecklistIsComplete(t *testing.T) {
	probes := map[string]struct{}{}
	for _, p := range generationReadProbes() {
		probes[p.name] = struct{}{}
	}

	writeFences := generationWriteFenceNames()

	listed := map[string]struct{}{}
	storeType := reflect.TypeOf((*Store)(nil))
	for _, c := range generationCapabilityChecklist() {
		ifaceType := reflect.TypeOf(c.iface).Elem()
		name := ifaceType.Name()
		if _, dup := listed[name]; dup {
			t.Errorf("capability %s is listed twice", name)
		}
		listed[name] = struct{}{}
		if !storeType.Implements(ifaceType) {
			t.Errorf("capability %s: *Store does not implement it", name)
		}
		switch {
		case c.probe != "" && c.skip != "":
			t.Errorf("capability %s: names both a probe and a skip", name)
		case c.probe != "":
			if _, ok := probes[c.probe]; !ok {
				t.Errorf("capability %s: probe %q is not in generationReadProbes", name, c.probe)
			}
		case c.skip == "":
			t.Errorf("capability %s: needs either a probe or a stated skip reason", name)
		}
		// A mutating-surface skip is only as good as the test it defers to, so
		// the name it cites has to resolve to one the write fence really runs.
		switch {
		case c.skip == skipWrite && c.writeFence == "":
			t.Errorf("capability %s: a mutating-surface skip must name the write-fence test that covers it", name)
		case c.skip != skipWrite && c.writeFence != "":
			t.Errorf("capability %s: names a write fence but is not a mutating-surface skip", name)
		case c.writeFence != "":
			if _, ok := writeFences[c.writeFence]; !ok {
				t.Errorf("capability %s: write fence %q is not a test the write fence runs", name, c.writeFence)
			}
		}
	}

	asserted := storeCapabilityAssertions(t)
	for name := range asserted {
		if _, ok := listed[name]; !ok {
			t.Errorf("graph.%s is asserted on *Store but missing from the generation checklist", name)
		}
	}
	for name := range listed {
		if _, ok := asserted[name]; !ok {
			t.Errorf("checklist lists graph.%s, which the package no longer asserts on *Store", name)
		}
	}
}

// storeCapabilityAssertions parses the package's non-test sources and returns
// every interface named by a `var _ graph.X = (*Store)(nil)` declaration.
func storeCapabilityAssertions(t *testing.T) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || value.Names[0].Name != "_" || len(value.Values) != 1 {
					continue
				}
				iface, ok := graphInterfaceName(value.Type)
				if !ok || !isStoreNilConversion(value.Values[0]) {
					continue
				}
				out[iface] = struct{}{}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no graph capability assertions; the completeness check covers nothing")
	}
	return out
}

func graphInterfaceName(expr ast.Expr) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "graph" {
		return "", false
	}
	return sel.Sel.Name, true
}

func isStoreNilConversion(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	if ident, ok := call.Args[0].(*ast.Ident); !ok || ident.Name != "nil" {
		return false
	}
	paren, ok := call.Fun.(*ast.ParenExpr)
	if !ok {
		return false
	}
	star, ok := paren.X.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Store"
}

// TestColdGraphStoreEmptyIgnoresDerivedGenerations pins the bulk-load gate to
// generation 0. The cold path rebuilds the base corpus; a derived generation's
// rows are not what it is deciding about.
func TestColdGraphStoreEmptyIgnoresDerivedGenerations(t *testing.T) {
	base, err := Open(filepath.Join(t.TempDir(), "cold_gate.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })

	conn, err := base.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if !coldGraphStoreEmpty(context.Background(), conn) {
		t.Fatal("a fresh store must read as cold")
	}
	base.AtGeneration(1).AddBatch(genReadNodes(genOneMark), genReadEdges(genOneMark))
	if !coldGraphStoreEmpty(context.Background(), conn) {
		t.Fatal("generation-1 rows must not close the generation-0 cold gate")
	}
	base.AddBatch(genReadNodes(genZeroMark), nil)
	if coldGraphStoreEmpty(context.Background(), conn) {
		t.Fatal("generation-0 rows must close the cold gate")
	}
}
