package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// discardCrashStore fails the discard of resume points and drops every node
// write after it, as a crash right after the failed delete would.
type discardCrashStore struct {
	*store_sqlite.Store
	crashed bool
}

func (s *discardCrashStore) ClearRepoEnrichmentResume(string) error {
	s.crashed = true
	return errors.New("injected: discard resume points failed")
}

func (s *discardCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.crashed {
		return
	}
	s.Store.AddBatch(nodes, edges)
}

// A crash right after a failed discard leaves no later write: the in-place
// re-enriched file must already be on the ledger, from its re-parse's fact
// write.
func TestCrashAfterFailedResumeDiscardKeepsFileOnLedger(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "")
	repo := commitGitRepo(t)
	store := &discardCrashStore{Store: openTestSqlite(t)}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	_, err := idx.Index(repo)
	require.NoError(t, err)
	require.NoError(t, store.SetEnrichmentResume(graph.EnrichmentResumePoint{
		RepoPrefix: "r", Provider: "spy", IndexedSHA: "abc", Phase: 1, After: "r/z.go",
	}))
	attachScopeSpyWith(idx, &scopeSpyProvider{}, semantic.Config{Enabled: true, EnrichOnWatch: true})

	main := filepath.Join(repo, "main.go")
	require.NoError(t, os.WriteFile(main, []byte("package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n"), 0o644))
	require.NoError(t, idx.IndexFile(main))
	require.True(t, store.crashed, "the in-place re-enrich must try to discard the points")

	restarted := New(store.Store, newTestRegistry(), config.Default().Index, zap.NewNop())
	restarted.SetRepoPrefix("r")
	restarted.SetRootPath(repo)
	assert.Equal(t, []string{"r/main.go"}, pendingLedger(t, restarted))
	assert.NotEmpty(t, reparseEpoch(t, store.Store, "r/main.go"))
}

// enrichCrashStore stops persisting once in-place enrichment has run and
// the indexer next writes a marker or discards the resume points, as a
// crash right after the enrichment wrote its facts would.
type enrichCrashStore struct {
	*store_sqlite.Store
	spy     *scopeSpyProvider
	crashed bool
}

func (s *enrichCrashStore) enriched() bool {
	s.spy.spyEnrichProvider.mu.Lock()
	defer s.spy.spyEnrichProvider.mu.Unlock()
	return len(s.spy.files) > 0
}

func (s *enrichCrashStore) ClearRepoEnrichmentResume(repoPrefix string) error {
	if s.crashed || s.enriched() {
		s.crashed = true
		return nil
	}
	return s.Store.ClearRepoEnrichmentResume(repoPrefix)
}

func (s *enrichCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if !s.crashed && len(edges) == 0 && s.enriched() {
		s.crashed = true
	}
	if s.crashed {
		return
	}
	s.Store.AddBatch(nodes, edges)
}

// A forced re-resolve rewrites the file's edges and enriches it in place
// with no re-parse to stamp the ledger. A crash right after the enrichment
// must still leave the file on the ledger, so a restarted pass does not
// resume a point recorded before.
func TestForcedReresolveCrashAfterEnrichKeepsFileOnLedger(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "")
	repo := commitGitRepo(t)
	spy := &scopeSpyProvider{}
	store := &enrichCrashStore{Store: openTestSqlite(t), spy: spy}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	_, err := idx.Index(repo)
	require.NoError(t, err)
	require.Empty(t, pendingLedger(t, idx))
	require.NoError(t, store.SetEnrichmentResume(graph.EnrichmentResumePoint{
		RepoPrefix: "r", Provider: "spy", IndexedSHA: "abc", Phase: 1, After: "r/z.go",
	}))
	attachScopeSpyWith(idx, spy, semantic.Config{Enabled: true, EnrichOnWatch: true})

	require.NoError(t, idx.reresolveFileScopedRaw(filepath.Join(repo, "main.go")))
	require.True(t, store.crashed, "the forced re-resolve must enrich in place")

	restarted := New(store.Store, newTestRegistry(), config.Default().Index, zap.NewNop())
	restarted.SetRepoPrefix("r")
	restarted.SetRootPath(repo)
	assert.Equal(t, []string{"r/main.go"}, pendingLedger(t, restarted))
}

// factWriteCrashStore drops the node write that re-adds path's file node
// and every node and mtime write after it, as a crash between a re-parse's
// eviction and its fact write would.
type factWriteCrashStore struct {
	*store_sqlite.Store
	path    string
	crashed bool
}

func (s *factWriteCrashStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.path != "" && !s.crashed {
		for _, n := range nodes {
			if n != nil && n.Kind == graph.KindFile && n.FilePath == s.path {
				s.crashed = true
				break
			}
		}
	}
	if s.crashed {
		return
	}
	s.Store.AddBatch(nodes, edges)
}

func (s *factWriteCrashStore) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if s.crashed {
		return nil
	}
	return s.Store.BulkSetFileMtimes(repoPrefix, mtimes)
}

func (s *factWriteCrashStore) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if s.crashed {
		return nil
	}
	return s.Store.ReplaceFileMtimes(repoPrefix, mtimes)
}

