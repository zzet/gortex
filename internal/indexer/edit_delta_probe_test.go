package indexer

import (
	"fmt"
	"iter"
	"math/rand"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// fingerprintNeutralReader hides the per-file extraction fingerprints the
// incremental path stamps on file nodes (source_*_fingerprint). They are index
// bookkeeping — a pure function of the file's bytes that decides whether the
// next save may take the delta shortcut — and a whole index never writes
// them, so a strict row comparison has to set them aside to compare content.
type fingerprintNeutralReader struct{ graph.Reader }

func neutralNode(n *graph.Node) *graph.Node {
	if n == nil || len(n.Meta) == 0 {
		return n
	}
	var drop bool
	for k := range n.Meta {
		if strings.HasPrefix(k, "source_") && strings.HasSuffix(k, "_fingerprint") {
			drop = true
			break
		}
	}
	if !drop {
		return n
	}
	c := *n
	c.Meta = make(map[string]any, len(n.Meta))
	for k, v := range n.Meta {
		if strings.HasPrefix(k, "source_") && strings.HasSuffix(k, "_fingerprint") {
			continue
		}
		c.Meta[k] = v
	}
	return &c
}

func neutralNodes(in []*graph.Node) []*graph.Node {
	out := make([]*graph.Node, len(in))
	for i, n := range in {
		out[i] = neutralNode(n)
	}
	return out
}

func (r fingerprintNeutralReader) GetNode(id string) *graph.Node {
	return neutralNode(r.Reader.GetNode(id))
}
func (r fingerprintNeutralReader) GetNodeByQualName(q string) *graph.Node {
	return neutralNode(r.Reader.GetNodeByQualName(q))
}
func (r fingerprintNeutralReader) FindNodesByName(n string) []*graph.Node {
	return neutralNodes(r.Reader.FindNodesByName(n))
}
func (r fingerprintNeutralReader) GetFileNodes(p string) []*graph.Node {
	return neutralNodes(r.Reader.GetFileNodes(p))
}
func (r fingerprintNeutralReader) GetRepoNodes(p string) []*graph.Node {
	return neutralNodes(r.Reader.GetRepoNodes(p))
}
func (r fingerprintNeutralReader) AllNodes() []*graph.Node { return neutralNodes(r.Reader.AllNodes()) }
func (r fingerprintNeutralReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	in := r.Reader.GetNodesByIDs(ids)
	out := make(map[string]*graph.Node, len(in))
	for k, v := range in {
		out[k] = neutralNode(v)
	}
	return out
}
func (r fingerprintNeutralReader) NodesByKind(k graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for n := range r.Reader.NodesByKind(k) {
			if !yield(neutralNode(n)) {
				return
			}
		}
	}
}

// editDeltaProbeGate skips the diagnostic probes unless GX_EDIT_DELTA_PROBE=1:
// they measure the per-save engine against a whole index and are expected to
// report the engine's residual classes, not to pass.
func editDeltaProbeGate(t *testing.T) {
	t.Helper()
	if os.Getenv("GX_EDIT_DELTA_PROBE") != "1" {
		t.Skip("diagnostic probe; set GX_EDIT_DELTA_PROBE=1")
	}
}

// TestEditDeltaProbePrimaryPerSaveAgainstFlat measures how far the primary
// per-save path (the engine the delta path reuses) is from a flat whole index
// on the generated corpus. Diagnostic only.
func TestEditDeltaProbePrimaryPerSaveAgainstFlat(t *testing.T) {
	editDeltaProbeGate(t)
	for _, seed := range propSeedCorpus(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			repo := propNewRepo(t, rng)
			store := builderOpenStore(t, "base")
			idx := New(store, builderRegistry(), propIndexConfig(), zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			if _, err := idx.Index(repo.dir); err != nil {
				t.Fatalf("index: %v", err)
			}
			script, touched := propApplyScript(t, repo, rng)
			for _, mv := range script.Renames {
				touched = append(touched, mv[0], mv[1])
			}
			if _, err := idx.IncrementalReindexPaths(repo.dir, touched); err != nil {
				t.Fatalf("incremental: %v", err)
			}
			flat := builderOpenStore(t, "flat")
			propIndex(t, flat, repo.dir)
			propReportDivergence(t, seed, []propScript{script}, nil,
				propCompare(fingerprintNeutralReader{store}, fingerprintNeutralReader{flat}))
		})
	}
}

