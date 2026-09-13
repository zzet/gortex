package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The context/output separation acceptance.
//
// The sparse builder widens its file set to the affected closure because the
// pass can only resolve against what the same generation carries. What it
// KEEPS is a separate question, and this file pins the answer: a closure file
// the pass merely read, and re-derived identically to the layer below, leaves
// the generation carrying no payload and claiming nothing — while a closure
// file whose resolution genuinely moved keeps its claim, payload and all.
//
// Both halves are checked against the same oracle every other builder
// acceptance uses: the composed view must equal a fresh isolated index of the
// same tree, field for field.

// builderContextTreeA is a tree with one forward dependency (core.go calls
// helper.go's Helper) and one reverse dependency (caller.go calls core.go's
// Compute), so the closure of a change in core.go spans both directions.
func builderContextTreeA() map[string]string {
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
	}
}

// builderContextTreeBodyEdit changes one function BODY and nothing else. No
// signature, no identity, no exported shape: every closure file re-derives to
// exactly what the layer below already holds.
func builderContextTreeBodyEdit() map[string]string {
	tree := builderContextTreeA()
	tree["core.go"] = `package fixture

type Options struct{}

func Compute(o Options) {
	Helper()
	Helper()
}
`
	return tree
}

// builderContextTreeRename renames the symbol the closure's reverse dependency
// calls, and leaves that dependency's bytes alone. caller.go is therefore a
// context file by every syntactic measure — the change set does not name it —
// while its RESOLUTION moves: the call it makes no longer binds where it did.
// Serving it from the layer below would serve a call to a symbol that the
// target tree does not have.
func builderContextTreeRename() map[string]string {
	tree := builderContextTreeA()
	tree["core.go"] = `package fixture

type Options struct{}

func Calculate(o Options) {
	Helper()
}
`
	return tree
}

// builderContextGeneration runs one commit-layer build from treeA to treeB
// through the production entrypoint and returns the generation and its report.
func builderContextGeneration(
	t *testing.T, store *store_sqlite.Store, treeA, treeB map[string]string,
) (string, int64, BuildReport) {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	builderWriteTree(t, repoDir, treeA)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	baseTree := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	dirA := builderTempDir(t, "checkout-a")
	builderWriteTree(t, dirA, treeA)

	builderWriteTree(t, repoDir, treeB)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	targetTree := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	targetCommit := builderGit(t, repoDir, "rev-parse", "HEAD")

	builderIndex(t, store, dirA)

	generationID, report, err := builderNewBuilder(store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-fixture",
			LayerID:             "layer-" + targetTree,
			CheckoutID:          "checkout-fixture",
			ProvenanceCommitOID: targetCommit,
		},
		Base:          store,
		RepoDir:       repoDir,
		BaseTreeOID:   baseTree,
		TargetTreeOID: targetTree,
		RootPath:      dirA,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	return repoDir, generationID, report
}

