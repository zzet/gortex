package indexer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// editDeltaBuilder is the property builder, with a development logger when
// GX_EDIT_DELTA_DEBUG is set.
func editDeltaBuilder(t *testing.T, b *SparseGenerationBuilder) *SparseGenerationBuilder {
	t.Helper()
	if os.Getenv("GX_EDIT_DELTA_DEBUG") != "" {
		logger, _ := zap.NewDevelopment()
		b.Logger = logger
	}
	return b
}

// TestEditDeltaDirtyEquivalenceProperty is the three-layer oracle. It runs the
// commit property's setup, then a SECOND generated script that is left
// uncommitted — with a random half of its paths staged, so the checkout
// carries staged and unstaged modifications, untracked additions and unstaged
// deletions at once — builds the working-tree layer over the commit layer by
// the per-file delta path, and requires the whole stack to answer every read
// like a plain whole index of what is on disk, except rows the primary
// per-save engine produces identically (the engine's own residual classes).
func TestEditDeltaDirtyEquivalenceProperty(t *testing.T) {
	for _, seed := range propSeedCorpus(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			propRunDirtyDeltaCase(t, seed)
		})
	}
}

func propRunDirtyDeltaCase(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	repo := propNewRepo(t, rng)
	baseTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	propIndex(t, store, repo.dir)

	commitScript, _ := propApplyScript(t, repo, rng)
	builderGit(t, repo.dir, "add", "-A")
	builderGit(t, repo.dir, "commit", "-m", "B")
	targetTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")
	commitOID := builderGit(t, repo.dir, "rev-parse", "HEAD")
	if targetTree == baseTree {
		t.Fatalf("seed %d: script %s left the tree where it was", seed, commitScript)
	}
	commitGeneration, commitReport := propBuildCommitLayer(t, store, repo, baseTree, targetTree, commitOID)

	dirtyScript, touched := propApplyScript(t, repo, rng)
	staged := propStage(t, repo.dir, rng, touched)

	corpus := store.AtGeneration(0)
	commitLayer, err := graphview.NewGenerationLayer(store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatalf("NewGenerationLayer(commit %d): %v", commitGeneration, err)
	}
	dirtyBase := commitLayerBase{
		Reader: graph.NewOverlaidViewWithLayer(corpus, commitLayer),
		corpus: corpus,
	}

	recordLastEditDelta(nil)
	dirtyGeneration, dirtyReport, err := editDeltaBuilder(t, propNewBuilder(store)).BuildDirtyLayer(
		context.Background(), DirtyLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind:        "dedicated_graph",
				GraphID:          propGraphID,
				LayerID:          propDirtyLayerID,
				CheckoutID:       "checkout-property",
				BaseGenerationID: commitGeneration,
			},
			Base:         dirtyBase,
			CheckoutRoot: repo.dir,
			RepoPrefix:   builderRepoPrefix,
			WorkspaceID:  builderRepoPrefix,
			ProjectID:    builderRepoPrefix,
		})
	if err != nil {
		t.Fatalf("seed %d: BuildDirtyLayer after %s: %v", seed, dirtyScript, err)
	}
	delta := LastEditDeltaReport()
	if delta == nil {
		t.Fatalf("seed %d: the working-tree layer was not built by the delta path", seed)
	}
	propAssertPublished(t, store, dirtyGeneration)
	t.Logf("seed %d: commit %s | dirty %s\n  staged=%v touched=%v delta=%+v",
		seed, commitScript, dirtyScript, staged, touched, *delta)

	flat := builderOpenStore(t, "flat")
	propIndex(t, flat, repo.dir)
	composed := propComposeStack(t, store, commitGeneration, dirtyGeneration)
	if base := builderNodeIDs(corpus); slices.Equal(base, builderNodeIDs(composed)) {
		t.Fatalf("seed %d: the composed stack carries the corpus's identities verbatim", seed)
	}
	found := propCompare(fingerprintNeutralReader{composed}, fingerprintNeutralReader{flat})
	if len(found) > 0 {
		// The per-save engine the delta runs has its own residual classes
		// against a whole index (dataflow parameter targets, placeholder
		// value-flow sources, restub provenance on references that stay
		// unresolved). The primary checkout's per-save path, applied to a
		// store holding the committed state, is the reference for them:
		// every row on which the delta differs from the whole index must be
		// a row the primary gets exactly the same, and every row on which it
		// differs from the primary must match the whole index.
		primary := propPrimaryPerSave(t, repo, touched, dirtyScript)
		bad := editDeltaTriangulate(fingerprintNeutralReader{composed},
			fingerprintNeutralReader{primary}, fingerprintNeutralReader{flat})
		if len(bad) > 0 {
			propReportDivergence(t, seed,
				[]propScript{commitScript, dirtyScript},
				[]BuildReport{commitReport, dirtyReport}, found)
			t.Errorf("seed %d: rows where the delta differs from both the whole index and the primary per-save path:\n  %s",
				seed, strings.Join(bad, "\n  "))
		} else {
			t.Logf("seed %d: %d reads differ from the whole index; every differing row is the primary per-save engine's own",
				seed, len(found))
		}
	}
	builderAssertMasksValidate(t, store, dirtyGeneration)
	propAssertRedundantPathlessPruned(t, store, dirtyGeneration, dirtyBase)
}

