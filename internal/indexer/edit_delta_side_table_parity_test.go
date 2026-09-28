package indexer

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Side-table parity: what a composed view serves from each per-file side table
// for the rows it holds, against a clean whole index of the same working tree,
// triangulated against the primary per-save path.
//
// A side table is not composed by the view reader; each consumer composes it
// over the stack's generation handles:
//
//   - symbol full-text documents: every generation's corpus is searched and a
//     hit is dropped when a higher generation speaks for its file or identity
//     (query.viewTextCandidates), so a node is searchable exactly when the
//     topmost generation speaking for it holds its document;
//   - constant values and clone shingles are keyed by node ID and read, by the
//     derivations that consume them, from the generation that owns the node;
//   - reference facts are the union over the stack (ancestryRefFacts), a
//     superset by design: a fact a clean index lacks is reported, not failed
//     on, and a fact a clean index holds that the union lacks is a loss.
//
// The rules are applied per node the view and the clean index both hold (node
// parity itself is the kind-parity tests' job).

// sideSource is one corpus of a composed view's stack, bottom first: the
// indexed corpus (no layer) and one generation per entry above it.
type sideSource struct {
	handle *store_sqlite.Store
	layer  graph.OverlayLayerReader
}

func sideSourcesOfChain(t *testing.T, store *store_sqlite.Store, chain []int64) []sideSource {
	t.Helper()
	out := []sideSource{{handle: store.AtGeneration(0)}}
	for _, id := range chain {
		handle := store.AtGeneration(id)
		layer, err := graphview.NewGenerationLayer(handle)
		if err != nil {
			t.Fatalf("NewGenerationLayer(%d): %v", id, err)
		}
		out = append(out, sideSource{handle: handle, layer: layer})
	}
	return out
}

func sideSourcesOfView(store *store_sqlite.Store, view *graphview.RepoView) []sideSource {
	out := []sideSource{{handle: store.AtGeneration(0)}}
	for _, source := range view.GenerationSources() {
		if source.Handle != nil {
			out = append(out, sideSource{handle: source.Handle, layer: source.Layer})
		}
	}
	return out
}

// sideOwner is the topmost source that speaks for n: by its identity alone
// (the symbol lanes' rule, query.hiddenAboveSource) or also by its file.
func sideOwner(sources []sideSource, n *graph.Node, byFile bool) int {
	for i := len(sources) - 1; i > 0; i-- {
		l := sources[i].layer
		if l == nil {
			continue
		}
		if (byFile && n.FilePath != "" && l.HasFile(n.FilePath)) || l.CoversNodeID(n.ID) || l.OwnsNodeIdentity(n.ID) {
			return i
		}
	}
	return 0
}

// sideTableRows renders, per table, what a reader of the stack is served.
type sideTableRows map[string][]string

