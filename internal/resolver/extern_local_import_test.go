package resolver

import (
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestGoExternPrefersEligibleLocalPackageBeforeForeignDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name, language, localPath           string
		omitLocal, rejectLocal, wantForeign bool
	}{
		{name: "foreign_first", language: "go", localPath: "repo/pkg/p.go"},
		{name: "local_path_ineligible", language: "go", localPath: "repo/unimported/p.go", wantForeign: true},
		{name: "local_ownership_ineligible", language: "go", localPath: "repo/pkg/p.go", rejectLocal: true, wantForeign: true},
		{name: "foreign_only", language: "go", omitLocal: true, wantForeign: true},
		{name: "non_go_order_unchanged", language: "python", localPath: "repo/pkg/p.py", wantForeign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			caller := &graph.Node{ID: "repo/consumer/c.go::Call", Name: "Call", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/consumer/c.go", Language: tc.language}
			local := &graph.Node{ID: tc.localPath + "::Use", Name: "Use", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: tc.localPath, Language: tc.language}
			foreign := &graph.Node{ID: "fixture/pkg/p.go::Use", Name: "Use", Kind: graph.KindFunction, RepoPrefix: "fixture", FilePath: "fixture/pkg/p.go", Language: tc.language}
			nodes := []*graph.Node{caller, foreign}
			if !tc.omitLocal {
				nodes = append(nodes, local)
			}
			g.AddBatch(nodes, nil)
			r := New(g)
			e := &graph.Edge{From: caller.ID, To: "unresolved::extern::example.test/fixture/pkg::Use", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 5, CrossRepo: true}
			scope, _ := r.resolverNameScopeForEdge(e, "")
			candidates := []*graph.Node{foreign}
			if !tc.omitLocal {
				candidates = append(candidates, local)
			}
			// Global candidate order must not decide which duplicated package a Go
			// selector imports. Keep the foreign definition deliberately first.
			r.nodesByExternLanguageName = map[string]map[string][]*graph.Node{scope.languageKey: {"Use": candidates}}
			if tc.rejectLocal {
				r.SetGoPackageOwnership(func(q GoImportCandidate) GoPackageOwnershipResult {
					if q.CandidateID == local.ID {
						return GoPackageOwnershipDifferent
					}
					return GoPackageOwnershipUnknown
				})
			}
			var stats ResolveStats
			r.resolveExtern(e, "example.test/fixture/pkg::Use", &stats)
			want := local.ID
			if tc.wantForeign {
				want = foreign.ID
			}
			if e.To != want || e.CrossRepo != tc.wantForeign || stats.Resolved != 1 || stats.External != 0 || stats.Unresolved != 0 {
				t.Fatalf("edge=%+v stats=%+v want=%s cross_repo=%v", e, stats, want, tc.wantForeign)
			}
		})
	}
}
