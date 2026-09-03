package indexer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The sparse-generation equivalence properties.
//
// builder_acceptance_test.go pins one hand-built fixture against a flat index
// of the tree it describes. These two tests generalise that single case into a
// property over generated mutation scripts: whatever a script did to the
// repository, a reader must not be able to tell the composed layer stack from
// a plain whole index of the state the stack claims to describe.
//
// The oracle is the acceptance fixture's own differential, reused rather than
// re-derived — same node and edge rendering, same probe set built from the
// union of both graphs, same reader surface — with the answers RETURNED
// instead of asserted, so a failing case can name the category of read that
// diverged first rather than dumping both graphs.
//
// There is no shrinker. A failure logs the seed, the whole script, and the
// first divergence, which is what it takes to replay the case by hand: the
// generator is a pure function of the seed.

const (
	// propSeedsEnv raises the number of generated cases per property. The
	// default corpus is fixed so a CI run is deterministic and cheap; a soak
	// run sets this to a few hundred.
	propSeedsEnv = "GORTEX_BUILDER_PROPERTY_SEEDS"
	// propDefaultSeeds is the committed corpus size, chosen to keep both
	// properties together well inside a minute and a half.
	propDefaultSeeds = 12
	// propGraphID names the dedicated graph every generated case builds for.
	propGraphID = "graph-property"
	// propDirtyLayerID names the working-tree layer of the generated checkout.
	propDirtyLayerID = "layer-worktree"
)

// propIndexConfig is the index configuration BOTH halves of the differential
// run under: the layer build and the flat reference, so nothing the comparison
// sees is a difference in configuration rather than a difference in the
// builder.
//
// It turns near-duplicate detection off. Clone detection is a whole-CORPUS
// statistic — a Count-Min Sketch of shingle frequencies, length-stratified LSH
// banding, and a diffusion pass over the resulting clone graph — and a sparse
// generation carries part of a corpus by construction. A generated corpus is
// dense in exact duplicates (every function body normalises to the same token
// stream once identifiers are erased), so leaving the pass on would make the
// property measure the clone pass's corpus rather than the closure's
// completeness: a change that turns a claimed file into a near-duplicate of an
// UNTOUCHED one produces an edge recorded in each of the two files, and no walk
// over the base layer's edges can discover a pair the base does not yet hold.
//
// Switching the pass off narrows what these properties measure; it does not
// waive the divergence. A build that leaves the pass ON declares
// graph.similarity incomplete for this exact reason, so the generation carries
// the limitation itself rather than leaving it to a comment, and
// TestClosureCarriesCloneCounterpart pins the half the closure CAN reach — a
// pair the base already records. Every other pass, including the resolution the
// closure exists to serve, runs exactly as it does in production.
func propIndexConfig() config.IndexConfig {
	cfg := config.Default().Index
	off := false
	cfg.Coverage.Clones.Enabled = &off
	return cfg
}

// propIndex runs one plain whole index of dir into store under the property
// configuration.
func propIndex(t *testing.T, store *store_sqlite.Store, dir string) {
	t.Helper()
	idx := New(store, builderRegistry(), propIndexConfig(), zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("index %s: %v", dir, err)
	}
}

// propNewBuilder is builderNewBuilder under the property configuration.
func propNewBuilder(store *store_sqlite.Store) *SparseGenerationBuilder {
	b := builderNewBuilder(store)
	b.Config = propIndexConfig()
	return b
}

func propSeedCorpus(t *testing.T) []int64 {
	t.Helper()
	n := propDefaultSeeds
	if raw := os.Getenv(propSeedsEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("%s=%q is not a positive iteration count", propSeedsEnv, raw)
		}
		n = parsed
	}
	seeds := make([]int64, n)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	return seeds
}

// --- the generated repository -------------------------------------------

// propRepo is one generated repository: the git checkout, the corpus it was
// rendered from, and the tree currently on disk.
type propRepo struct {
	dir    string
	corpus *propCorpus
	tree   map[string]string
}

func propNewRepo(t *testing.T, rng *rand.Rand) *propRepo {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	corpus := propInitialCorpus(rng)
	tree := corpus.tree()
	builderWriteTree(t, dir, tree)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	return &propRepo{dir: dir, corpus: corpus, tree: tree}
}

