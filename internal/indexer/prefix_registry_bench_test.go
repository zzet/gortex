package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func BenchmarkResolveTrackPrefixRegistryReaders(b *testing.B) {
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("workspace: owner\nexclude:\n"+strings.Repeat("  - ignored-file\n", 10000)), 0o644); err != nil {
		b.Fatal(err)
	}
	cm, err := config.NewConfigManager(filepath.Join(b.TempDir(), "global.yaml"))
	if err != nil {
		b.Fatal(err)
	}
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	defer func() { _ = mi.Close(context.Background()) }()
	stop, done, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		entry := config.RepoEntry{Path: root, Name: "src"}
		mi.resolveTrackPrefix(&entry, root, nil, nil)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				entry := config.RepoEntry{Path: root, Name: "src"}
				mi.resolveTrackPrefix(&entry, root, nil, nil)
			}
		}
	}()
	<-started
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mi.GetMetadata("src")
	}
	b.StopTimer()
	close(stop)
	<-done
}
