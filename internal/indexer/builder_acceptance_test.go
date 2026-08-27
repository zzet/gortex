package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The sparse-generation acceptance fixture.
//
// Two committed states of one repository, indexed two ways. The reference is a
// plain whole index of the second state. The subject is the first state's
// corpus with a commit layer built by the production builder composed on top.
// Everything a graph.Reader answers is compared between them: a sparse
// generation is only correct if no reader can tell it from the flat index of
// the tree it describes.
//
// The fixture is shaped so the closure has to do real work. core.go's function
// is renamed AND its signature changed, and caller.go — which is not in the
// diff at all — calls it. Without the reverse half of the closure, caller.go
// would keep the base layer's edge into the old identity while the flat index
// has it parked on an unresolved stub. helper.go is the forward half: core.go
// calls into it, so the generation must carry it or core.go's own call would
// fail to bind. island.go is touched by neither and must keep showing through
// from the base layer, which is what makes the generation sparse.

const builderRepoPrefix = "repo"

// Every signature in the fixture is typed by a repository-local type. That is
// not decoration: a builtin type would make the resolver materialise a
// repo-scoped stub, which lives at no file and is therefore the one class of
// payload a file-granular mask cannot reach. It has a claim of its own and a
// test of its own — TestSparseGenerationClaimsPathlessIdentities — and keeping
// it out of this fixture is what lets the differential below be exhaustive
// rather than qualified.
func builderTreeA() map[string]string {
	return map[string]string{
		"core.go": `package fixture

type Options struct{}

func Compute(o Options) {
	Helper()
}
`,
		"caller.go": `package fixture

func Run() {
	Compute(Options{})
}
`,
		"helper.go": `package fixture

func Helper() {
}
`,
		"island.go": `package fixture

func Island() {
}
`,
		"gone.go": `package fixture

func Gone() {
}
`,
		"oldname.go": `package fixture

func Renamed() {
}
`,
	}
}

func builderTreeB() map[string]string {
	tree := builderTreeA()
	delete(tree, "gone.go")
	renamed := tree["oldname.go"]
	delete(tree, "oldname.go")
	tree["newname.go"] = renamed
	tree["core.go"] = `package fixture

type Options struct{}

func Calculate(o Options, again Options) {
	Helper()
}
`
	tree["added.go"] = `package fixture

func Added() {
	Calculate(Options{}, Options{})
}
`
	return tree
}

// --- fixture plumbing ---------------------------------------------------

// builderIsolateGit pins the git environment for the whole test: no user or
// system config, fixed identity, no prompts. A developer's ~/.gitconfig must
// not be able to change what the fixture commits.
func builderIsolateGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "builder test")
	t.Setenv("GIT_AUTHOR_EMAIL", "builder@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "builder test")
	t.Setenv("GIT_COMMITTER_EMAIL", "builder@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// builderTempDir returns a temp directory with every symlink resolved. On
// macOS t.TempDir() sits under /var, which is a symlink to /private/var, and
// git reports the resolved spelling.
func builderTempDir(t testing.TB, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

func builderGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// builderWriteTree replaces dir's contents with tree, leaving any .git alone.
func builderWriteTree(t testing.TB, dir string, tree map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatalf("remove %s: %v", entry.Name(), err)
		}
	}
	for name, body := range tree {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func builderOpenStore(t testing.TB, name string) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatalf("open %s store: %v", name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func builderRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return reg
}

// builderIndex runs one plain whole index of dir into store — the reference
// half of every differential below.
func builderIndex(t testing.TB, store *store_sqlite.Store, dir string) {
	t.Helper()
	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("index %s: %v", dir, err)
	}
}

func builderNewBuilder(store *store_sqlite.Store) *SparseGenerationBuilder {
	return &SparseGenerationBuilder{
		Store:    store,
		Registry: builderRegistry(),
		Config:   config.Default().Index,
		Logger:   zap.NewNop(),
	}
}

// builderComposed stacks one published generation over the base corpus, the
// same way MaterializeCheckout composes a checkout's commit generation.
func builderComposed(t *testing.T, store *store_sqlite.Store, generationID int64) graph.Reader {
	t.Helper()
	layer, err := graphview.NewGenerationLayer(store.AtGeneration(generationID))
	if err != nil {
		t.Fatalf("NewGenerationLayer(%d): %v", generationID, err)
	}
	id, err := graphview.NewRepoViewID(builderRepoPrefix, "graph-fixture", generationID)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	reader, _, err := graphview.ComposeRepoView(
		graph.NewOverlaidViewWithLayer(store.AtGeneration(0), layer), nil, id)
	if err != nil {
		t.Fatalf("ComposeRepoView: %v", err)
	}
	return reader
}

// builderAssertMasksValidate checks a published generation's ownership claims
// against the payload it carries.
//
// PublishPayloadGeneration already runs this check, so a build that reached
// publish has passed it once. Restating it here is the pin: the invariant
// currently holds by construction — writeMasks derives replace masks from
// exactly the files-rows-or-node-paths set the validator probes, and refuses
// payload at a deleted path — and nothing in this package would notice if
// either the derivation or the store's publish-time guard stopped agreeing.
func builderAssertMasksValidate(t *testing.T, store *store_sqlite.Store, generationID int64) {
	t.Helper()
	if err := store.AtGeneration(generationID).ValidateGenerationMasks(); err != nil {
		t.Errorf("generation %d carries payload its own masks do not speak for: %v", generationID, err)
	}
}

// --- the differential ---------------------------------------------------

// builderRenderNode prints every field of a node except the one the graph
// never persists: AbsoluteFilePath is stamped on per-response copies by the
// MCP layer, so it is view-only metadata rather than content.
func builderRenderNode(n *graph.Node) string {
	if n == nil {
		return "<nil>"
	}
	copied := *n
	copied.AbsoluteFilePath = ""
	return fmt.Sprintf("%+v", copied)
}

// builderRenderEdge prints every field of an edge except the ones response
// encoders fill in on demand and the store never writes.
func builderRenderEdge(e *graph.Edge) string {
	if e == nil {
		return "<nil>"
	}
	copied := *e
	copied.Context = ""
	copied.ReturnUsage = ""
	copied.Via = ""
	copied.Alias = ""
	copied.NameOnly = false
	return fmt.Sprintf("%+v", copied)
}

func builderRenderNodes(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, builderRenderNode(n))
	}
	slices.Sort(out)
	return out
}

func builderRenderEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, builderRenderEdge(e))
	}
	slices.Sort(out)
	return out
}

// builderNodeIDs is a reader's identity set, sorted.
func builderNodeIDs(r graph.Reader) []string {
	nodes := r.AllNodes()
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func builderCollectNodes(seq func(func(*graph.Node) bool)) []*graph.Node {
	var out []*graph.Node
	for n := range seq {
		out = append(out, n)
	}
	return out
}

func builderCollectEdges(seq func(func(*graph.Edge) bool)) []*graph.Edge {
	var out []*graph.Edge
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func builderSameStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("%s\n composed: %s\n     flat: %s",
		what, strings.Join(got, "\n           "), strings.Join(want, "\n           "))
}

// builderProbes is every identity, path, name and kind either graph could
// answer for. Building it from the union of both sides is what makes the
// comparison exhaustive: an identity only one side carries is probed on both,
// so a row that should have vanished and did not is caught by name and not
// only as a set difference.
type builderProbes struct {
	ids       []string
	files     []string
	names     []string
	qualNames []string
	kinds     []graph.NodeKind
	edgeKinds []graph.EdgeKind
}

func builderProbesFor(readers ...graph.Reader) builderProbes {
	var p builderProbes
	ids := map[string]struct{}{}
	files := map[string]struct{}{}
	names := map[string]struct{}{}
	quals := map[string]struct{}{}
	kinds := map[graph.NodeKind]struct{}{}
	edgeKinds := map[graph.EdgeKind]struct{}{}
	for _, r := range readers {
		for _, n := range r.AllNodes() {
			if n == nil {
				continue
			}
			ids[n.ID] = struct{}{}
			if n.FilePath != "" {
				files[n.FilePath] = struct{}{}
			}
			if n.Name != "" {
				names[n.Name] = struct{}{}
			}
			if n.QualName != "" {
				quals[n.QualName] = struct{}{}
			}
			kinds[n.Kind] = struct{}{}
		}
		for _, e := range r.AllEdges() {
			if e == nil {
				continue
			}
			ids[e.From] = struct{}{}
			ids[e.To] = struct{}{}
			edgeKinds[e.Kind] = struct{}{}
		}
	}
	for id := range ids {
		p.ids = append(p.ids, id)
	}
	for f := range files {
		p.files = append(p.files, f)
	}
	for n := range names {
		p.names = append(p.names, n)
	}
	for q := range quals {
		p.qualNames = append(p.qualNames, q)
	}
	for k := range kinds {
		p.kinds = append(p.kinds, k)
	}
	for k := range edgeKinds {
		p.edgeKinds = append(p.edgeKinds, k)
	}
	slices.Sort(p.ids)
	slices.Sort(p.files)
	slices.Sort(p.names)
	slices.Sort(p.qualNames)
	slices.Sort(p.kinds)
	slices.Sort(p.edgeKinds)
	return p
}

// builderAssertReadersAgree drives every read on the graph.Reader surface
// through both graphs and compares the answers.
func builderAssertReadersAgree(t *testing.T, composed, flat graph.Reader) {
	t.Helper()
	probes := builderProbesFor(composed, flat)

	builderSameStrings(t, "AllNodes",
		builderRenderNodes(composed.AllNodes()), builderRenderNodes(flat.AllNodes()))
	builderSameStrings(t, "AllEdges",
		builderRenderEdges(composed.AllEdges()), builderRenderEdges(flat.AllEdges()))
	if got, want := composed.NodeCount(), flat.NodeCount(); got != want {
		t.Errorf("NodeCount = %d, flat corpus has %d", got, want)
	}
	if got, want := composed.EdgeCount(), flat.EdgeCount(); got != want {
		t.Errorf("EdgeCount = %d, flat corpus has %d", got, want)
	}

	for _, id := range probes.ids {
		builderSameStrings(t, "GetNode("+id+")",
			[]string{builderRenderNode(composed.GetNode(id))},
			[]string{builderRenderNode(flat.GetNode(id))})
		builderSameStrings(t, "GetOutEdges("+id+")",
			builderRenderEdges(composed.GetOutEdges(id)), builderRenderEdges(flat.GetOutEdges(id)))
		builderSameStrings(t, "GetInEdges("+id+")",
			builderRenderEdges(composed.GetInEdges(id)), builderRenderEdges(flat.GetInEdges(id)))
	}

	composedOut, flatOut := composed.GetOutEdgesByNodeIDs(probes.ids), flat.GetOutEdgesByNodeIDs(probes.ids)
	composedIn, flatIn := composed.GetInEdgesByNodeIDs(probes.ids), flat.GetInEdgesByNodeIDs(probes.ids)
	composedNodes, flatNodes := composed.GetNodesByIDs(probes.ids), flat.GetNodesByIDs(probes.ids)
	for _, id := range probes.ids {
		builderSameStrings(t, "GetOutEdgesByNodeIDs["+id+"]",
			builderRenderEdges(composedOut[id]), builderRenderEdges(flatOut[id]))
		builderSameStrings(t, "GetInEdgesByNodeIDs["+id+"]",
			builderRenderEdges(composedIn[id]), builderRenderEdges(flatIn[id]))
		builderSameStrings(t, "GetNodesByIDs["+id+"]",
			[]string{builderRenderNode(composedNodes[id])}, []string{builderRenderNode(flatNodes[id])})
	}

	for _, path := range probes.files {
		builderSameStrings(t, "GetFileNodes("+path+")",
			builderRenderNodes(composed.GetFileNodes(path)), builderRenderNodes(flat.GetFileNodes(path)))
	}
	builderSameStrings(t, "GetRepoNodes",
		builderRenderNodes(composed.GetRepoNodes(builderRepoPrefix)),
		builderRenderNodes(flat.GetRepoNodes(builderRepoPrefix)))

	for _, name := range probes.names {
		builderSameStrings(t, "FindNodesByName("+name+")",
			builderRenderNodes(composed.FindNodesByName(name)), builderRenderNodes(flat.FindNodesByName(name)))
	}
	for _, qual := range probes.qualNames {
		builderSameStrings(t, "GetNodeByQualName("+qual+")",
			[]string{builderRenderNode(composed.GetNodeByQualName(qual))},
			[]string{builderRenderNode(flat.GetNodeByQualName(qual))})
	}
	for _, kind := range probes.kinds {
		builderSameStrings(t, "NodesByKind("+string(kind)+")",
			builderRenderNodes(builderCollectNodes(composed.NodesByKind(kind))),
			builderRenderNodes(builderCollectNodes(flat.NodesByKind(kind))))
	}
	for _, kind := range probes.edgeKinds {
		builderSameStrings(t, "EdgesByKind("+string(kind)+")",
			builderRenderEdges(builderCollectEdges(composed.EdgesByKind(kind))),
			builderRenderEdges(builderCollectEdges(flat.EdgesByKind(kind))))
	}
}

// --- acceptance 1: the commit layer -------------------------------------

func TestCommitLayerComposesLikeAFlatIndex(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, builderTreeA())

	builderWriteTree(t, repoDir, builderTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dirA)

	generationID, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-fixture",
			LayerID:             "layer-" + treeB,
			CheckoutID:          "checkout-fixture",
			ProvenanceCommitOID: commitB,
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   treeA,
		TargetTreeOID: treeB,
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}

	// The generation has to be sparse and the closure has to have fired, or
	// agreeing with the flat index would prove nothing about either.
	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d in a six-file fixture", report.ClosureCap)
	}
	for _, want := range []string{"caller.go", "helper.go"} {
		if !slices.Contains(report.ClosurePaths, want) {
			t.Fatalf("closure %v does not carry %s", report.ClosurePaths, want)
		}
	}
	if slices.Contains(report.IndexedPaths, "island.go") {
		t.Fatalf("island.go is in the generation's file set %v — the build is not sparse", report.IndexedPaths)
	}
	if report.DeleteMasks != 2 {
		t.Fatalf("delete masks = %d, want gone.go and oldname.go", report.DeleteMasks)
	}
	// Every identity in this fixture lives in a file, so the file masks reach
	// all of it and neither pathless claim has anything to say. A tombstone
	// here would mean the masks missed something.
	if report.UnmaskedPayloadNodes != 0 || report.NodeTombstones != 0 {
		t.Fatalf("%d payload nodes live at no path in a fixture that has none",
			report.UnmaskedPayloadNodes)
	}
	if report.ContestedEdgeSources != 0 {
		t.Fatalf("%d edge sources were replaced while the corpus still held edges from them",
			report.ContestedEdgeSources)
	}

	flat := builderOpenStore(t, "flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderTreeB())
	builderIndex(t, flat, dirB)

	composed := builderComposed(t, store, generationID)
	// The layer has to be doing work, or agreeing with the flat index would
	// prove nothing about it. Node counts can coincide, so the identities are
	// what is compared.
	if corpus := builderNodeIDs(store.AtGeneration(0)); slices.Equal(corpus, builderNodeIDs(composed)) {
		t.Fatalf("the composed view carries the corpus's identities verbatim — the layer changed nothing")
	}
	if n := composed.GetNode(builderRepoPrefix + "/gone.go::Gone"); n != nil {
		t.Errorf("a deleted file's symbol survived the layer: %s", builderRenderNode(n))
	}
	if n := composed.GetNode(builderRepoPrefix + "/island.go::Island"); n == nil {
		t.Error("island.go is masked by no generation and must show through from the corpus")
	}
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// --- acceptance 2: edges recorded outside the generation's files ---------

// builderReverseFlowTreeA is the smallest tree where one symbol's outgoing
// edges are recorded in more than one file.
//
// A tail call makes the callee's Result flow into the caller, and the resolver
// records that flow at the CALLER's file while its source is the callee. So
// helper.go::Helper has two outgoing value flows — alpha.go's and core.go's —
// and neither one lives in helper.go.
func builderReverseFlowTreeA() map[string]string {
	return map[string]string{
		"types.go": `package fixture

type Options struct{}

type Result struct{}
`,
		"helper.go": `package fixture

func Helper(o Options) Result {
	return Result{}
}
`,
		"alpha.go": `package fixture

func Alpha(o Options) Result {
	return Helper(o)
}
`,
		"core.go": `package fixture

func Core(o Options) Result {
	return Helper(o)
}
`,
	}
}

func builderReverseFlowTreeB() map[string]string {
	tree := builderReverseFlowTreeA()
	tree["core.go"] = `package fixture

func Core(o Options) Result {
	// revision 1
	return Helper(o)
}
`
	return tree
}

// TestCommitLayerKeepsEdgesRecordedOutsideItsFiles pins whose edge a generation
// replaces: the file the edge was RECORDED in, not the file its source lives
// in.
//
// Only core.go changes, so the closure carries helper.go — core.go resolves
// into it — and stops there: alpha.go is a dependent of helper.go, not of a
// seed, and one hop does not reach it. The generation therefore claims
// helper.go and re-derives every edge helper.go holds, which does not include
// alpha.go's value flow out of Helper. A layer that read its claim on
// helper.go as a claim on Helper's whole adjacency would hide that base row
// and put nothing back, and the flat index of the same tree still has it.
func TestCommitLayerKeepsEdgesRecordedOutsideItsFiles(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	builderWriteTree(t, repoDir, builderReverseFlowTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, builderReverseFlowTreeA())

	builderWriteTree(t, repoDir, builderReverseFlowTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dirA)

	generationID, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-fixture",
			LayerID:             "layer-" + treeB,
			CheckoutID:          "checkout-fixture",
			ProvenanceCommitOID: commitB,
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   treeA,
		TargetTreeOID: treeB,
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	// The case only bites while the generation claims the callee's file and
	// not the untouched caller's.
	if !slices.Contains(report.ClosurePaths, "helper.go") {
		t.Fatalf("closure %v does not carry helper.go — core.go's callee must be re-derived", report.ClosurePaths)
	}
	if slices.Contains(report.IndexedPaths, "alpha.go") {
		t.Fatalf("alpha.go is in the generation's file set %v — the untouched caller must stay below",
			report.IndexedPaths)
	}

	flat := builderOpenStore(t, "flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderReverseFlowTreeB())
	builderIndex(t, flat, dirB)

	composed := builderComposed(t, store, generationID)
	helper := builderRepoPrefix + "/helper.go::Helper"
	alphaFlow := builderRenderEdges(flat.GetOutEdges(helper))
	if len(alphaFlow) == 0 {
		t.Fatal("the flat index records no edge out of Helper — the fixture no longer produces a reverse flow")
	}
	builderSameStrings(t, "GetOutEdges("+helper+") lost a row recorded in an unclaimed file",
		builderRenderEdges(composed.GetOutEdges(helper)), alphaFlow)
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// --- the pathless identities a file mask cannot reach --------------------

// TestSparseGenerationClaimsPathlessIdentities pins the one class of payload a
// file-granular mask cannot speak for: the resolver's repo-scoped stubs, whose
// ids carry no path component. The generation re-materialises them, so without
// an identity-level claim its copy would surface beside the one still showing
// through from the corpus.
//
// It also pins the reach of that claim. The node tombstone and the edge-source
// marker settle every reader that answers by identity. The two that answer from
// the layer's FILE list — GetRepoNodes and the node counter — cannot see a node
// that lives at no path, and that is a gap in the composition rather than in
// the claim: nothing the builder can write puts a pathless node into a file
// list. Naming it here is what keeps it from being rediscovered as a mystery.
func TestSparseGenerationClaimsPathlessIdentities(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	// A builtin return type is all it takes: the resolver materialises one
	// repo-scoped `builtin` node for `int` and every signature binds to it.
	treeA := map[string]string{
		"core.go": `package fixture

func Compute() int {
	return 1
}
`,
		"caller.go": `package fixture

func Run() int {
	return Compute()
}
`,
	}
	treeB := map[string]string{
		"core.go": `package fixture

func Calculate() int {
	return 2
}
`,
		"caller.go": treeA["caller.go"],
	}

	builderWriteTree(t, repoDir, treeA)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeAOID := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, treeA)

	builderWriteTree(t, repoDir, treeB)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeBOID := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dirA)

	generationID, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
			LayerID: "layer-" + treeBOID, CheckoutID: "checkout-fixture",
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   treeAOID,
		TargetTreeOID: treeBOID,
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if report.UnmaskedPayloadNodes == 0 {
		t.Fatal("the fixture's builtin return type produces no pathless payload — it no longer tests anything")
	}
	if report.NodeTombstones != report.UnmaskedPayloadNodes {
		t.Fatalf("tombstones = %d for %d pathless nodes; every one of them must be claimed",
			report.NodeTombstones, report.UnmaskedPayloadNodes)
	}

	tombstones, err := store.AtGeneration(generationID).NodeTombstones()
	if err != nil {
		t.Fatalf("NodeTombstones: %v", err)
	}
	for _, id := range tombstones {
		if graph.IDFile(id) != "" && !graph.IsStub(id) {
			t.Errorf("tombstone %q names an identity that lives in a file — its file mask already replaces it", id)
		}
	}

	builtinID := builderRepoPrefix + "::builtin::go::type::int"
	if !slices.Contains(tombstones, builtinID) {
		t.Fatalf("tombstones %v do not claim the builtin the generation re-materialised", tombstones)
	}

	composed := builderComposed(t, store, generationID)
	if n := composed.GetNode(builtinID); n == nil {
		t.Fatal("the claimed builtin vanished from the composed view instead of being replaced")
	}
	if got := builderCountID(builderNodeIDs(composed), builtinID); got != 1 {
		t.Errorf("AllNodes carries the builtin %d times, want once", got)
	}
	if got := len(composed.FindNodesByName("int")); got != 1 {
		t.Errorf("FindNodesByName(int) returns %d nodes, want one", got)
	}

	// The known gap, asserted so a fix to the composition breaks this test
	// rather than going unnoticed: both readers below answer from the layer's
	// file list, which no pathless node can appear in.
	flat := builderOpenStore(t, "flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, treeB)
	builderIndex(t, flat, dirB)

	repoNodes := builderRenderNodes(composed.GetRepoNodes(builderRepoPrefix))
	if len(repoNodes) != len(builderRenderNodes(flat.GetRepoNodes(builderRepoPrefix)))-1 {
		t.Errorf("GetRepoNodes now returns %d nodes against the flat index's %d — "+
			"the composition may have learned to union the layer's pathless identities",
			len(repoNodes), len(flat.GetRepoNodes(builderRepoPrefix)))
	}
	if composed.NodeCount() != flat.NodeCount()-1 {
		t.Errorf("NodeCount is %d against the flat index's %d — "+
			"the counter may have learned to price the layer's pathless identities",
			composed.NodeCount(), flat.NodeCount())
	}
}

// TestCommitLayerRefusesANonOID pins the gate on the two values that go
// straight to a git subprocess. An abbreviation is refused along with the
// obvious junk: an abbreviated id is ambiguous, and a caller naming the tree a
// generation is built from must name exactly one.
func TestCommitLayerRefusesANonOID(t *testing.T) {
	const good = "0123456789abcdef0123456789abcdef01234567"
	for _, bad := range []string{
		"", "HEAD", "main", "0123456", strings.ToUpper(good),
		good[:39], good + "0", "--upload-pack=/bin/sh", "0123456789abcdef0123456789abcdef0123456g",
	} {
		if validGitOID(bad) {
			t.Errorf("validGitOID(%q) accepted a value that is not an object id", bad)
		}
	}
	if !validGitOID(good) || !validGitOID(good+good[:24]) {
		t.Error("validGitOID rejected a full sha-1 or sha-256 object id")
	}

	store := builderOpenStore(t, "base")
	_, _, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity:      GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "graph-fixture"},
		Base:          store,
		RepoDir:       t.TempDir(),
		BaseTreeOID:   good,
		TargetTreeOID: "HEAD",
		RepoPrefix:    builderRepoPrefix,
	})
	if !errors.Is(err, ErrInvalidTreeOID) {
		t.Fatalf("BuildCommitLayer accepted a ref where a tree id belongs: %v", err)
	}
}