func TestEditDeltaProbeWholeIndexArgOf(t *testing.T) {
	editDeltaProbeGate(t)
	rng := rand.New(rand.NewSource(1))
	repo := propNewRepo(t, rng)
	flat := builderOpenStore(t, "flat")
	propIndex(t, flat, repo.dir)
	param, fn := 0, 0
	for e := range flat.EdgesByKind(graph.EdgeArgOf) {
		if strings.Contains(e.To, "#param:") {
			param++
		} else {
			fn++
			t.Logf("arg_of %s -> %s", e.From, e.To)
		}
	}
	t.Logf("whole index arg_of: param=%d other=%d", param, fn)
}

func TestEditDeltaProbeWholeIndexReturnsTo(t *testing.T) {
	editDeltaProbeGate(t)
	store := builderOpenStore(t, "acc")
	repoDir := accumulatedDirtyRepo(t, accumulatedDirtyIndependent, store)
	_ = repoDir
	n := 0
	for e := range store.EdgesByKind(graph.EdgeReturnsTo) {
		if strings.Contains(e.FilePath, "p000") {
			t.Logf("returns_to %s -> %s meta=%v", e.From, e.To, e.Meta)
		}
		n++
	}
	t.Logf("returns_to edges: %d", n)
}

func TestEditDeltaProbeSignature(t *testing.T) {
	editDeltaProbeGate(t)
	r := newInvalidationRun(t, "signature", 1)
	src := `package b

import "` + accumulatedDirtyModule + `/chain/c"

// B calls down the chain.
func B(delta int) int {
return c.C() + delta
}
`
	r.write("chain/b/b.go", src)
	id, _ := r.build()
	t.Logf("delta: %+v", *LastEditDeltaReport())
	composed := r.composed(id)
	for _, e := range composed.GetOutEdges(builderRepoPrefix + "/chain/a/a.go::A") {
		t.Logf("delta A out: %s -> %s %s meta=%v", e.From, e.To, e.Kind, e.Meta)
	}
	primary := primaryPerSaveOf(t, r.repoDir, []string{accumulatedDirtyUnitPath(invalidationLayout, 0), "chain/b/b.go"}, config.Default().Index)
	for _, e := range primary.GetOutEdges(builderRepoPrefix + "/chain/a/a.go::A") {
		t.Logf("primary A out: %s -> %s %s meta=%v", e.From, e.To, e.Kind, e.Meta)
	}
}

func TestEditDeltaProbeDeleteDirtyFile(t *testing.T) {
	editDeltaProbeGate(t)
	r := newInvalidationRun(t, "delete", 1)
	r.write("chain/d/d.go", invalidationDirtyD)
	r.build()
	r.remove("chain/d/d.go")
	id, _ := r.build()
	t.Logf("delta: %+v", *LastEditDeltaReport())
	composed := r.composed(id)
	primary := primaryPerSaveOf(t, r.repoDir, []string{accumulatedDirtyUnitPath(invalidationLayout, 0), "chain/d/d.go"}, config.Default().Index)
	clean := builderOpenStore(t, "clean")
	builderIndex(t, clean, r.repoDir)
	for _, row := range editDeltaTriangulate(composed, primary, clean) {
		t.Logf("BAD %s", row)
	}
	for _, id := range []string{"dep::example.com/fixture/chain/d::D", "external-call::dep::example.com/fixture/chain/d", "module::go:example.com/fixture/chain/d"} {
		t.Logf("%s delta=%v primary=%v clean=%v", id, composed.GetNode(id) != nil, primary.GetNode(id) != nil, clean.GetNode(id) != nil)
	}
}

func TestEditDeltaProbePrimaryImportIntoEditedFile(t *testing.T) {
	editDeltaProbeGate(t)
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderWriteTree(t, dir, replayTree())
	store := builderOpenStore(t, "p")
	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(dir); err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		for _, e := range store.GetOutEdges(builderRepoPrefix + "/c/c.go") {
			if e.Kind == graph.EdgeImports && e.To == builderRepoPrefix+"/a/a.go" {
				return true
			}
		}
		return false
	}
	t.Logf("before: c imports a = %v", has())
	full := dir + "/a/a.go"
	src, _ := os.ReadFile(full)
	edited := strings.Replace(string(src), "{\n", "{\n\t_ = 1\n", 1)
	if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.IncrementalReindexPaths(dir, []string{"a/a.go"}); err != nil {
		t.Fatal(err)
	}
	t.Logf("after primary per-save: c imports a = %v", has())
}
