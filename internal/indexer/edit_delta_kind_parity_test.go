package indexer

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
)

// Parity by kind over a chain.
//
// An edit that re-derives a file restates the rows the parse and the resolver
// produce for it; rows another producer wrote for that file must be restated
// too, or a chain masks the lower copy and they disappear. These tests compare
// the composed view of a chain with a clean whole index of the same working
// tree for every node kind and every edge kind, over a fixture whose one file
// (prod/producers.go) carries rows of every producer and is re-derived, never
// changed, by edits of its package siblings: a body edit, a signature change,
// a rename and a deletion — on a clean checkout, and at every depth of a
// chain up to the bound.
//
// Comparison rules (the clean index is not deterministic in one place): a
// node is its ID, kind, file, name and span; an edge its source, target,
// kind, recording file and line; metadata is compared separately and
// reported, not failed on. The source of an owner edge (provides, consumes,
// handles_route) into a contract ID that owner edges recorded in two or more
// files share is the registry's pick among them, which differs between two
// whole indexes of one tree; for those edges alone the source is left out.

const kindParityCloneBody = `	total := 0
	for i, x := range xs {
		if x > 0 {
			total += x * i
		} else {
			total -= x
		}
		if total > 1000 {
			total = total % 997
		}
	}
	return total
`

func kindParityTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/kp\n\ngo 1.22\n\nrequire github.com/gin-gonic/gin v1.9.1\n",
		"prod/producers.go": `package prod

import (
	"os"
	"os/exec"

	"github.com/gin-gonic/gin"
)

// Limit and Name are constant values.
const Limit = 42
const Name = "producer"

// Store is implemented by memStore (hierarchy).
type Store interface {
	Get(key string) string
}

type memStore struct {
	data map[string]string
	hits int
}

// Get reads and writes fields (accesses_field).
func (m *memStore) Get(key string) string {
	m.hits++
	return m.data[key]
}

// Env reads the environment (reads_env).
func Env() string { return os.Getenv("PROD_HOME") }

// Launch runs a process (executes_process).
func Launch() error { return exec.Command("true").Run() }

// Routes registers a route (contracts, framework synthesis).
func Routes(r *gin.Engine) {
	r.GET("/prod/items", Items)
}

// Items handles the route.
func Items() {}

// Apply takes a function value (fn-value, dataflow).
func Apply(f func(int) int, v int) int { return f(v) }

// Twice calls a sibling (the signature edit re-derives this file).
func Twice(v int) int { return Helper(v) * 2 }

// UseApply passes Twice as a value.
func UseApply() int { return Apply(Twice, Limit) }

// CloneA and CloneB are a clone pair.
func CloneA(xs []int) int {
` + kindParityCloneBody + `}

func CloneB(xs []int) int {
` + kindParityCloneBody + `}

// UseOther and UseGone call siblings that are edited, renamed or deleted.
func UseOther() int { return Other() + Gone() }
`,
		"prod/sib_a.go": "package prod\n\n// Helper is a sibling whose signature changes.\nfunc Helper(v int) int {\n\treturn v + 1\n}\n",
		"prod/sib_b.go": "package prod\n\n// Other is a sibling whose body changes.\nfunc Other() int {\n\treturn Limit\n}\n",
		"prod/sib_c.go": "package prod\n\n// Gone is a sibling that is renamed.\nfunc Gone() int {\n\treturn 1\n}\n",
		"prod/sib_d.go": "package prod\n\n// Extra is a sibling that is deleted.\nfunc Extra() int {\n\treturn 2\n}\n",
		"prod/sib_e.go": "package prod\n\n// Spare is a second sibling to delete.\nfunc Spare() int {\n\treturn 3\n}\n",
	}
}

// kindParityEdit is one edit of a sibling of prod/producers.go.
type kindParityEdit struct {
	name  string
	write map[string]string
	drop  []string
}

