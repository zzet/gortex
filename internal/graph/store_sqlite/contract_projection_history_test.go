package store_sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// A real committed metadata update exercises the same existing gate/clock
// boundary as production mutation methods. This private helper supplies an
// explicit descriptor; it does not certify production writer coverage.
func commitProjectionTestValue(t *testing.T, s *Store, id, value string, domains []ContractProjectionDomain, commit bool) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	res, err := tx.Exec(`UPDATE nodes SET name=? WHERE id=? AND view_gen=?`, value, id, s.viewGen)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	if !commit {
		return
	}
	require.NoError(t, tx.Commit())
	s.finishAnalysisMutationLocked(n > 0, contractProjectionMutation{complete: true, domains: domains})
}

func TestContractProjectionHistoryDefaultsUnknownAndOwnsDomains(t *testing.T) {
	s := openPayloadStore(t)
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"})
	require.Nil(t, s.contractProjectionHistory, "ordinary graph tools allocate no contract history")
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	s.AddNode(&graph.Node{ID: "repo/b.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "repo/b.go"})
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "a writer without a complete descriptor is never excluded")
	require.Nil(t, delta.Domains)

	from := delta.After
	domains := []ContractProjectionDomain{{Kind: "node_id", Key: "repo/a.go::A"}, {Kind: "name", Scope: "repo", Key: "A"}, {Kind: "name", Scope: "repo", Key: "After"}}
	commitProjectionTestValue(t, s, "repo/a.go::A", "After", domains, true)
	domains[0].Key = "mutated-caller"
	delta, err = s.ContractProjectionChanges(t.Context(), from)
	require.NoError(t, err)
	require.True(t, delta.Complete)
	require.Equal(t, "repo/a.go::A", delta.Domains[0].Key)
	delta.Domains[0].Key = "mutated-result"
	again, err := s.ContractProjectionChanges(t.Context(), from)
	require.NoError(t, err)
	require.Equal(t, "repo/a.go::A", again.Domains[0].Key)
	require.Equal(t, "After", s.GetNode("repo/a.go::A").Name)
	charge, err := s.ContractProjectionHistoryBytes(t.Context())
	require.NoError(t, err)
	require.LessOrEqual(t, charge, int64(4096))
}

func TestContractProjectionHistoryCoversLateSemanticMutation(t *testing.T) {
	s := openPayloadStore(t)
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"})
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	token := s.BeginMutationReceipt()
	require.Equal(t, 1, s.PersistSemanticNodeStamps([]graph.SemanticNodeStamp{{NodeID: "repo/a.go::A", SemanticType: "func() string", SemanticSource: "test"}}))
	resolution := s.EndMutationReceipt(token)
	require.True(t, resolution.Complete)
	require.False(t, resolution.ResolutionRelevant, "resolution-neutral does not mean contract-neutral")
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "late metadata must not inherit receipt-only authority")
	require.NotEqual(t, before, delta.After)
}

func TestContractProjectionHistoryPhysicalScopeAndRollback(t *testing.T) {
	s := openPayloadStore(t)
	selected := s.AtGeneration(11)
	selected.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"})
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := selected.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	s.AddNode(&graph.Node{ID: "base", Name: "base", Kind: graph.KindFunction})
	delta, err := selected.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.True(t, delta.Complete, "base0 is not implicitly added to an exact physical capture")
	require.Empty(t, delta.Domains)

	commitProjectionTestValue(t, selected, "repo/a.go::A", "RolledBack", []ContractProjectionDomain{{Kind: "node_id", Key: "repo/a.go::A"}}, false)
	delta, err = selected.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.True(t, delta.Complete)
	require.Equal(t, before, delta.After)
	require.Equal(t, "A", selected.GetNode("repo/a.go::A").Name)

	wrongGeneration, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, wrongGeneration.Complete)
	other := openPayloadStore(t)
	require.NoError(t, other.EnableContractProjectionHistory(t.Context(), 4096))
	wrongStore, err := other.AtGeneration(11).ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, wrongStore.Complete)
}

func TestContractProjectionHistoryGapsCapsAndUnrecordedInputs(t *testing.T) {
	s := openPayloadStore(t)
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"})
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	before, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	for i := 0; i < 2*len(s.contractProjectionHistory.slots); i++ {
		commitProjectionTestValue(t, s, "repo/a.go::A", "Current", []ContractProjectionDomain{{Kind: "node_id", Key: "repo/a.go::A"}}, true)
	}
	delta, err := s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "evicted history is not absence of change")
	before = delta.After
	commitProjectionTestValue(t, s, "repo/a.go::A", "Large", []ContractProjectionDomain{{Kind: "name", Key: strings.Repeat("x", 8192)}}, true)
	delta, err = s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "oversize descriptor retains unknown without cloning it")
	charge, err := s.ContractProjectionHistoryBytes(t.Context())
	require.NoError(t, err)
	require.LessOrEqual(t, charge, int64(4096))

	before = delta.After
	require.NoError(t, s.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/a.go"}}))
	delta, err = s.ContractProjectionChanges(t.Context(), before)
	require.NoError(t, err)
	require.False(t, delta.Complete, "an input/mask/copy counter not in this journal requires full proof")
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	require.Error(t, s.EnableContractProjectionHistory(t.Context(), 8192), "another actor cannot silently grow retained memory")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.CaptureContractProjectionCursor(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestContractProjectionHistoryConcurrentCommitAndCapture(t *testing.T) {
	s := openPayloadStore(t)
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go"})
	require.NoError(t, s.EnableContractProjectionHistory(t.Context(), 4096))
	cursor, err := s.CaptureContractProjectionCursor(t.Context())
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			commitProjectionTestValue(t, s, "repo/a.go::A", "Current", []ContractProjectionDomain{{Kind: "node_id", Key: "repo/a.go::A"}}, true)
		}
	}()
	for {
		delta, err := s.ContractProjectionChanges(t.Context(), cursor)
		require.NoError(t, err)
		if delta.Complete && delta.After != cursor {
			require.NotEmpty(t, delta.Domains, "a committed revision cannot appear complete before its event")
			for _, domain := range delta.Domains {
				require.Equal(t, ContractProjectionDomain{Kind: "node_id", Key: "repo/a.go::A"}, domain)
			}
		}
		if !delta.Complete {
			require.Nil(t, delta.Domains, "reader lag can force fallback but never usable partial authority")
		}
		cursor = delta.After
		select {
		case <-done:
			return
		default:
		}
	}
}
