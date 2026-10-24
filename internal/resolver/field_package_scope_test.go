package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveFieldRefSameReceiverPackageScope(t *testing.T) {
	for _, tc := range []struct {
		name, callerFile, fieldFile, language, fieldLanguage, scope, fieldScope string
		match                                                                   bool
	}{
		{"other-folder", "a/type.go", "b/type.go", "go", "go", "", "", false},
		{"same-folder", "a/type.go", "a/field.go", "go", "go", "", "", true},
		{"swift-other-folder", "extensions/Node.swift", "main/Node.swift", "swift", "swift", "", "", true},
		{"shared-jvm-package", "test/Node.java", "main/Node.java", "java", "java", "com.example", "com.example", true},
		{"other-jvm-package", "test/Node.java", "main/Node.java", "java", "java", "com.example", "com.other", false},
		{"other-language", "test/Node.java", "main/Node.kt", "java", "kotlin", "com.example", "com.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			caller := &graph.Node{ID: "caller", Kind: graph.KindMethod, Name: "Update", FilePath: tc.callerFile, Language: tc.language,
				Meta: map[string]any{"receiver": "Node", "scope_pkg": tc.scope}}
			field := &graph.Node{ID: "field", Kind: graph.KindField, Name: "inner", FilePath: tc.fieldFile, Language: tc.fieldLanguage,
				Meta: map[string]any{"receiver": "Node", "scope_pkg": tc.fieldScope}}
			if tc.scope == "" {
				delete(caller.Meta, "scope_pkg")
			}
			if tc.fieldScope == "" {
				delete(field.Meta, "scope_pkg")
			}
			g.AddBatch([]*graph.Node{caller, field}, nil)
			edge := &graph.Edge{From: caller.ID, To: "unresolved::inner", Kind: graph.EdgeWrites, FilePath: caller.FilePath}
			stats := &ResolveStats{}
			got := New(g).resolveFieldRef(edge, "inner", stats)
			if got != tc.match {
				t.Fatalf("resolved=%v, want %v; target=%s", got, tc.match, edge.To)
			}
			if tc.match && (edge.To != field.ID || stats.Resolved != 1) {
				t.Fatalf("target=%s, resolved=%d", edge.To, stats.Resolved)
			}
			if !tc.match && (edge.To != "unresolved::inner" || stats.Resolved != 0) {
				t.Fatalf("rejected field changed edge: %#v, stats=%#v", edge, stats)
			}
		})
	}
}
