package indexer

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

type restageFixtureExtractor struct {
	parser.Extractor
	fail bool
	name string
}

func (e *restageFixtureExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == "failed.go" {
		if e.fail {
			return nil, errors.New("synthetic permanent parse failure")
		}
		src = []byte(strings.ReplaceAll(string(src), "Keep", e.name))
	}
	return e.Extractor.Extract(path, src)
}

func TestExtractorRestageParseFailureRecoveryReplacesOldExtraction(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"good.go", "also_good.go", "failed.go"} {
		writeFile(t, filepath.Join(root, name), "package fixture\nfunc Keep() {}\n")
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fixture.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	extractor := &restageFixtureExtractor{Extractor: languages.NewGoExtractor(), name: "OldExtraction"}
	reg := parser.NewRegistry()
	reg.Register(extractor)
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NotNil(t, store.GetNode("fixture/failed.go::OldExtraction"))
	baselineStats := store.RepoStats()
	baselineNodes, baselineEdges := len(store.AllNodes()), len(store.AllEdges())
	assertCounts := func() {
		t.Helper()
		require.Len(t, store.AllNodes(), baselineNodes, "reconciliation must not inflate or discard nodes")
		require.Len(t, store.AllEdges(), baselineEdges, "reconciliation must not inflate or discard edges")
		require.Equal(t, baselineStats, store.RepoStats(), "authoritative repository aggregates must remain stable")
	}
	state, found, err := store.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.True(t, found)
	state.ExtractorVersions = snapshotJSON(t, map[string]int{"go": extractorVersionForLang("go") - 1})
	require.NoError(t, store.SetRepoIndexState(state))
	extractor.fail, extractor.name = true, "NewExtraction"
	for pass, want := range []int{3, 1, 1} {
		result, err := idx.IncrementalReindexPaths(root, nil)
		require.NoError(t, err)
		require.Equal(t, want, result.StaleFileCount, "pass %d", pass)
		require.Len(t, result.FailedFiles, 1)
		require.NotNil(t, store.GetNode("fixture/failed.go::OldExtraction"))
		assertCounts()
	}
	extractor.fail = false
	result, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	require.Empty(t, result.FailedFiles)
	require.Empty(t, LoadFileIndexFailures(store, "fixture"))
	require.Nil(t, store.GetNode("fixture/failed.go::OldExtraction"))
	require.NotNil(t, store.GetNode("fixture/failed.go::NewExtraction"))
	assertCounts()
}

type restageFailureLedgerStore struct {
	*graph.Graph
	readErr, writeErr error
	state             graph.RepoIndexState
	found             bool
}

func (s *restageFailureLedgerStore) GetRepoIndexState(string) (graph.RepoIndexState, bool, error) {
	return s.state, s.found, nil
}

func (s *restageFailureLedgerStore) SetRepoIndexState(state graph.RepoIndexState) error {
	s.state, s.found = state, true
	return nil
}

func (s *restageFailureLedgerStore) FileIndexFailuresForRepo(repo string) ([]graph.FileIndexFailure, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.Graph.FileIndexFailuresForRepo(repo)
}

func (s *restageFailureLedgerStore) ReplaceFileIndexFailures(repo string, rows []graph.FileIndexFailure) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.Graph.ReplaceFileIndexFailures(repo, rows)
}

func TestExtractorRestageRequiresPersistedFailureCoverage(t *testing.T) {
	for _, mode := range []string{"persisted", "missing", "read_error", "write_error", "no_capability"} {
		t.Run(mode, func(t *testing.T) {
			store := &restageFailureLedgerStore{Graph: graph.New()}
			var g graph.Store = store
			if mode == "no_capability" {
				g = struct{ graph.Store }{store}
			}
			idx := newContentSourceIndexer(t, g)
			idx.SetRootPath(t.TempDir())
			idx.SetRepoPrefix("fixture")
			path := filepath.Join(idx.rootPath, "failed.go")
			if mode != "missing" {
				idx.noteFileIndexFailure(path, errors.New("synthetic parse failure"))
			}
			if mode == "read_error" {
				store.readErr = errors.New("ledger read unavailable")
			}
			if mode == "write_error" {
				store.writeErr = errors.New("ledger write unavailable")
			}
			require.Equal(t, mode == "persisted", idx.failedFilesSafelyRetryable([]string{path}))
		})
	}
}