func kindParityEdits() []kindParityEdit {
	return []kindParityEdit{
		{name: "body", write: map[string]string{"prod/sib_b.go": "package prod\n\n// Other is a sibling whose body changes.\nfunc Other() int {\n\t_ = 1\n\treturn Limit + 1\n}\n"}},
		{name: "signature", write: map[string]string{
			"prod/sib_a.go": "package prod\n\n// Helper is a sibling whose signature changes.\nfunc Helper(v int, w ...int) int {\n\treturn v + 1 + len(w)\n}\n"}},
		{name: "rename", drop: []string{"prod/sib_c.go"}, write: map[string]string{
			"prod/sib_c2.go": "package prod\n\n// Gone is a sibling that is renamed.\nfunc Gone() int {\n\treturn 1\n}\n"}},
		{name: "delete", drop: []string{"prod/sib_d.go"}},
		{name: "body_again", write: map[string]string{"prod/sib_b.go": "package prod\n\n// Other is a sibling whose body changes.\nfunc Other() int {\n\t_ = 2\n\treturn Limit + 2\n}\n"}},
		{name: "signature_back", write: map[string]string{
			"prod/sib_a.go": "package prod\n\n// Helper is a sibling whose signature changes.\nfunc Helper(v int) int {\n\treturn v + 2\n}\n"}},
		{name: "rename_back", drop: []string{"prod/sib_c2.go"}, write: map[string]string{
			"prod/sib_c.go": "package prod\n\n// Gone is a sibling that is renamed.\nfunc Gone() int {\n\treturn 1\n}\n"}},
		{name: "delete_second", drop: []string{"prod/sib_e.go"}},
	}
}

func applyKindParityEdit(t *testing.T, repoDir string, edit kindParityEdit) {
	t.Helper()
	for rel, body := range edit.write {
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, repoDir, rel, body)
	}
	for _, rel := range edit.drop {
		if err := os.Remove(filepath.Join(repoDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
}

// kindParityRows is one reader's rows, rendered under the comparison rules,
// grouped by kind.
type kindParityRows struct {
	nodes, edges       map[string][]string // kind -> identity renders
	nodeMeta, edgeMeta map[string][]string // kind -> identity+meta renders
}

func collectKindParityRows(r graph.Reader) kindParityRows {
	out := kindParityRows{nodes: map[string][]string{}, edges: map[string][]string{}, nodeMeta: map[string][]string{}, edgeMeta: map[string][]string{}}
	nodes := r.AllNodes()
	kindOf := make(map[string]graph.NodeKind, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		kindOf[n.ID] = n.Kind
		id := fmt.Sprintf("%s|%s|%s|%d-%d", n.ID, n.FilePath, n.Name, n.StartLine, n.EndLine)
		out.nodes[string(n.Kind)] = append(out.nodes[string(n.Kind)], id)
		out.nodeMeta[string(n.Kind)] = append(out.nodeMeta[string(n.Kind)], id+"|"+kindParityMeta(n.Meta))
	}
	edges := r.AllEdges()
	// Contract IDs whose owner edges are recorded in two or more files.
	ownerFiles := map[string]map[string]struct{}{}
	for _, e := range edges {
		if e == nil || !kindParityOwnerEdge(e.Kind) || kindOf[e.To] != graph.KindContract {
			continue
		}
		if ownerFiles[e.To] == nil {
			ownerFiles[e.To] = map[string]struct{}{}
		}
		ownerFiles[e.To][e.FilePath] = struct{}{}
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		from := e.From
		if kindParityOwnerEdge(e.Kind) && len(ownerFiles[e.To]) > 1 {
			from = "*"
		}
		id := fmt.Sprintf("%s>%s|%s:%d", from, e.To, e.FilePath, e.Line)
		out.edges[string(e.Kind)] = append(out.edges[string(e.Kind)], id)
		out.edgeMeta[string(e.Kind)] = append(out.edgeMeta[string(e.Kind)], id+"|"+kindParityMeta(e.Meta))
	}
	for _, m := range []map[string][]string{out.nodes, out.edges, out.nodeMeta, out.edgeMeta} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return out
}

func kindParityOwnerEdge(kind graph.EdgeKind) bool {
	return kind == graph.EdgeProvides || kind == graph.EdgeConsumes || kind == graph.EdgeHandlesRoute
}

func kindParityMeta(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, meta[k]))
	}
	return strings.Join(parts, ",")
}

// kindParityLine is one kind's row of the report.
type kindParityLine struct {
	class, kind         string
	clean, view         int
	onlyClean, onlyView []string
	metaDiffers         int
	metaFields          []string
	// residual is the rows the primary per-save path differs on as well.
	residual []string
}

