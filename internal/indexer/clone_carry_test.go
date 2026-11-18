package indexer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// A delta reports the files holding a changed body (their clone rows are owed
// to the follow-up), carries the clone rows of a file it re-derives unchanged,
// and carries nothing for a path the request names for recomputation.
func TestCloneCarryReportsChangedBodiesAndRecomputesNamedPaths(t *testing.T) {
	ctx := context.Background()
	const producers = "prod/producers.go"
	rename := kindParityEdits()[2] // re-derives prod/producers.go unchanged
	if rename.name != "rename" {
		t.Fatalf("edit 2 is %q, want the rename", rename.name)
	}

	// An unchanged re-derivation carries CloneA's clone_sig and reports nothing.
	repoDir, store := kindParityRepo(t, "clone-carry-unchanged")
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	applyKindParityEdit(t, repoDir, rename)
	id, report, err := builder.BuildDirtyLayer(ctx, h.request())
	if err != nil {
		t.Fatal(err)
	}
	view := dirtyChainComposed(t, store, []int64{id})
	if n := view.GetNode(builderRepoPrefix + "/" + producers + "::CloneA"); n == nil || n.Meta[cloneSigMetaKey] == nil {
		t.Fatalf("an unchanged re-derivation did not carry CloneA's clone_sig: %+v", n)
	}
	// The renamed file's function is a new identity, so it is owed; the file
	// re-derived unchanged is not.
	if slices.Contains(report.ChangedBodyFiles, builderRepoPrefix+"/"+producers) ||
		!slices.Contains(report.ChangedBodyFiles, builderRepoPrefix+"/prod/sib_c2.go") {
		t.Fatalf("changed bodies %v, want the renamed file only", report.ChangedBodyFiles)
	}

	// The same edit with the path named for recomputation carries nothing
	// there and reports it as owed.
	repoDir, store = kindParityRepo(t, "clone-carry-recompute")
	builder = builderNewBuilder(store)
	h = newDirtyChainBuilder(t, builder, store, repoDir, false)
	applyKindParityEdit(t, repoDir, rename)
	req := h.request()
	req.RecomputeDerivedPaths = []string{producers}
	id, report, err = builder.BuildDirtyLayer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	view = dirtyChainComposed(t, store, []int64{id})
	if n := view.GetNode(builderRepoPrefix + "/" + producers + "::CloneA"); n == nil || n.Meta[cloneSigMetaKey] != nil {
		t.Fatalf("a path named for recomputation carried CloneA's clone_sig: %+v", n)
	}
	if !slices.Contains(report.ChangedBodyFiles, builderRepoPrefix+"/"+producers) {
		t.Fatalf("changed bodies %v, want %s owed", report.ChangedBodyFiles, producers)
	}

	// A new body in an edited file is reported as owed.
	repoDir, store = kindParityRepo(t, "clone-carry-new-body")
	builder = builderNewBuilder(store)
	h = newDirtyChainBuilder(t, builder, store, repoDir, false)
	src, err := os.ReadFile(filepath.Join(repoDir, producers))
	if err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, repoDir, producers, string(src)+"\n// Fresh is new.\nfunc Fresh(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x * 3\n\t}\n\treturn total\n}\n")
	_, report, err = builder.BuildDirtyLayer(ctx, h.request())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.ChangedBodyFiles, builderRepoPrefix+"/"+producers) {
		t.Fatalf("changed bodies %v, want the file with a new body", report.ChangedBodyFiles)
	}
}

// A generation holding a changed body records graph.similarity incomplete with
// the deferral token; one without keeps the sparse generation's reason.
func TestSimilarityRowCarriesTheDeferralTokenOnlyForChangedBodies(t *testing.T) {
	ctx := context.Background()
	similarityReason := func(t *testing.T, report BuildReport) string {
		t.Helper()
		for _, row := range report.Producers {
			if row.Producer == string(graphview.CapSimilarity) {
				if row.State != store_sqlite.ProducerStateIncomplete {
					t.Fatalf("similarity state %q, want incomplete", row.State)
				}
				return row.Reason
			}
		}
		t.Fatal("no similarity row")
		return ""
	}
	repoDir, store := kindParityRepo(t, "similarity-token-unchanged")
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	applyKindParityEdit(t, repoDir, kindParityEdits()[3]) // delete: no body owed
	_, report, err := builder.BuildDirtyLayer(ctx, h.request())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ChangedBodyFiles) == 0 && similarityReason(t, report) == graphview.ReasonDeferredToFollowup {
		t.Fatal("a generation owing no clone rows carries the deferral token")
	}

	repoDir, store = kindParityRepo(t, "similarity-token-changed")
	builder = builderNewBuilder(store)
	h = newDirtyChainBuilder(t, builder, store, repoDir, false)
	src, err := os.ReadFile(filepath.Join(repoDir, "prod/producers.go"))
	if err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, repoDir, "prod/producers.go", string(src)+"\nfunc Fresh(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x * 3\n\t}\n\treturn total\n}\n")
	_, report, err = builder.BuildDirtyLayer(ctx, h.request())
	if err != nil {
		t.Fatal(err)
	}
	if got := similarityReason(t, report); got != graphview.ReasonDeferredToFollowup {
		t.Fatalf("similarity reason %q with changed bodies %v, want the deferral token", got, report.ChangedBodyFiles)
	}
}
