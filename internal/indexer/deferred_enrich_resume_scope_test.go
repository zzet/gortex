package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// scopeSpyProvider is a context-aware provider that records the resume
// scope the manager attached to its repo pass.
type scopeSpyProvider struct {
	spyEnrichProvider
	mu     sync.Mutex
	scopes []semantic.EnrichResumeScope
}

func (s *scopeSpyProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, _ semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	if scope, ok := semantic.EnrichResumeFrom(ctx); ok {
		s.mu.Lock()
		s.scopes = append(s.scopes, scope)
		s.mu.Unlock()
	}
	return s.EnrichRepo(g, repoPrefix, repoRoot)
}

func (s *scopeSpyProvider) recorded() []semantic.EnrichResumeScope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]semantic.EnrichResumeScope(nil), s.scopes...)
}

func newScopeSpyIndexer(t *testing.T) (*Indexer, *scopeSpyProvider, string) {
	t.Helper()
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "")
	repo := commitGitRepo(t)
	store := openTestSqlite(t)
	spy := &scopeSpyProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	return idx, spy, repo
}

func attachScopeSpy(idx *Indexer, spy *scopeSpyProvider) {
	attachScopeSpyWith(idx, spy, semantic.Config{Enabled: true})
}

func attachScopeSpyWith(idx *Indexer, spy *scopeSpyProvider, cfg semantic.Config) {
	mgr := semantic.NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(spy)
	idx.SetSemanticManager(mgr)
}

// A scoped re-parse leaves its files on the per-file ledger, so the forced
// pass may still resume and re-apply them; a whole-repo re-parse leaves
// nothing a resumed pass could re-apply, so it must start from the first
// page.
func TestRunDeferredEnrich_ResumeScopeFollowsReparseKind(t *testing.T) {
	t.Run("scoped re-parse", func(t *testing.T) {
		idx, spy, _ := newScopeSpyIndexer(t)
		attachScopeSpy(idx, spy)
		seedWatchIndexedFile(t, idx.graph, "r", "main.go")
		idx.reparsedThisRun.Store(true)
		idx.markPendingEnrichFull()
		idx.runDeferredEnrich()
		scopes := spy.recorded()
		require.Len(t, scopes, 1, "the whole-repo pass must carry a resume scope")
		assert.True(t, scopes[0].Record)
		assert.True(t, scopes[0].Resume)
	})
	t.Run("whole-repo re-parse", func(t *testing.T) {
		idx, spy, repo := newScopeSpyIndexer(t)
		_, err := idx.Index(repo)
		require.NoError(t, err)
		attachScopeSpy(idx, spy)
		idx.runDeferredEnrich()
		scopes := spy.recorded()
		require.Len(t, scopes, 1)
		assert.True(t, scopes[0].Record)
		assert.False(t, scopes[0].Resume)
	})
}

// The whole-repo re-parse distrusts only points recorded before it. A first
// pass that is cut records its own points over the re-parsed graph, so the
// next pass in the same run resumes them — while still being forced past
// the completion marker the re-parse invalidated.
func TestRunDeferredEnrich_RetryAfterWholeRepoReparseResumes(t *testing.T) {
	idx, spy, repo := newScopeSpyIndexer(t)
	_, err := idx.Index(repo)
	require.NoError(t, err)
	attachScopeSpy(idx, spy)
	sha, dirty := repoHeadAndDirty(repo)
	require.NotEmpty(t, sha)
	require.False(t, dirty)
	require.NoError(t, idx.graph.(graph.EnrichmentStateStore).SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "r", Provider: spy.Name(), IndexedSHA: sha,
	}))

	spy.spyEnrichProvider.mu.Lock()
	spy.partial = true
	spy.spyEnrichProvider.mu.Unlock()
	idx.runDeferredEnrich()
	spy.spyEnrichProvider.mu.Lock()
	spy.partial = false
	spy.spyEnrichProvider.mu.Unlock()
	idx.runDeferredEnrich()

	scopes := spy.recorded()
	require.Len(t, scopes, 2, "the retry must still run past the same-HEAD completion marker")
	assert.False(t, scopes[0].Resume, "the first pass after the re-parse starts from the first page")
	assert.True(t, scopes[1].Resume, "the retry resumes what the first pass recorded")
}

// A scoped re-parse that re-enriches its file in place clears the file's
// ledger marker, but the re-parse also evicted the edges its importers' type
// facts point into it, and in-place enrichment does not restore those. The
// next whole-repo pass therefore must not resume; a save that defers its
// enrichment leaves the file on the ledger, and that pass may.
func TestRunDeferredEnrich_InPlaceReenrichDistrustsResume(t *testing.T) {
	for _, tc := range []struct {
		name       string
		onWatch    bool
		wantResume bool
	}{
		{name: "save re-enriched in place", onWatch: true, wantResume: false},
		{name: "save left on the ledger", onWatch: false, wantResume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, spy, repo := newScopeSpyIndexer(t)
			_, err := idx.Index(repo)
			require.NoError(t, err)
			attachScopeSpyWith(idx, spy, semantic.Config{Enabled: true, EnrichOnWatch: tc.onWatch})
			// The first pass consumes the whole-repo re-parse's latch and is
			// cut, so a point would be left for the next pass to resume.
			spy.spyEnrichProvider.mu.Lock()
			spy.partial = true
			spy.spyEnrichProvider.mu.Unlock()
			idx.runDeferredEnrich()

			main := filepath.Join(repo, "main.go")
			require.NoError(t, os.WriteFile(main, []byte("package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n"), 0o644))
			require.NoError(t, idx.IndexFile(main))
			runGit(t, repo, "commit", "-q", "-am", "save")
			ledger := pendingLedger(t, idx)
			spy.spyEnrichProvider.mu.Lock()
			enrichedInPlace := append([]string(nil), spy.files...)
			spy.spyEnrichProvider.mu.Unlock()
			if tc.onWatch {
				require.Equal(t, []string{"r/main.go"}, enrichedInPlace, "the save re-enriched its file in place")
				require.Empty(t, ledger, "in-place enrichment cleared the file's marker")
			} else {
				require.Empty(t, enrichedInPlace)
				require.Equal(t, []string{"r/main.go"}, ledger, "the save deferred its enrichment to the ledger")
			}

			spy.spyEnrichProvider.mu.Lock()
			spy.partial = false
			spy.spyEnrichProvider.mu.Unlock()
			idx.runDeferredEnrich()
			scopes := spy.recorded()
			require.NotEmpty(t, scopes)
			assert.False(t, scopes[0].Resume, "the first pass after the whole-repo re-parse starts from the first page")
			last := scopes[len(scopes)-1]
			assert.True(t, last.Record)
			assert.Equal(t, tc.wantResume, last.Resume)
		})
	}
}