func TestExtractorRestageUnavailableLedgerKeepsVersionsStale(t *testing.T) {
	for _, mode := range []string{"read_error", "write_error", "no_capability"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"good.go", "failed.go"} {
				writeFile(t, filepath.Join(root, name), "package fixture\nfunc Keep() {}\n")
			}
			store := &restageFailureLedgerStore{Graph: graph.New()}
			var g graph.Store = store
			if mode == "no_capability" {
				g = &indexStateStore{Store: store}
			}
			idx := newContentSourceIndexer(t, g)
			idx.SetRepoPrefix("fixture")
			_, err := idx.Index(root)
			require.NoError(t, err)
			reader := g.(graph.RepoIndexStateReader)
			writer := g.(graph.RepoIndexStateWriter)
			state, found, err := reader.GetRepoIndexState("fixture")
			require.NoError(t, err)
			require.True(t, found)
			state.ExtractorVersions = snapshotJSON(t, map[string]int{"go": extractorVersionForLang("go") - 1})
			require.NoError(t, writer.SetRepoIndexState(state))
			fs, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = fs.Close() })
			idx.SetContentSource(&failingIndexSource{ContentSource: fs, opens: map[string]int{}, errs: map[string][]error{"failed.go": {syscall.EACCES}}})
			switch mode {
			case "read_error":
				store.readErr = errors.New("ledger unavailable")
			case "write_error":
				store.writeErr = errors.New("ledger unavailable")
			}
			for range 2 {
				result, err := idx.IncrementalReindexPaths(root, nil)
				require.NoError(t, err)
				require.Len(t, result.FailedFiles, 1)
				require.Equal(t, 2, result.StaleFileCount)
				stored, _, err := reader.GetRepoIndexState("fixture")
				require.NoError(t, err)
				require.Equal(t, state.ExtractorVersions, stored.ExtractorVersions)
			}
		})
	}
}

func TestExtractorRestageLedgeredFailureDoesNotRestageSuccessfulFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"good.go", "also_good.go", "failed.go"} {
		writeFile(t, filepath.Join(root, name), "package fixture\nfunc Keep() {}\n")
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fixture.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	idx := newContentSourceIndexer(t, store)
	idx.SetRepoPrefix("fixture")
	_, err = idx.Index(root)
	require.NoError(t, err)
	state, found, err := store.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.True(t, found)
	state.ExtractorVersions = snapshotJSON(t, map[string]int{"go": extractorVersionForLang("go") - 1})
	require.NoError(t, store.SetRepoIndexState(state))
	fs, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })
	failing := &failingIndexSource{ContentSource: fs, opens: map[string]int{}, errs: map[string][]error{
		"failed.go": {syscall.EACCES},
	}}
	idx.SetContentSource(failing)
	first, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 3, first.StaleFileCount)
	require.Len(t, first.FailedFiles, 1)
	require.Len(t, LoadFileIndexFailures(store, "fixture"), 1)
	second, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, second.StaleFileCount, "only the ledgered failure must retry")
	state, _, err = store.GetRepoIndexState("fixture")
	require.NoError(t, err)
	require.Empty(t, ExtractorVersionStaleLangs(state.ExtractorVersions))
	idx.SetContentSource(fs)
	recovered, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, recovered.StaleFileCount)
	require.Empty(t, recovered.FailedFiles)
	require.Empty(t, LoadFileIndexFailures(store, "fixture"))
	clean, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Zero(t, clean.StaleFileCount)
}

// R6 (PR #432 review): the extractor version is mixed into the Merkle
// leaf salt, so under Merkle change detection a bump restages the
// language's files on its own. The mtime ledger knows nothing of
// versions — a non-Merkle store upgraded to a binary with a bumped
// extractor kept serving the OLD extraction of mtime-unchanged files
// forever. A full-tree pass must count those files stale and, once the
// restage lands, re-stamp the stored versions so it happens only once.

// indexStateStore wraps the in-memory graph with a durable-index-state
// capability so the version comparison has a stored row to read.
type indexStateStore struct {
	graph.Store
	st    graph.RepoIndexState
	found bool
}

func (s *indexStateStore) GetRepoIndexState(string) (graph.RepoIndexState, bool, error) {
	return s.st, s.found, nil
}

func (s *indexStateStore) SetRepoIndexState(st graph.RepoIndexState) error {
	s.st, s.found = st, true
	return nil
}

