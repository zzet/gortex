package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveAll_PHPDeclaredStatic(t *testing.T) {
	for _, tc := range []struct {
		name, language, method                       string
		metadata, duplicate, declared, otherDeclared bool
	}{
		{name: "declared make", language: "php", method: "make", metadata: true, declared: true},
		{name: "declared create", language: "php", method: "create", metadata: true, declared: true},
		{name: "declared query", language: "php", method: "query", metadata: true, declared: true},
		{name: "finder unchanged", language: "php", method: "find", metadata: true},
		{name: "ambiguous imported class", language: "php", method: "make", metadata: true, duplicate: true, declared: true, otherDeclared: true},
		{name: "only other namespace declares", language: "php", method: "make", metadata: true, duplicate: true, otherDeclared: true},
		{name: "other language", language: "javascript", method: "make", metadata: true, declared: true},
		{name: "no metadata", language: "php", method: "make", declared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := graph.New()
			callerFile := "app/Services/Checkout.php"
			caller := callerFile + "::Checkout.total"
			s.AddNode(&graph.Node{ID: caller, Kind: graph.KindMethod, Name: "total", FilePath: callerFile, Language: tc.language})
			addType := func(file, namespace, name string, declared bool) string {
				id := file + "::" + name
				s.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: name, FilePath: file, Language: "php",
					Meta: map[string]any{"namespace": namespace}})
				methodID := id + "." + tc.method
				if declared {
					s.AddNode(&graph.Node{ID: methodID, Kind: graph.KindMethod, Name: tc.method, FilePath: file, Language: "php",
						Meta: map[string]any{"receiver": name}})
				}
				return methodID
			}
			want := addType("app/Values/Money.php", `Acme\Values`, "Money", tc.declared)
			if tc.duplicate {
				addType("app/Archive/Money.php", `Acme\Archive`, "Money", tc.otherDeclared)
				// Even with import evidence, ambiguity must never pick the other class.
				s.AddNode(&graph.Node{ID: callerFile + "::import::Money", Kind: graph.KindImport, Name: "Money", FilePath: callerFile, Language: "php",
					Meta: map[string]any{"import_path": `Acme\Values\Money`}})
			}
			addType("app/Support/Finder.php", `Acme\Support`, "Finder", true)
			meta := map[string]any{}
			if tc.metadata {
				meta["eloquent_model"], meta["eloquent_method"] = "Money", tc.method
			}
			s.AddEdge(&graph.Edge{From: caller, To: "unresolved::Money", Kind: graph.EdgeCalls, FilePath: callerFile, Line: 8, Meta: meta})

			New(s).ResolveAll()

			out := s.GetOutEdges(caller)
			require.Len(t, out, 1)
			if tc.declared && !tc.duplicate && tc.language == "php" && tc.metadata {
				assert.Equal(t, want, out[0].To, "declared static method must beat the Eloquent shortcut")
				assert.Equal(t, graph.OriginASTResolved, out[0].Origin)
				assert.Equal(t, 0.85, out[0].Confidence)
			} else {
				assert.Equal(t, "unresolved::Money", out[0].To, "existing fallback must remain unchanged")
			}
		})
	}
}