// TestEditDeltaMatchesPrimaryPerSave isolates the delta layer from the
// per-save engine it runs: the same edit applied by the primary checkout's
// per-save path to a store holding the pre-edit state, and by the delta path
// as a working-tree generation over the same state, must serve identical
// graphs. Any divergence here is the DeltaWriter's (materialization, claims,
// payload, composition), never the engine's.
func TestEditDeltaMatchesPrimaryPerSave(t *testing.T) {
	for _, seed := range propSeedCorpus(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			repo := propNewRepo(t, rng)

			deltaStore := builderOpenStore(t, "delta")
			propIndex(t, deltaStore, repo.dir)
			primary := builderOpenStore(t, "primary")
			idx := New(primary, builderRegistry(), propIndexConfig(), zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			if _, err := idx.Index(repo.dir); err != nil {
				t.Fatalf("index: %v", err)
			}

			script, touched := propApplyScript(t, repo, rng)
			propStage(t, repo.dir, rng, touched)
			for _, mv := range script.Renames {
				touched = append(touched, mv[0], mv[1])
			}
			t.Logf("seed %d: %s touched=%v", seed, script, touched)

			corpus := deltaStore.AtGeneration(0)
			recordLastEditDelta(nil)
			generation, _, err := editDeltaBuilder(t, propNewBuilder(deltaStore)).BuildDirtyLayer(
				context.Background(), DirtyLayerRequest{
					Identity: GenerationIdentity{
						OwnerKind: "dedicated_graph", GraphID: propGraphID,
						LayerID: propDirtyLayerID, CheckoutID: "checkout-property",
					},
					Base:         commitLayerBase{Reader: corpus, corpus: corpus},
					CheckoutRoot: repo.dir,
					RepoPrefix:   builderRepoPrefix,
					WorkspaceID:  builderRepoPrefix,
					ProjectID:    builderRepoPrefix,
				})
			if err != nil {
				t.Fatalf("seed %d: BuildDirtyLayer: %v", seed, err)
			}
			if LastEditDeltaReport() == nil {
				t.Fatalf("seed %d: not built by the delta path", seed)
			}
			t.Logf("delta: %+v", *LastEditDeltaReport())
			if _, err := idx.IncrementalReindexPaths(repo.dir, touched); err != nil {
				t.Fatalf("primary per-save: %v", err)
			}
			layer, err := graphview.NewGenerationLayer(deltaStore.AtGeneration(generation))
			if err != nil {
				t.Fatalf("NewGenerationLayer: %v", err)
			}
			composed := graph.NewOverlaidViewWithLayer(corpus, layer)
			found := propCompare(fingerprintNeutralReader{composed}, fingerprintNeutralReader{primary})
			if len(found) > 0 {
				// The delta may differ from the primary only where the delta
				// agrees with a whole index of the edited tree: a row the
				// primary's per-save path gets wrong (it drops the edges other
				// files recorded out of a re-parsed file's surviving symbols).
				flat := builderOpenStore(t, "flat")
				propIndex(t, flat, repo.dir)
				if bad := editDeltaTriangulate(fingerprintNeutralReader{composed},
					fingerprintNeutralReader{primary}, fingerprintNeutralReader{flat}); len(bad) > 0 {
					propReportDivergence(t, seed, []propScript{script}, nil, found)
					t.Errorf("seed %d: rows where the delta differs from the primary AND from a whole index:\n  %s",
						seed, strings.Join(bad, "\n  "))
				} else {
					t.Logf("seed %d: %d reads differ from the primary, every differing row matches a whole index", seed, len(found))
				}
			}
			builderAssertMasksValidate(t, deltaStore, generation)
		})
	}
}

