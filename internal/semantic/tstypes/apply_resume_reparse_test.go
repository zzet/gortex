package tstypes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

func resumeIndexer(store *store_sqlite.Store) *indexer.Indexer {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return indexer.New(store, reg, config.Default().Index, zap.NewNop())
}

// indexedResumeStore indexes files into a new store the way a track does,
// resolver included, and closes it.
func indexedResumeStore(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	}
	dbPath := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	_, err = resumeIndexer(store).Index(root)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	return dbPath, root
}

// reparseThroughIndexer saves content to one file and re-indexes it the
// way a save does: incoming references are parked on stubs, the file is
// evicted and re-extracted, the parked references are re-resolved, and —
// with enrichment deferred — the file goes on the ledger.
func reparseThroughIndexer(t *testing.T, dbPath, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	idx := resumeIndexer(store)
	idx.SetRootPath(root)
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer mgr.Close()
	for _, p := range DefaultProviders(zap.NewNop()) {
		mgr.RegisterProvider(p)
	}
	idx.SetSemanticManager(mgr)
	_, err = idx.IncrementalReindexPaths(root, []string{abs})
	require.NoError(t, err)
}

// Saves through the indexer's real re-parse path between cut runs: the
// re-parsed file's importers hold calls and an extends the type engine
// bound into it, and each save changes the targets' signatures so the
// indexer parks those references and re-resolves them instead of carrying
// them over. Resumed runs, re-applying the file and its importers behind
// the cursor, must reach what an uncut pass reaches over the same history.
func TestStreamedApplyResumeMatchesUncutPassAcrossSaves(t *testing.T) {
	files := resumeFixture(100)
	const reparsed = "src/a.ts"
	// The file declares run and done twice, so a reference parked on its
	// bare name is ambiguous to the structural resolver; only the receiver
	// type binds it. Each save changes the targets' signatures, so the
	// indexer parks the references instead of carrying them over.
	saves := [2]string{`export class Svc { run(): void {} }
export class Base { done(): void {} }
export class Other { run(): void {} done(): void {} }
`, `export class Svc { run(n?: number): void {} }
export class Base { done(n?: number): void {} }
export class Other { run(): void {} done(): void {} }
`}
	files[reparsed] = saves[0]
	files["src/b.ts"] = `import { Svc } from "./a";
export function callB(s: Svc): void { s.run(); }
`
	files["src/c.ts"] = `import { Base } from "./a";
export class C extends Base { finish(): void { this.done(); } }
`
	clean := semantic.RepoEnrichState{SHA: resumeSHA}

	dbPath, root := indexedResumeStore(t, files)
	const reappliedPhases = 3 // supers, metas, calls; all three files share a page
	const cutAfter = reappliedPhases + 1
	var got *semantic.EnrichResult
	runs, rev := 0, 0
	for runs < 64 {
		runs++
		got, _ = resumeRun(t, dbPath, root, clean, cutAfter)
		if !got.Partial {
			break
		}
		rev++
		reparseThroughIndexer(t, dbPath, root, reparsed, saves[rev%2])
		require.Equal(t, []string{reparsed}, reparsePendingFiles(t, dbPath), "the re-parse must leave the file on the ledger")
	}
	require.False(t, got.Partial, "still partial after %d runs", runs)
	require.Greater(t, runs, reappliedPhases, "the cursor must pass the importers in several phases")

	// The oracle: the same history — enriched, the same saves — then one
	// pass from the first page.
	wantDB, wantRoot := indexedResumeStore(t, files)
	first, _ := resumeRun(t, wantDB, wantRoot, clean, 0)
	require.False(t, first.Partial)
	for r := 1; r <= rev; r++ {
		reparseThroughIndexer(t, wantDB, wantRoot, reparsed, saves[r%2])
	}
	want, _ := resumeRun(t, wantDB, wantRoot, semantic.RepoEnrichState{SHA: resumeSHA, Force: true}, 0)
	require.False(t, want.Partial)
	assert.Equal(t, resumeSnapshot(t, wantDB), resumeSnapshot(t, dbPath), "resumed runs must reach the uncut graph")
}
