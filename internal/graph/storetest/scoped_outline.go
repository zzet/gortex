package storetest

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// RunScopedOutline compares a backend's scoped aggregates against the graph
// reference, including ownership fallback, false allow entries and externals.
func RunScopedOutline(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ag, ok := s.(graph.ScopedOutlineAggregator)
	if !ok {
		t.Skip("backend does not implement graph.ScopedOutlineAggregator")
	}
	nodes := []*graph.Node{
		{ID: "a/main", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/main.go"},
		{ID: "a/lib", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/lib.go"},
		{ID: "b/main", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "q", Language: "rust", FilePath: "b/main.rs"},
		{ID: "c/main", RepoPrefix: "c", WorkspaceID: "other", ProjectID: "p", Language: "python", FilePath: "c/main.py"},
		{ID: "legacy/main", RepoPrefix: "legacy", Language: "go", FilePath: "legacy/main.go"},
		{ID: "unowned", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "source.go"},
		{ID: "global", Language: "go", FilePath: "external::go:fmt"},
		{ID: "global-call", ProjectID: "ignored", FilePath: "external-call::symbol"},
		{ID: "empty-path"},
		{ID: "case-sensitive", FilePath: "External::source"},
		{ID: "external-with-workspace", WorkspaceID: "other", FilePath: "external::not-global"},
		{ID: "source-unowned", FilePath: "source.go"},
		{ID: "module-source", FilePath: "module::source"},
		{ID: "no-file", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go"},
	}
	edges := []*graph.Edge{
		{From: "a/main", To: "a/lib", Kind: graph.EdgeImports},
		{From: "b/main", To: "a/lib", Kind: graph.EdgeImports}, // target-only scope
		{From: "c/main", To: "a/lib", Kind: graph.EdgeImports},
		{From: "a/main", To: "global", Kind: graph.EdgeCalls},
		{From: "b/main", To: "global", Kind: graph.EdgeImports},
		{From: "a/main", To: "unowned", Kind: graph.EdgeImports},
		{From: "a/main", To: "b/main", Kind: graph.EdgeCalls},
		{From: "a/main", To: "no-file", Kind: graph.EdgeImports},
	}
	for _, n := range nodes {
		if strings.Contains(n.ID, "main") {
			n.Name = "main"
		}
	}
	s.AddBatch(nodes, edges)
	ref := graph.New()
	ref.AddBatch(nodes, edges)
	for name, scope := range map[string]graph.NodeScope{
		"unbound":           {},
		"repo":              {RepoAllow: map[string]bool{"a": true}},
		"workspace":         {WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true, "b": true}},
		"workspace-only":    {WorkspaceID: "ws"},
		"project":           {WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "b": true}},
		"project-with-repo": {ProjectID: "p", RepoAllow: map[string]bool{"a": true}},
		"project-alone":     {ProjectID: "p"},
		"fallback":          {WorkspaceID: "legacy", ProjectID: "legacy", RepoAllow: map[string]bool{"legacy": true}},
		"unknown":           {WorkspaceID: "unknown", RepoAllow: map[string]bool{"unknown": true}},
		"false-repo":        {RepoAllow: map[string]bool{"a": false}},
		"mixed-allow":       {RepoAllow: map[string]bool{"a": true, "b": false}},
	} {
		t.Run(name, func(t *testing.T) {
			if nodeReader, ok := s.(graph.ScopedOutlineNodeReader); ok {
				ids := make([]string, 0, len(nodes)+1)
				for _, n := range nodes {
					ids = append(ids, n.ID)
				}
				ids = append(ids, "missing")
				gotIDs, err := nodeReader.OutlineNodeIDsInScope(scope, ids)
				if err != nil {
					t.Fatal(err)
				}
				wantIDs, _ := ref.OutlineNodeIDsInScope(scope, ids)
				slices.Sort(gotIDs)
				slices.Sort(wantIDs)
				if !slices.Equal(gotIDs, wantIDs) {
					t.Fatalf("scoped membership=%v want %v", gotIDs, wantIDs)
				}
				gotNames, err := nodeReader.OutlineNodesByName(scope, "main")
				if err != nil {
					t.Fatal(err)
				}
				wantNames, _ := ref.OutlineNodesByName(scope, "main")
				if !slices.Equal(sortNodeIDs(gotNames), sortNodeIDs(wantNames)) {
					t.Fatalf("scoped name IDs=%v want %v", sortNodeIDs(gotNames), sortNodeIDs(wantNames))
				}
			}
			got, err := ag.ScopedOutlineCounts(scope)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ref.ScopedOutlineCounts(scope)
			if err != nil {
				t.Fatal(err)
			}
			sort.Slice(got.FileImports, func(i, j int) bool { return got.FileImports[i].FilePath < got.FileImports[j].FilePath })
			sort.Slice(want.FileImports, func(i, j int) bool { return want.FileImports[i].FilePath < want.FileImports[j].FilePath })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scoped outline = %+v, want %+v", got, want)
			}
			if name == "project" && (got.TotalNodes != 6 || got.TotalEdges != 4) {
				t.Fatalf("project counts = %+v, want 6 nodes, 4 edges", got)
			}
			if name == "project" {
				for _, row := range got.FileImports {
					if row.FilePath == "a/lib.go" && row.Count != 3 {
						t.Fatalf("imports must count all sources: %+v", row)
					}
				}
			}
		})
	}
}

