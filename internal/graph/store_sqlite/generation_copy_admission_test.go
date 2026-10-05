package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGenerationCopyCancellationWhileWriterHeld(t *testing.T) {
	for _, api := range []string{"whole", "flatten"} {
		for _, cancellation := range []string{"cancel", "deadline"} {
			t.Run(api+"/"+cancellation, func(t *testing.T) {
				s := openCatalogStore(t)
				chain := foldChain(t, s, 0)
				copyPayload := func(ctx context.Context, target int64) (GenerationCopyCounts, error) {
					if api == "whole" {
						return s.CopyGenerationPayloadWhole(ctx, chain[len(chain)-1], target)
					}
					return s.FlattenGenerationChain(ctx, chain, target)
				}
				reference := reservedGeneration(t, s, "reference")
				want, err := copyPayload(t.Context(), reference)
				require.NoError(t, err)
				target := reservedGeneration(t, s, "cancelled-admission")
				ctx, cancel := context.WithCancel(t.Context())
				if cancellation == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
				}
				defer cancel()
				s.writeMu.Lock()
				held, joined := true, false
				done := make(chan error, 1)
				go func() {
					_, copyErr := copyPayload(ctx, target)
					done <- copyErr
				}()
				// The pre-change control must fail the await assertion, then
				// release its writer and join the blocked copier before Close.
				defer func() {
					cancel()
					if held {
						s.writeMu.Unlock()
					}
					if !joined {
						<-done
					}
				}()
				require.Eventually(t, func() bool { return s.writeMu.waiting() > 0 }, time.Second, time.Millisecond)
				if cancellation == "cancel" {
					cancel()
				}
				select {
				case err = <-done:
					joined = true
				case <-time.After(time.Second):
					t.Fatal("copy must return on cancellation while the writer remains held")
				}
				if cancellation == "cancel" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
				// An independent read remains possible while the test owns the
				// writer; no destination payload may have been started.
				tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				empty, emptyErr := generationPayloadEmptyTx(t.Context(), tx, target)
				rollbackErr := tx.Rollback()
				require.NoError(t, emptyErr)
				require.NoError(t, rollbackErr)
				require.True(t, empty, "every destination payload family must remain empty")
				s.writeMu.Unlock()
				held = false
				got, err := copyPayload(t.Context(), target)
				require.NoError(t, err)
				require.Equal(t, want.Nodes, got.Nodes)
				require.Equal(t, want.Edges, got.Edges)
				require.Equal(t, want.Rows, got.Rows)
				require.Equal(t, renderGenerationNodes(t, s, reference), renderGenerationNodes(t, s, target))
				require.Equal(t, renderGenerationEdges(t, s, reference), renderGenerationEdges(t, s, target))
				require.Equal(t, renderFoldMasks(t, s, reference), renderFoldMasks(t, s, target))
				require.Equal(t, renderFoldFTS(t, s, reference), renderFoldFTS(t, s, target))
			})
		}
	}
}
