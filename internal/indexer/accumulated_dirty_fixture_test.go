package indexer

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The accumulated-dirty-edit fixture.
//
// A Go module holding accumulatedDirtyUnits edit units plus two reserved
// target units, and a narrow dependency chain a -> b -> c -> d with one
// importing consumer. A unit is one file declaring one exported function and
// one unexported package-local caller of it. The two layouts differ only in
// where the units live:
//
//   - independent: every unit is its own one-file package (ind/pNNN/pNNN.go),
//     so no unit's compiler frontier reaches another's;
//   - same package: every unit is a file of one package (same/fNNN.go), the
//     honest case where a Go compiler legitimately needs the whole package.
//
// There are no generated files, no build tags, no init functions, no package
// variables and no //line or //go: directives anywhere in the module.
//
// Edits are chosen so they cannot change an externally relevant fact:
//
//   - a body edit inserts one local statement into the UNEXPORTED caller
//     (`v += 1`), which moves that function's end line and nothing else;
//   - a comment edit rewrites the ordinary (non-directive) doc comment on the
//     unexported caller.

const (
	accumulatedDirtyUnits = 200
	// The reserved units are never part of the accumulated dirty set.
	accumulatedDirtyBodyTarget    = accumulatedDirtyUnits
	accumulatedDirtyCommentTarget = accumulatedDirtyUnits + 1
	accumulatedDirtyModule        = "example.com/fixture"
)

type accumulatedDirtyLayout string

const (
	accumulatedDirtyIndependent accumulatedDirtyLayout = "independent_packages"
	accumulatedDirtySamePackage accumulatedDirtyLayout = "same_package"
)

// accumulatedDirtyUnitPath is the repo-relative path of unit i.
func accumulatedDirtyUnitPath(layout accumulatedDirtyLayout, i int) string {
	if layout == accumulatedDirtyIndependent {
		return fmt.Sprintf("ind/p%03d/p%03d.go", i, i)
	}
	return fmt.Sprintf("same/f%03d.go", i)
}

func accumulatedDirtyUnitPackage(layout accumulatedDirtyLayout, i int) string {
	if layout == accumulatedDirtyIndependent {
		return fmt.Sprintf("p%03d", i)
	}
	return "same"
}

const accumulatedDirtyCommentBase = "is the package-local caller of"

// accumulatedDirtyUnitSource renders unit i in its committed form, optionally
// with the body edit and/or the comment edit applied.
func accumulatedDirtyUnitSource(layout accumulatedDirtyLayout, i int, body, comment bool) string {
	doc := accumulatedDirtyCommentBase
	if comment {
		doc = "is the package-local caller (comment revised) of"
	}
	extra := ""
	if body {
		extra = "\tv += 1\n"
	}
	return fmt.Sprintf(`package %[1]s

// Exported%03[2]d is this unit's exported declaration.
func Exported%03[2]d() int {
	return %[2]d
}

// caller%03[2]d %[3]s Exported%03[2]d.
func caller%03[2]d() int {
	v := Exported%03[2]d()
%[4]s	return v
}
`, accumulatedDirtyUnitPackage(layout, i), i, doc, extra)
}

// accumulatedDirtyTree is the committed tree for one layout.
func accumulatedDirtyTree(layout accumulatedDirtyLayout) map[string]string {
	tree := map[string]string{
		"go.mod": "module " + accumulatedDirtyModule + "\n\ngo 1.22\n",
		"chain/d/d.go": `package d

// D is the bottom of the chain.
func D() int {
	return 1
}
`,
		"chain/c/c.go": `package c

import "` + accumulatedDirtyModule + `/chain/d"

// C calls down the chain.
func C() int {
	return d.D() + 1
}
`,
		"chain/b/b.go": `package b

import "` + accumulatedDirtyModule + `/chain/c"

// B calls down the chain.
func B() int {
	return c.C() + 1
}
`,
		"chain/a/a.go": `package a

import "` + accumulatedDirtyModule + `/chain/b"

// A calls down the chain.
func A() int {
	return b.B() + 1
}
`,
		"consumer/consumer.go": `package consumer

import "` + accumulatedDirtyModule + `/chain/a"

// Consume is the chain's one importing consumer.
func Consume() int {
	return a.A()
}
`,
	}
	for i := 0; i <= accumulatedDirtyCommentTarget; i++ {
		tree[accumulatedDirtyUnitPath(layout, i)] = accumulatedDirtyUnitSource(layout, i, false, false)
	}
	return tree
}

