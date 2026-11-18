package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"weak"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestOriginalAcknowledgementHoldCannotCreditChangedOwner(t *testing.T) {
	for _, change := range []string{"newer_commit", "same_bytes_new_owner", "suspect", "external_write"} {
		t.Run(change, func(t *testing.T) {
			previous := pendingFileProjections
			registry := newPendingProjections()
			pendingFileProjections = registry
			t.Cleanup(func() { pendingFileProjections = previous })
			fixture := newProjectionFixture(t)
			entry := fixture.edit("a.go", "package p\n\nfunc Original() {}\n")
			ack := &projectionAcknowledgement{}
			holdProjectionForAcknowledgement(ack, registry, entry)
			require.NoError(t, registry.publish(entry))
			require.NoError(t, fixture.idx.IndexFile(entry.absPath))
			require.Zero(t, registry.retirePaths(fixture.g, []string{entry.absPath}))
			require.Same(t, entry, liveEntry(registry, entry.absPath))
			require.False(t, entry.checkedRetired.Load(), "a deferred drop is not a retirement retry hint")
			record := &mutationCommitRecord{disk: mutationDiskCommitted, readbackExact: true, fileProjection: weak.Make(entry)}
			before := map[string]any{}
			record.attachFileProjection(before)
			require.Equal(t, true, before["readback_exact"])
			var replacement *projectionEntry
			switch change {
			case "newer_commit":
				replacement = fixture.edit("a.go", "package p\n\nfunc NewerCommittedOwner() {}\n")
				require.NoError(t, registry.publish(replacement))
			case "same_bytes_new_owner":
				replacement = entry.committedCopy(entry.commitSeq+1, entry.disk)
				require.NoError(t, registry.publish(replacement))
			case "suspect":
				registry.markSuspect(entry.absPath)
			case "external_write":
				require.NoError(t, os.WriteFile(entry.absPath, []byte("package p\n\nfunc ExternalWriteHasDifferentBytes() {}\n"), 0o644))
			}
			response := map[string]any{}
			record.attachFileProjection(response)
			require.Equal(t, false, response["readback_exact"], "holding the old object must not pin registry authority or physical bytes")
			requireMutationFileViewState(t, response, "unavailable")
			require.True(t, response["file_view"].(mutationFileView).Historical)
			ack.finish()
			ack.finish() // actual completion and hold release remain idempotent
			if replacement != nil {
				require.Same(t, replacement, liveEntry(registry, entry.absPath), "old cleanup cannot retire a newer owner")
			}
			if change == "suspect" {
				require.Same(t, entry, liveEntry(registry, entry.absPath))
				require.True(t, entry.suspect.Load(), "cleanup cannot consume the pending invalidation")
			}
			after := map[string]any{}
			record.attachFileProjection(after)
			require.Equal(t, false, after["readback_exact"])
		})
	}
}

func TestOriginalAcknowledgementHoldReleasesOnActualFailure(t *testing.T) {
	for _, ending := range []string{"error", "panic", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			previous := pendingFileProjections
			registry := newPendingProjections()
			pendingFileProjections = registry
			t.Cleanup(func() { pendingFileProjections = previous })
			fixture := newProjectionFixture(t)
			entry := fixture.edit("a.go", "package p\n\nfunc ActualFailure() {}\n")
			s, _ := ackModeServer(t, "full")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			seen := make(chan *projectionAcknowledgement, 1)
			request := editFileRequest(map[string]any{"path": filepath.Join(fixture.dir, "a.go")})
			_, _ = s.boundToolHandler(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				ack := projectionAcknowledgementFrom(ctx, s)
				seen <- ack
				holdProjectionForAcknowledgement(ack, registry, entry)
				if err := registry.publish(entry); err != nil {
					return nil, err
				}
				if err := fixture.idx.IndexFile(entry.absPath); err != nil {
					return nil, err
				}
				if registry.retirePaths(fixture.g, []string{entry.absPath}) != 0 {
					return nil, errors.New("verified retirement bypassed original ACK hold")
				}
				switch ending {
				case "panic":
					panic("modeled failure after exact publication")
				case "cancel":
					cancel()
					return nil, ctx.Err()
				default:
					return nil, errors.New("modeled failure after exact publication")
				}
			})(ctx, request)
			var ack *projectionAcknowledgement
			select {
			case ack = <-seen:
			case <-time.After(3 * time.Second):
				t.Fatal("original acknowledged handler never started")
			}
			require.NotNil(t, ack)
			require.Eventually(t, func() bool {
				ack.mu.Lock()
				defer ack.mu.Unlock()
				return ack.closed && liveEntry(registry, entry.absPath) == nil
			}, 3*time.Second, time.Millisecond, "the original actual actor must join after cancellation or failure")
			require.Nil(t, liveEntry(registry, entry.absPath))
			require.Positive(t, registry.retired.Load())
			ack.mu.Lock()
			holds, closed := len(ack.holds), ack.closed
			ack.mu.Unlock()
			require.Zero(t, holds, "the retained diagnostic owner must not keep hold references")
			require.True(t, closed)
		})
	}
}
