package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

func TestGoPackageOwnershipUsesFullImmutableManifestAuthority(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "go-ownership")
	builderGit(t, root, "init", "--initial-branch=main")
	files := map[string]string{
		"go.mod":              "module example.test/root\n\ngo 1.24\n",
		"root.go":             "package root\n",
		"internal/graph/a.go": "package graph\n",
		"misc/graph/b.go":     "package graph\n",
		"nested/go.mod":       "module example.test/nested\n\ngo 1.24\n",
		"nested/graph/n.go":   "package graph\n",
		"bad/go.mod":          "module [invalid\n",
		"bad/graph/b.go":      "package graph\n",
		"huge/go.mod":         "module example.test/huge\n" + strings.Repeat("// padding\n", 7000),
		"huge/graph/h.go":     "package graph\n",
		"vendor/example.test/dependency/graph/v.go": "package graph\n",
		"internal/graph/not_go.py":                  "pass\n",
	}
	builderWriteTree(t, root, files)
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "target module authority")
	tree := builderGit(t, root, "rev-parse", "HEAD^{tree}")
	// Poison live manifests after the immutable target exists. Snapshot facts
	// must come from the retained Git source, not this working-copy content.
	builderWriteFile(t, root, "go.mod", "module wrong.test/live\n")
	builderWriteFile(t, root, "nested/go.mod", "module wrong.test/nested\n")
	ctx := t.Context()
	target, err := source.NewGitTreeSource(ctx, root, tree)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = target.Close()
		}
	})
	var nodes []*graph.Node
	var admitted []string
	for file := range files {
		language := "go"
		if strings.HasSuffix(file, ".py") {
			language = "python"
		} else if !strings.HasSuffix(file, ".go") {
			continue
		}
		nodes = append(nodes, &graph.Node{ID: builderRepoPrefix + "/" + file, Kind: graph.KindFile,
			FilePath: builderRepoPrefix + "/" + file, RepoPrefix: builderRepoPrefix, Language: language})
		admitted = append(admitted, file)
	}
	seq := func(yield func(*graph.Node) bool) {
		for _, node := range nodes {
			if !yield(node) {
				return
			}
		}
	}
	lookup, err := buildGoPackageOwnership(ctx, target, builderRepoPrefix, seq)
	if err != nil || lookup == nil {
		t.Fatalf("full target authority: %v", err)
	}
	// A parser-admission wrapper lacks a certified full regular-file reader.
	// It must not infer that excluded nested go.mod is absent and inherit root.
	narrowed, err := buildGoPackageOwnership(ctx, newFileSetSource(target, admitted), builderRepoPrefix, seq)
	if err != nil || narrowed == nil {
		t.Fatalf("narrow source conservative map: %v", err)
	}
	query := func(file, importPath string) resolver.GoImportCandidate {
		return resolver.GoImportCandidate{ImportPath: importPath, ImporterRepoPrefix: builderRepoPrefix,
			ImporterFilePath: builderRepoPrefix + "/root.go", CandidateRepoPrefix: builderRepoPrefix,
			CandidateID: builderRepoPrefix + "/" + file, CandidateFilePath: builderRepoPrefix + "/" + file}
	}
	if got := narrowed(query("nested/graph/n.go", "example.test/root/nested/graph")); got != resolver.GoPackageOwnershipUnknown {
		t.Fatalf("narrow parser inventory created authoritative inherited ownership: %v", got)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	for _, tc := range []struct {
		file, importPath string
		want             resolver.GoPackageOwnershipResult
	}{
		{"root.go", "example.test/root", resolver.GoPackageOwnershipExact},
		{"internal/graph/a.go", "example.test/root/internal/graph", resolver.GoPackageOwnershipExact},
		{"misc/graph/b.go", "example.test/root/internal/graph", resolver.GoPackageOwnershipDifferent},
		{"nested/graph/n.go", "example.test/nested/graph", resolver.GoPackageOwnershipExact},
		{"nested/graph/n.go", "example.test/root/nested/graph", resolver.GoPackageOwnershipDifferent},
		{"bad/graph/b.go", "example.test/root/bad/graph", resolver.GoPackageOwnershipUnknown},
		{"huge/graph/h.go", "example.test/root/huge/graph", resolver.GoPackageOwnershipUnknown},
		{"vendor/example.test/dependency/graph/v.go", "example.test/dependency/graph", resolver.GoPackageOwnershipUnknown},
		{"internal/graph/not_go.py", "example.test/root/internal/graph", resolver.GoPackageOwnershipUnknown},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.file, tc.importPath), func(t *testing.T) {
			if got := lookup(query(tc.file, tc.importPath)); got != tc.want {
				t.Fatalf("ownership=%v want=%v", got, tc.want)
			}
		})
	}
}

type canceledGoOwnershipSource struct {
	source.ContentSource
	cancel context.CancelFunc
}

func (s canceledGoOwnershipSource) ReadRegularFile(context.Context, string, int64) ([]byte, source.FileMeta, error) {
	s.cancel()
	return nil, source.FileMeta{}, context.Canceled
}

func TestGoPackageOwnershipCancellationPublishesNoPartialMap(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seq := func(yield func(*graph.Node) bool) {
		yield(&graph.Node{ID: "repo/pkg/a.go", Kind: graph.KindFile, FilePath: "repo/pkg/a.go", RepoPrefix: "repo", Language: "go"})
	}
	lookup, err := buildGoPackageOwnership(ctx, canceledGoOwnershipSource{cancel: cancel}, "repo", seq)
	if lookup != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("partial canceled authority: lookup=%v err=%v", lookup != nil, err)
	}
}