// --- the truncated closure ----------------------------------------------

// TestSparseGenerationReportsATruncatedClosure pins the one completeness fact
// a sparse generation cannot fix and must therefore state: when the affected
// closure hits its cap, a dependent past the cut keeps reading the layer below
// and is stale against the content the generation carries.
//
// Silence here would be the failure mode. The generation still publishes — a
// partly-refreshed view is worth more than none — so the only thing standing
// between a caller and a wrong answer is the report and the narrowed
// local-resolution producer state.
func TestSparseGenerationReportsATruncatedClosure(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, builderTreeA())

	builderWriteTree(t, repoDir, builderTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dirA)

	builder := builderNewBuilder(store)
	// core.go's change reaches two files — its dependent and its dependency —
	// so a cap of one has to cut the closure.
	builder.Config.AffectedByReresolveMax = 1

	generationID, report, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
			LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture",
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   treeA,
		TargetTreeOID: treeB,
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if !report.ClosureTruncated {
		t.Fatalf("closure %v was not reported truncated at cap %d", report.ClosurePaths, report.ClosureCap)
	}
	if report.ClosureCap != 1 || report.ClosureFiles != 1 {
		t.Fatalf("closure cap %d kept %d files, want 1 and 1", report.ClosureCap, report.ClosureFiles)
	}

	states, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	var local store_sqlite.ProducerCompleteness
	for _, row := range states {
		if row.Producer == string(graphview.CapResolutionLocal) {
			local = row
		}
	}
	if local.State != store_sqlite.ProducerStateIncomplete {
		t.Fatalf("local resolution is %q after a truncated closure, want incomplete", local.State)
	}
	if !strings.Contains(local.Reason, "truncated") {
		t.Fatalf("local resolution's reason %q does not say the closure was cut", local.Reason)
	}
}

// builderCountID counts how many times id appears in a sorted identity list.
func builderCountID(ids []string, id string) int {
	n := 0
	for _, candidate := range ids {
		if candidate == id {
			n++
		}
	}
	return n
}