func compareKindParity(view, clean kindParityRows) []kindParityLine {
	var lines []kindParityLine
	for _, class := range []string{"node", "edge"} {
		viewRows, cleanRows, viewMeta, cleanMeta := view.nodes, clean.nodes, view.nodeMeta, clean.nodeMeta
		if class == "edge" {
			viewRows, cleanRows, viewMeta, cleanMeta = view.edges, clean.edges, view.edgeMeta, clean.edgeMeta
		}
		kinds := map[string]struct{}{}
		for k := range viewRows {
			kinds[k] = struct{}{}
		}
		for k := range cleanRows {
			kinds[k] = struct{}{}
		}
		sorted := make([]string, 0, len(kinds))
		for k := range kinds {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			onlyView, onlyClean := contractRowsDiff(viewRows[k], cleanRows[k])
			metaView, metaClean := contractRowsDiff(viewMeta[k], cleanMeta[k])
			lines = append(lines, kindParityLine{
				class: class, kind: k, clean: len(cleanRows[k]), view: len(viewRows[k]),
				onlyClean: onlyClean, onlyView: onlyView, metaDiffers: len(metaView) - len(onlyView),
				metaFields: kindParityMetaFields(metaView, metaClean),
			})
		}
	}
	return lines
}

// reportKindParity logs the table and returns the kinds whose rows differ.
func reportKindParity(t *testing.T, label string, lines []kindParityLine) []string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n| class | kind | clean | view | delta lost | delta added | engine residual | meta differs |\n|---|---|---|---|---|---|---|---|\n", label)
	var differ []string
	for _, l := range lines {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d |\n", l.class, l.kind, l.clean, l.view, len(l.onlyClean), len(l.onlyView), len(l.residual), l.metaDiffers)
		for _, r := range l.residual {
			fmt.Fprintf(&b, "RESIDUAL %s %s: %s\n", l.class, l.kind, r)
		}
		for _, f := range l.metaFields {
			fmt.Fprintf(&b, "META %s %s: %s\n", l.class, l.kind, f)
		}
		if len(l.onlyClean)+len(l.onlyView) > 0 {
			differ = append(differ, fmt.Sprintf("%s %s: only clean %v; only view %v", l.class, l.kind, kindParityHead(l.onlyClean), kindParityHead(l.onlyView)))
		}
	}
	t.Log(b.String())
	if out := os.Getenv("GX_KIND_PARITY_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = f.WriteString(b.String() + "\n")
			for _, d := range differ {
				_, _ = f.WriteString("DIFF " + label + ": " + d + "\n")
			}
			_ = f.Close()
		}
	}
	return differ
}

func kindParityHead(rows []string) []string {
	if len(rows) > 4 {
		return rows[:4]
	}
	return rows
}

// kindParityCheck compares a chain's composed view with a clean index of the
// working tree by kind. A row the view and the clean index disagree on is
// judged against the primary per-save path over the same changed paths (the
// triangulation TestEditDeltaAccumulatedCleanParity uses): where the primary
// per-save path produces the same difference, it is the incremental engine's
// own residual, reported but not failed on; every other difference is the
// delta's, and fails.
func kindParityCheck(t *testing.T, label, repoDir string, view graph.Reader, covered, changedRel []string) {
	t.Helper()
	kindParityCheckWith(t, label, repoDir, view, covered, func() graph.Reader {
		if len(changedRel) == 0 {
			return nil
		}
		return primaryPerSaveOf(t, repoDir, changedRel, config.Default().Index)
	})
}

// kindParityCheckWith is kindParityCheck with the primary per-save path's
// state given (nil: no triangulation).
func kindParityCheckWith(t *testing.T, label, repoDir string, view graph.Reader, covered []string, primaryOf func() graph.Reader) {
	t.Helper()
	kindParityCheckIndexed(t, label, repoDir, view, covered, primaryOf, builderIndex)
}