// builderContextPayloadPaths is the payload census: every path the generation
// physically carries a node or an edge at.
func builderContextPayloadPaths(t *testing.T, store *store_sqlite.Store, generationID int64) []string {
	t.Helper()
	handle := store.AtGeneration(generationID)
	seen := map[string]struct{}{}
	for _, node := range handle.AllNodes() {
		if node != nil {
			seen[node.FilePath] = struct{}{}
		}
	}
	for _, edge := range handle.AllEdges() {
		if edge != nil && edge.FilePath != "" {
			seen[edge.FilePath] = struct{}{}
		}
	}
	rows, err := handle.FileMetasForRepo(builderRepoPrefix)
	if err != nil {
		t.Fatalf("FileMetasForRepo: %v", err)
	}
	for _, row := range rows {
		seen[row.FilePath] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

func builderContextMaskModes(
	t *testing.T, store *store_sqlite.Store, generationID int64,
) map[string]store_sqlite.OwnershipMode {
	t.Helper()
	masks, err := store.AtGeneration(generationID).FileMasks()
	if err != nil {
		t.Fatalf("FileMasks: %v", err)
	}
	modes := make(map[string]store_sqlite.OwnershipMode, len(masks))
	for _, mask := range masks {
		modes[mask.FilePath] = mask.Mode
	}
	return modes
}

// TestBodyEditCarriesPayloadForTheChangedFileAlone is the gate-3 first clause
// end to end, through BuildCommitLayer: a body-only edit whose closure spans
// the files on both sides of it leaves the generation carrying payload for the
// edited file and nothing else, while the closure files it read are declared
// read-only context. The composed view still equals a fresh isolated index.
func TestBodyEditCarriesPayloadForTheChangedFileAlone(t *testing.T) {
	store := builderOpenStore(t, "base")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextTreeA(), builderContextTreeBodyEdit())

	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d in a four-file fixture", report.ClosureCap)
	}
	if len(report.ClosurePaths) == 0 {
		t.Fatal("the closure added no context file — the fixture proves nothing about context")
	}
	if len(report.ContextRetainedPaths) != 0 {
		t.Fatalf("a body-only edit retained a claim over %v", report.ContextRetainedPaths)
	}
	if report.ContextMasks != len(report.ClosurePaths) {
		t.Fatalf("context masks = %d for %d closure paths (%v)",
			report.ContextMasks, len(report.ClosurePaths), report.ClosurePaths)
	}

	// The payload census. The changed file is the only path the generation
	// carries a row at — that is the write the closure used to multiply.
	changed := builderRepoPrefix + "/core.go"
	if got := builderContextPayloadPaths(t, store, generationID); !slices.Equal(got, []string{changed}) {
		t.Fatalf("the generation carries payload at %v, want only %q", got, changed)
	}

	modes := builderContextMaskModes(t, store, generationID)
	if modes[changed] != store_sqlite.OwnershipReplace {
		t.Fatalf("mask on the changed file = %q, want replace", modes[changed])
	}
	for _, rel := range report.ClosurePaths {
		graphPath := builderRepoPrefix + "/" + rel
		if modes[graphPath] != store_sqlite.OwnershipContext {
			t.Fatalf("mask on closure path %q = %q, want context", graphPath, modes[graphPath])
		}
	}
	if len(modes) != 1+len(report.ClosurePaths) {
		t.Fatalf("masks = %v, want one replace and one context per closure path", modes)
	}

	// The oracle: composing the sparse generation over the corpus must equal a
	// fresh isolated index of the same tree.
	flat := builderOpenStore(t, "flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderContextTreeBodyEdit())
	builderIndex(t, flat, dirB)

	composed := builderComposed(t, store, generationID)
	// The context files' identities still answer, from below, and the changed
	// file's re-derived calls into them still land.
	helper := builderRepoPrefix + "/helper.go::Helper"
	if composed.GetNode(helper) == nil {
		t.Fatalf("a context file's symbol vanished from the composed view")
	}
	if len(composed.GetInEdges(helper)) == 0 {
		t.Fatalf("the changed file's calls into a context symbol were lost")
	}
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// TestContextFileWhoseResolutionMovedKeepsItsClaim is the other half of the
// same rule. caller.go's bytes do not change, but the symbol it calls is
// renamed, so its re-derivation disagrees with the layer below — and serving
// it from below would serve a call to a symbol that no longer exists. The file
// keeps its claim, is named in the report, and the oracle still holds.
func TestContextFileWhoseResolutionMovedKeepsItsClaim(t *testing.T) {
	store := builderOpenStore(t, "base")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextTreeA(), builderContextTreeRename())

	callerPath := builderRepoPrefix + "/caller.go"
	if !slices.Contains(report.ClosurePaths, "caller.go") {
		t.Fatalf("caller.go is not in the closure %v — the fixture proves nothing about retention",
			report.ClosurePaths)
	}
	if !slices.Contains(report.ContextRetainedPaths, callerPath) {
		t.Fatalf("caller.go was withdrawn although its resolution moved: withdrawn=%v retained=%v",
			report.ContextPaths, report.ContextRetainedPaths)
	}
	if slices.Contains(report.ContextPaths, callerPath) {
		t.Fatalf("caller.go is both withdrawn and retained")
	}
	modes := builderContextMaskModes(t, store, generationID)
	// A retained context file keeps the claim it has today: replace, over the
	// payload the pass wrote for it.
	if modes[callerPath] != store_sqlite.OwnershipReplace {
		t.Fatalf("mask on the retained context file = %q, want replace", modes[callerPath])
	}
	if !slices.Contains(builderContextPayloadPaths(t, store, generationID), callerPath) {
		t.Fatalf("a retained context file carries no payload — the claim has nothing behind it")
	}
	for _, path := range report.ContextPaths {
		if modes[path] != store_sqlite.OwnershipContext {
			t.Fatalf("withdrawn path %q carries mask %q, want context", path, modes[path])
		}
	}
	for _, path := range report.ContextRetainedPaths {
		if modes[path] != store_sqlite.OwnershipReplace {
			t.Fatalf("retained path %q carries mask %q, want replace", path, modes[path])
		}
	}

	flat := builderOpenStore(t, "flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderContextTreeRename())
	builderIndex(t, flat, dirB)

	composed := builderComposed(t, store, generationID)
	if n := composed.GetNode(builderRepoPrefix + "/core.go::Compute"); n != nil {
		t.Errorf("the renamed symbol survived: %s", builderRenderNode(n))
	}
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// TestContextSeparationIsInertWithoutAClosure pins the fallback: a build whose
// plan has no context files withdraws nothing, writes no context mask and
// behaves exactly as it did before the separation existed.
func TestContextSeparationIsInertWithoutAClosure(t *testing.T) {
	store := builderOpenStore(t, "inert")
	builder := builderNewBuilder(store)
	report := BuildReport{}
	withdrawn, err := builder.separateContextPayload(
		context.Background(), BuildRequest{RepoPrefix: builderRepoPrefix, Base: store},
		buildPlan{indexed: []string{"core.go"}}, store.AtGeneration(1), &report)
	if err != nil || withdrawn != nil {
		t.Fatalf("separateContextPayload on a contextless plan = %v, %v", withdrawn, err)
	}
	if report.ContextMasks != 0 || len(report.ContextPaths) != 0 {
		t.Fatalf("a contextless plan declared context: %+v", report)
	}
}

// TestContextWithdrawalKeepsEdgesIntoWithdrawnIdentities pins the one edge
// class the eviction would otherwise take with it: a call the CHANGED file
// makes into a withdrawn context symbol belongs to the change set's own
// payload, and the generation must still carry it.
func TestContextWithdrawalKeepsEdgesIntoWithdrawnIdentities(t *testing.T) {
	store := builderOpenStore(t, "restore")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextTreeA(), builderContextTreeBodyEdit())

	if !slices.Contains(report.ContextPaths, builderRepoPrefix+"/helper.go") {
		t.Fatalf("helper.go was not withdrawn: %v", report.ContextPaths)
	}
	handle := store.AtGeneration(generationID)
	helper := builderRepoPrefix + "/helper.go::Helper"
	var into int
	for _, edge := range handle.AllEdges() {
		if edge != nil && edge.To == helper {
			into++
			if edge.FilePath != builderRepoPrefix+"/core.go" {
				t.Fatalf("a restored edge is recorded outside the changed file: %+v", edge)
			}
		}
	}
	if into != 2 {
		t.Fatalf("the generation carries %d calls into the withdrawn symbol, want the two the edit made", into)
	}
	// And nothing LEAVING a withdrawn identity survives: that adjacency is the
	// context file's own and the layer below serves it again.
	for _, edge := range handle.AllEdges() {
		if edge != nil && graph.IDFile(edge.From) == builderRepoPrefix+"/helper.go" {
			t.Fatalf("an edge out of a withdrawn identity survived: %+v", edge)
		}
	}
}

// builderContextScaleTree is the shape the measurement needs: a small change
// set whose closure is two orders of magnitude larger. leaf files are pure
// definitions, hub.go calls every one of them, and five more files each call
// one leaf — so editing the six bodies pulls every leaf into the closure as
// read-only context.
func builderContextScaleTree(leaves int, edited bool) map[string]string {
	tree := make(map[string]string, leaves+6)
	var hub strings.Builder
	hub.WriteString("package fixture\n\nfunc Hub() {\n")
	for i := range leaves {
		name := fmt.Sprintf("Leaf%03d", i)
		tree[fmt.Sprintf("leaf%03d.go", i)] = fmt.Sprintf("package fixture\n\nfunc %s() {\n}\n", name)
		hub.WriteString("\t" + name + "()\n")
	}
	if edited {
		hub.WriteString("\tLeaf000()\n")
	}
	hub.WriteString("}\n")
	tree["hub.go"] = hub.String()
	for i := range 5 {
		body := "\tLeaf000()\n"
		if edited {
			body += "\tLeaf000()\n"
		}
		tree[fmt.Sprintf("edge%d.go", i)] = fmt.Sprintf("package fixture\n\nfunc Edge%d() {\n%s}\n", i, body)
	}
	return tree
}

// TestContextSeparationScalesWithTheChangeSetNotTheClosure is the write audit:
// six modified files whose closure spans two hundred more must leave the
// generation carrying payload for six files, not two hundred and six. The
// numbers it logs are the before/after the ledger records — the pass's own
// output counts are what the generation used to keep.
func TestContextSeparationScalesWithTheChangeSetNotTheClosure(t *testing.T) {
	const leaves = 200
	store := builderOpenStore(t, "scale")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextScaleTree(leaves, false), builderContextScaleTree(leaves, true))

	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d; the measurement needs the whole closure", report.ClosureCap)
	}
	if len(report.ClosurePaths) < leaves/2 {
		t.Fatalf("closure spans %d files, want the leaf population", len(report.ClosurePaths))
	}

	carried := builderContextPayloadPaths(t, store, generationID)
	handle := store.AtGeneration(generationID)
	carriedNodes := len(handle.AllNodes())
	carriedEdges := len(handle.AllEdges())
	rows, err := handle.FileMetasForRepo(builderRepoPrefix)
	if err != nil {
		t.Fatalf("FileMetasForRepo: %v", err)
	}
	var carriedBytes int64
	for _, row := range rows {
		carriedBytes += int64(row.Size)
	}
	t.Logf("write audit: pass indexed %d files (%d bytes) producing %d nodes / %d edges; "+
		"the generation keeps %d files (%d bytes), %d nodes / %d edges; "+
		"context masks %d, retained %d",
		len(report.IndexedPaths), report.SourceBytes, report.NodeCount, report.EdgeCount,
		len(carried), carriedBytes, carriedNodes, carriedEdges,
		report.ContextMasks, len(report.ContextRetainedPaths))

	if len(report.ContextRetainedPaths) != 0 {
		t.Fatalf("a body-only edit retained %d closure files: %v",
			len(report.ContextRetainedPaths), report.ContextRetainedPaths)
	}
	want := []string{builderRepoPrefix + "/hub.go"}
	for i := range 5 {
		want = append(want, fmt.Sprintf("%s/edge%d.go", builderRepoPrefix, i))
	}
	slices.Sort(want)
	if !slices.Equal(carried, want) {
		t.Fatalf("the generation carries payload at %d paths, want the %d changed files",
			len(carried), len(want))
	}
	if carriedNodes >= report.NodeCount {
		t.Fatalf("the generation keeps %d of the %d nodes the pass produced — nothing was separated",
			carriedNodes, report.NodeCount)
	}
}