// propApplyScript generates one mutation script, applies it to the corpus and
// lands the result on disk. Renames go through git mv — a staged rename is a
// state the dirty sampler distinguishes and a tree difference cannot express —
// and everything else is written or removed from the difference between the
// old rendering and the new one.
//
// It returns the script and every path it touched, which is the set the dirty
// property stages a random subset of.
func propApplyScript(t *testing.T, r *propRepo, rng *rand.Rand) (propScript, []string) {
	t.Helper()
	script := propGenerateScript(rng, r.corpus)
	// A script whose operations cancelled out — two signature flips on one
	// file, an import toggled twice — leaves the tree where it was and there
	// is nothing for a layer to describe. One forced revision makes the case
	// a real one without changing what the script exercises.
	if len(script.Renames) == 0 && propTreesEqual(r.tree, r.corpus.tree()) && len(r.corpus.files) > 0 {
		r.corpus.files[0].fn.rev++
		script.Ops = append(script.Ops, fmt.Sprintf("body_only %s rev %d (forced: the script cancelled out)",
			r.corpus.files[0].path, r.corpus.files[0].fn.rev))
	}

	old := r.tree
	for _, mv := range script.Renames {
		builderGit(t, r.dir, "mv", mv[0], mv[1])
		body := old[mv[0]]
		delete(old, mv[0])
		old[mv[1]] = body
	}
	current := r.corpus.tree()
	delta := propDiffTrees(old, current)
	for _, path := range delta.Written {
		full := filepath.Join(r.dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(current[path]), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for _, path := range delta.Removed {
		if err := os.Remove(filepath.Join(r.dir, filepath.FromSlash(path))); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
	}
	r.tree = current

	// The rename's two paths are deliberately left out: git mv already staged
	// both halves, and `git add` on the source refuses a pathspec that matches
	// neither the index nor the working tree.
	touched := append(append([]string{}, delta.Written...), delta.Removed...)
	sort.Strings(touched)
	return script, slices.Compact(touched)
}

func propTreesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, body := range a {
		if b[path] != body {
			return false
		}
	}
	return true
}

// propStage stages a random half of the touched paths, so the working tree
// carries staged modifications, unstaged ones, untracked additions and
// unstaged deletions at once — the four states the dirty sampler tells apart.
func propStage(t *testing.T, dir string, rng *rand.Rand, touched []string) []string {
	t.Helper()
	var staged []string
	for _, path := range touched {
		if rng.Intn(2) != 0 {
			continue
		}
		builderGit(t, dir, "add", "-A", "--", path)
		staged = append(staged, path)
	}
	return staged
}

// --- the differential, as findings rather than assertions ----------------

// propDivergence is one read the composed stack and the flat index disagreed
// on. Category is the reader that was called; Probe is the argument it was
// called with.
type propDivergence struct {
	Category string
	Probe    string
	Composed []string
	Flat     []string
}

// propCompare drives the whole graph.Reader surface through both graphs and
// collects the disagreements. It covers exactly what builderAssertReadersAgree
// asserts — whole-graph node and edge payloads, both counters, per-identity
// nodes and both edge directions, the batched forms of all three, file lists,
// the repo node list, name and qualified-name lookup, and both kind
// iterations — and is built from the same rendering helpers, so a row the
// acceptance fixture would call equal is called equal here too.
func propCompare(composed, flat graph.Reader) []propDivergence {
	var found []propDivergence
	add := func(category, probe string, got, want []string) {
		if slices.Equal(got, want) {
			return
		}
		found = append(found, propDivergence{Category: category, Probe: probe, Composed: got, Flat: want})
	}
	probes := builderProbesFor(composed, flat)

	add("AllNodes", "", builderRenderNodes(composed.AllNodes()), builderRenderNodes(flat.AllNodes()))
	add("AllEdges", "", builderRenderEdges(composed.AllEdges()), builderRenderEdges(flat.AllEdges()))
	add("NodeCount", "", []string{strconv.Itoa(composed.NodeCount())}, []string{strconv.Itoa(flat.NodeCount())})
	add("EdgeCount", "", []string{strconv.Itoa(composed.EdgeCount())}, []string{strconv.Itoa(flat.EdgeCount())})

	for _, id := range probes.ids {
		add("GetNode", id,
			[]string{builderRenderNode(composed.GetNode(id))}, []string{builderRenderNode(flat.GetNode(id))})
		add("GetOutEdges", id,
			builderRenderEdges(composed.GetOutEdges(id)), builderRenderEdges(flat.GetOutEdges(id)))
		add("GetInEdges", id,
			builderRenderEdges(composed.GetInEdges(id)), builderRenderEdges(flat.GetInEdges(id)))
	}

	composedOut, flatOut := composed.GetOutEdgesByNodeIDs(probes.ids), flat.GetOutEdgesByNodeIDs(probes.ids)
	composedIn, flatIn := composed.GetInEdgesByNodeIDs(probes.ids), flat.GetInEdgesByNodeIDs(probes.ids)
	composedNodes, flatNodes := composed.GetNodesByIDs(probes.ids), flat.GetNodesByIDs(probes.ids)
	for _, id := range probes.ids {
		add("GetOutEdgesByNodeIDs", id,
			builderRenderEdges(composedOut[id]), builderRenderEdges(flatOut[id]))
		add("GetInEdgesByNodeIDs", id,
			builderRenderEdges(composedIn[id]), builderRenderEdges(flatIn[id]))
		add("GetNodesByIDs", id,
			[]string{builderRenderNode(composedNodes[id])}, []string{builderRenderNode(flatNodes[id])})
	}

	for _, path := range probes.files {
		add("GetFileNodes", path,
			builderRenderNodes(composed.GetFileNodes(path)), builderRenderNodes(flat.GetFileNodes(path)))
	}
	add("GetRepoNodes", builderRepoPrefix,
		builderRenderNodes(composed.GetRepoNodes(builderRepoPrefix)),
		builderRenderNodes(flat.GetRepoNodes(builderRepoPrefix)))

	for _, name := range probes.names {
		add("FindNodesByName", name,
			builderRenderNodes(composed.FindNodesByName(name)), builderRenderNodes(flat.FindNodesByName(name)))
	}
	for _, qual := range probes.qualNames {
		add("GetNodeByQualName", qual,
			[]string{builderRenderNode(composed.GetNodeByQualName(qual))},
			[]string{builderRenderNode(flat.GetNodeByQualName(qual))})
	}
	for _, kind := range probes.kinds {
		add("NodesByKind", string(kind),
			builderRenderNodes(builderCollectNodes(composed.NodesByKind(kind))),
			builderRenderNodes(builderCollectNodes(flat.NodesByKind(kind))))
	}
	for _, kind := range probes.edgeKinds {
		add("EdgesByKind", string(kind),
			builderRenderEdges(builderCollectEdges(composed.EdgesByKind(kind))),
			builderRenderEdges(builderCollectEdges(flat.EdgesByKind(kind))))
	}
	return found
}

// propMaxDeltaRows bounds how much of a divergence is printed. The first few
// rows on either side are what identifies the class; the rest is repetition.
const propMaxDeltaRows = 6

func propRenderDivergence(d propDivergence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  first divergence: %s(%s)\n", d.Category, d.Probe)
	for _, row := range propOnly(d.Composed, d.Flat) {
		fmt.Fprintf(&b, "    only in composed: %s\n", row)
	}
	for _, row := range propOnly(d.Flat, d.Composed) {
		fmt.Fprintf(&b, "    only in flat:     %s\n", row)
	}
	return b.String()
}

// propOnly returns the rows got holds that want does not, counted, capped at
// propMaxDeltaRows.
func propOnly(got, want []string) []string {
	remaining := make(map[string]int, len(want))
	for _, row := range want {
		remaining[row]++
	}
	var out []string
	for _, row := range got {
		if remaining[row] > 0 {
			remaining[row]--
			continue
		}
		if len(out) == propMaxDeltaRows {
			out = append(out, "... (further rows elided)")
			break
		}
		out = append(out, row)
	}
	return out
}

// propCategories lists the distinct reader categories that diverged, in the
// order they were probed.
func propCategories(found []propDivergence) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range found {
		if seen[d.Category] {
			continue
		}
		seen[d.Category] = true
		out = append(out, d.Category)
	}
	return out
}