// accumulatedDirtyRepo commits the layout's tree in a fresh private git
// repository and indexes the committed state into store's base corpus.
func accumulatedDirtyRepo(t *testing.T, layout accumulatedDirtyLayout, store *store_sqlite.Store) string {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "checkout-"+string(layout))
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, accumulatedDirtyTree(layout))
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	return repoDir
}

func accumulatedDirtyWriteUnit(t *testing.T, repoDir string, layout accumulatedDirtyLayout, i int, body, comment bool) {
	t.Helper()
	full := filepath.Join(repoDir, filepath.FromSlash(accumulatedDirtyUnitPath(layout, i)))
	if err := os.WriteFile(full, []byte(accumulatedDirtyUnitSource(layout, i, body, comment)), 0o644); err != nil {
		t.Fatalf("write unit %d: %v", i, err)
	}
}

// --- the clean-parity oracle ---------------------------------------------

// cleanParityResult is one comparison of a composed dirty view against an
// independent clean index of the same checkout.
type cleanParityResult struct {
	NodesEqual         bool `json:"nodes_equal"`
	EdgesEqual         bool `json:"edges_equal"`
	FilesEqual         bool `json:"files_equal"`
	SymbolFTSEqual     bool `json:"symbol_fts_equal"`
	SemanticEqual      bool `json:"semantic_bindings_equal"`
	ConstantsEqual     bool `json:"constant_values_equal"`
	SearchProbesEqual  bool `json:"search_probes_equal"`
	MasksValid         bool `json:"masks_valid"`
	ReaderSurfaceAgree bool `json:"reader_surface_agrees"`
	// ReaderSurfaceProbed is false when the exhaustive graph.Reader probe
	// comparison was not run (a recording-only call); ReaderSurfaceAgree is
	// then vacuous.
	ReaderSurfaceProbed bool     `json:"reader_surface_probed"`
	ComposedDigest      string   `json:"composed_digest"`
	CleanDigest         string   `json:"clean_digest"`
	Nodes               int      `json:"nodes"`
	Edges               int      `json:"edges"`
	SymbolFTSRows       int      `json:"symbol_fts_rows"`
	Diffs               []string `json:"diffs,omitempty"`
}

func (r cleanParityResult) ok() bool {
	return r.NodesEqual && r.EdgesEqual && r.FilesEqual && r.SymbolFTSEqual && r.SemanticEqual &&
		r.ConstantsEqual && r.SearchProbesEqual && r.MasksValid && r.ReaderSurfaceAgree &&
		r.ComposedDigest == r.CleanDigest
}