func collectSideTables(nodes []*graph.Node, sources []sideSource) sideTableRows {
	out := sideTableRows{}
	shingles := map[int]map[string][]uint64{}
	for _, n := range nodes {
		if n == nil || n.Name == "" {
			continue
		}
		// The trailing space makes the query non-identifier-shaped, so the
		// search reads the full-text documents rather than the exact-name
		// tier over the nodes table (which node parity already covers).
		hits, _ := sources[sideOwner(sources, n, false)].handle.SearchSymbols(n.Name+" ", 500)
		src := sources[sideOwner(sources, n, true)].handle
		found := false
		for _, h := range hits {
			if h.NodeID == n.ID {
				found = true
				break
			}
		}
		out["symbol_fts"] = append(out["symbol_fts"], fmt.Sprintf("%s searchable=%v", n.ID, found))
		if n.Kind == graph.KindConstant {
			values, _ := src.ConstantValuesByNodeIDs([]string{n.ID})
			out["constant_values"] = append(out["constant_values"], fmt.Sprintf("%s=%q", n.ID, values[n.ID]))
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			i := sideOwner(sources, n, true)
			if shingles[i] == nil {
				shingles[i], _ = sources[i].handle.LoadCloneShingles(builderRepoPrefix)
				if shingles[i] == nil {
					shingles[i] = map[string][]uint64{}
				}
			}
			out["clone_shingles"] = append(out["clone_shingles"], fmt.Sprintf("%s=%d:%x", n.ID, len(shingles[i][n.ID]), sideHash(shingles[i][n.ID])))
		}
	}
	seen := map[string]struct{}{}
	for _, s := range sources {
		facts, _ := s.handle.LoadRefFactsByFiles(builderRepoPrefix, nil)
		for _, f := range facts {
			key := fmt.Sprintf("%s>%s|%s|%s|%s:%d", f.FromID, f.ToID, f.Kind, f.RefName, f.FilePath, f.Line)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out["ref_facts"] = append(out["ref_facts"], key)
		}
	}
	// Content sections, composed as mcp.viewContentSearcher does: every
	// corpus of the stack, a lower row dropped when a layer above holds its
	// file or speaks for its identity. Rendered with a hash of the body, so a
	// row served twice or served stale shows.
	for i, s := range sources {
		_ = s.handle.ScanContent(builderRepoPrefix, func(nodeID, filePath, body string) bool {
			for j := i + 1; j < len(sources); j++ {
				l := sources[j].layer
				if l != nil && ((filePath != "" && l.HasFile(filePath)) || l.CoversNodeID(nodeID) || l.OwnsNodeIdentity(nodeID)) {
					return true
				}
			}
			out["content_fts"] = append(out["content_fts"], fmt.Sprintf("%s|%s|%x", nodeID, filePath, sideHash([]uint64{uint64(len(body)), sideStringHash(body)})))
			return true
		})
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func sideStringHash(s string) uint64 {
	h := uint64(1469598103934665603)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 1099511628211
	}
	return h
}

func sideHash(xs []uint64) uint64 {
	h := uint64(1469598103934665603)
	for _, x := range xs {
		h = (h ^ x) * 1099511628211
	}
	return h
}

// sideCommonNodes is the nodes of view that clean also holds, by ID.
func sideCommonNodes(view, clean graph.Reader) []*graph.Node {
	have := map[string]struct{}{}
	for _, n := range clean.AllNodes() {
		if n != nil {
			have[n.ID] = struct{}{}
		}
	}
	var out []*graph.Node
	for _, n := range view.AllNodes() {
		if n == nil {
			continue
		}
		if _, ok := have[n.ID]; ok {
			out = append(out, n)
		}
	}
	return out
}

func sideDiff(got, want []string) (onlyGot, onlyWant []string) {
	count := map[string]int{}
	for _, w := range want {
		count[w]++
	}
	for _, g := range got {
		if count[g] > 0 {
			count[g]--
			continue
		}
		onlyGot = append(onlyGot, g)
	}
	for w, c := range count {
		for ; c > 0; c-- {
			onlyWant = append(onlyWant, w)
		}
	}
	sort.Strings(onlyWant)
	return onlyGot, onlyWant
}

// sideTableCheck compares the stack's side tables with a clean index and the
// primary per-save store, per table. A table row the view and the primary
// share against the clean index is an engine residual; a reference fact only
// the union holds is the union's superset by design; anything else fails.
func sideTableCheck(t *testing.T, label, repoDir string, view graph.Reader, sources []sideSource, primary *store_sqlite.Store) {
	t.Helper()
	clean := builderOpenStore(t, "side-clean-"+strings.NewReplacer("/", "-", " ", "-").Replace(label))
	builderIndex(t, clean, repoDir)
	nodes := sideCommonNodes(view, clean)
	got := collectSideTables(nodes, sources)
	want := collectSideTables(nodes, []sideSource{{handle: clean}})
	var prim sideTableRows
	if primary != nil {
		prim = collectSideTables(sideCommonNodes(view, primary), []sideSource{{handle: primary}})
	}
	tables := []string{"symbol_fts", "content_fts", "constant_values", "clone_shingles", "ref_facts"}
	t.Logf("%s\n        | table | clean | view | delta lost | delta added | engine residual | union superset |", label)
	var failing []string
	for _, table := range tables {
		onlyView, onlyClean := sideDiff(got[table], want[table])
		inPrimary := map[string]int{}
		for _, r := range prim[table] {
			inPrimary[r]++
		}
		var lost, added, residual, superset []string
		for _, r := range onlyClean {
			if primary != nil && inPrimary[r] == 0 {
				residual = append(residual, "clean only: "+r)
				continue
			}
			lost = append(lost, r)
		}
		for _, r := range onlyView {
			switch {
			case table == "ref_facts" && !slices.Contains(want[table], r):
				superset = append(superset, r)
			case primary != nil && inPrimary[r] > 0:
				inPrimary[r]--
				residual = append(residual, "incremental only: "+r)
			default:
				added = append(added, r)
			}
		}
		if table == "ref_facts" {
			// A fact the clean index or the primary per-save path holds and
			// the union lacks is a loss; the rest of the union is its
			// superset. (A whole index persists no reference facts for this
			// fixture, so the primary path is the one that can show a loss.)
			added = nil
			for _, r := range prim[table] {
				if !slices.Contains(got[table], r) {
					lost = append(lost, "primary only: "+r)
				}
			}
		}
		t.Logf("        | %s | %d | %d | %d | %d | %d | %d |", table, len(want[table]), len(got[table]), len(lost), len(added), len(residual), len(superset))
		for _, r := range residual {
			t.Logf("        RESIDUAL %s: %s", table, r)
		}
		stale := 0
		for _, r := range superset {
			tag := "SUPERSET"
			if !slices.Contains(prim[table], r) {
				tag, stale = "SUPERSET not in primary", stale+1
			}
			t.Logf("        %s %s: %s", tag, table, r)
		}
		if len(superset) > 0 {
			t.Logf("        | %s superset rows the primary per-save path lacks: %d |", table, stale)
		}
		if len(lost)+len(added) > 0 {
			line := fmt.Sprintf("%s: lost %v; added %v", table, lost, added)
			failing = append(failing, line)
		}
	}
	if len(failing) > 0 {
		t.Errorf("%s: the stack serves side-table rows other than a clean index's where the primary per-save path does not:\n%s", label, strings.Join(failing, "\n"))
	}
}

// Each sibling edit alone on a clean checkout (depth 1).
func TestSideTableParityCleanCheckout(t *testing.T) {
	for _, edit := range kindParityEdits()[:4] {
		t.Run(edit.name, func(t *testing.T) {
			repoDir, store := kindParityRepo(t, "side-clean-"+edit.name)
			builder := builderNewBuilder(store)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			primary := newKindParityPrimary(t, repoDir)
			applyKindParityEdit(t, repoDir, edit)
			primary.save(edit.paths())
			id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
			if err != nil {
				t.Fatal(err)
			}
			chain := []int64{id}
			sideTableCheck(t, "clean/"+edit.name, repoDir, dirtyChainComposed(t, store, chain), sideSourcesOfChain(t, store, chain), primary.store)
		})
	}
}

// The sibling edits as a chain, to depth 8.
func TestSideTableParityChain(t *testing.T) {
	repoDir, store := kindParityRepo(t, "side-chain")
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	primary := newKindParityPrimary(t, repoDir)
	for _, edit := range kindParityEdits() {
		applyKindParityEdit(t, repoDir, edit)
		primary.save(edit.paths())
		_, _, chain := h.build()
		sideTableCheck(t, fmt.Sprintf("depth %d/%s", len(chain), edit.name), repoDir, dirtyChainComposed(t, store, chain), sideSourcesOfChain(t, store, chain), primary.store)
	}
}

// sideContentTree is the kind-parity fixture plus a content file (plain text,
// which the content index takes as sections) and a Markdown file.
func sideContentTree() map[string]string {
	tree := kindParityTree()
	tree["notes/plan.txt"] = "Plan for the producers package.\n\nThe quartz milestone ships the route.\nThe basalt milestone ships the clone pair.\n"
	tree["docs/guide.md"] = "# Guide\n\n## Producers\n\nThe producers file holds every producer.\n\n## Siblings\n\nSiblings are edited, renamed and deleted.\n"
	return tree
}

func sideContentEdits() []kindParityEdit {
	return []kindParityEdit{
		{name: "text_direct", write: map[string]string{"notes/plan.txt": "Plan for the producers package.\n\nThe quartz milestone ships the route.\nThe obsidian milestone replaces basalt.\n"}},
		{name: "sibling", write: map[string]string{"prod/sib_b.go": "package prod\n\n// Other is a sibling whose body changes.\nfunc Other() int {\n\t_ = 3\n\treturn Limit + 3\n}\n"}},
		{name: "markdown_direct", write: map[string]string{"docs/guide.md": "# Guide\n\n## Producers\n\nThe producers file holds every producer and the clone pair.\n\n## Siblings\n\nSiblings are edited, renamed and deleted.\n"}},
		{name: "text_again", write: map[string]string{"notes/plan.txt": "Plan for the producers package.\n\nThe quartz milestone ships the route.\n"}},
		{name: "text_delete", drop: []string{"notes/plan.txt"}},
	}
}

// Content full-text over a chain: a direct edit of the text file, a sibling
// edit, a direct edit of the Markdown file, another text edit and the text
// file's deletion, each compared as the other side tables are.
func TestSideTableParityContent(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "side-content")
	repoDir := builderTempDir(t, "checkout-side-content")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, sideContentTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	if n := len(collectSideTables(nil, []sideSource{{handle: store.AtGeneration(0)}})["content_fts"]); n == 0 {
		t.Fatal("fixture precondition: the base index holds no content section")
	}
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	primary := newKindParityPrimary(t, repoDir)
	for _, edit := range sideContentEdits() {
		applyKindParityEdit(t, repoDir, edit)
		primary.save(edit.paths())
		_, _, chain := h.build()
		sideTableCheck(t, fmt.Sprintf("content depth %d/%s", len(chain), edit.name), repoDir, dirtyChainComposed(t, store, chain), sideSourcesOfChain(t, store, chain), primary.store)
	}
}
