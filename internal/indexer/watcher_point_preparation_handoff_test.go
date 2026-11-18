package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestPointPreparationRejectsUnknownOrChangedHandoff(t *testing.T) {
	for _, name := range []string{"same_stat_bytes", "owner", "graph", "snapshot", "cancel", "config", "extractor", "different_path", "opaque", "observed", "manifest"} {
		t.Run(name, func(t *testing.T) {
			path, idx, store := pointSourceProofFixture(t)
			require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Edited() {}\n"), 0600))
			watcher, err := NewWatcher(idx, config.Default().Watch, zap.NewNop())
			require.NoError(t, err)
			watcher.degradedNoFsnotify = true
			t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			watcher.pendingGeneration[path] = 1 // component branch identity, not an accepted source witness
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := &watcherPointPreparation{watcher: watcher, path: path, kind: ChangeModified, generation: 1}
			if name == "opaque" {
				watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
					t.Fatal("not executed by preparation")
					return nil, nil
				}
			}
			if name == "observed" {
				ctx = WithPublicationStamps(ctx)
			}
			if name == "manifest" {
				p.path = filepath.Join(filepath.Dir(path), "go.mod")
				watcher.pendingGeneration[p.path] = 1
				require.NoError(t, os.WriteFile(p.path, []byte("module test\n"), 0600))
			}
			p.prepare(ctx)
			defer p.release()
			if name == "opaque" || name == "observed" || name == "manifest" {
				require.Nil(t, p.prepared)
				return
			}
			require.NotNil(t, p.prepared)
			switch name {
			case "same_stat_bytes":
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Mutate() {}\n"), 0600))
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
				after, err := os.Stat(path)
				require.NoError(t, err)
				require.True(t, sameFileVersion(info, after))
			case "owner":
				idx.repoPrefix = "different-owner"
			case "graph":
				store.AddNode(&graph.Node{ID: "new-node", Name: "New", Kind: graph.KindFunction, FilePath: "main.go"})
			case "snapshot":
				src, err := source.NewFilesystemSource(filepath.Dir(path))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, src.Close()) })
				idx.SetContentSource(src)
			case "cancel":
				cancel()
			case "config":
				idx.config.MaxFileSize++
			case "extractor":
				idx.registry.Register(languages.NewGoExtractor())
			case "different_path":
				path = filepath.Join(filepath.Dir(path), "other.go")
			}
			_, ok := p.takeProbe(idx, path)
			require.False(t, ok, "every handoff re-reads actual bytes before returning a probe")
			require.Nil(t, p.prepared)
			require.Nil(t, p.installed)
			_, again := p.takeProbe(idx, path)
			require.False(t, again, "single-use carrier cannot be replayed")
		})
	}
}

// This is a private read-fence control, not a public tool receipt. The real
// two-ticket point integration roots exercise admission and publication.
func TestPointPreparationHandoffUsesProtectedReadFrontier(t *testing.T) {
	for _, name := range []string{"validated_read", "changed_after_read", "different_frontier"} {
		t.Run(name, func(t *testing.T) {
			path, idx, _ := pointSourceProofFixture(t)
			require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Edited() {}\n"), 0600))
			watcher, err := NewWatcher(idx, config.Default().Watch, zap.NewNop())
			require.NoError(t, err)
			watcher.degradedNoFsnotify = true
			t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			watcher.pendingGeneration[path] = 1
			p := &watcherPointPreparation{watcher: watcher, path: path, kind: ChangeModified, generation: 1}
			p.prepare(t.Context())
			t.Cleanup(p.release)
			require.NotNil(t, p.prepared)
			require.Nil(t, p.probe.readVersion.relationsRead, "early extraction predates the executor's read frontier")
			reads := 0
			frontier := &relationsReadFrontier{path: path, capture: func() uint64 { reads++; return 2 }}
			probe, ok := p.takeProbe(idx, path, frontier)
			require.True(t, ok)
			require.Equal(t, 1, reads, "handoff must perform an actual protected read even for inert work")
			require.Same(t, frontier, probe.readVersion.relationsRead)
			require.Equal(t, uint64(2), probe.readVersion.relationsGeneration)
			require.Zero(t, frontier.covered, "handoff alone never accepts a relation frontier")
			if name == "changed_after_read" {
				require.NoError(t, os.WriteFile(path, []byte("package p\nfunc NewerAfterRead() {}\n"), 0600))
			}
			fresh, stale := idx.recordFileReadVersionsBatched([]fileReadReceipt{{absPath: path, mtimeKey: idx.relKey(path), readVersion: probe.readVersion}})
			if name == "changed_after_read" {
				require.Empty(t, fresh)
				require.Equal(t, []string{path}, stale)
				require.Zero(t, frontier.covered)
			} else {
				require.Empty(t, stale)
				require.Equal(t, []string{path}, fresh)
				require.Equal(t, uint64(2), frontier.covered)
			}
			if name == "different_frontier" {
				next := &relationsReadFrontier{path: path, capture: func() uint64 { return 9 }}
				next.accept(path, probe.readVersion)
				require.Zero(t, next.covered, "a prepared read cannot be borrowed by a later executor")
			}
		})
	}
}
