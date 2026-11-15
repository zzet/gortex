package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression prepared from the R16 observed mtime_duplicate boundary. This
// component does not claim which MCP enqueue site produced that observation.
// The lifecycle and a real index pass establish acceptance; no accepted source
// fingerprint, receipt, or publication result is manufactured by the test.
func TestWatcherPointNoopPreservesAcceptedSourceWitness(t *testing.T) {
	for _, change := range []bool{false, true} {
		name := "unchanged_duplicate"
		if change {
			name = "real_physical_write"
		}
		t.Run(name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			defer f.close()
			leases := f.lc.ViewLeases()
			require.NotNil(t, leases)
			authority := NewOutputGenerationAuthority(leases)
			f.mi.SetOutputGenerationAuthority(authority)
			root := f.gitRepo("point-noop")
			tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, tracked.CatalogErr)
			results, err := f.mi.IndexAll()
			require.NoError(t, err)
			require.NotNil(t, results[tracked.Prefix])
			require.Empty(t, results[tracked.Prefix].FailedFiles)
			idx := f.mi.GetIndexer(tracked.Prefix)
			require.NotNil(t, idx)
			path := filepath.Join(root, "point-noop.go")
			// Explicit indexing uses actual unchanged fixture bytes and persists their
			// receipt, so duplicate proof does not rely on a timestamp assumption.
			require.NoError(t, idx.IndexFile(path))
			require.True(t, idx.fileMtimeMatches(path))
			pin := leases.AcquireBaseCorpus(tracked.Prefix)
			defer pin.Release()
			require.True(t, pin.OwnerPinned())
			require.True(t, pin.Witnessed())
			require.NoError(t, pin.ValidateAcceptedCurrent())
			before := authority.Stats()
			require.Equal(t, before.Issued, before.Settled)
			require.Zero(t, before.LiveOwners)
			originalBytes, err := os.ReadFile(path)
			require.NoError(t, err)
			originalInfo, err := os.Stat(path)
			require.NoError(t, err)
			originalHash := sha256.Sum256(originalBytes)
			graphRevisionBefore := f.store.MutationRevision()
			analysisRevisionBefore := f.store.AnalysisMutationRevision()
			watcher, err := NewWatcher(idx, config.Default().Watch, zap.NewNop())
			require.NoError(t, err)
			// This component exercises the real explicit queue/timer/point executor.
			// It does not start a native backend or claim native-event provenance.
			// Reuse the explicit point-publication component's no-start cleanup
			// primitive. No native backend is started or claimed by this component.
			watcher.degradedNoFsnotify = true
			stopped := false
			stop := func() {
				if !stopped {
					stopped = true
					require.NoError(t, watcher.Stop())
				}
			}
			defer stop()
			executorEntered := false
			if change {
				require.NoError(t, os.WriteFile(path, []byte("package a\nfunc Changed() {}\n"), 0600))
				require.False(t, idx.fileMtimeMatches(path))
			} else {
				// The actual persisted duplicate gate must return before any executor;
				// an unexpected entry is a precondition failure, not the expected RED.
				watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
					executorEntered = true
					return nil, errors.New("unchanged duplicate entered executor")
				}
			}
			ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
			require.NoError(t, err)
			require.NotNil(t, ticket)
			var actual MutationResult
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			select {
			case result, ok := <-ticket.Done:
				require.True(t, ok, "actual ticket closed without a result")
				actual = result
			case <-ctx.Done():
				t.Fatal("actual point mutation did not join:", ctx.Err())
			}
			require.NoError(t, actual.Err)
			require.GreaterOrEqual(t, actual.AppliedGeneration, ticket.Generation)
			// Done can precede callback return. Stop closes further admission and
			// joins asyncWork/stormWork, so the original output/source Complete and
			// every callback defer have actually returned before these observations.
			stop()
			after := authority.Stats()
			require.Equal(t, before.Issued+1, after.Issued)
			require.Equal(t, before.Settled+1, after.Settled)
			require.Equal(t, after.Issued, after.Settled)
			require.Zero(t, after.LiveOwners)
			require.Equal(t, before.Witnessed+1, after.Witnessed)
			require.Equal(t, before.Entries[OutputEntryWatcherPatchGraph]+1, after.Entries[OutputEntryWatcherPatchGraph])
			finalBytes, err := os.ReadFile(path)
			require.NoError(t, err)
			finalInfo, err := os.Stat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(originalInfo, finalInfo))
			finalHash := sha256.Sum256(finalBytes)
			graphRevisionAfter := f.store.MutationRevision()
			analysisRevisionAfter := f.store.AnalysisMutationRevision()
			if change {
				require.NotEqual(t, originalBytes, finalBytes)
				require.NotEqual(t, originalHash, finalHash)
				require.Greater(t, graphRevisionAfter, graphRevisionBefore)
				require.Greater(t, analysisRevisionAfter, analysisRevisionBefore)
				require.NotNil(t, f.store.GetNode(tracked.Prefix+"/point-noop.go::Changed"))
				t.Logf("ACTUAL_POINT_PREREQUISITES case=real_physical_write original_pin_accepted=true physical_changed=true graph_changed=true callback_joined=true output_settled=true issued_delta=1 settled_delta=1 witnessed_delta=1")
				require.ErrorIs(t, pin.ValidateAcceptedCurrent(), graphview.ErrBaseCorpusChanged)
				t.Logf("ACTUAL_POINT_WITNESS physical_write=true generation=%d witnessed_delta=1 pin_changed=true", ticket.Generation)
			} else {
				require.False(t, executorEntered)
				require.True(t, idx.fileMtimeMatches(path))
				require.Equal(t, originalBytes, finalBytes)
				require.Equal(t, originalHash, finalHash)
				require.Equal(t, originalInfo.Size(), finalInfo.Size())
				require.Equal(t, originalInfo.ModTime(), finalInfo.ModTime())
				require.Equal(t, graphRevisionBefore, graphRevisionAfter)
				require.Equal(t, analysisRevisionBefore, analysisRevisionAfter)
				t.Logf("ACTUAL_POINT_PREREQUISITES case=unchanged_duplicate original_pin_accepted=true same_file=true bytes_equal=true sha256_equal=true graph_revision_equal=true analysis_revision_equal=true duplicate_receipt=true executor_entered=false callback_joined=true output_settled=true issued_delta=1 settled_delta=1 witnessed_delta=1")
				actualPinErr := pin.ValidateAcceptedCurrent()
				t.Logf("ACTUAL_POINT_WITNESS physical_write=false unchanged_receipt=true executor_entered=false generation=%d witnessed_delta=1 unchanged_delta=%d original_pin_error=%v", ticket.Generation, after.Unchanged-before.Unchanged, actualPinErr)
				require.NoError(t, actualPinErr, "a verified no-op point patch must preserve an accepted original source witness")
			}
		})
	}
}
