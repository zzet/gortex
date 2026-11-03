package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/pathkey"
)

func TestPendingSearchCannotClearAnotherRequestsObservedSourceEvent(t *testing.T) {
	for _, facade := range []bool{false, true} {
		mode := "legacy"
		if facade {
			mode = "facade"
		}
		for _, event := range []bool{false, true} {
			name := mode + "/unchanged owner"
			if event {
				name = mode + "/observed same bytes event"
			}
			t.Run(name, func(t *testing.T) {
				previous := pendingFileProjections
				pendingFileProjections = newPendingProjections()
				t.Cleanup(func() { pendingFileProjections = previous })
				server, root := ackModeServer(t, "local")
				searchProjectedAck(t, server, root, "ObservedSearchOwner")
				path := filepath.Join(root, "main.go")
				old := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
				require.NotNil(t, old)
				require.Positive(t, old.commitSeq, "start with the actual physical ACK owner")
				ctx := t.Context()
				if facade {
					server.NoteSessionToolPolicy("search-source-event", FacadeSurfaceVersion, "full")
					ctx = WithSessionID(ctx, "search-source-event")
				}
				pinned, err := server.bindPendingSearch(ctx)
				require.NoError(t, err)
				facts := pinned.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
				require.Same(t, old, facts.entries[old.key], "the first request really borrowed this owner")
				if event {
					pendingFileProjections.markSuspect(path)
					require.True(t, old.suspect.Load())
				}
				// A distinct reader may either revalidate through a new immutable
				// owner or refuse. It cannot clear the first request's invalidation.
				_, _ = server.bindPendingSearch(ctx)
				tool := "search_symbols"
				args := map[string]any{"query": "ObservedSearchOwner", "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0, "format": "json"}
				if facade {
					tool = "search"
					args = map[string]any{"operation": "symbols", "query": "ObservedSearchOwner", "format": "json"}
				}
				result := callToolByName(t, server, pinned, tool, args)
				if event {
					require.True(t, result.IsError, "another request revived an invalidated public selection: %v", result.Content)
					require.Contains(t, toolResultText(result), "superseded")
					require.True(t, old.suspect.Load(), "never clear an old reader or physical receipt's observed event")
					current := pendingFileProjections.snap.Load().entries[old.key]
					require.NotNil(t, current)
					require.NotSame(t, old, current)
					require.Same(t, old.layer, current.layer, "same-byte refresh must reuse the immutable parse")
					require.Same(t, old.names, current.names, "reuse the existing immutable name cache")
					require.Zero(t, current.commitSeq, "read revalidation cannot inherit physical receipt authority")
					fresh := callToolByName(t, server, ctx, tool, args)
					require.False(t, fresh.IsError, "a new request must serve its newly checked owner: %v", fresh.Content)
				} else {
					require.False(t, result.IsError, "ordinary cached selection must remain usable: %v", result.Content)
					require.Same(t, old, pendingFileProjections.snap.Load().entries[old.key])
				}
			})
		}
	}
}

func TestPendingSearchSourceRefreshHonorsOriginalDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "original private deadline"
		if canceled {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			server, root := ackModeServer(t, "local")
			searchProjectedAck(t, server, root, "DeadlineSearchOwner")
			path := filepath.Join(root, "main.go")
			old := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
			require.NotNil(t, old)
			pendingFileProjections.markSuspect(path)
			ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, time.Now().Add(-time.Second))
			want := context.DeadlineExceeded
			if canceled {
				caller, cancel := context.WithCancel(t.Context())
				cancel()
				ctx, want = caller, context.Canceled
			}
			_, err := server.bindPendingSearch(ctx)
			require.ErrorIs(t, err, want)
			require.Same(t, old, pendingFileProjections.snap.Load().entries[old.key])
			require.True(t, old.suspect.Load(), "failed revalidation must not revive the old owner")
		})
	}
}
