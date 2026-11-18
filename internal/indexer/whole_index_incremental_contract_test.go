package indexer

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// The contract between a whole index and the per-save path, per derived row
// class: every row one writes, the other writes identically.
//
//   - dataflow: a whole index lifts arg_of / returns_to placeholders exactly
//     as the per-save path does (materializeDataflowParams right after its
//     resolve);
//   - file-node fingerprints: a whole index stamps the extraction
//     fingerprints a per-save of the file stamps;
//   - deletion: the importers of a deleted file are re-derived from source,
//     so they carry the external terminals a whole index of the tree after
//     the deletion gives them.

const contractModule = "example.com/contract"

func contractTree() map[string]string {
	return map[string]string{
		"go.mod": "module " + contractModule + "\n\ngo 1.22\n",
		"p/p.go": `package p

func Take(x int) int {
	return x + 1
}
`,
		"q/q.go": `package q

import (
	"example.com/contract/p"
)

func Use(n int) int {
	return p.Take(n)
}
`,
		"r/r.go": `package r

import (
	"example.com/contract/q"
)

func Top() int {
	return q.Use(2)
}
`,
	}
}

func contractRepo(t *testing.T) string {
	t.Helper()
	builderIsolateGit(t)
	repo := builderTempDir(t, "repo")
	builderGit(t, repo, "init", "--initial-branch=main")
	builderWriteTree(t, repo, contractTree())
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "base")
	return repo
}

func TestWholeIndexLiftsDataflowParamsLikeThePerSavePath(t *testing.T) {
	repo := contractRepo(t)
	clean := builderOpenStore(t, "clean")
	builderIndex(t, clean, repo)
	const param = builderRepoPrefix + "/p/p.go::Take#param:x"
	if clean.GetNode(param) == nil {
		t.Fatalf("fixture has no param node %s", param)
	}
	lifted := false
	for _, e := range clean.GetInEdges(param) {
		if e.Kind == graph.EdgeArgOf {
			lifted = true
		}
	}
	if !lifted {
		t.Fatalf("a whole index left q's argument on Take instead of lifting it to %s", param)
	}

	// The per-save path over the same (unchanged) file writes the same rows.
	primary := primaryPerSaveOf(t, repo, []string{"q/q.go"}, config.Default().Index)
	if rec := replayCompare(primary, clean, 20); rec.differing() != 0 {
		t.Fatalf("per-save of an unchanged file differs from the whole index:\n  %s", strings.Join(rec.Sample, "\n  "))
	}
}

func TestWholeIndexStampsTheFileFingerprintsAPerSaveStamps(t *testing.T) {
	repo := contractRepo(t)
	clean := builderOpenStore(t, "clean")
	builderIndex(t, clean, repo)
	file := clean.GetNode(builderRepoPrefix + "/q/q.go")
	if file == nil {
		t.Fatal("no file node for q/q.go")
	}
	for _, key := range []string{
		sourceSemanticFingerprintMeta, sourceMetadataFingerprintMeta, sourceCoreFingerprintMeta,
		sourceDerivedDeclFingerprintMeta, sourceDerivedImportFingerprintMeta,
		sourceDerivedRuntimeFingerprintMeta, sourceDerivedArtifactFingerprintMeta,
	} {
		if v, _ := file.Meta[key].(string); v == "" {
			t.Errorf("a whole index wrote no %s on the file node", key)
		}
	}
	primary := primaryPerSaveOf(t, repo, []string{"q/q.go"}, config.Default().Index)
	if got, want := builderRenderNode(primary.GetNode(file.ID)), builderRenderNode(file); got != want {
		t.Fatalf("per-save file row differs from the whole index:\n  per-save %s\n  whole    %s", got, want)
	}
}

func TestPerSaveDeletionReDerivesTheImportersLikeAWholeIndex(t *testing.T) {
	repo := contractRepo(t)
	// p is deleted: its only file goes, q's call and import now name a
	// package the repository no longer has.
	builderGit(t, repo, "rm", "-q", "p/p.go")
	primary := primaryPerSaveOf(t, repo, []string{"p/p.go"}, config.Default().Index)
	clean := builderOpenStore(t, "clean")
	builderIndex(t, clean, repo)

	// The rows a whole index mints for q's references into the gone package.
	for _, id := range []string{
		"dep::" + contractModule + "/p::Take",
		"module::go:" + contractModule + "/p",
	} {
		want := clean.GetNode(id)
		if want == nil {
			t.Fatalf("fixture: the whole index has no %s", id)
		}
		if got := primary.GetNode(id); got == nil || builderRenderNode(got) != builderRenderNode(want) {
			t.Errorf("per-save deletion: %s = %v, want %s", id, got, builderRenderNode(want))
		}
	}
	rec := replayCompare(primary, clean, 20)
	for _, row := range rec.Sample {
		// value_flow placeholder sources are the resolver's (class B) — not
		// this frontier's; everything else must match.
		if strings.Contains(row, "Kind:value_flow") {
			continue
		}
		t.Errorf("per-save deletion differs from the whole index: %s", row)
	}
}
