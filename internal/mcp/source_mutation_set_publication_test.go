package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

type setPublicationBoundaryWatcher struct {
	canonicalPublicationWatcher
	mode string
	sets int
}

func (w *setPublicationBoundaryWatcher) EnqueueFileMutations(ctx context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.sets++
	switch w.mode {
	case "cancelled":
		return nil, context.Canceled
	case "stopped":
		return nil, errors.New("watcher stopped")
	case "incomplete":
		return map[string]*indexer.MutationTicket{paths[0]: {Path: paths[0], Generation: 1, Done: make(chan indexer.MutationResult)}}, nil
	default:
		return nil, nil // explicit unsupported/no admission, not a partial failure
	}
}

func TestMutationSetAdmissionFailureSettlesOnlySelectedRecords(t *testing.T) {
	for _, mode := range []string{"cancelled", "stopped", "incomplete", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s := newMutationServer()
			w := &setPublicationBoundaryWatcher{mode: mode, canonicalPublicationWatcher: canonicalPublicationWatcher{hold: true}}
			s.SetWatcher(w)
			t.Cleanup(func() {
				for i, done := range w.results {
					done <- indexer.MutationResult{RequestedGeneration: uint64(i + 1), AppliedGeneration: uint64(i + 1), Reindexed: true}
					close(done)
				}
				s.DrainBackground()
			})
			ctx := canonicalTimingContext()
			root := t.TempDir()
			a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
			first := commitTimingFixture(t, s, ctx, a, "package p\nfunc A(){}\n")
			second := commitTimingFixture(t, s, ctx, b, "package p\nfunc B(){}\n")
			admissions := s.admitSourceMutationFrontier(ctx, []sourceMutationTarget{captureSourceMutationTarget(ctx, a), captureSourceMutationTarget(ctx, b)})
			require.Equal(t, 1, w.sets)
			for _, record := range []*mutationCommitRecord{first, second} {
				publication := s.openCommittedPublicationRecord(ctx, record)
				require.NotNil(t, publication)
				if mode == "unsupported" {
					require.False(t, publication.Snapshot().Terminal, "nil/noerror fallback must not pre-fail the original owner")
					require.Contains(t, w.records, publication, "point fallback must reuse the same existing record")
				} else {
					require.True(t, publication.Snapshot().Terminal)
					require.Contains(t, publicationOffsets(publication.Snapshot()), indexer.PublicationTicketFailed)
				}
			}
			if mode == "unsupported" {
				require.Len(t, w.records, 2)
				for _, a := range admissions {
					require.NotNil(t, a.receipt)
				}
			} else {
				require.Empty(t, w.records, "no point fallback after error/incomplete admission")
				for _, a := range admissions {
					require.Error(t, a.immediate.Err)
				}
			}
		})
	}
}