// assertCleanIndexParity independently indexes the checkout at repoDir into a
// fresh private store and compares it, surface by surface, with the view that
// composes generation generationID over store's base corpus (generation 0).
//
// Compared, after deterministic ordering:
//   - every node and edge (identity, kind, name, file, lines/columns, targets,
//     confidence, origin/provenance, metadata) — and every graph.Reader probe
//     via builderAssertReadersAgree, which also fails the test on its own;
//   - the files inventory (content hash, size, node count, parse errors) —
//     the observable-source fingerprint the index recorded per path;
//   - symbol FTS documents (node id + indexed tokens) and a fixed set of
//     symbol search probes, including a probe for every name in either side,
//     so a stale document for a removed term is a difference;
//   - semantic binding types and constant values (per-file semantic facts);
//   - the generation's own masks, via ValidateGenerationMasks.
//
// Composition of the sidecars is re-implemented here from the mask contract
// (a file mask hides every base row at that path; a node tombstone hides that
// base identity), independently of the production composed reader, so the
// oracle does not grade the production code with itself.
//
// Normalised — and ONLY these, because none is semantic:
//   - Node.AbsoluteFilePath: stamped per response by the MCP layer, never
//     persisted (builderRenderNode);
//   - Edge.Context / ReturnUsage / Via / Alias / NameOnly: filled by response
//     encoders, never written by the store (builderRenderEdge);
//   - view_gen and FTS docids (fts_rowid): physical placement, not content.
//
// strict fails the test on any difference and also drives the exhaustive
// graph.Reader probe comparison (in a subtest, so its verdict is recorded
// exactly); a non-strict call only records the result.
func assertCleanIndexParity(
	t *testing.T, store *store_sqlite.Store, generationID int64, repoDir, label string, strict bool,
) cleanParityResult {
	t.Helper()
	var result cleanParityResult
	clean := builderOpenStore(t, "clean-"+label)
	builderIndex(t, clean, repoDir)
	composed := builderComposed(t, store, generationID)

	composedNodes := builderRenderNodes(composed.AllNodes())
	cleanNodes := builderRenderNodes(clean.AllNodes())
	composedEdges := builderRenderEdges(composed.AllEdges())
	cleanEdges := builderRenderEdges(clean.AllEdges())
	result.Nodes, result.Edges = len(cleanNodes), len(cleanEdges)
	result.NodesEqual = slices.Equal(composedNodes, cleanNodes)
	result.EdgesEqual = slices.Equal(composedEdges, cleanEdges)
	if !result.NodesEqual {
		result.Diffs = append(result.Diffs, parityDiff("nodes", composedNodes, cleanNodes)...)
	}
	if !result.EdgesEqual {
		result.Diffs = append(result.Diffs, parityDiff("edges", composedEdges, cleanEdges)...)
	}

	result.ReaderSurfaceProbed = strict
	if strict {
		result.ReaderSurfaceAgree = t.Run("reader-surface-"+label, func(t *testing.T) {
			builderAssertReadersAgree(t, composed, clean)
		})
	} else {
		result.ReaderSurfaceProbed = false
		result.ReaderSurfaceAgree = true
	}

	result.MasksValid = store.AtGeneration(generationID).ValidateGenerationMasks() == nil

	dirtyDB := parityOpenRaw(t, store)
	cleanDB := parityOpenRaw(t, clean)
	masks := parityMasks(t, dirtyDB, generationID)

	composedFiles := parityComposeFileRows(t, dirtyDB, generationID, masks,
		`SELECT file_path, content_hash || '|' || size || '|' || node_count || '|' || errors FROM files WHERE repo_prefix = ? AND view_gen = ?`)
	cleanFiles := parityRows(t, cleanDB,
		`SELECT file_path, content_hash || '|' || size || '|' || node_count || '|' || errors FROM files WHERE repo_prefix = ? AND view_gen = ?`, 0)
	result.FilesEqual = slices.Equal(composedFiles, cleanFiles)
	if !result.FilesEqual {
		result.Diffs = append(result.Diffs, parityDiff("files", composedFiles, cleanFiles)...)
	}

	composedSemantic := parityComposeFileRows(t, dirtyDB, generationID, masks,
		`SELECT file_path, line || '|' || name || '|' || type_name FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`)
	cleanSemantic := parityRows(t, cleanDB,
		`SELECT file_path, line || '|' || name || '|' || type_name FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`, 0)
	result.SemanticEqual = slices.Equal(composedSemantic, cleanSemantic)
	if !result.SemanticEqual {
		result.Diffs = append(result.Diffs, parityDiff("semantic_binding_types", composedSemantic, cleanSemantic)...)
	}

	composedConstants := parityComposeFileRows(t, dirtyDB, generationID, masks,
		`SELECT file_path, node_id || '|' || value FROM constant_values WHERE repo_prefix = ? AND view_gen = ?`)
	cleanConstants := parityRows(t, cleanDB,
		`SELECT file_path, node_id || '|' || value FROM constant_values WHERE repo_prefix = ? AND view_gen = ?`, 0)
	result.ConstantsEqual = slices.Equal(composedConstants, cleanConstants)
	if !result.ConstantsEqual {
		result.Diffs = append(result.Diffs, parityDiff("constant_values", composedConstants, cleanConstants)...)
	}

	composedFTS := parityComposeFTS(t, dirtyDB, generationID, masks)
	cleanFTS := parityFTSRows(t, cleanDB, 0)
	result.SymbolFTSRows = len(cleanFTS)
	result.SymbolFTSEqual = slices.Equal(composedFTS, cleanFTS)
	if !result.SymbolFTSEqual {
		result.Diffs = append(result.Diffs, parityDiff("symbol_fts", composedFTS, cleanFTS)...)
	}
	result.SearchProbesEqual = paritySearchProbes(composedFTS, cleanFTS)

	result.ComposedDigest = parityDigest(composedNodes, composedEdges, composedFiles, composedSemantic, composedConstants, composedFTS)
	result.CleanDigest = parityDigest(cleanNodes, cleanEdges, cleanFiles, cleanSemantic, cleanConstants, cleanFTS)
	if len(result.Diffs) > 20 {
		result.Diffs = append(result.Diffs[:20], fmt.Sprintf("... %d more", len(result.Diffs)-20))
	}
	if strict && !result.ok() {
		t.Errorf("%s: composed generation %d does not match the clean index: %+v", label, generationID, result)
	}
	return result
}