// kindParityCheckIndexed is kindParityCheckWith with the clean index's
// indexer given (the go/types pass on or off, as the view's builds run it).
func kindParityCheckIndexed(t *testing.T, label, repoDir string, view graph.Reader, covered []string, primaryOf func() graph.Reader, index func(testing.TB, *store_sqlite.Store, string)) {
	t.Helper()
	clean := builderOpenStore(t, "kind-parity-clean-"+strings.NewReplacer("/", "-", " ", "-").Replace(label))
	index(t, clean, repoDir)
	lines := compareKindParity(collectKindParityRows(view), collectKindParityRows(clean))
	differs := false
	for _, l := range lines {
		if len(l.onlyClean)+len(l.onlyView) > 0 {
			differs = true
		}
	}
	var primaryReader graph.Reader
	if differs {
		primaryReader = primaryOf()
	}
	if primaryReader != nil {
		primary := collectKindParityRows(primaryReader)
		for i := range lines {
			l := &lines[i]
			rows := primary.nodes[l.kind]
			if l.class == "edge" {
				rows = primary.edges[l.kind]
			}
			// Counted, so a row the view holds twice and the primary path
			// once is a delta defect, not a residual.
			has := make(map[string]int, len(rows))
			for _, r := range rows {
				has[r]++
			}
			lost := maps.Clone(has)
			var onlyClean, onlyView []string
			for _, r := range l.onlyClean {
				if lost[r] > 0 {
					lost[r]--
					onlyClean = append(onlyClean, r) // the primary path has it: the delta lost it
				} else {
					l.residual = append(l.residual, "clean only: "+r)
				}
			}
			for _, r := range l.onlyView {
				if has[r] > 0 {
					has[r]--
					l.residual = append(l.residual, "incremental only: "+r)
				} else {
					onlyView = append(onlyView, r)
				}
			}
			l.onlyClean, l.onlyView = onlyClean, onlyView
		}
	}
	differ := reportKindParity(t, fmt.Sprintf("%s (the edit's generation covers %v)", label, covered), lines)
	if len(differ) > 0 {
		t.Errorf("%s: the composed view differs from a clean index where the primary per-save path does not:\n%s", label, strings.Join(differ, "\n"))
	}
}

func kindParityRepo(t *testing.T, name string) (string, *store_sqlite.Store) {
	t.Helper()
	builderIsolateGit(t)
	store := builderOpenStore(t, name)
	repoDir := builderTempDir(t, "checkout-"+name)
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, kindParityTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	return repoDir, store
}

// generationCovered is the paths a published generation covers.
func generationCovered(t *testing.T, store *store_sqlite.Store, generation int64) []string {
	t.Helper()
	layer, err := graphview.NewGenerationLayer(store.AtGeneration(generation))
	if err != nil {
		t.Fatalf("open generation %d: %v", generation, err)
	}
	return layer.FilePaths()
}

// Each sibling edit, alone on a clean checkout.
func TestKindParityCleanCheckout(t *testing.T) {
	for _, edit := range kindParityEdits()[:4] {
		t.Run(edit.name, func(t *testing.T) {
			repoDir, store := kindParityRepo(t, "kind-parity-clean-"+edit.name)
			builder := builderNewBuilder(store)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			applyKindParityEdit(t, repoDir, edit)
			id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
			if err != nil {
				t.Fatal(err)
			}
			kindParityCheck(t, "clean/"+edit.name, repoDir, dirtyChainComposed(t, store, []int64{id}), generationCovered(t, store, id), edit.paths())
		})
	}
}

// The sibling edits one after another as a chain, to the depth bound.
func TestKindParityChain(t *testing.T) {
	repoDir, store := kindParityRepo(t, "kind-parity-chain")
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	primary := newKindParityPrimary(t, repoDir)
	for i, edit := range kindParityEdits() {
		applyKindParityEdit(t, repoDir, edit)
		primary.save(edit.paths())
		id, report, chain := h.build()
		if i > 0 && report.ParentGenerationID == 0 {
			t.Fatalf("%s: built direct (%q), want chained", edit.name, report.ChainFallbackReason)
		}
		kindParityCheckWith(t, fmt.Sprintf("depth %d/%s", len(chain), edit.name), repoDir, dirtyChainComposed(t, store, chain), generationCovered(t, store, id), func() graph.Reader { return primary.store })
	}
}

