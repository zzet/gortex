package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The history-replay parity oracle.
//
// A checkout is taken through a sequence of working-tree states — a body
// edit, a signature change, an added declaration, a rename across a package, an
// added file, a removed file, an import change, a partial undo and a full undo
// — one state at a time, each published through the checkout coordinator
// exactly as a save would be. After every step the view the coordinator serves
// for the checkout is compared, row by row (every node and every edge, all
// fields but the view-only ones builderRenderNode / builderRenderEdge drop),
// with a fresh whole index of the same working tree. A step passes when the two
// row sets are identical; the only rows a step may differ by are those an
// entry of replayAllowList names, and that list is empty: a whole index of a
// tree is row-for-row reproducible (two whole indexes of the real repository in
// separate processes differ by 0 rows; the order-independence pins are
// TestExternalCallCollisionSurvivorIsIndependentOfOrder,
// TestReceiverMutationBindsTheMethodsOwnPackageField,
// TestContractNodeIsTheFirstRecordWhateverTheListingOrder and
// TestSharedSyntheticNodeKeepsTheSmallestFileWhateverTheOrder), so every
// differing row is attributable to the edit path.
//
// The fixture arm runs in the normal suite, with the coordinator's working-tree
// chaining off and on. The real-repository arm runs when
// GX_REPLAY_REAL_TREE names a private clone of this repository (a standalone git
// repository whose HEAD is the committed base), GX_REPLAY_BASE a whole-index
// store of that HEAD (built here, with this binary's code, when the path does
// not exist yet; cloned per run, never written) and GX_REPLAY_STORE_DIR a
// private directory for the replay store, the linked worktree and the
// artifact. Nothing here touches a user store.

// replayAllowance names one class of rows a replay step may differ by. The list
// is empty and must stay empty: an entry is a documented, row-level exception,
// never a way to make a failing edit path pass.
type replayAllowance struct {
	class string
	match func(kind, row string) bool
}

var replayAllowList []replayAllowance

// replayStep is one working-tree state transition.
type replayStep struct {
	name  string
	apply func(t testing.TB, root string)
}

// replayStepRecord is what one step measured.
type replayStepRecord struct {
	Step            string                     `json:"step"`
	CycleMS         float64                    `json:"cycle_ms"`
	WAL             store_sqlite.WALWriteDelta `json:"wal"`
	DirtyBuilt      bool                       `json:"dirty_built"`
	Generation      int64                      `json:"generation"`
	Parent          int64                      `json:"parent"`
	Depth           int                        `json:"depth"`
	Fallback        string                     `json:"fallback,omitempty"`
	CleanIndexMS    float64                    `json:"clean_index_ms"`
	CompareMS       float64                    `json:"compare_ms"`
	Nodes           int                        `json:"nodes"`
	Edges           int                        `json:"edges"`
	ServedOnlyNodes int                        `json:"served_only_nodes"`
	CleanOnlyNodes  int                        `json:"clean_only_nodes"`
	ServedOnlyEdges int                        `json:"served_only_edges"`
	CleanOnlyEdges  int                        `json:"clean_only_edges"`
	Allowed         int                        `json:"allowed"`
	ByKind          map[string]int             `json:"differing_by_kind,omitempty"`
	Sample          []string                   `json:"sample,omitempty"`
	// NodePlacement names, for each differing node ID (at most the sample
	// limit), the generations holding a row or a tombstone for it: where a
	// served-only or missing node comes from.
	NodePlacement map[string]string `json:"node_placement,omitempty"`
	differingIDs  []string
}

func (r replayStepRecord) differing() int {
	return r.ServedOnlyNodes + r.CleanOnlyNodes + r.ServedOnlyEdges + r.CleanOnlyEdges - r.Allowed
}

// --- the edit vocabulary -------------------------------------------------

// replayOriginals remembers the committed bytes of every path a step touched,
// so an undo can put exactly them back.
type replayOriginals struct {
	root  string
	bytes map[string][]byte // nil value: the path did not exist
}