// editDeltaTriangulate returns the node and edge rows on which the delta view
// and the primary disagree and the delta view also disagrees with a whole
// index.
func editDeltaTriangulate(delta, primary, flat graph.Reader) []string {
	var bad []string
	check := func(d, p, f []string) {
		count := func(rows []string) map[string]int {
			out := make(map[string]int, len(rows))
			for _, r := range rows {
				out[r]++
			}
			return out
		}
		dc, pc, fc := count(d), count(p), count(f)
		seen := map[string]struct{}{}
		for _, rows := range [][]string{d, p} {
			for _, r := range rows {
				if _, dup := seen[r]; dup {
					continue
				}
				seen[r] = struct{}{}
				if dc[r] != pc[r] && dc[r] != fc[r] {
					bad = append(bad, fmt.Sprintf("delta=%d primary=%d flat=%d %s", dc[r], pc[r], fc[r], r))
				}
			}
		}
	}
	check(builderRenderNodes(delta.AllNodes()), builderRenderNodes(primary.AllNodes()), builderRenderNodes(flat.AllNodes()))
	check(builderRenderEdges(delta.AllEdges()), builderRenderEdges(primary.AllEdges()), builderRenderEdges(flat.AllEdges()))
	return bad
}

// propPrimaryPerSave is the primary checkout's per-save path over the same
// states: a whole index of the committed tree, then the dirty paths re-indexed
// through IncrementalReindexPaths. The working tree must already hold the
// dirty state; the committed tree is read from HEAD into a scratch checkout.
func propPrimaryPerSave(t *testing.T, repo *propRepo, touched []string, script propScript) *store_sqlite.Store {
	t.Helper()
	paths := append([]string(nil), touched...)
	for _, mv := range script.Renames {
		paths = append(paths, mv[0], mv[1])
	}
	return primaryPerSaveOf(t, repo.dir, paths, propIndexConfig())
}

// primaryPerSaveOf indexes HEAD of repoDir whole into a fresh store, brings a
// scratch checkout to the working tree's bytes at paths, and applies them
// through the primary per-save path.
func primaryPerSaveOf(t *testing.T, repoDir string, paths []string, cfg config.IndexConfig) *store_sqlite.Store {
	t.Helper()
	scratch := builderTempDir(t, "primary")
	builderGit(t, repoDir, "worktree", "add", "--detach", scratch, "HEAD")
	store := builderOpenStore(t, "primary")
	idx := New(store, builderRegistry(), cfg, zap.NewNop())
	t.Cleanup(func() { idx.Close() })
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(scratch); err != nil {
		t.Fatalf("primary index: %v", err)
	}
	for _, rel := range paths {
		src := filepath.Join(repoDir, filepath.FromSlash(rel))
		dst := filepath.Join(scratch, filepath.FromSlash(rel))
		data, err := os.ReadFile(src)
		switch {
		case err == nil:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dst, err)
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				t.Fatalf("write %s: %v", dst, err)
			}
		case os.IsNotExist(err):
			_ = os.Remove(dst)
		default:
			t.Fatalf("read %s: %v", src, err)
		}
	}
	if _, err := idx.IncrementalReindexPaths(scratch, paths); err != nil {
		t.Fatalf("primary per-save: %v", err)
	}
	return store
}