// propReportDivergence is the shrinking-lite failure path: the seed that
// replays the case, the script that produced it, what the build said about
// its own completeness, and the first read that disagreed.
func propReportDivergence(
	t *testing.T,
	seed int64,
	scripts []propScript,
	reports []BuildReport,
	found []propDivergence,
) {
	t.Helper()
	if len(found) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d: the composed stack answers %d reads differently from a flat index (%s)\n",
		seed, len(found), strings.Join(propCategories(found), ", "))
	fmt.Fprintf(&b, "  replay: go test -run '%s' ./internal/indexer/\n", t.Name())
	for i, script := range scripts {
		fmt.Fprintf(&b, "  script %d: %s\n", i+1, script)
	}
	for i, report := range reports {
		fmt.Fprintf(&b, "  build %d: closure=%v indexed=%v deleted=%d tombstones=%d unmasked=%d contested=%d\n",
			i+1, report.ClosurePaths, report.IndexedPaths, report.DeletedFiles,
			report.NodeTombstones, report.UnmaskedPayloadNodes, report.ContestedEdgeSources)
	}
	b.WriteString(propRenderDivergence(found[0]))
	t.Error(b.String())
}

// --- shared build plumbing ----------------------------------------------

// propAssertPublished pins that the build reached publish. Mask validation is
// a publish-time precondition, so a generation that is ready has passed it;
// builderAssertMasksValidate re-runs it afterwards as the acceptance fixture
// does, and this is the half that says the publish happened at all.
func propAssertPublished(t *testing.T, store *store_sqlite.Store, generationID int64) {
	t.Helper()
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("read generation %d: found=%v err=%v", generationID, found, err)
	}
	if row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation %d is %s, want %s", generationID, row.State, store_sqlite.ViewGenerationReady)
	}
	if row.PublishedAt == 0 {
		t.Fatalf("generation %d is ready but carries no publish timestamp", generationID)
	}
}