func RunFilteredScopedOutline(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ag, ok := s.(graph.FilteredScopedOutlineAggregator)
	if !ok {
		t.Skip("backend does not implement graph.FilteredScopedOutlineAggregator")
	}
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", Language: "go", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", Language: "go", FilePath: "repo/b.go"},
		{ID: "foreign", Kind: graph.KindFunction, RepoPrefix: "foreign", FilePath: "foreign/a.go"},
		{ID: "contract", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/api.yaml"},
		{ID: "config", Kind: graph.KindConfigKey, RepoPrefix: "repo", FilePath: "repo/config.yaml"},
		{ID: "table", Kind: graph.KindTable, RepoPrefix: "repo", FilePath: "repo/schema.sql"},
	}
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls},
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 2, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a", To: "b", Kind: graph.EdgeImports, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "foreign", To: "b", Kind: graph.EdgeImports, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "foreign", To: "b", Kind: graph.EdgeImports, Line: 2},
		{From: "missing", To: "b", Kind: graph.EdgeImports},
		{From: "contract", To: "b", Kind: graph.EdgeImports},
		{From: "a", To: "config", Kind: graph.EdgeReadsConfig},
		{From: "a", To: "table", Kind: graph.EdgeProvides}, // shared kind remains visible
		{From: "a", To: "contract", Kind: graph.EdgeProvides},
		{From: "a", To: "b", Kind: graph.EdgeMatches},
	}
	s.AddBatch(nodes, edges)
	ref := graph.New()
	ref.AddBatch(nodes, edges)
	for name, filter := range map[string]graph.OutlineEdgeFilter{
		"none":                {},
		"core":                {ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches, graph.EdgeBridges, graph.EdgeHandlesRoute, graph.EdgeReadsConfig}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey}, ExcludeVia: "spring.Bean"},
		"explicit-identities": {ExcludeNodeIDs: map[string]bool{"b": true, "a": false}},
		"empty-identities":    {ExcludeNodeIDs: map[string]bool{}},
		"false-identities":    {ExcludeNodeIDs: map[string]bool{"a": false}},
		"empty-kind":          {ExcludeNodeKinds: []graph.NodeKind{""}},
	} {
		t.Run(name, func(t *testing.T) {
			scope := graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}
			got, err := ag.ScopedOutlineCountsFiltered(scope, filter)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ref.ScopedOutlineCountsFiltered(scope, filter)
			if err != nil {
				t.Fatal(err)
			}
			sort.Slice(got.FileImports, func(i, j int) bool { return got.FileImports[i].FilePath < got.FileImports[j].FilePath })
			sort.Slice(want.FileImports, func(i, j int) bool { return want.FileImports[i].FilePath < want.FileImports[j].FilePath })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filtered outline = %+v, want %+v", got, want)
			}
			if name == "core" && (got.TotalEdges != 2 || len(got.FileImports) != 1 || got.FileImports[0].Count != 2) {
				t.Fatalf("core counts = %+v, want two edges and two imports", got)
			}
		})
	}
}