// The same fixture through the checkout's MCP edits, with the stepped fold:
// four edits, a fold of them held before its first step, two edits above it,
// the landing (a re-base of the lowest layer above), and two edits after it.
// The served view is compared by kind after every edit and after the landing.
func TestKindParityFoldAndRebase(t *testing.T) {
	f, c, l := mcpChainFixture(t, kindParityTree(), false)
	edits := kindParityEdits()
	primary := newKindParityPrimary(t, f.worktree)
	check := func(label string) {
		t.Helper()
		view := chainMaterialize(t, f)
		defer view.Close()
		kindParityCheckWith(t, label, f.worktree, view.Reader, nil, func() graph.Reader { return primary.store })
	}
	var trigger CheckoutCycle
	for i := 0; i < 4; i++ {
		trigger = mcpEdit(t, l, f, func() { applyKindParityEdit(t, f.worktree, edits[i]) })
		primary.save(edits[i].paths())
		check(fmt.Sprintf("mcp depth %d/%s", trigger.DirtyChainDepth, edits[i].name))
	}
	if !trigger.CompactionScheduled {
		t.Fatalf("the fourth edit scheduled no fold (depth %d)", trigger.DirtyChainDepth)
	}
	proceed, result := startSteppedFold(t, c, trigger)
	for i := 4; i < 6; i++ {
		out := mcpEdit(t, l, f, func() { applyKindParityEdit(t, f.worktree, edits[i]) })
		primary.save(edits[i].paths())
		check(fmt.Sprintf("mcp over the running fold depth %d/%s", out.DirtyChainDepth, edits[i].name))
	}
	proceed()
	report := <-result
	if report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the fold did not land: %s (%v)", report.Outcome, report.Err)
	}
	check("after the landing (" + report.Landing + ")")
	for i := 6; i < 8; i++ {
		out := mcpEdit(t, l, f, func() { applyKindParityEdit(t, f.worktree, edits[i]) })
		primary.save(edits[i].paths())
		check(fmt.Sprintf("mcp after the landing depth %d/%s", out.DirtyChainDepth, edits[i].name))
	}
}

// A reproduction: six chained MCP edits of the builder fixture; the view
// differed from a clean index at depth 6 (clone_sig meta and 2 edges).
func TestKindParityBuilderFixtureSixEdits(t *testing.T) {
	f, _, l := mcpChainFixture(t, builderTreeA(), false)
	primary := newKindParityPrimary(t, f.worktree)
	for i := 0; i < 6; i++ {
		out := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
		primary.save([]string{[]string{"island.go", "helper.go", "caller.go", "core.go"}[i%4]})
		view := chainMaterialize(t, f)
		kindParityCheckWith(t, fmt.Sprintf("builder depth %d", out.DirtyChainDepth), f.worktree, view.Reader, nil, func() graph.Reader { return primary.store })
		view.Close()
	}
}

// kindParityMetaFields names, for rows whose identity matches on both sides
// but whose metadata differs, each differing key with both values.
func kindParityMetaFields(view, clean []string) []string {
	split := func(row string) (string, map[string]string) {
		i := strings.LastIndex(row, "|")
		fields := map[string]string{}
		if row[i+1:] != "" {
			for _, kv := range strings.Split(row[i+1:], ",") {
				if j := strings.IndexByte(kv, '='); j > 0 {
					fields[kv[:j]] = kv[j+1:]
				}
			}
		}
		return row[:i], fields
	}
	cleanByID := map[string]map[string]string{}
	for _, row := range clean {
		id, fields := split(row)
		cleanByID[id] = fields
	}
	seen := map[string]struct{}{}
	var out []string
	for _, row := range view {
		id, v := split(row)
		c, ok := cleanByID[id]
		if !ok {
			continue
		}
		keys := map[string]struct{}{}
		for k := range v {
			keys[k] = struct{}{}
		}
		for k := range c {
			keys[k] = struct{}{}
		}
		for k := range keys {
			if v[k] == c[k] {
				continue
			}
			line := fmt.Sprintf("%s: clean=%q view=%q", k, kindParityClip(c[k]), kindParityClip(v[k]))
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, id+" "+line)
		}
	}
	sort.Strings(out)
	return out
}

