package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

func TestGoPackageOwnershipImporterReplacementAndParentWorkspaceAreUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, importerExtra, candidateExtra, workspace string
		want                                           resolver.GoPackageOwnershipResult
	}{
		{"ordinary_nested_modules", "", "", "", resolver.GoPackageOwnershipDifferent},
		{"importer_local_replace", "replace example.test/imported => ../lib\n", "", "", resolver.GoPackageOwnershipUnknown},
		{"candidate_replace", "", "replace example.test/dep => ../other\n", "", resolver.GoPackageOwnershipUnknown},
		{"parent_workspace_replace", "", "", "go 1.23\nuse (\n ./app\n ./lib\n)\nreplace example.test/imported => ./lib\n", resolver.GoPackageOwnershipUnknown},
		{"parent_workspace_without_replace", "", "", "go 1.23\nuse (\n ./app\n ./lib\n)\n", resolver.GoPackageOwnershipUnknown},
		{"invalid_parent_workspace", "", "", "invalid workspace authority\n", resolver.GoPackageOwnershipUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builderIsolateGit(t)
			root := builderTempDir(t, "go-ownership-replace")
			builderGit(t, root, "init", "--initial-branch=main")
			files := map[string]string{
				"app/go.mod":   "module example.test/app\n\ngo 1.23\n" + tc.importerExtra,
				"app/main.go":  "package app\nfunc Call() {}\n",
				"lib/go.mod":   "module example.test/local\n\ngo 1.23\n" + tc.candidateExtra,
				"lib/pkg/a.go": "package pkg\nfunc Use() {}\n",
			}
			if tc.workspace != "" {
				files["go.work"] = tc.workspace
			}
			builderWriteTree(t, root, files)
			builderGit(t, root, "add", "-A")
			builderGit(t, root, "commit", "-m", "module authority boundary")
			tree := builderGit(t, root, "rev-parse", "HEAD^{tree}")
			target, err := source.NewGitTreeSource(t.Context(), root, tree)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = target.Close() })
			var nodes []*graph.Node
			for _, file := range []string{"app/main.go", "lib/pkg/a.go"} {
				nodes = append(nodes, &graph.Node{ID: builderRepoPrefix + "/" + file, Kind: graph.KindFile, FilePath: builderRepoPrefix + "/" + file, RepoPrefix: builderRepoPrefix, Language: "go"})
			}
			seq := func(yield func(*graph.Node) bool) {
				for _, node := range nodes {
					if !yield(node) {
						return
					}
				}
			}
			lookup, err := buildGoPackageOwnership(t.Context(), target, builderRepoPrefix, seq)
			if err != nil || lookup == nil {
				t.Fatalf("prepare: lookup=%v err=%v", lookup != nil, err)
			}
			query := resolver.GoImportCandidate{
				ImportPath: "example.test/imported/pkg", ImporterRepoPrefix: builderRepoPrefix, ImporterFilePath: nodes[0].FilePath,
				CandidateRepoPrefix: builderRepoPrefix, CandidateID: nodes[1].ID, CandidateFilePath: nodes[1].FilePath,
			}
			if got := lookup(query); got != tc.want {
				t.Fatalf("importer/workspace authority=%v want=%v", got, tc.want)
			}
			// Unknown authority must stay Unknown even for the canonical local
			// spelling; it is not an exact-match claim disguised as a fallback.
			query.ImportPath = "example.test/local/pkg"
			want := tc.want
			if want == resolver.GoPackageOwnershipDifferent {
				want = resolver.GoPackageOwnershipExact
			}
			if got := lookup(query); got != want {
				t.Fatalf("canonical spelling under authority=%v want=%v", got, want)
			}
		})
	}
}