// warmCensus reports what a restarted process, holding only the persisted
// mtimes, would re-index.
func warmCensus(t *testing.T, store *store_sqlite.Store, repo string) []string {
	t.Helper()
	restarted := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	restarted.SetRepoPrefix("r")
	restarted.SetRootPath(repo)
	restarted.SetFileMtimes(store.LoadFileMtimes("r"))
	changed, _, err := restarted.ChangedSinceMtimes(repo)
	require.NoError(t, err)
	return changed
}

// A file re-derived with no write to it keeps the mtime its receipt
// records. A crash after its eviction, before its fact write, must leave
// the restart census a reason to re-derive it, on the batched path (the
// importer of a deleted file) and on the per-file fallback; a re-parse that
// completes keeps the receipt.
func TestUnchangedReparseCrashBeforeFactWriteIsRederivedOnRestart(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	for _, tc := range []struct {
		name    string
		reparse func(t *testing.T, idx *Indexer, repo string)
	}{
		{"batched importer of a deleted file", func(t *testing.T, idx *Indexer, repo string) {
			gone := filepath.Join(repo, "helper.go")
			require.NoError(t, os.Remove(gone))
			_, err := idx.IncrementalReindexPaths(repo, []string{gone})
			require.NoError(t, err)
		}},
		{"per-file fallback", func(t *testing.T, idx *Indexer, repo string) {
			require.NoError(t, idx.indexFile(filepath.Join(repo, "main.go"), true))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := func(t *testing.T) (*factWriteCrashStore, *Indexer, string) {
				repo := commitGitRepo(t)
				require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { helper() }\n"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(repo, "helper.go"), []byte("package main\n\nfunc helper() {}\n"), 0o644))
				store := &factWriteCrashStore{Store: openTestSqlite(t)}
				idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
				idx.SetRepoPrefix("r")
				idx.SetRootPath(repo)
				_, err := idx.Index(repo)
				require.NoError(t, err)
				attachScopeSpyWith(idx, &scopeSpyProvider{}, semantic.Config{Enabled: true})
				require.NotContains(t, warmCensus(t, store.Store, repo), "main.go")
				return store, idx, repo
			}

			store, idx, repo := setup(t)
			tc.reparse(t, idx, repo)
			assert.NotContains(t, warmCensus(t, store.Store, repo), "main.go", "a completed re-parse keeps the receipt")

			store, idx, repo = setup(t)
			store.path = "r/main.go"
			tc.reparse(t, idx, repo)
			require.True(t, store.crashed, "the fact write must be reached")
			assert.Contains(t, warmCensus(t, store.Store, repo), "main.go",
				"the restart must re-derive the file the crash left without its nodes")
		})
	}
}

// A file that grows past the size cap, or whose parse is skipped (here a
// binary payload), loses its facts to a synthetic node with no re-parse to
// put it on the ledger. The eviction must discard the repository's resume
// points first, and the synthetic node must carry a new re-parse epoch so a
// point a concurrent type pass records over the old ledger is refused after
// a restart. When the delete fails, the re-index fails before it writes
// anything and the file keeps its facts.
func TestSkipEvictionDiscardsResumePoints(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"size", "package main\n\n// " + strings.Repeat("x", 128) + "\nfunc main() {}\n"},
		{"binary", "package main\x00\nfunc main() {}\n"},
	} {
		for _, failDiscard := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failDiscard=%v", tc.name, failDiscard), func(t *testing.T) {
				repo := commitGitRepo(t)
				sqlite := openTestSqlite(t)
				var store graph.Store = sqlite
				if failDiscard {
					store = clearResumeFailStore{sqlite}
				}
				cfg := config.Default().Index
				cfg.MaxFileSize = 64
				idx := New(store, newTestRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix("r")
				idx.SetRootPath(repo)
				_, err := idx.Index(repo)
				require.NoError(t, err)
				require.NoError(t, sqlite.SetEnrichmentResume(graph.EnrichmentResumePoint{
					RepoPrefix: "r", Provider: "spy", IndexedSHA: "abc", Phase: 1, After: "r/z.go",
				}))
				attachScopeSpyWith(idx, &scopeSpyProvider{}, semantic.Config{Enabled: true})
				before := len(sqlite.GetFileNodes("r/main.go"))
				require.Greater(t, before, 1, "the file has facts to evict")
				epoch := reparseEpoch(t, sqlite, "r/main.go")

				main := filepath.Join(repo, "main.go")
				require.NoError(t, os.WriteFile(main, []byte(tc.content), 0o644))
				err = idx.IndexFile(main)
				_, found, getErr := sqlite.GetEnrichmentResume("r", "spy")
				require.NoError(t, getErr)
				if failDiscard {
					assert.Error(t, err, "a failed discard fails the re-index")
					assert.True(t, found)
					assert.Len(t, sqlite.GetFileNodes("r/main.go"), before, "the file keeps its facts")
					return
				}
				require.NoError(t, err)
				assert.False(t, found, "the skip eviction discards the points")
				assert.True(t, idx.resumeUntrusted.Load(), "the skip eviction distrusts resume in this process")
				assert.Len(t, sqlite.GetFileNodes("r/main.go"), 1, "only the skip node is left")
				got := reparseEpoch(t, sqlite, "r/main.go")
				assert.NotEmpty(t, got, "the skip node carries a re-parse epoch")
				assert.NotEqual(t, epoch, got, "the skip moves the file's re-parse epoch")
			})
		}
	}
}