func TestIncrementalReindex_NonMerkleExtractorBumpRestagesLanguage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)

	store := &indexStateStore{Store: graph.New()}
	reg := parser.NewRegistry()
	reg.Register(languages.NewCSharpExtractor())
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	// Simulate the upgrade: the stored row predates the current csharp
	// extractor version. File content and mtimes are untouched.
	stale, _ := json.Marshal(map[string]int{
		postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
		"csharp":                        extractorVersionForLang("csharp") - 1,
	})
	store.st = graph.RepoIndexState{ExtractorVersions: string(stale)}
	store.found = true

	res, err := idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.StaleFileCount, 1,
		"a version-stale language's files must restage on a full-tree pass even with unchanged mtimes")

	var restamped map[string]int
	require.NoError(t, json.Unmarshal([]byte(store.st.ExtractorVersions), &restamped))
	assert.Equal(t, extractorVersionForLang("csharp"), restamped["csharp"],
		"a clean restage must re-stamp the stored versions so it does not repeat forever")

	res, err = idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, res.StaleFileCount,
		"after the re-stamp an unchanged tree must be a no-op again")
}

// TestIncrementalReindex_NonMerkleVersionCurrentNoRestage: a stored row
// at the current versions must not trigger any restage.
func TestIncrementalReindex_NonMerkleVersionCurrentNoRestage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)

	store := &indexStateStore{Store: graph.New()}
	reg := parser.NewRegistry()
	reg.Register(languages.NewCSharpExtractor())
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	current, _ := json.Marshal(map[string]int{
		postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
		"csharp":                        extractorVersionForLang("csharp"),
	})
	store.st = graph.RepoIndexState{ExtractorVersions: string(current)}
	store.found = true

	res, err := idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, res.StaleFileCount,
		"current extractor versions with unchanged mtimes must stay a no-op")
	if !strings.Contains(store.st.ExtractorVersions, "csharp") {
		t.Fatalf("stored row lost: %q", store.st.ExtractorVersions)
	}
}

func TestIncrementalReindex_NonMerkleAdmissionPolicyUpgradeRestoresPersistedORMFacts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "model.go"), "package model\n\ntype User struct {\n\tID int `gorm:\"primaryKey\"`\n}\n")

	dbPath := filepath.Join(t.TempDir(), "warm.sqlite")
	store1, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	store1Closed := false
	t.Cleanup(func() {
		if !store1Closed {
			_ = store1.Close()
		}
	})

	const repoPrefix = "warm"
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(store1, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRepoPrefix(repoPrefix)
	idx.SetRootPath(root)
	_, err = idx.IndexCtx(testCtx(), root)
	require.NoError(t, err)

	graphPath := idx.prefixPath("model.go")
	nodes, edges := store1.GetFileSubGraph(graphPath)
	hasTable, hasModelEdge := persistedORMFacts(nodes, edges)
	require.True(t, hasTable, "fixture must initially emit an ORM table")
	require.True(t, hasModelEdge, "fixture must initially emit models_table")

	// Recreate the pre-policy persisted graph: the source file and its mtime
	// stay current, but the old strip pass removed the ORM table and its edge.
	strippedIDs := make(map[string]struct{})
	keptNodes := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		dialect, _ := node.Meta["dialect"].(string)
		if node.Kind == graph.KindTable && dialect == "orm" {
			strippedIDs[node.ID] = struct{}{}
			continue
		}
		if node.Kind == graph.KindFile && node.Meta != nil {
			// The pre-policy index predates the whole-index fingerprints
			// too: its file node carries none, so the restage cannot prove
			// the file inert against the new policy's extraction.
			copied := *node
			copied.Meta = make(map[string]any, len(node.Meta))
			for key, value := range node.Meta {
				if !isFingerprintMeta(key) {
					copied.Meta[key] = value
				}
			}
			node = &copied
		}
		keptNodes = append(keptNodes, node)
	}
	require.NotEmpty(t, strippedIDs)
	keptEdges := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		_, fromStripped := strippedIDs[edge.From]
		_, toStripped := strippedIDs[edge.To]
		if fromStripped || toStripped {
			continue
		}
		keptEdges = append(keptEdges, edge)
	}
	store1.EvictFile(graphPath)
	store1.AddBatch(keptNodes, keptEdges)
	hasTable, hasModelEdge = persistedORMFacts(store1.GetFileSubGraph(graphPath))
	require.False(t, hasTable)
	require.False(t, hasModelEdge)

	state, found, err := store1.GetRepoIndexState(repoPrefix)
	require.NoError(t, err)
	require.True(t, found)
	legacyVersions := extractorVersionsSnapshot()
	delete(legacyVersions, postExtractionPolicySnapshotKey)
	encoded, err := json.Marshal(legacyVersions)
	require.NoError(t, err)
	state.ExtractorVersions = string(encoded)
	require.NoError(t, store1.SetRepoIndexState(state))
	require.NoError(t, store1.Close())
	store1Closed = true

	store2, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	reg = parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx = New(store2, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRepoPrefix(repoPrefix)
	idx.SetRootPath(root)
	persistedMtimes := store2.LoadFileMtimes(repoPrefix)
	require.NotEmpty(t, persistedMtimes)
	idx.SetFileMtimes(persistedMtimes)

	result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.StaleFileCount, 1,
		"the missing global policy epoch must restage unchanged source")
	hasTable, hasModelEdge = persistedORMFacts(store2.GetFileSubGraph(graphPath))
	require.True(t, hasTable, "upgrade restage must restore the ORM table")
	require.True(t, hasModelEdge, "upgrade restage must restore models_table")

	state, found, err = store2.GetRepoIndexState(repoPrefix)
	require.NoError(t, err)
	require.True(t, found)
	var restamped map[string]int
	require.NoError(t, json.Unmarshal([]byte(state.ExtractorVersions), &restamped))
	require.Equal(t, postExtractionPolicyVersion, restamped[postExtractionPolicySnapshotKey])

	result, err = idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount,
		"the persisted current epoch must make the second reconcile a no-op")
}

