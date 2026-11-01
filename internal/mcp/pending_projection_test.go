package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
)

type projectionFixture struct {
	t   *testing.T
	dir string
	g   *graph.Graph
	idx *indexer.Indexer
	seq uint64
}

// newProjectionFixture indexes a one-file Go repository into an in-memory
// store, the state before a tool edit.
func newProjectionFixture(t *testing.T) *projectionFixture {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc Old() {}\n"), 0o644))
	g := graph.New()
	idx := newProjectionTestIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return &projectionFixture{t: t, dir: dir, g: g, idx: idx}
}

func newProjectionTestIndexer(t testing.TB, g graph.Store) *indexer.Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := indexer.New(g, registry, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	return idx
}

// edit writes content to rel as a tool would and projects it.
func (f *projectionFixture) edit(rel, content string) *projectionEntry {
	f.t.Helper()
	path := filepath.Join(f.dir, rel)
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
	stamp, err := statDiskStamp(path)
	require.NoError(f.t, err)
	f.seq++
	e, err := projectFile(context.Background(), f.idx, path, []byte(content), f.seq, stamp)
	require.NoError(f.t, err)
	return e
}

func liveEntry(p *pendingProjections, path string) *projectionEntry {
	return p.snap.Load().entries[pathkey.MutationKey(path)]
}

type projectionRetirementSpy struct {
	*graph.Graph
	metadataReads int
	nodeReads     int
}

func (s *projectionRetirementSpy) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	s.metadataReads++
	return s.Graph.FileMetasByPaths(prefix, paths)
}

func (s *projectionRetirementSpy) GetFileNodes(path string) []*graph.Node {
	s.nodeReads++
	return s.Graph.GetFileNodes(path)
}

func TestPendingProjectionRetirementUsesBackendContentEvidence(t *testing.T) {
	f := newProjectionFixture(t)
	p := newPendingProjections()
	e := f.edit("a.go", "package p\n\nfunc New() {}\n")
	require.NoError(t, p.publish(e))
	store := &projectionRetirementSpy{Graph: f.g}
	wrapped := newContractCoreEdges(store, t.Context(), nil)
	require.NotImplements(t, (*graph.FileMetaPathReader)(nil), wrapped,
		"durable retirement evidence is backend state, not a request-view capability")
	require.Zero(t, p.retire(store), "old durable bytes cannot retire a newer projection")
	require.Equal(t, 1, store.metadataReads)
	require.Zero(t, store.nodeReads, "mismatched hashes need no node comparison")

	require.NoError(t, f.idx.IndexFile(filepath.Join(f.dir, "a.go")))
	require.Equal(t, 1, p.retire(store))
	require.Equal(t, 3, store.metadataReads, "retirement brackets its node comparison with durable hashes")
	require.Equal(t, 1, store.nodeReads)
	require.Zero(t, p.divergence.Load())
	require.Nil(t, liveEntry(p, e.absPath))
}

