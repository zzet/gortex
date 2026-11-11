package mcp

import (
	"testing"
	"weak"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Keep the real durable hash/node/hash retirement path. The callback delivers
// the existing watcher source event between its two backend hash reads.
type acknowledgementRetirementEventStore struct {
	*projectionRetirementSpy
	afterNodes func()
}

func (s *acknowledgementRetirementEventStore) GetFileNodes(path string) []*graph.Node {
	nodes := s.projectionRetirementSpy.GetFileNodes(path)
	if callback := s.afterNodes; callback != nil {
		s.afterNodes = nil
		callback()
	}
	return nodes
}

func TestOriginalAcknowledgementHoldSurvivesSourceEventDuringRetirement(t *testing.T) {
	for _, sourceEvent := range []bool{false, true} {
		name := "unchanged_retirement_defers_until_actual_ack_finish"
		if sourceEvent {
			name = "source_event_during_checked_retirement"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			registry := newPendingProjections()
			pendingFileProjections = registry
			t.Cleanup(func() { pendingFileProjections = previous })
			fixture := newProjectionFixture(t)
			entry := fixture.edit("a.go", "package p\n\nfunc Acknowledged() {}\n")
			ack := &projectionAcknowledgement{}
			t.Cleanup(ack.finish)
			holdProjectionForAcknowledgement(ack, registry, entry)
			require.NoError(t, registry.publish(entry))
			require.NoError(t, fixture.idx.IndexFile(entry.absPath))

			watcher := &projectionSourceEventWatcher{}
			server := newTestServer(t)
			server.SetWatcher(watcher)
			require.NotNil(t, watcher.onSourceEvent)
			store := &acknowledgementRetirementEventStore{
				projectionRetirementSpy: &projectionRetirementSpy{Graph: fixture.g},
			}
			events := 0
			if sourceEvent {
				store.afterNodes = func() {
					events++
					watcher.onSourceEvent(entry.absPath)
				}
			}
			retired := registry.retirePaths(store, []string{entry.absPath})
			require.Equal(t, 2, store.metadataReads, "actual retirement must bracket node comparison with durable hashes")
			require.Equal(t, 1, store.nodeReads)
			require.Equal(t, sourceEvent, events == 1)
			t.Logf("ACK_RETIREMENT_EVENT source_event=%v retired=%d suspect=%v", sourceEvent, retired, entry.suspect.Load())
			require.Zero(t, retired, "a source event cannot bypass the original active acknowledgement hold")
			require.Same(t, entry, liveEntry(registry, entry.absPath))
			require.False(t, entry.checkedRetired.Load(), "held or invalidated proof cannot credit checked retirement")

			if sourceEvent {
				require.True(t, entry.suspect.Load())
				record := &mutationCommitRecord{disk: mutationDiskCommitted, readbackExact: true, fileProjection: weak.Make(entry)}
				response := map[string]any{}
				record.attachFileProjection(response)
				require.Equal(t, false, response["readback_exact"], "preserving ownership must not turn suspect bytes into exact ACK evidence")
				ack.finish()
				require.Same(t, entry, liveEntry(registry, entry.absPath))
				require.True(t, entry.suspect.Load(), "actual ACK completion must preserve the invalidation witness")
				current, err := registry.lookup(entry.absPath)
				require.NoError(t, err)
				require.Same(t, entry, current, "only the real lookup may revalidate unchanged bytes")
				require.Equal(t, 1, registry.retirePaths(store, []string{entry.absPath}), "fresh proof after lookup must still retire the entry")
			} else {
				ack.finish()
			}
			require.Nil(t, liveEntry(registry, entry.absPath))
		})
	}
}