type parityMaskSet struct {
	files      map[string]struct{}
	tombstones map[string]struct{}
}

func parityOpenRaw(t *testing.T, store *store_sqlite.Store) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func parityMasks(t *testing.T, db *sql.DB, generationID int64) parityMaskSet {
	t.Helper()
	set := parityMaskSet{files: map[string]struct{}{}, tombstones: map[string]struct{}{}}
	// A context mask claims nothing (store_sqlite.OwnershipContext): the layer
	// below keeps answering for that file whole, so only replace and delete
	// masks hide base rows.
	for _, p := range parityColumn(t, db, `SELECT file_path FROM generation_file_masks WHERE view_gen = ? AND ownership_mode <> 'context'`, generationID) {
		set.files[p] = struct{}{}
	}
	for _, id := range parityColumn(t, db, `SELECT node_id FROM generation_node_tombstones WHERE view_gen = ?`, generationID) {
		set.tombstones[id] = struct{}{}
	}
	return set
}

func parityColumn(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return out
}

// parityRows reads (file_path, rendered) pairs and renders them sorted.
func parityRows(t *testing.T, db *sql.DB, query string, generationID int64) []string {
	t.Helper()
	rows, err := db.Query(query, builderRepoPrefix, generationID)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var file, rendered string
		if err := rows.Scan(&file, &rendered); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, file+"\t"+rendered)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	slices.Sort(out)
	return out
}

// parityComposeFileRows composes a file-keyed sidecar: the generation's own
// rows plus every base row at a path the generation does not mask.
func parityComposeFileRows(t *testing.T, db *sql.DB, generationID int64, masks parityMaskSet, query string) []string {
	t.Helper()
	out := parityRows(t, db, query, generationID)
	for _, row := range parityRows(t, db, query, 0) {
		file, _, _ := strings.Cut(row, "\t")
		if _, masked := masks.files[file]; masked {
			continue
		}
		out = append(out, row)
	}
	slices.Sort(out)
	return out
}

