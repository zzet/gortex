package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

func TestBusyControllerCollisionWithin1s(t *testing.T) {
	c, _, _, dir := buildCatalogController(t)
	_, owner := trackEnrichRepo(t, c, dir, "src")
	loser := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(loser, 0o755))
	c.mu.Lock() // another mutation owns the controller for this entire call
	result := make(chan error, 1)
	go func() {
		_, err := c.Track(context.Background(), daemon.TrackParams{Path: loser})
		result <- err
	}()
	select {
	case err := <-result:
		c.mu.Unlock()
		var collision *indexer.PrefixCollisionError
		require.ErrorAs(t, err, &collision)
		require.Equal(t, owner, collision.ExistingPath)
	case <-time.After(time.Second):
		c.mu.Unlock()
		<-result // join the admitted call before fixture teardown
		t.Fatal("collision queued behind the controller mutex")
	}
}