func newReplayOriginals(root string) *replayOriginals {
	return &replayOriginals{root: root, bytes: map[string][]byte{}}
}

func (o *replayOriginals) remember(t testing.TB, rel string) {
	t.Helper()
	if _, seen := o.bytes[rel]; seen {
		return
	}
	data, err := os.ReadFile(filepath.Join(o.root, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		o.bytes[rel] = data
	case errors.Is(err, os.ErrNotExist):
		o.bytes[rel] = nil
	default:
		t.Fatalf("read %s: %v", rel, err)
	}
}

func (o *replayOriginals) restore(t testing.TB, rel string) {
	t.Helper()
	data, seen := o.bytes[rel]
	if !seen {
		return
	}
	full := filepath.Join(o.root, filepath.FromSlash(rel))
	if data == nil {
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove %s: %v", rel, err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("restore %s: %v", rel, err)
	}
}

func (o *replayOriginals) restoreAll(t testing.TB) {
	t.Helper()
	paths := make([]string, 0, len(o.bytes))
	for rel := range o.bytes {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		o.restore(t, rel)
	}
}

func replayRead(t testing.TB, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func replayWrite(t testing.TB, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// replayBodyEdit inserts a statement at the top of the first function body
// whose signature line ends in "{": no declaration, signature or import moves.
func replayBodyEdit(o *replayOriginals, rel, marker string) replayStep {
	return replayStep{name: "body " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		lines := strings.Split(replayRead(t, root, rel), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "func ") && strings.HasSuffix(strings.TrimSpace(line), "{") {
				lines = append(lines[:i+1], append([]string{"\t_ = len(\"" + marker + "\")"}, lines[i+1:]...)...)
				replayWrite(t, root, rel, strings.Join(lines, "\n"))
				return
			}
		}
		t.Fatalf("no function body in %s", rel)
	}}
}

var replayNoArgExported = regexp.MustCompile(`(?m)^func ([A-Z][A-Za-z0-9_]*)\(\)`)

// replaySignatureChange gives the first exported parameterless top-level
// function of rel a parameter: its declaration surface moves, its name and
// every caller's reference stay.
func replaySignatureChange(o *replayOriginals, rel string) replayStep {
	return replayStep{name: "signature " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		src := replayRead(t, root, rel)
		loc := replayNoArgExported.FindStringSubmatchIndex(src)
		if loc == nil {
			t.Fatalf("no exported parameterless function in %s", rel)
		}
		name := src[loc[2]:loc[3]]
		src = src[:loc[0]] + "func " + name + "(replayArg int)" + src[loc[1]:]
		replayWrite(t, root, rel, src)
	}}
}

// replayAddDeclaration appends a new exported function to rel.
func replayAddDeclaration(o *replayOriginals, rel string) replayStep {
	return replayStep{name: "declaration " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		replayWrite(t, root, rel, replayRead(t, root, rel)+
			"\n// ReplayProbe is a declaration added by the replay oracle.\nfunc ReplayProbe() int { return 1 }\n")
	}}
}

var replayUnexportedFunc = regexp.MustCompile(`(?m)^func ([a-z][A-Za-z0-9_]*)\(`)