// The accumulated-edit clean-parity oracle, with the working-tree generations
// built by the delta path (both layouts): every sidecar (files inventory,
// symbol full-text documents and search probes, semantic binding types,
// constant values) and the masks must match a clean index strictly; nodes and
// edges must match it everywhere except rows the primary per-save path
// produces identically (the engine's own residual classes).
func TestEditDeltaAccumulatedCleanParityIndependentPackages(t *testing.T) {
	editDeltaAccumulatedParity(t, accumulatedDirtyIndependent, 25)
}

func TestEditDeltaAccumulatedCleanParitySamePackage(t *testing.T) {
	editDeltaAccumulatedParity(t, accumulatedDirtySamePackage, 25)
}

func editDeltaAccumulatedParity(t *testing.T, layout accumulatedDirtyLayout, n int) {
	t.Helper()
	store := builderOpenStore(t, fmt.Sprintf("delta-%s-%d", layout, n))
	repoDir := accumulatedDirtyRepo(t, layout, store)
	builder := builderNewBuilder(store)
	chains := newDirtyChainBuilder(t, builder, store, repoDir, true)
	var chain []int64
	var dirty []string
	build := func() {
		t.Helper()
		recordLastEditDelta(nil)
		_, _, chain = chains.build()
		if LastEditDeltaReport() == nil {
			t.Fatalf("%s: the working-tree generation was not built by the delta path", layout)
		}
		chains.settle()
	}
	check := func(label string) {
		t.Helper()
		result := assertCleanIndexParityChain(t, store, chain, repoDir, label, false)
		sidecars := result.FilesEqual && result.SymbolFTSEqual && result.SearchProbesEqual &&
			result.SemanticEqual && result.ConstantsEqual && result.MasksValid
		if !sidecars {
			t.Errorf("%s: sidecars differ from the clean index: %+v", label, result)
		}
		if result.NodesEqual && result.EdgesEqual {
			return
		}
		primary := primaryPerSaveOf(t, repoDir, dirty, config.Default().Index)
		clean := builderOpenStore(t, "clean-tri-"+label)
		builderIndex(t, clean, repoDir)
		composed := dirtyChainComposed(t, store, chain)
		if bad := editDeltaTriangulate(composed, primary, clean); len(bad) > 0 {
			t.Errorf("%s: rows where the delta differs from both the clean index and the primary per-save path:\n  %s",
				label, strings.Join(bad, "\n  "))
			return
		}
		t.Logf("%s: nodes equal=%t edges equal=%t; every differing row is the primary per-save engine's own",
			label, result.NodesEqual, result.EdgesEqual)
	}
	for i := 0; i < n; i++ {
		accumulatedDirtyWriteUnit(t, repoDir, layout, i, true, false)
		dirty = append(dirty, accumulatedDirtyUnitPath(layout, i))
		build()
	}
	body := accumulatedDirtyUnitPath(layout, accumulatedDirtyBodyTarget)
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyBodyTarget, true, false)
	build()
	dirty = append(dirty, body)
	check(fmt.Sprintf("%s-body-%d", layout, n))
	// Undo the body edit (a partial undo: the path returns to its committed
	// bytes), then a comment edit in a fresh unit.
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyBodyTarget, false, false)
	build()
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyCommentTarget, false, true)
	dirty = append(dirty, accumulatedDirtyUnitPath(layout, accumulatedDirtyCommentTarget))
	build()
	check(fmt.Sprintf("%s-comment-%d", layout, n))
}