func kindParityClip(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// paths is the repository-relative paths an edit writes or removes.
func (e kindParityEdit) paths() []string {
	out := append([]string(nil), e.drop...)
	for rel := range e.write {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// A direct edit of the producers' own file, on a clean checkout. The edit
// shifts lines, so the primary per-save path re-derives the file as the delta
// does: a same-line body edit is inert to the primary path, which then keeps
// its prior rows and no longer tells an engine residual from a delta defect.
func TestKindParityOwnFileEdit(t *testing.T) {
	t.Run("same_line", func(t *testing.T) {
		kindParityOwnFile(t, "func Env() string { return", "func Env() string { _ = 1; return")
	})
	t.Run("line_shift", func(t *testing.T) {
		kindParityOwnFile(t, "func Env() string { return", "func Env() string {\n\t_ = 1\n\treturn")
	})
}

func kindParityOwnFile(t *testing.T, from, to string) {
	repoDir, store := kindParityRepo(t, "kind-parity-own-"+strings.ReplaceAll(t.Name(), "/", "-"))
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	src := kindParityTree()["prod/producers.go"]
	builderWriteFile(t, repoDir, "prod/producers.go", strings.Replace(src, from, to, 1))
	id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	kindParityCheck(t, "own-file/"+t.Name(), repoDir, dirtyChainComposed(t, store, []int64{id}), generationCovered(t, store, id), []string{"prod/producers.go"})
}

// The older oracle (TestEditDeltaMatchesPrimaryPerSave's triangulation: a row
// may differ from the primary per-save path only where it matches a whole
// index) over the producers fixture, whose function values, interface and
// clone pair its seeded repositories do not carry. The edits shift lines, so
// the primary path re-derives the file as the delta does.
func TestEditDeltaMatchesPrimaryPerSaveProducers(t *testing.T) {
	src := kindParityTree()["prod/producers.go"]
	cases := map[string]kindParityEdit{
		"own_file":  {name: "own_file", write: map[string]string{"prod/producers.go": strings.Replace(src, "func Env() string { return", "func Env() string {\n\t_ = 1\n\treturn", 1)}},
		"signature": kindParityEdits()[1],
		"rename":    kindParityEdits()[2],
		"delete":    kindParityEdits()[3],
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			repoDir, store := kindParityRepo(t, "delta-vs-primary-"+name)
			builder := builderNewBuilder(store)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			applyKindParityEdit(t, repoDir, edit)
			id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
			if err != nil {
				t.Fatal(err)
			}
			primary := primaryPerSaveOf(t, repoDir, edit.paths(), config.Default().Index)
			flat := builderOpenStore(t, "delta-vs-primary-flat-"+name)
			builderIndex(t, flat, repoDir)
			composed := dirtyChainComposed(t, store, []int64{id})
			bad := editDeltaTriangulate(fingerprintNeutralReader{composed}, fingerprintNeutralReader{primary}, fingerprintNeutralReader{flat})
			if len(bad) > 0 {
				t.Errorf("%s: rows where the delta differs from the primary AND from a whole index:\n  %s", name, strings.Join(bad, "\n  "))
			}
		})
	}
}

// kindParityPrimary is the primary checkout's per-save path over the same
// sequence of saves as a chain: a whole index of HEAD in a scratch worktree,
// then each edit's paths re-indexed through IncrementalReindexPaths in turn.
type kindParityPrimary struct {
	t       *testing.T
	repoDir string
	scratch string
	store   *store_sqlite.Store
	idx     *Indexer
}

func newKindParityPrimary(t *testing.T, repoDir string) *kindParityPrimary {
	t.Helper()
	return newKindParityPrimaryWith(t, repoDir, nil)
}

// newKindParityPrimaryWith is newKindParityPrimary with a semantic manager:
// the primary path's whole index and every save run the go/types pass.
func newKindParityPrimaryWith(t *testing.T, repoDir string, mgr *semantic.Manager) *kindParityPrimary {
	t.Helper()
	scratch := builderTempDir(t, "primary-seq")
	builderGit(t, repoDir, "worktree", "add", "--detach", scratch, "HEAD")
	store := builderOpenStore(t, "primary-seq")
	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(func() { idx.Close() })
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if mgr != nil {
		idx.SetSemanticManager(mgr)
	}
	if _, err := idx.Index(scratch); err != nil {
		t.Fatalf("primary index: %v", err)
	}
	return &kindParityPrimary{t: t, repoDir: repoDir, scratch: scratch, store: store, idx: idx}
}

// save brings paths of the scratch worktree to the working tree's bytes and
// applies them through the primary per-save path.
func (p *kindParityPrimary) save(paths []string) {
	p.t.Helper()
	for _, rel := range paths {
		src := filepath.Join(p.repoDir, filepath.FromSlash(rel))
		dst := filepath.Join(p.scratch, filepath.FromSlash(rel))
		data, err := os.ReadFile(src)
		switch {
		case err == nil:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				p.t.Fatal(err)
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				p.t.Fatal(err)
			}
		case os.IsNotExist(err):
			_ = os.Remove(dst)
		default:
			p.t.Fatal(err)
		}
	}
	if _, err := p.idx.IncrementalReindexPaths(p.scratch, paths); err != nil {
		p.t.Fatalf("primary per-save: %v", err)
	}
}