func parityFTSQuery() string {
	return `SELECT n.file_path, r.node_id, f.tokens
FROM symbol_fts_rowid AS r
JOIN symbol_fts AS f ON f.rowid = r.fts_rowid
LEFT JOIN nodes AS n ON n.id = r.node_id AND n.view_gen = r.view_gen
WHERE r.view_gen = ?`
}

// parityFTSRowsWithFile returns symbol FTS documents as (file, "id\ttokens").
func parityFTSRowsWithFile(t *testing.T, db *sql.DB, generationID int64) [][2]string {
	t.Helper()
	rows, err := db.Query(parityFTSQuery(), generationID)
	if err != nil {
		t.Fatalf("query symbol fts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var file sql.NullString
		var id, tokens string
		if err := rows.Scan(&file, &id, &tokens); err != nil {
			t.Fatalf("scan symbol fts: %v", err)
		}
		out = append(out, [2]string{file.String, id + "\t" + tokens})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows symbol fts: %v", err)
	}
	return out
}

func parityFTSRows(t *testing.T, db *sql.DB, generationID int64) []string {
	t.Helper()
	var out []string
	for _, row := range parityFTSRowsWithFile(t, db, generationID) {
		out = append(out, row[1])
	}
	slices.Sort(out)
	return out
}

// parityComposeFTS composes symbol FTS: the generation's documents plus every
// base document whose node lives at an unmasked path and is not tombstoned.
func parityComposeFTS(t *testing.T, db *sql.DB, generationID int64, masks parityMaskSet) []string {
	t.Helper()
	out := parityFTSRows(t, db, generationID)
	for _, row := range parityFTSRowsWithFile(t, db, 0) {
		if _, masked := masks.files[row[0]]; masked && row[0] != "" {
			continue
		}
		id, _, _ := strings.Cut(row[1], "\t")
		if _, gone := masks.tombstones[id]; gone {
			continue
		}
		out = append(out, row[1])
	}
	slices.Sort(out)
	return out
}

// paritySearchProbes answers one exact-token probe per indexed token on both
// sides and compares the matching node sets, so a stale or missing document
// for any term is a difference even when the row sets were compared already.
func paritySearchProbes(composed, clean []string) bool {
	index := func(docs []string) map[string][]string {
		byToken := map[string][]string{}
		for _, doc := range docs {
			id, tokens, _ := strings.Cut(doc, "\t")
			for _, tok := range strings.Fields(tokens) {
				byToken[tok] = append(byToken[tok], id)
			}
		}
		for tok := range byToken {
			slices.Sort(byToken[tok])
		}
		return byToken
	}
	a, b := index(composed), index(clean)
	if len(a) != len(b) {
		return false
	}
	for tok, ids := range a {
		if !slices.Equal(ids, b[tok]) {
			return false
		}
	}
	return true
}

func parityDigest(parts ...[]string) string {
	h := sha256.New()
	for _, part := range parts {
		for _, line := range part {
			h.Write([]byte(line))
			h.Write([]byte{'\n'})
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func parityDiff(what string, composed, clean []string) []string {
	var out []string
	inClean := map[string]struct{}{}
	for _, s := range clean {
		inClean[s] = struct{}{}
	}
	inComposed := map[string]struct{}{}
	for _, s := range composed {
		inComposed[s] = struct{}{}
		if _, ok := inClean[s]; !ok {
			out = append(out, what+" only composed: "+s)
		}
	}
	for _, s := range clean {
		if _, ok := inComposed[s]; !ok {
			out = append(out, what+" only clean: "+s)
		}
	}
	if len(out) == 0 && len(composed) != len(clean) {
		out = append(out, fmt.Sprintf("%s duplicate rows: composed %d, clean %d", what, len(composed), len(clean)))
	}
	return out
}

var _ graph.Reader = (*store_sqlite.Store)(nil)