// builderContextDerivedHandle opens a bare derived generation over store: the
// cheapest fixture that can exercise separateContextPayload's comparison
// directly, without a git tree or a pass.
func builderContextDerivedHandle(t *testing.T, store *store_sqlite.Store) *store_sqlite.Store {
	t.Helper()
	_, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "graph-fixture", LayerID: "context-unit",
		GenerationKind: "dirty", TreeOID: "tree", CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	return handle
}

// TestContextFileWithAContentSectionIsNeverWithdrawn pins the second
// conservatism in matchesBase. Content section bodies live in content_fts,
// keyed by the FILE and reached through a separate index this withdrawal does
// not touch: evicting the nodes at such a path would leave the content rows
// behind in a generation that no longer carries the nodes they belong to. So a
// candidate carrying a content section keeps its claim even when every node
// and edge at it compares equal to the layer below.
func TestContextFileWithAContentSectionIsNeverWithdrawn(t *testing.T) {
	store := builderOpenStore(t, "content-context")
	contentRel, plainRel := "notes.md", "helper.go"
	contentPath := builderGraphPath(builderRepoPrefix, contentRel)
	plainPath := builderGraphPath(builderRepoPrefix, plainRel)
	section := func() *graph.Node {
		return &graph.Node{
			ID: contentPath + "::section-1", Kind: graph.KindDoc, Name: "section-1",
			FilePath: contentPath, RepoPrefix: builderRepoPrefix,
			Meta: map[string]interface{}{"data_class": "content"},
		}
	}
	plain := func() *graph.Node {
		return &graph.Node{
			ID: plainPath + "::Helper", Kind: graph.KindFunction, Name: "Helper",
			FilePath: plainPath, RepoPrefix: builderRepoPrefix,
		}
	}
	// The layer below and the generation's own re-derivation are identical for
	// BOTH paths: the only thing separating them is the content section.
	store.AddBatch([]*graph.Node{section(), plain()}, nil)
	handle := builderContextDerivedHandle(t, store)
	handle.AddBatch([]*graph.Node{section(), plain()}, nil)

	report := BuildReport{}
	withdrawn, err := builderNewBuilder(store).separateContextPayload(
		context.Background(),
		BuildRequest{RepoPrefix: builderRepoPrefix, Base: store},
		buildPlan{indexed: []string{contentRel, plainRel}, context: []string{contentRel, plainRel}},
		handle, &report)
	if err != nil {
		t.Fatalf("separateContextPayload: %v", err)
	}
	if _, gone := withdrawn[contentPath]; gone {
		t.Fatalf("a context path with a content section was withdrawn: %v", report.ContextPaths)
	}
	if !slices.Contains(report.ContextRetainedPaths, contentPath) {
		t.Fatalf("the content path is not declared retained: %+v", report.ContextRetainedPaths)
	}
	// The control: the identical plain file next to it IS withdrawn, so the
	// fixture is not simply failing to compare equal.
	if _, gone := withdrawn[plainPath]; !gone {
		t.Fatalf("the plain context path was not withdrawn: %v", report.ContextPaths)
	}
	if handle.GetNode(contentPath+"::section-1") == nil {
		t.Fatalf("the content section lost its node row")
	}
}