// propBuildCommitLayer builds the layer spanning two committed trees of the
// generated repository and checks the two completeness facts a generated case
// of this size must not hit.
func propBuildCommitLayer(
	t *testing.T,
	store *store_sqlite.Store,
	repo *propRepo,
	baseTree, targetTree, commitOID string,
) (int64, BuildReport) {
	t.Helper()
	generationID, report, err := propNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             propGraphID,
			LayerID:             "layer-" + targetTree,
			CheckoutID:          "checkout-property",
			ProvenanceCommitOID: commitOID,
		},
		Base:          store,
		RepoDir:       repo.dir,
		BaseTreeOID:   baseTree,
		TargetTreeOID: targetTree,
		RootPath:      repo.dir,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	propAssertPublished(t, store, generationID)
	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d in a corpus of %d files — the cap is not what this property tests",
			report.ClosureCap, len(repo.tree))
	}
	return generationID, report
}

// propComposeStack stacks a working-tree generation over a commit generation
// over the base corpus, the way the materializer assembles a checkout's view.
func propComposeStack(t *testing.T, store *store_sqlite.Store, commitGeneration, dirtyGeneration int64) graph.Reader {
	t.Helper()
	commitLayer, err := graphview.NewGenerationLayer(store.AtGeneration(commitGeneration))
	if err != nil {
		t.Fatalf("NewGenerationLayer(commit %d): %v", commitGeneration, err)
	}
	dirtyLayer, err := graphview.NewGenerationLayer(store.AtGeneration(dirtyGeneration))
	if err != nil {
		t.Fatalf("NewGenerationLayer(dirty %d): %v", dirtyGeneration, err)
	}
	id, err := graphview.NewRepoViewID(builderRepoPrefix, propGraphID, commitGeneration,
		graphview.LayerRef{Kind: graphview.LayerDirty, LayerID: propDirtyLayerID, Generation: dirtyGeneration})
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	base := graph.NewOverlaidViewWithLayer(store.AtGeneration(0), commitLayer)
	reader, _, err := graphview.ComposeRepoView(base, []graph.OverlayLayerReader{dirtyLayer}, id)
	if err != nil {
		t.Fatalf("ComposeRepoView: %v", err)
	}
	return reader
}