// replayRenameInPackage renames the first unexported top-level function of rel
// in every Go file of rel's directory (an unexported name cannot be referenced
// from another package): the declaration and every same-package caller move.
func replayRenameInPackage(o *replayOriginals, rel string) replayStep {
	return replayStep{name: "rename in " + filepath.ToSlash(filepath.Dir(rel)), apply: func(t testing.TB, root string) {
		src := replayRead(t, root, rel)
		m := replayUnexportedFunc.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no unexported function in %s", rel)
		}
		old := m[1]
		word := regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`)
		dir := filepath.Dir(rel)
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		renamed := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			file := filepath.ToSlash(filepath.Join(dir, entry.Name()))
			body := replayRead(t, root, file)
			if !word.MatchString(body) {
				continue
			}
			o.remember(t, file)
			replayWrite(t, root, file, word.ReplaceAllString(body, old+"Renamed"))
			renamed++
		}
		if renamed == 0 {
			t.Fatalf("rename of %s touched no file", old)
		}
	}}
}

// replayAddFile writes a new file.
func replayAddFile(o *replayOriginals, rel, body string) replayStep {
	return replayStep{name: "add " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		replayWrite(t, root, rel, body)
	}}
}

// replayRemoveFile deletes a file.
func replayRemoveFile(o *replayOriginals, rel string) replayStep {
	return replayStep{name: "remove " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("remove %s: %v", rel, err)
		}
	}}
}

// replayImportEdit adds an import to rel's import block and a use of it.
func replayImportEdit(o *replayOriginals, rel, importPath, use string) replayStep {
	return replayStep{name: "import " + rel, apply: func(t testing.TB, root string) {
		o.remember(t, rel)
		src := replayRead(t, root, rel)
		i := strings.Index(src, "\nimport (")
		if i < 0 {
			t.Fatalf("no import block in %s", rel)
		}
		i += len("\nimport (")
		src = src[:i] + "\n\t\"" + importPath + "\"" + src[i:]
		if use != "" {
			src += "\n" + use + "\n"
		}
		replayWrite(t, root, rel, src)
	}}
}

// replayUndo puts the committed bytes of rel back (a partial undo).
func replayUndo(o *replayOriginals, rel string) replayStep {
	return replayStep{name: "undo " + rel, apply: func(t testing.TB, root string) {
		o.restore(t, rel)
	}}
}

// replayUndoAll puts back every path any step touched: the working tree is
// clean again.
func replayUndoAll(o *replayOriginals) replayStep {
	return replayStep{name: "undo all", apply: func(t testing.TB, root string) {
		o.restoreAll(t)
	}}
}

// --- the comparison ------------------------------------------------------

type replayRowKey [16]byte

func replayHash(row string) replayRowKey {
	sum := sha256.Sum256([]byte(row))
	var key replayRowKey
	copy(key[:], sum[:16])
	return key
}

// replayRenderNode and replayRenderEdge are the strict oracles' renderers
// (builderRenderNode / builderRenderEdge: every stored field, metadata as
// JSON).
func replayRenderNode(n *graph.Node) string { return builderRenderNode(n) }

func replayRenderEdge(e *graph.Edge) string { return builderRenderEdge(e) }

// replayCompare diffs the served view's rows against the clean index's as
// multisets of rendered rows. Rows are hashed so a repository-sized view is
// never held twice as strings; only differing rows are rendered for the
// record.
func replayCompare(served, clean graph.Reader, sampleLimit int) replayStepRecord {
	var rec replayStepRecord
	rec.ByKind = map[string]int{}
	// Samples are kept per (side, kind), so a large class (a file's worth of
	// node rows) cannot crowd a small one (a handful of edges) out of the
	// record.
	perKind := max(sampleLimit/8, 3)
	note := func(side, kind, row string) {
		if len(rec.Sample) < sampleLimit && rec.ByKind[side+" "+kind] < perKind {
			rec.Sample = append(rec.Sample, side+" "+row)
		}
		rec.ByKind[side+" "+kind]++
		for _, allowed := range replayAllowList {
			if allowed.match(kind, row) {
				rec.Allowed++
				rec.ByKind["allowed "+allowed.class]++
				return
			}
		}
	}

	cleanNodes := clean.AllNodes()
	rec.Nodes = len(cleanNodes)
	counts := make(map[replayRowKey]int, len(cleanNodes))
	for _, n := range cleanNodes {
		counts[replayHash(replayRenderNode(n))]++
	}
	for _, n := range served.AllNodes() {
		row := replayRenderNode(n)
		key := replayHash(row)
		if counts[key] > 0 {
			counts[key]--
			continue
		}
		rec.ServedOnlyNodes++
		note("served-only node", string(n.Kind), row)
		if len(rec.differingIDs) < sampleLimit {
			rec.differingIDs = append(rec.differingIDs, n.ID)
		}
	}
	for _, n := range cleanNodes {
		row := replayRenderNode(n)
		key := replayHash(row)
		if counts[key] > 0 {
			counts[key]--
			rec.CleanOnlyNodes++
			note("clean-only node", string(n.Kind), row)
			if len(rec.differingIDs) < sampleLimit {
				rec.differingIDs = append(rec.differingIDs, n.ID)
			}
		}
	}

	cleanEdges := clean.AllEdges()
	rec.Edges = len(cleanEdges)
	counts = make(map[replayRowKey]int, len(cleanEdges))
	for _, e := range cleanEdges {
		counts[replayHash(replayRenderEdge(e))]++
	}
	for _, e := range served.AllEdges() {
		row := replayRenderEdge(e)
		key := replayHash(row)
		if counts[key] > 0 {
			counts[key]--
			continue
		}
		rec.ServedOnlyEdges++
		note("served-only edge", string(e.Kind), row)
	}
	for _, e := range cleanEdges {
		row := replayRenderEdge(e)
		key := replayHash(row)
		if counts[key] > 0 {
			counts[key]--
			rec.CleanOnlyEdges++
			note("clean-only edge", string(e.Kind), row)
		}
	}
	return rec
}

// --- the driver ----------------------------------------------------------

// replayHarness drives one checkout through a step list.
type replayHarness struct {
	f *coordinatorFixture
	c *CheckoutCoordinator
	// clean indexes the worktree as it is now from scratch; release, when
	// non-nil, frees what it returned once the step is compared.
	clean       func(t *testing.T, label string) (reader graph.Reader, release func())
	sampleLimit int
}

func (h *replayHarness) run(t *testing.T, steps []replayStep) []replayStepRecord {
	t.Helper()
	ctx := context.Background()
	var records []replayStepRecord
	for i, step := range steps {
		step.apply(t, h.f.worktree)
		mark := h.f.store.WALWriteMark()
		started := time.Now()
		out := coordinatorReconcile(t, h.c)
		// A change imported file by file takes one cycle per file; the step
		// is compared once the import reached the working tree.
		for out.DirtyBatchRemaining > 0 {
			next := coordinatorReconcile(t, h.c)
			next.DirtyBuilt = next.DirtyBuilt || out.DirtyBuilt
			out = next
		}
		cycle := time.Since(started)
		wal := store_sqlite.WALWrittenBetween(mark, h.f.store.WALWriteMark())
		h.c.SweepRetirements(ctx)

		view := chainMaterialize(t, h.f)
		cleanStarted := time.Now()
		clean, release := h.clean(t, fmt.Sprintf("replay-%02d", i))
		cleanMS := ms(time.Since(cleanStarted))
		compareStarted := time.Now()
		rec := replayCompare(view.Reader, clean, h.sampleLimit)
		view.Close()
		if release != nil {
			release()
		}
		rec.Step = step.name
		rec.CycleMS = ms(cycle)
		rec.WAL = wal
		rec.DirtyBuilt = out.DirtyBuilt
		rec.Generation = out.DirtyGenerationID
		rec.Parent = out.DirtyParentGenerationID
		rec.Depth = out.DirtyChainDepth
		rec.Fallback = out.DirtyChainReason
		rec.CleanIndexMS = cleanMS
		rec.CompareMS = ms(time.Since(compareStarted))
		rec.NodePlacement = replayNodePlacement(t, h.f.store, rec.differingIDs)
		t.Logf("replay step %d %q: cycle %.1f ms built=%t gen=%d parent=%d depth=%d wal{frames=%d bytes=%d reset=%t} "+
			"rows{nodes=%d edges=%d} differing{served-only nodes=%d clean-only nodes=%d served-only edges=%d clean-only edges=%d allowed=%d} by kind %v",
			i, step.name, rec.CycleMS, rec.DirtyBuilt, rec.Generation, rec.Parent, rec.Depth,
			rec.WAL.Frames, rec.WAL.Bytes, rec.WAL.Reset, rec.Nodes, rec.Edges,
			rec.ServedOnlyNodes, rec.CleanOnlyNodes, rec.ServedOnlyEdges, rec.CleanOnlyEdges, rec.Allowed, rec.ByKind)
		if rec.differing() != 0 {
			t.Errorf("replay step %d %q: the served view differs from a whole index of the working tree by %d rows outside the allow-list; first rows:\n  %s",
				i, step.name, rec.differing(), strings.Join(rec.Sample, "\n  "))
			if len(rec.NodePlacement) > 0 {
				t.Logf("replay step %d %q: differing node placement %v", i, step.name, rec.NodePlacement)
			}
		}
		records = append(records, rec)
	}
	return records
}

// replayNodePlacement reads, per node ID, which generations store a row for it
// and which tombstone it ("rows=[0 11] tombstones=[12]").
func replayNodePlacement(t *testing.T, store *store_sqlite.Store, ids []string) map[string]string {
	t.Helper()
	if len(ids) == 0 {
		return nil
	}
	db := parityOpenRaw(t, store)
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		rows := parityColumn(t, db, `SELECT CAST(view_gen AS TEXT) FROM nodes WHERE id = ? ORDER BY view_gen`, id)
		tombs := parityColumn(t, db, `SELECT CAST(view_gen AS TEXT) FROM generation_node_tombstones WHERE node_id = ? ORDER BY view_gen`, id)
		out[id] = fmt.Sprintf("rows=%v tombstones=%v", rows, tombs)
	}
	return out
}

// --- the fixture arm -----------------------------------------------------

const replayModule = "example.com/replay"

// replayTree is a small module with cross-package calls, a same-package
// helper with two callers, a method mutating its receiver and a leaf package.
func replayTree() map[string]string {
	return map[string]string{
		"go.mod": "module " + replayModule + "\n\ngo 1.22\n",
		"a/a.go": `package a

import (
	"example.com/replay/b"
)

func A() int {
	return b.B() + helper()
}

func helper() int {
	return 1
}
`,
		"a/more.go": `package a

func More() int {
	return helper() * 2
}
`,
		"b/b.go": `package b

type T struct {
	n int
}

func (t *T) Inc() {
	t.n++
}

func (t *T) Twice() {
	t.Inc()
	t.Inc()
}

func B() int {
	return 2
}
`,
		"c/c.go": `package c

import (
	"example.com/replay/a"
)

func C() int {
	return a.A() + a.More()
}
`,
		"d/d.go": `package d

func D() string {
	return "d"
}
`,
	}
}

func replayFixtureSteps(o *replayOriginals) []replayStep {
	return []replayStep{
		replayBodyEdit(o, "a/a.go", "replay-body"),
		replaySignatureChange(o, "b/b.go"),
		replayAddDeclaration(o, "b/b.go"),
		replayRenameInPackage(o, "a/a.go"),
		replayAddFile(o, "e/e.go", `package e

import (
	"example.com/replay/b"
	"example.com/replay/d"
)

func E() string {
	_ = b.ReplayProbe()
	return d.D()
}
`),
		replayRemoveFile(o, "d/d.go"),
		replayImportEdit(o, "c/c.go", replayModule+"/b", "func CB() int { return b.B(3) }"),
		replayUndo(o, "a/a.go"),
		replayUndoAll(o),
	}
}

// TestHistoryReplayMatchesAWholeIndexAfterEveryStep is the fixture arm of the
// oracle: every step through the coordinator, in three arms — chained deltas
// compacted at the depth bound, a chain folded by copy after every edit, and
// every multi-file change imported file by file.
func TestHistoryReplayMatchesAWholeIndexAfterEveryStep(t *testing.T) {
	arms := []struct {
		name        string
		importPaths int
	}{{"chained", 0}, {"import-file-by-file", 1}}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			if arm.importPaths > 0 {
				old := importInteractivePaths
				importInteractivePaths = arm.importPaths
				t.Cleanup(func() { importInteractivePaths = old })
			}
			f := newCoordinatorFixtureWithTree(t, replayTree())
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			coordinatorReconcile(t, c)
			h := &replayHarness{f: f, c: c, sampleLimit: 40, clean: func(t *testing.T, label string) (graph.Reader, func()) {
				store := builderOpenStore(t, label)
				builderIndex(t, store, f.worktree)
				return store, nil
			}}
			records := h.run(t, replayFixtureSteps(newReplayOriginals(f.worktree)))
			if len(records) != 9 {
				t.Fatalf("ran %d steps, want 9", len(records))
			}
			built := 0
			for _, rec := range records {
				if rec.DirtyBuilt {
					built++
				}
			}
			if built < len(records)-1 {
				t.Fatalf("only %d of %d steps built a working-tree layer", built, len(records))
			}
		})
	}
}

// TestHistoryReplayOracleSeesAnEditPathDifference is the oracle's own
// sensitivity check: a served view missing one edge the whole index holds is a
// failing step, not a pass.
func TestHistoryReplayOracleSeesAnEditPathDifference(t *testing.T) {
	repo := builderTempDir(t, "repo")
	builderWriteTree(t, repo, replayTree())
	a := builderOpenStore(t, "a")
	builderIndex(t, a, repo)
	b := builderOpenStore(t, "b")
	builderIndex(t, b, repo)
	if rec := replayCompare(a, b, 5); rec.differing() != 0 {
		t.Fatalf("two whole indexes of one tree differ: %+v", rec)
	}
	var victim *graph.Edge
	for _, e := range b.AllEdges() {
		if e.Kind == graph.EdgeCalls {
			victim = e
			break
		}
	}
	if victim == nil {
		t.Fatal("the fixture has no call edge")
	}
	if !b.RemoveEdge(victim.From, victim.To, victim.Kind) {
		t.Fatalf("remove %v", victim)
	}
	rec := replayCompare(b, a, 5)
	if rec.CleanOnlyEdges == 0 || rec.differing() == 0 {
		t.Fatalf("a served view missing %s -> %s compares clean: %+v", victim.From, victim.To, rec)
	}
}

// --- the real-repository arm ---------------------------------------------

// TestHistoryReplayRealRepository is the gated real-repository arm. Knobs:
// GX_REPLAY_REAL_TREE (the private clone), GX_REPLAY_BASE (a whole-index store
// of its HEAD, cloned per run, never written), GX_REPLAY_STORE_DIR (scratch),
// GX_REPLAY_OUT (a JSON artifact of the step records).
func TestHistoryReplayRealRepository(t *testing.T) {
	tree := os.Getenv("GX_REPLAY_REAL_TREE")
	base := os.Getenv("GX_REPLAY_BASE")
	storeDir := os.Getenv("GX_REPLAY_STORE_DIR")
	if tree == "" || base == "" || storeDir == "" {
		t.Skip("set GX_REPLAY_REAL_TREE, GX_REPLAY_BASE and GX_REPLAY_STORE_DIR to a private clone, its base store and a scratch directory")
	}
	builderIsolateGit(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	full, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's config: %v", err)
	}
	if status := builderGit(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("the clone is not clean at HEAD:\n%s", status)
	}

	// The linked worktree's directory name is its git admin name, which the
	// fixture's catalog identity records.
	worktree := filepath.Join(storeDir, coordinatorAdminName)
	storePath := filepath.Join(storeDir, "replay.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(storePath + suffix)
	}
	_ = exec.Command("git", "-C", tree, "worktree", "remove", "--force", worktree).Run()
	_ = os.RemoveAll(worktree)
	branch := fmt.Sprintf("replay-%d", time.Now().UnixNano())
	builderGit(t, tree, "worktree", "add", "-b", branch, worktree)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", tree, "worktree", "remove", "--force", worktree).Run()
		_ = exec.Command("git", "-C", tree, "branch", "-D", branch).Run()
		if os.Getenv("GX_REPLAY_KEEP") != "1" {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(storePath + suffix)
			}
		}
	})
	logPath := filepath.Join(storeDir, "replay.log")
	logger := realRepoLogger(t, logPath)
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		started := time.Now()
		baseStore := builderOpenStoreAt(t, base+".tmp")
		idx := New(baseStore, builderRegistry(), full.Index, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(tree); err != nil {
			t.Fatalf("index the base corpus: %v", err)
		}
		idx.Close()
		if err := baseStore.Close(); err != nil {
			t.Fatalf("close the base store: %v", err)
		}
		if err := os.Rename(base+".tmp", base); err != nil {
			t.Fatalf("publish the base store: %v", err)
		}
		t.Logf("indexed the base corpus in %v", time.Since(started))
	}
	if out, err := exec.Command("cp", "-c", base, storePath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("open the replay store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	f := &coordinatorFixture{
		t: t, store: store, storePath: storePath, catalog: store.Catalog(), leases: graphview.NewLeaseManager(),
		primary: tree, worktree: worktree,
		familyID: "family-replay", graphID: GraphIDFor(builderRepoPrefix),
		primaryID: "checkout-primary", checkoutID: "checkout-replay",
		treeA: builderGit(t, tree, "rev-parse", "HEAD^{tree}"),
	}
	f.writeCatalogIdentity()
	f.registerRosterOwner(t)
	c := replayRealCoordinator(t, f, full, logger)
	coordinatorReconcile(t, c)

	o := newReplayOriginals(worktree)
	steps := []replayStep{
		replayBodyEdit(o, "internal/config/config.go", "replay-body-config"),
		replayBodyEdit(o, "internal/mcp/checkout_binding.go", "replay-body-binding"),
		replaySignatureChange(o, "internal/config/config.go"),
		replayAddDeclaration(o, "internal/config/config.go"),
		replayRenameInPackage(o, "internal/config/config.go"),
		replayAddFile(o, "internal/config/replay_added.go", "package config\n\n// ReplayAdded is a file added by the replay oracle.\nfunc ReplayAdded() *Config { return Default() }\n"),
		replayRemoveFile(o, "internal/progress/glyphs.go"),
		replayImportEdit(o, "internal/config/config.go", "hash/adler32", "var replayChecksum = adler32.Checksum(nil)"),
		replayUndo(o, "internal/mcp/checkout_binding.go"),
		replayUndoAll(o),
	}
	// GX_REPLAY_STEPS=n runs the first n steps, then the full undo.
	if n, err := strconv.Atoi(os.Getenv("GX_REPLAY_STEPS")); err == nil && n > 0 && n < len(steps) {
		steps = append(steps[:n:n], replayUndoAll(o))
	}
	h := &replayHarness{f: f, c: c, sampleLimit: 60, clean: func(t *testing.T, label string) (graph.Reader, func()) {
		// One clean store at a time: a repository-sized whole index is
		// ~1 GB, so each is removed as soon as its step is compared.
		path := filepath.Join(storeDir, label+".clean.sqlite")
		remove := func() {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
		}
		remove()
		clean := builderOpenStoreAt(t, path)
		release := func() {
			_ = clean.Close()
			remove()
		}
		idx := New(clean, builderRegistry(), full.Index, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(worktree); err != nil {
			release()
			t.Fatalf("%s: whole index of the worktree: %v", label, err)
		}
		idx.Close()
		return clean, release
	}}
	records := h.run(t, steps)
	_ = logger.Sync()
	if path := os.Getenv("GX_REPLAY_OUT"); path != "" {
		data, _ := json.MarshalIndent(records, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// replayRealCoordinator is fixture.coordinator with the clone's own index
// configuration (excludes and languages) instead of the
// defaults, so the coordinator builds what the base index was built with.
func replayRealCoordinator(t *testing.T, f *coordinatorFixture, full *config.Config, logger *zap.Logger) *CheckoutCoordinator {
	t.Helper()
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:   f.checkoutID,
		CheckoutRoot: f.worktree,
		FamilyID:     f.familyID,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Store:        f.store,
		Builder: &SparseGenerationBuilder{
			Store: f.store, Registry: builderRegistry(), Config: full.Index, Logger: logger,
		},
		Leases:         f.leases,
		Config:         full.Index,
		ConfigSections: dedicatedBaseConfigSections(full),
		Logger:         logger,
		PollInterval:   -1,
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the coordinator loop: %v", err)
	}
	return coordinator
}