// TestContextWithdrawalClearsSymbolFTSForWithdrawnIdentities pins the FTS half
// of the withdrawal. EvictFiles deletes nodes and edges by adjacency; it does
// not reach symbol_fts, whose rows are keyed by node id in the sidecar. A
// withdrawn identity that kept its FTS row would leave the generation's own
// symbol search answering for a symbol the generation no longer carries.
func TestContextWithdrawalClearsSymbolFTSForWithdrawnIdentities(t *testing.T) {
	store := builderOpenStore(t, "fts-withdraw")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextTreeA(), builderContextTreeBodyEdit())

	withdrawn := builderRepoPrefix + "/helper.go"
	if !slices.Contains(report.ContextPaths, withdrawn) {
		t.Fatalf("helper.go was not withdrawn: %v", report.ContextPaths)
	}
	changed := builderRepoPrefix + "/core.go"

	rows := builderGenerationSymbolFTSRows(t, store, generationID)
	// The control first: without rows for the CHANGED file the census below
	// would be vacuously empty and could never go red.
	if rows[changed] == 0 {
		t.Fatalf("the generation indexed no symbol FTS rows at all: %v", rows)
	}
	if got := rows[withdrawn]; got != 0 {
		t.Fatalf("the withdrawn path kept %d symbol FTS rows: %v", got, rows)
	}
}

// builderGenerationSymbolFTSRows counts a generation's symbol FTS sidecar rows
// per file, read straight from the database because the sidecar is deliberately
// not part of the graph reader surface.
func builderGenerationSymbolFTSRows(
	t *testing.T, store *store_sqlite.Store, generationID int64,
) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(
		`SELECT node_id FROM symbol_fts_rowid WHERE view_gen = ?`, generationID)
	if err != nil {
		t.Fatalf("query symbol_fts_rowid: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			t.Fatalf("scan symbol_fts_rowid: %v", err)
		}
		file := nodeID
		if idx := strings.Index(nodeID, "::"); idx >= 0 {
			file = nodeID[:idx]
		}
		out[file]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("symbol_fts_rowid rows: %v", err)
	}
	return out
}