// The five invalidation classes (invalidation_classes_test.go) with the
// working-tree generations built by the delta path. The sparse builder's
// cases also assert its own frontier (masks, closure and context paths),
// which the delta path does not have: here each case is judged by the
// triangulated oracle instead — the composed view must match a clean index of
// the checkout on every node and edge row, except rows the primary checkout's
// per-save path produces identically — plus mask validity and the files
// inventory against the clean index.
func TestEditDeltaInvalidationClasses(t *testing.T) {
	mod := accumulatedDirtyModule
	cases := []struct {
		name  string
		paths []string
		edit  func(r *invalidationRun)
	}{
		{"signature", []string{"chain/b/b.go"}, func(r *invalidationRun) {
			r.write("chain/b/b.go", "package b\n\nimport \""+mod+"/chain/c\"\n\n// B calls down the chain.\nfunc B(delta int) int {\n\treturn c.C() + delta\n}\n")
			r.build()
		}},
		{"import", []string{"consumer/consumer.go"}, func(r *invalidationRun) {
			r.write("consumer/consumer.go", "package consumer\n\nimport \""+mod+"/chain/b\"\n\n// Consume is the chain's one importing consumer.\nfunc Consume() int {\n\treturn b.B()\n}\n")
			r.build()
		}},
		{"delete", []string{"chain/d/d.go"}, func(r *invalidationRun) {
			r.write("chain/d/d.go", invalidationDirtyD)
			r.build()
			r.remove("chain/d/d.go")
			r.build()
		}},
		{"rename", []string{"chain/d/d.go", "chain/e/e.go", "chain/c/c.go"}, func(r *invalidationRun) {
			r.write("chain/d/d.go", invalidationDirtyD)
			r.build()
			r.remove("chain/d/d.go")
			r.write("chain/e/e.go", strings.Replace(invalidationDirtyD, "package d", "package e", 1))
			r.write("chain/c/c.go", "package c\n\nimport \""+mod+"/chain/e\"\n\n// C calls down the chain.\nfunc C() int {\n\treturn e.D() + 1\n}\n")
			r.build()
		}},
		{"undo", []string{"chain/b/b.go"}, func(r *invalidationRun) {
			r.write("chain/b/b.go", "package b\n\nimport \""+mod+"/chain/c\"\n\n// B calls down the chain.\nfunc B() int {\n\tv := c.C() + 1\n\treturn v\n}\n")
			r.build()
			r.write("chain/b/b.go", invalidationCommitted(t, "chain/b/b.go"))
			r.build()
		}},
	}
	const accumulated = 5
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newInvalidationRun(t, "delta-"+c.name, accumulated)
			c.edit(r)
			id := r.chains.chain[len(r.chains.chain)-1]
			if LastEditDeltaReport() == nil {
				t.Fatalf("%s: not built by the delta path", c.name)
			}
			result := assertCleanIndexParityChain(t, r.store, r.chainFor(id), r.repoDir, "delta-"+c.name, false)
			if !result.MasksValid || !result.FilesEqual {
				t.Errorf("%s: masks valid=%t files equal=%t: %v", c.name, result.MasksValid, result.FilesEqual, result.Diffs)
			}
			t.Logf("%s: nodes=%t edges=%t fts=%t probes=%t semantic=%t constants=%t",
				c.name, result.NodesEqual, result.EdgesEqual, result.SymbolFTSEqual, result.SearchProbesEqual,
				result.SemanticEqual, result.ConstantsEqual)
			if result.NodesEqual && result.EdgesEqual {
				return
			}
			dirty := append([]string(nil), c.paths...)
			for p := range r.accumulated {
				dirty = append(dirty, p)
			}
			primary := primaryPerSaveOf(t, r.repoDir, dirty, config.Default().Index)
			clean := builderOpenStore(t, "clean-tri-"+c.name)
			builderIndex(t, clean, r.repoDir)
			if bad := editDeltaTriangulate(r.composed(id), primary, clean); len(bad) > 0 {
				t.Errorf("%s: rows where the delta differs from both the clean index and the primary per-save path:\n  %s",
					c.name, strings.Join(bad, "\n  "))
			}
		})
	}
}