func persistedORMFacts(nodes []*graph.Node, edges []*graph.Edge) (hasTable, hasModelEdge bool) {
	tableIDs := make(map[string]struct{})
	for _, node := range nodes {
		if node == nil || node.Kind != graph.KindTable {
			continue
		}
		dialect, _ := node.Meta["dialect"].(string)
		if dialect == "orm" {
			tableIDs[node.ID] = struct{}{}
			hasTable = true
		}
	}
	for _, edge := range edges {
		if edge == nil || edge.Kind != graph.EdgeModelsTable {
			continue
		}
		if _, ok := tableIDs[edge.To]; ok {
			hasModelEdge = true
		}
	}
	return hasTable, hasModelEdge
}

// TestIncrementalReindex_NonMerkleNewlyTrackedLanguageRestages is the Julia
// shape: `.jl` entered extractorSaltExtLang in the same change that set
// julia@2, so a repository indexed by the PREVIOUS release carries a snapshot
// with no julia key at all. Comparing only the keys that snapshot happens to
// hold leaves every such repository serving the regex-era Julia graph forever
// — no content change re-triggers extraction. Merkle mode catches this through
// the changed leaf salt; this is the default non-Merkle path.
func TestIncrementalReindex_NonMerkleNewlyTrackedLanguageRestages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "geom.jl"), "module Geom\nradius(c) = c.r\nend\n")

	store := &indexStateStore{Store: graph.New()}
	reg := parser.NewRegistry()
	reg.Register(languages.NewJuliaExtractor())
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	// The previous release's snapshot: every language it tracked, at the
	// versions it shipped — but no julia key, because it had no .jl mapping.
	previous := extractorVersionsSnapshot()
	delete(previous, "julia")
	encoded, err := json.Marshal(previous)
	require.NoError(t, err)
	store.st = graph.RepoIndexState{ExtractorVersions: string(encoded)}
	store.found = true

	res, err := idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	// Exactly one: the repository holds a single .jl file and nothing
	// else, so a count above one would mean the restage stopped being
	// per-language — the whole point of the design.
	assert.Equal(t, 1, res.StaleFileCount,
		"a language whose first tracked version postdates the stored snapshot must restage")

	var restamped map[string]int
	require.NoError(t, json.Unmarshal([]byte(store.st.ExtractorVersions), &restamped))
	assert.Equal(t, extractorVersionForLang("julia"), restamped["julia"],
		"the clean restage must record julia so it does not repeat forever")

	res, err = idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, res.StaleFileCount,
		"after the re-stamp an unchanged tree must be a no-op again")
}