func TestPendingProjectionPublishRetire(t *testing.T) {
	f := newProjectionFixture(t)
	p := newPendingProjections()
	require.Nil(t, pendingProjectionHealth(p))
	path := filepath.Join(f.dir, "a.go")

	first := f.edit("a.go", "package p\n\nfunc New() {}\n")
	require.NoError(t, p.publish(first))
	view := graph.NewOverlaidView(f.g, first.layer)
	require.Nil(t, view.GetNode("a.go::Old"))
	require.NotNil(t, view.GetNode("a.go::New"))

	t.Run("a newer entry replaces the older at once", func(t *testing.T) {
		second := f.edit("a.go", "package p\n\nfunc Newer() {}\n")
		require.NoError(t, p.publish(second))
		require.Same(t, second, liveEntry(p, path))
		require.ErrorIs(t, p.publish(first), projectionSuperseded)
		require.Same(t, second, liveEntry(p, path))
	})

	t.Run("a failed patch keeps the entry", func(t *testing.T) {
		require.Zero(t, p.retire(f.g), "the store still holds the old bytes")
		require.NotNil(t, liveEntry(p, path))
	})

	t.Run("the entry retires once the store holds its bytes", func(t *testing.T) {
		require.NoError(t, f.idx.IndexFile(path))
		require.Equal(t, 1, p.retire(f.g))
		require.Nil(t, liveEntry(p, path))
		require.Zero(t, p.divergence.Load())
	})

	t.Run("a retired entry that disagrees with the store counts as divergence", func(t *testing.T) {
		e := f.edit("a.go", "package p\n\nfunc Newest() {}\n")
		require.NoError(t, f.idx.IndexFile(path))
		e.layer.AddNode(e.graphPath, &graph.Node{ID: "a.go::Phantom", Kind: graph.KindFunction, FilePath: "a.go"})
		require.NoError(t, p.publish(e))
		require.Equal(t, 1, p.retire(f.g))
		require.EqualValues(t, 1, p.divergence.Load())
	})

	t.Run("a tombstone hides the file's symbols until the store drops the file", func(t *testing.T) {
		require.NoError(t, os.Remove(path))
		f.seq++
		tomb := newProjectionTombstone(path, "a.go", "", f.seq)
		require.NoError(t, p.publish(tomb))
		e, err := p.lookup(path)
		require.NoError(t, err)
		view := graph.NewOverlaidView(f.g, e.layer)
		require.NotEmpty(t, f.g.GetFileNodes("a.go"))
		require.Empty(t, view.GetFileNodes("a.go"))
		require.Empty(t, view.FindNodesByName("Newest"))
		require.Nil(t, view.GetNode("a.go::Newest"))

		require.Zero(t, p.retire(f.g))
		require.NoError(t, f.g.DeleteFileMetasByFiles("", []string{"a.go"}))
		f.g.EvictFile("a.go")
		require.Equal(t, 1, p.retire(f.g))
	})

	t.Run("a case variant of a live entry is refused", func(t *testing.T) {
		prev := pathkey.CaseInsensitivePaths
		pathkey.CaseInsensitivePaths = true
		t.Cleanup(func() { pathkey.CaseInsensitivePaths = prev })
		live := f.edit("b.go", "package p\n\nfunc B() {}\n")
		require.NoError(t, p.publish(live))
		variant := newProjectionEntry(filepath.Join(f.dir, "B.go"), "", &indexer.Projection{GraphPath: "B.go"}, nil, f.seq+1, diskStamp{})
		require.ErrorIs(t, p.publish(variant), projectionPathCase)
		require.Same(t, live, liveEntry(p, filepath.Join(f.dir, "b.go")))
	})

	health := pendingProjectionHealth(p)
	require.Equal(t, 1, health["files"])
	require.EqualValues(t, 3, health["retired"])
	require.EqualValues(t, 1, health["projection_divergence"])
	require.Equal(t, map[string]int64{"superseded": 1, "path_case": 1}, health["refused"])
}

func TestDiskStampCapturesFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\n"), 0o644))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	require.NotNil(t, stamp.info)
	// No identity comparison before removal: Windows Lstat can defer loading
	// the file ID until SameFile, when this path no longer identifies the file.
	require.NoError(t, os.Remove(path))
	require.True(t, os.SameFile(stamp.info, stamp.info), "a stamp must retain identity without reopening its path")
}

