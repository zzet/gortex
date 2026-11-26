package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/testutil/reporoot"
)

func TestIndexerShadowsForwardRejectedAttemptsToDurableOwner(t *testing.T) {
	tests := []struct {
		name string
		path graph.StructuralDropPath
	}{
		{name: "cold", path: graph.StructuralPathShadowCold},
		{name: "streaming", path: graph.StructuralPathShadowStreaming},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := graph.New()
			idx := &Indexer{repoPrefix: "repo-shadow"}
			shadow := idx.newStructuralIntegrityShadow(owner, tt.path)
			shadow.AddBatch(nil, []*graph.Edge{{
				From: "source", To: "target#param:x", Kind: graph.EdgeImplements, Origin: "LSP",
			}})
			// Simulate cancellation before the throwaway shadow crosses its durable
			// drain boundary: the shadow is discarded, but the attempted rejection
			// must already belong to the logical durable store.
			shadow = nil
			_ = shadow

			snapshot := owner.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
			if snapshot.Totals.WriteRejected != 1 || len(snapshot.Attribution) != 1 || len(snapshot.Samples) != 1 {
				t.Fatalf("rejected attempt did not survive shadow discard: %+v", snapshot)
			}
			got := snapshot.Attribution[0]
			if got.Repo != "repo-shadow" || got.Path != tt.path || got.Count != 1 {
				t.Fatalf("shadow attribution mismatch: %+v", got)
			}
		})
	}
}

// TestIndexCtxRawUsesIntegrityAwareColdAndStreamingShadows guards the actual
// staging call sites. The end-to-end indexCtxRaw paths require the full cold
// indexing pipeline (and streaming mode is intentionally disabled by default),
// so this AST assertion makes a regression back to graph.New fail at the two
// precise shadow assignments while the behavioral test above covers forwarding
// and aborted-attempt retention.
func TestIndexCtxRawUsesIntegrityAwareColdAndStreamingShadows(t *testing.T) {
	indexerPath := reporoot.Path(t, "internal", "indexer", "indexer.go")
	source, err := os.ReadFile(indexerPath)
	if err != nil {
		t.Fatalf("read indexer source: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), indexerPath, source, 0)
	if err != nil {
		t.Fatalf("parse indexer source: %v", err)
	}

	type expectedCall struct {
		owner string
		path  string
		count int
	}
	expected := map[string]*expectedCall{
		"inMemShadow": {owner: "diskTarget", path: "StructuralPathShadowCold"},
		"chunkShadow": {owner: "streamingDisk", path: "StructuralPathShadowStreaming"},
	}
	methodFound := false
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok || method.Name.Name != "indexCtxRaw" || method.Body == nil {
			continue
		}
		methodFound = true
		ast.Inspect(method.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assignment.Rhs {
				if i >= len(assignment.Lhs) {
					continue
				}
				lhs, ok := assignment.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				want, ok := expected[lhs.Name]
				if !ok {
					continue
				}
				call, ok := rhs.(*ast.CallExpr)
				if ok && integrityShadowCallMatches(call, want.owner, want.path) {
					want.count++
				}
			}
			return true
		})
	}
	if !methodFound {
		t.Fatal("indexCtxRaw method not found")
	}
	for assignment, want := range expected {
		if want.count != 1 {
			t.Fatalf("%s must be assigned exactly once from idx.newStructuralIntegrityShadow(%s, graph.%s); found %d", assignment, want.owner, want.path, want.count)
		}
	}
}

func integrityShadowCallMatches(call *ast.CallExpr, owner, path string) bool {
	if len(call.Args) != 2 {
		return false
	}
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "newStructuralIntegrityShadow" {
		return false
	}
	receiver, ok := method.X.(*ast.Ident)
	if !ok || receiver.Name != "idx" {
		return false
	}
	ownerArg, ok := call.Args[0].(*ast.Ident)
	if !ok || ownerArg.Name != owner {
		return false
	}
	pathArg, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok || pathArg.Sel.Name != path {
		return false
	}
	pathPackage, ok := pathArg.X.(*ast.Ident)
	return ok && pathPackage.Name == "graph"
}