// --- property 1: the commit layer ---------------------------------------

// TestBuildEquivalenceProperty is the commit-layer oracle. For every seed:
// commit the generated corpus, index that state as the base, run a generated
// mutation script, commit it, build the layer spanning the two trees, and
// require the composition to answer every read the way a plain whole index of
// the second tree does.
func TestBuildEquivalenceProperty(t *testing.T) {
	for _, seed := range propSeedCorpus(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			propRunCommitCase(t, seed)
		})
	}
}

func propRunCommitCase(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	repo := propNewRepo(t, rng)
	baseTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	propIndex(t, store, repo.dir)

	script, _ := propApplyScript(t, repo, rng)
	builderGit(t, repo.dir, "add", "-A")
	builderGit(t, repo.dir, "commit", "-m", "B")
	targetTree := builderGit(t, repo.dir, "rev-parse", "HEAD^{tree}")
	commitOID := builderGit(t, repo.dir, "rev-parse", "HEAD")
	if targetTree == baseTree {
		t.Fatalf("seed %d: script %s left the tree where it was", seed, script)
	}

	generationID, report := propBuildCommitLayer(t, store, repo, baseTree, targetTree, commitOID)
	t.Logf("seed %d: %s\n  closure=%v indexed=%v nodes=%d edges=%d",
		seed, script, report.ClosurePaths, report.IndexedPaths, report.NodeCount, report.EdgeCount)

	flat := builderOpenStore(t, "flat")
	propIndex(t, flat, repo.dir)
	composed := builderComposed(t, store, generationID)
	if corpus := builderNodeIDs(store.AtGeneration(0)); slices.Equal(corpus, builderNodeIDs(composed)) {
		t.Fatalf("seed %d: the composed view carries the corpus's identities verbatim — the layer changed nothing",
			seed)
	}
	propReportDivergence(t, seed, []propScript{script}, []BuildReport{report}, propCompare(composed, flat))
	builderAssertMasksValidate(t, store, generationID)
}

// --- property 2: the working-tree layer over the commit layer ------------

// TestDirtyEquivalenceProperty is the three-layer oracle. It runs the commit
// property's setup, then a SECOND generated script that is left uncommitted —
// with a random half of its paths staged, so the checkout carries staged and
// unstaged modifications, untracked additions and unstaged deletions at once —
// builds the working-tree layer over the commit layer, and requires the whole
// stack to answer like a plain whole index of what is on disk.
func TestDirtyEquivalenceProperty(t *testing.T) {
	for _, seed := range propSeedCorpus(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			propRunDirtyCase(t, seed)
		})
	}
}

func propRunDirtyCase(t *testing.T, seed int64) {
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

	dirtyGeneration, dirtyReport, err := propNewBuilder(store).BuildDirtyLayer(
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
	propAssertPublished(t, store, dirtyGeneration)
	if dirtyReport.ClosureTruncated {
		t.Fatalf("closure truncated at %d in a corpus of %d files", dirtyReport.ClosureCap, len(repo.tree))
	}
	t.Logf("seed %d: commit %s | dirty %s\n  staged=%v touched=%v closure=%v indexed=%v",
		seed, commitScript, dirtyScript, staged, touched,
		dirtyReport.ClosurePaths, dirtyReport.IndexedPaths)

	flat := builderOpenStore(t, "flat")
	propIndex(t, flat, repo.dir)
	composed := propComposeStack(t, store, commitGeneration, dirtyGeneration)
	if base := builderNodeIDs(corpus); slices.Equal(base, builderNodeIDs(composed)) {
		t.Fatalf("seed %d: the composed stack carries the corpus's identities verbatim — neither layer changed anything",
			seed)
	}
	propReportDivergence(t, seed,
		[]propScript{commitScript, dirtyScript},
		[]BuildReport{commitReport, dirtyReport},
		propCompare(composed, flat))
	builderAssertMasksValidate(t, store, commitGeneration)
	builderAssertMasksValidate(t, store, dirtyGeneration)
}