func TestPendingProjectionLookupChecksDisk(t *testing.T) {
	f := newProjectionFixture(t)
	path := filepath.Join(f.dir, "a.go")
	const content = "package p\n\nfunc Edited() {}\n"
	sameSize := "package p\n\nfunc Changed() {}\n"[:len(content)]

	for _, tc := range []struct {
		name    string
		suspect bool
		change  func(t *testing.T, mtime time.Time)
		served  bool
	}{
		{name: "unchanged", served: true},
		{name: "unchanged but suspect", suspect: true, served: true},
		{name: "size", change: func(t *testing.T, _ time.Time) {
			require.NoError(t, os.WriteFile(path, []byte(content+"// more\n"), 0o644))
		}},
		{name: "mtime", change: func(t *testing.T, mtime time.Time) {
			require.NoError(t, os.Chtimes(path, mtime, mtime.Add(time.Second)))
		}},
		{name: "inode", change: func(t *testing.T, mtime time.Time) {
			replacement := path + ".tmp"
			require.NoError(t, os.WriteFile(replacement, []byte(content), 0o644))
			require.NoError(t, os.Chtimes(replacement, mtime, mtime))
			require.NoError(t, os.Rename(replacement, path))
		}},
		{name: "same stat, other bytes, after a watcher event", suspect: true, change: func(t *testing.T, mtime time.Time) {
			require.NoError(t, os.WriteFile(path, []byte(sameSize), 0o644))
			require.NoError(t, os.Chtimes(path, mtime, mtime))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPendingProjections()
			e := f.edit("a.go", content)
			require.NoError(t, p.publish(e))
			if tc.change != nil {
				tc.change(t, e.disk.info.ModTime())
			}
			if tc.suspect {
				p.markSuspect(path)
			}
			got, err := p.lookup(path)
			if tc.served {
				require.NoError(t, err)
				require.Same(t, e, got)
				return
			}
			require.ErrorIs(t, err, errProjectionExternalWrite)
			require.Nil(t, got)
			require.Nil(t, liveEntry(p, path), "a stale entry must not be served again")
			require.EqualValues(t, 1, p.externalWrites.Load())
		})
	}
}

func syntheticProjectionEntry(path, graphPath, hash string, seq uint64, stamp diskStamp, raw []byte) *projectionEntry {
	node := &graph.Node{ID: graphPath, Kind: graph.KindFile, FilePath: graphPath, StartLine: 1, EndLine: 1}
	projection := &indexer.Projection{GraphPath: graphPath, Language: "go", ContentHash: hash, Nodes: []*graph.Node{node}}
	return newProjectionEntry(path, "", projection, raw, seq, stamp)
}

func TestPendingProjectionCapsRefuseWithoutEviction(t *testing.T) {
	dir := t.TempDir()
	p := newPendingProjections()
	for i := range projectionMaxFiles {
		name := fmt.Sprintf("f%03d.go", i)
		require.NoError(t, p.publish(syntheticProjectionEntry(filepath.Join(dir, name), name, "h", 1, diskStamp{}, nil)))
	}
	over := syntheticProjectionEntry(filepath.Join(dir, "over.go"), "over.go", "h", 1, diskStamp{}, nil)
	require.ErrorIs(t, p.publish(over), projectionOverFiles)
	require.NoError(t, p.publish(syntheticProjectionEntry(filepath.Join(dir, "f000.go"), "f000.go", "h", 2, diskStamp{}, nil)),
		"replacing a live file at the cap is not growth")

	huge := syntheticProjectionEntry(filepath.Join(dir, "f001.go"), "f001.go", "h", 2, diskStamp{}, nil)
	huge.size = projectionMaxBytes
	require.ErrorIs(t, p.publish(huge), projectionOverBytes)

	snap := p.snap.Load()
	require.Len(t, snap.entries, projectionMaxFiles)
	require.NotSame(t, huge, liveEntry(p, filepath.Join(dir, "f001.go")))
	require.Nil(t, liveEntry(p, filepath.Join(dir, "over.go")))
}

func TestPendingProjectionKillSwitch(t *testing.T) {
	t.Setenv("GORTEX_PROJECTION", "0")
	p := newPendingProjections()
	path := filepath.Join(t.TempDir(), "a.go")
	require.ErrorIs(t, p.publish(syntheticProjectionEntry(path, "a.go", "h", 1, diskStamp{}, nil)), projectionDisabled)
	require.Empty(t, p.snap.Load().entries)
	e, err := p.lookup(path)
	require.NoError(t, err)
	require.Nil(t, e)
}

// storeTripwire forwards to a real store until it is cleared; any store call
// after that panics.
type storeTripwire struct{ graph.Store }

func TestProjectFileReadsNoStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	content := []byte("package p\n\nimport \"fmt\"\n\nfunc F() { fmt.Println(G()) }\n\nfunc G() int { return 1 }\n")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	store := &storeTripwire{Store: graph.New()}
	idx := newProjectionTestIndexer(t, store)
	idx.SetRootPath(dir)
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	p := newPendingProjections()

	backing := store.Store
	store.Store = nil
	func() {
		defer func() {
			store.Store = backing
			if r := recover(); r != nil {
				t.Fatalf("store accessed while building or publishing a projection: %v", r)
			}
		}()
		e, err := projectFile(context.Background(), idx, path, content, 1, stamp)
		require.NoError(t, err)
		require.NoError(t, p.publish(e))
		got, err := p.lookup(path)
		require.NoError(t, err)
		require.Same(t, e, got)
		require.NotEmpty(t, got.layer.NodesByName("G"))
	}()
}

// 64 goroutines publish, retire, mark and read at once. Readers never see a
// torn snapshot, and retirement never drops an entry a newer publish put in
// place: every file ends with its newest entry.
func TestPendingProjectionConcurrentPublishRetireRead(t *testing.T) {
	const (
		files      = 16
		publishers = 32
		rounds     = 50
		retirers   = 16
		readers    = 16
	)
	dir := t.TempDir()
	store := graph.New()
	paths := make([]string, files)
	stamps := make([]diskStamp, files)
	raw := []byte("package p\n")
	for i := range files {
		name := fmt.Sprintf("f%02d.go", i)
		paths[i] = filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(paths[i], raw, 0o644))
		stamp, err := statDiskStamp(paths[i])
		require.NoError(t, err)
		stamps[i] = stamp
		store.AddNode(&graph.Node{ID: name, Kind: graph.KindFile, FilePath: name, StartLine: 1, EndLine: 1})
		require.NoError(t, store.SetFileMetas("", []graph.FileMetaRow{{FilePath: name, ContentHash: "stale"}}))
	}

	p := newPendingProjections()
	var seq atomic.Uint64
	last := make([]atomic.Uint64, publishers)
	var publishing, background sync.WaitGroup
	done := make(chan struct{})
	for w := range publishers {
		publishing.Add(1)
		go func() {
			defer publishing.Done()
			file := w % files
			for r := range rounds {
				s := seq.Add(1)
				// Only the last round's bytes are newer than the store's, so
				// that entry must survive retirement.
				hash := "stale"
				if r == rounds-1 {
					hash = "fresh"
					last[w].Store(s)
				}
				e := syntheticProjectionEntry(paths[file], filepath.Base(paths[file]), hash, s, stamps[file], raw)
				if err := p.publish(e); err != nil && !errors.Is(err, projectionSuperseded) {
					t.Errorf("publish: %v", err)
				}
			}
		}()
	}
	for range retirers {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				select {
				case <-done:
					return
				default:
					p.retire(store)
				}
			}
		}()
	}
	for r := range readers {
		background.Add(1)
		go func() {
			defer background.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				path := paths[(r+i)%files]
				if i%3 == 0 {
					p.markSuspect(path)
				}
				e, err := p.lookup(path)
				if err != nil {
					t.Errorf("lookup %s: %v", path, err)
					return
				}
				if e != nil && (e.key != pathkey.MutationKey(path) || len(e.layer.FileNodes(e.graphPath)) != 1) {
					t.Errorf("lookup %s served a torn entry", path)
					return
				}
				snap := p.snap.Load()
				var bytes int64
				for _, e := range snap.entries {
					bytes += e.size
				}
				if bytes != snap.bytes {
					t.Errorf("snapshot bytes %d, entries sum to %d", snap.bytes, bytes)
					return
				}
			}
		}()
	}
	publishing.Wait()
	close(done)
	background.Wait()
	p.retire(store)

	for file, path := range paths {
		var newest uint64
		for w := range publishers {
			if w%files == file {
				newest = max(newest, last[w].Load())
			}
		}
		e := liveEntry(p, path)
		require.NotNil(t, e, "entry for %s was lost", path)
		require.Equal(t, newest, e.commitSeq, path)
		require.Equal(t, "fresh", e.hash, path)
	}
}
