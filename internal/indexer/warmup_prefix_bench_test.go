package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func BenchmarkWarmupResolve200(b *testing.B) {
	root := b.TempDir()
	cm, err := config.NewConfigManager(filepath.Join(root, "global.yaml"))
	if err != nil {
		b.Fatal(err)
	}
	entries := make([]config.RepoEntry, 200)
	for i := range entries {
		path := filepath.Join(root, fmt.Sprintf("repo-%03d", i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, ".gortex.yaml"), []byte("workspace: team\nexclude:\n  - vendor/**\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		entries[i] = config.RepoEntry{Path: path}
	}
	cm.Global().Repos = entries
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	b.Cleanup(func() { _ = mi.Close(context.Background()) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lookup := cm.Global().SnapshotRepoPaths()
		for _, entry := range entries {
			_, _ = mi.resolveTrackPrefix(&entry, entry.Path, nil, lookup)
		}
	}
}
