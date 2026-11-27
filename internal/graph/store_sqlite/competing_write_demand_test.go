package store_sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCompetingWriteDemandKeepsOrdinaryAndQueuedWriters(t *testing.T) {
	store := openCatalogStore(t)
	store.stopCheckpointLoop()
	first := store.AnnounceCheckoutRefresh()
	second := store.AtGeneration(1).AnnounceCheckoutRefresh()
	defer first()
	defer second()
	require.True(t, store.WriteWanted())
	require.False(t, store.CompetingWriteWanted(), "dependent checkout tickets cannot take a write turn yet")
	ordinary := store.AnnounceWrite()
	defer ordinary()
	require.True(t, store.CompetingWriteWanted(), "ordinary announced work must still take priority")
	ordinary()
	require.False(t, store.CompetingWriteWanted())
	require.True(t, store.WriteWanted(), "ordinary release must not consume checkout demand")

	// A real writer from any checkout still has priority as soon as it parks
	// at the shared gate, even without an ordinary announcement.
	store.writeMu.Lock()
	locked := true
	done := make(chan error, 1)
	joined := false
	defer func() {
		if locked {
			store.writeMu.Unlock()
		}
		if !joined {
			<-done
		}
	}()
	go func() {
		done <- store.AddBatchChecked([]*graph.Node{{ID: "queued", Name: "queued", Kind: graph.KindFunction}}, nil)
	}()
	require.Eventually(t, func() bool { return store.CompetingWriteWanted() }, time.Second, time.Millisecond)
	store.writeMu.Unlock()
	locked = false
	err := <-done
	joined = true
	require.NoError(t, err)
	require.NotNil(t, store.GetNode("queued"))
	require.False(t, store.CompetingWriteWanted())
	require.True(t, store.WriteWanted(), "a completed writer must not consume dependent tickets")
	first()
	second()
	require.False(t, store.WriteWanted())
}
