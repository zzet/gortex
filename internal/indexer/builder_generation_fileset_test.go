package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
)

type fileSetCountingSource struct {
	mu sync.Mutex

	metas      []source.FileMeta
	byPath     map[string]source.FileMeta
	bodies     map[string][]byte
	statErrors map[string]error

	statCalls  int
	openCalls  int
	walkVisits int
}

func newFileSetCountingSource(metas []source.FileMeta) *fileSetCountingSource {
	ordered := append([]source.FileMeta(nil), metas...)
	for i := range ordered {
		ordered[i].Path = path.Clean(ordered[i].Path)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	byPath := make(map[string]source.FileMeta, len(ordered))
	bodies := make(map[string][]byte, len(ordered))
	for _, meta := range ordered {
		byPath[meta.Path] = meta
		body := []byte("body:" + meta.Path)
		if meta.Symlink {
			body = []byte(meta.SymlinkTarget)
		}
		bodies[meta.Path] = body
	}
	return &fileSetCountingSource{
		metas:      ordered,
		byPath:     byPath,
		bodies:     bodies,
		statErrors: make(map[string]error),
	}
}

func newFileSetCorpusSource(files int) *fileSetCountingSource {
	metas := make([]source.FileMeta, files)
	for i := range metas {
		metas[i] = source.FileMeta{
			Path: fmt.Sprintf("pkg/file-%06d.sparse", i),
			Size: 16,
			Mode: 0o644,
		}
	}
	return newFileSetCountingSource(metas)
}

func (s *fileSetCountingSource) Identity() string { return "file-set-counting-source" }
func (s *fileSetCountingSource) Close() error     { return nil }

func (s *fileSetCountingSource) Stat(p string) (source.FileMeta, error) {
	p = path.Clean(p)
	s.mu.Lock()
	s.statCalls++
	err := s.statErrors[p]
	meta, ok := s.byPath[p]
	s.mu.Unlock()
	if err != nil {
		return source.FileMeta{}, err
	}
	if !ok {
		return source.FileMeta{}, fmt.Errorf("%s: %w", p, source.ErrNotInSource)
	}
	return meta, nil
}

func (s *fileSetCountingSource) Open(p string) (io.ReadCloser, source.FileMeta, error) {
	p = path.Clean(p)
	s.mu.Lock()
	s.openCalls++
	err := s.statErrors[p]
	meta, ok := s.byPath[p]
	body := append([]byte(nil), s.bodies[p]...)
	s.mu.Unlock()
	if err != nil {
		return nil, source.FileMeta{}, err
	}
	if !ok {
		return nil, source.FileMeta{}, fmt.Errorf("%s: %w", p, source.ErrNotInSource)
	}
	return io.NopCloser(bytes.NewReader(body)), meta, nil
}

func (s *fileSetCountingSource) Walk(ctx context.Context, fn func(source.FileMeta) error) (err error) {
	if fn == nil {
		return errors.New("file set counting source: nil walk function")
	}
	visited := 0
	defer func() {
		s.mu.Lock()
		s.walkVisits += visited
		s.mu.Unlock()
	}()
	for _, meta := range s.metas {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		s.mu.Lock()
		statErr := s.statErrors[meta.Path]
		s.mu.Unlock()
		if statErr != nil {
			return statErr
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

func (s *fileSetCountingSource) setBody(p string, body []byte) {
	p = path.Clean(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.byPath[p]
	meta.Size = int64(len(body))
	s.byPath[p] = meta
	for i := range s.metas {
		if s.metas[i].Path == p {
			s.metas[i] = meta
			break
		}
	}
	s.bodies[p] = append([]byte(nil), body...)
}

func (s *fileSetCountingSource) setStatError(p string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statErrors[path.Clean(p)] = err
}

func (s *fileSetCountingSource) counts() (stats, opens, visits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statCalls, s.openCalls, s.walkVisits
}

func (s *fileSetCountingSource) resetCounts() {
	s.mu.Lock()
	s.statCalls = 0
	s.openCalls = 0
	s.walkVisits = 0
	s.mu.Unlock()
}

type legacyFilteringFileSetSource struct {
	inner source.ContentSource
	keep  map[string]struct{}
}

func newLegacyFilteringFileSetSource(inner source.ContentSource, paths []string) *legacyFilteringFileSetSource {
	keep := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		keep[path.Clean(p)] = struct{}{}
	}
	return &legacyFilteringFileSetSource{inner: inner, keep: keep}
}

func (s *legacyFilteringFileSetSource) holds(p string) bool {
	_, ok := s.keep[path.Clean(p)]
	return ok
}

func (s *legacyFilteringFileSetSource) Identity() string {
	return fmt.Sprintf("%s#files:%d", s.inner.Identity(), len(s.keep))
}

func (s *legacyFilteringFileSetSource) Close() error { return s.inner.Close() }

func (s *legacyFilteringFileSetSource) Stat(p string) (source.FileMeta, error) {
	if !s.holds(p) {
		return source.FileMeta{}, fmt.Errorf("%s: outside the generation's file set: %w", p, source.ErrNotInSource)
	}
	return s.inner.Stat(p)
}

func (s *legacyFilteringFileSetSource) Open(p string) (io.ReadCloser, source.FileMeta, error) {
	if !s.holds(p) {
		return nil, source.FileMeta{}, fmt.Errorf("%s: outside the generation's file set: %w", p, source.ErrNotInSource)
	}
	return s.inner.Open(p)
}

func (s *legacyFilteringFileSetSource) Walk(ctx context.Context, fn func(source.FileMeta) error) error {
	return s.inner.Walk(ctx, func(meta source.FileMeta) error {
		if !s.holds(meta.Path) {
			return nil
		}
		return fn(meta)
	})
}

func TestFileSetSourceWalkMatchesLegacyFiltering(t *testing.T) {
	metas := []source.FileMeta{
		{Path: "a.sparse", Size: 11, Mode: 0o755},
		{
			Path:          "dir/link.sparse",
			Size:          int64(len("../target")),
			Mode:          fs.ModeSymlink | 0o777,
			Symlink:       true,
			SymlinkTarget: "../target",
		},
		{Path: "z.sparse", Size: 7, Mode: 0o640},
	}
	keep := []string{"z.sparse", "dir/link.sparse", "missing.sparse", "a.sparse"}
	directInner := newFileSetCountingSource(metas)
	legacyInner := newFileSetCountingSource(metas)

	var direct, legacy []source.FileMeta
	if err := newFileSetSource(directInner, keep).Walk(context.Background(), func(meta source.FileMeta) error {
		direct = append(direct, meta)
		return nil
	}); err != nil {
		t.Fatalf("direct walk: %v", err)
	}
	if err := newLegacyFilteringFileSetSource(legacyInner, keep).Walk(
		context.Background(), func(meta source.FileMeta) error {
			legacy = append(legacy, meta)
			return nil
		},
	); err != nil {
		t.Fatalf("legacy walk: %v", err)
	}
	if !reflect.DeepEqual(direct, legacy) {
		t.Fatalf("direct metadata differs from legacy filtering:\ndirect=%#v\nlegacy=%#v", direct, legacy)
	}
	if stats, _, visits := directInner.counts(); stats != len(keep) || visits != 0 {
		t.Fatalf("direct inner work = %d stats, %d walk visits; want %d, 0", stats, visits, len(keep))
	}
	if stats, _, visits := legacyInner.counts(); stats != 0 || visits != len(metas) {
		t.Fatalf("legacy inner work = %d stats, %d walk visits; want 0, %d", stats, visits, len(metas))
	}
}

func TestFileSetSourceCanonicalizesAndGatesPointReads(t *testing.T) {
	inner := newFileSetCountingSource([]source.FileMeta{
		{Path: "a.sparse", Size: 3, Mode: 0o644},
		{Path: "z.sparse", Size: 5, Mode: 0o755},
	})
	narrowed := newFileSetSource(inner, []string{
		"./z.sparse", "dir/../a.sparse", "a.sparse", "z.sparse",
	})
	if want := []string{"a.sparse", "z.sparse"}; !reflect.DeepEqual(narrowed.paths, want) {
		t.Fatalf("canonical paths = %#v, want %#v", narrowed.paths, want)
	}
	if len(narrowed.keep) != 2 {
		t.Fatalf("membership entries = %d, want 2", len(narrowed.keep))
	}

	meta, err := narrowed.Stat("./a.sparse")
	if err != nil {
		t.Fatalf("stat kept path: %v", err)
	}
	if meta.Path != "a.sparse" || meta.Mode != 0o644 {
		t.Fatalf("stat metadata = %#v", meta)
	}
	statsBefore, _, _ := inner.counts()
	if _, err := narrowed.Stat("outside.sparse"); !errors.Is(err, source.ErrNotInSource) {
		t.Fatalf("stat outside error = %v, want ErrNotInSource", err)
	}
	statsAfter, _, _ := inner.counts()
	if statsAfter != statsBefore {
		t.Fatalf("outside stat reached inner source: before=%d after=%d", statsBefore, statsAfter)
	}

	r, openMeta, err := narrowed.Open("dir/../z.sparse")
	if err != nil {
		t.Fatalf("open kept path: %v", err)
	}
	_ = r.Close()
	if openMeta.Path != "z.sparse" || openMeta.Mode != 0o755 {
		t.Fatalf("open metadata = %#v", openMeta)
	}
	_, opensBefore, _ := inner.counts()
	if _, _, err := narrowed.Open("outside.sparse"); !errors.Is(err, source.ErrNotInSource) {
		t.Fatalf("open outside error = %v, want ErrNotInSource", err)
	}
	_, opensAfter, _ := inner.counts()
	if opensAfter != opensBefore {
		t.Fatalf("outside open reached inner source: before=%d after=%d", opensBefore, opensAfter)
	}
}

func TestFileSetSourceWalkErrorSemantics(t *testing.T) {
	t.Run("missing closure path is skipped", func(t *testing.T) {
		inner := newFileSetCountingSource([]source.FileMeta{{Path: "a.sparse", Size: 1, Mode: 0o644}})
		var paths []string
		err := newFileSetSource(inner, []string{"missing.sparse", "a.sparse"}).Walk(
			context.Background(), func(meta source.FileMeta) error {
				paths = append(paths, meta.Path)
				return nil
			},
		)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		if want := []string{"a.sparse"}; !reflect.DeepEqual(paths, want) {
			t.Fatalf("paths = %#v, want %#v", paths, want)
		}
		if stats, _, _ := inner.counts(); stats != 2 {
			t.Fatalf("stats = %d, want 2", stats)
		}
	})

	t.Run("non-not-found stat error propagates", func(t *testing.T) {
		inner := newFileSetCountingSource([]source.FileMeta{{Path: "a.sparse", Size: 1, Mode: 0o644}})
		wantErr := errors.New("object unavailable")
		inner.setStatError("a.sparse", wantErr)
		err := newFileSetSource(inner, []string{"a.sparse"}).Walk(
			context.Background(), func(source.FileMeta) error { return nil },
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("walk error = %v, want %v", err, wantErr)
		}
	})

	t.Run("callback error stops enumeration", func(t *testing.T) {
		inner := newFileSetCountingSource([]source.FileMeta{
			{Path: "a.sparse", Size: 1, Mode: 0o644},
			{Path: "b.sparse", Size: 1, Mode: 0o644},
		})
		wantErr := errors.New("stop")
		err := newFileSetSource(inner, []string{"a.sparse", "b.sparse"}).Walk(
			context.Background(), func(source.FileMeta) error { return wantErr },
		)
		if !errors.Is(err, wantErr) {
			t.Fatalf("walk error = %v, want %v", err, wantErr)
		}
		if stats, _, _ := inner.counts(); stats != 1 {
			t.Fatalf("stats after callback error = %d, want 1", stats)
		}
	})

	t.Run("nil callback is rejected", func(t *testing.T) {
		inner := newFileSetCountingSource([]source.FileMeta{{Path: "a.sparse", Size: 1, Mode: 0o644}})
		if err := newFileSetSource(inner, []string{"a.sparse"}).Walk(context.Background(), nil); err == nil {
			t.Fatal("nil callback unexpectedly succeeded")
		}
	})
}

func TestFileSetSourceWalkHonorsCancellation(t *testing.T) {
	metas := []source.FileMeta{
		{Path: "a.sparse", Size: 1, Mode: 0o644},
		{Path: "b.sparse", Size: 1, Mode: 0o644},
		{Path: "c.sparse", Size: 1, Mode: 0o644},
	}
	t.Run("before enumeration", func(t *testing.T) {
		inner := newFileSetCountingSource(metas)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := newFileSetSource(inner, []string{"a.sparse"}).Walk(
			ctx, func(source.FileMeta) error { return nil },
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("walk error = %v, want context.Canceled", err)
		}
		if stats, _, _ := inner.counts(); stats != 0 {
			t.Fatalf("stats after pre-cancellation = %d, want 0", stats)
		}
	})

	t.Run("between kept paths", func(t *testing.T) {
		inner := newFileSetCountingSource(metas)
		ctx, cancel := context.WithCancel(context.Background())
		seen := 0
		err := newFileSetSource(inner, []string{"a.sparse", "b.sparse", "c.sparse"}).Walk(
			ctx, func(source.FileMeta) error {
				seen++
				cancel()
				return nil
			},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("walk error = %v, want context.Canceled", err)
		}
		if seen != 1 {
			t.Fatalf("callbacks = %d, want 1", seen)
		}
		if stats, _, _ := inner.counts(); stats != 1 {
			t.Fatalf("stats after cancellation = %d, want 1", stats)
		}
	})
}

func TestFileSetSourceWalkWorkScalesWithKeepSet(t *testing.T) {
	for _, corpus := range []int{1_000, 10_000, 100_000} {
		t.Run(fmt.Sprintf("corpus=%d", corpus), func(t *testing.T) {
			inner := newFileSetCorpusSource(corpus)
			for _, kept := range []int{1, 10} {
				t.Run(fmt.Sprintf("keep=%d", kept), func(t *testing.T) {
					inner.resetCounts()
					keep := fileSetBenchmarkPaths(corpus, kept)
					var got []string
					err := newFileSetSource(inner, keep).Walk(context.Background(), func(meta source.FileMeta) error {
						got = append(got, meta.Path)
						return nil
					})
					if err != nil {
						t.Fatalf("walk: %v", err)
					}
					expected := append([]string(nil), keep...)
					sort.Strings(expected)
					if !reflect.DeepEqual(got, expected) {
						t.Fatalf("paths = %#v, want %#v", got, expected)
					}
					stats, _, visits := inner.counts()
					if stats != kept || visits != 0 {
						t.Fatalf("inner work = %d stats, %d walk visits; want %d, 0", stats, visits, kept)
					}
				})
			}
		})
	}
}

type fileSetTestExtractor struct{}

func (fileSetTestExtractor) Language() string     { return "file-set-test" }
func (fileSetTestExtractor) Extensions() []string { return []string{".sparse"} }
func (fileSetTestExtractor) Extract(filePath string, body []byte) (*parser.ExtractionResult, error) {
	name := string(bytes.TrimSpace(body))
	return &parser.ExtractionResult{Nodes: []*graph.Node{
		{
			ID: filePath, Kind: graph.KindFile, Name: path.Base(filePath),
			FilePath: filePath, Language: "file-set-test",
		},
		{
			ID: filePath + "::" + name, Kind: graph.KindFunction, Name: name, QualName: name,
			FilePath: filePath, StartLine: 1, EndLine: 1, Language: "file-set-test",
		},
	}}, nil
}

type fileSetGraphSnapshot struct {
	Nodes               []graph.Node
	Edges               []graph.Edge
	FileMasks           []store_sqlite.FileMask
	NodeTombstones      []string
	EdgeSourceMasks     []store_sqlite.EdgeSourceMask
	ReplaceMaskCount    int
	DeleteMaskCount     int
	NodeTombstoneCount  int
	EdgeSourceMaskCount int
}

func TestFileSetSourceIndexAndMasksMatchLegacyFiltering(t *testing.T) {
	metas := []source.FileMeta{
		{Path: "a.sparse", Size: 5, Mode: 0o644},
		{Path: "dir/b.sparse", Size: 4, Mode: 0o755},
		{Path: "ignored.sparse", Size: 7, Mode: 0o644},
	}
	keep := []string{"dir/b.sparse", "a.sparse"}
	root := t.TempDir()

	directInner := newFileSetCountingSource(metas)
	directInner.setBody("a.sparse", []byte("alpha"))
	directInner.setBody("dir/b.sparse", []byte("beta"))
	direct := indexFileSetSnapshot(t, root, directInner, newFileSetSource(directInner, keep), keep)

	legacyInner := newFileSetCountingSource(metas)
	legacyInner.setBody("a.sparse", []byte("alpha"))
	legacyInner.setBody("dir/b.sparse", []byte("beta"))
	legacy := indexFileSetSnapshot(
		t, root, legacyInner, newLegacyFilteringFileSetSource(legacyInner, keep), keep,
	)

	if !reflect.DeepEqual(direct, legacy) {
		t.Fatalf("direct index differs from legacy filtering:\ndirect=%#v\nlegacy=%#v", direct, legacy)
	}
	if len(direct.Nodes) == 0 {
		t.Fatal("equivalent indexes are empty; graph comparison did not exercise extraction")
	}
	if len(direct.FileMasks) != 3 || direct.ReplaceMaskCount != 2 || direct.DeleteMaskCount != 1 {
		t.Fatalf("mask coverage = %d rows (%d replace, %d delete), want 3 (2 replace, 1 delete)",
			len(direct.FileMasks), direct.ReplaceMaskCount, direct.DeleteMaskCount)
	}
	if _, _, visits := directInner.counts(); visits != 0 {
		t.Fatalf("direct index invoked inner Walk %d times", visits)
	}
	if _, _, visits := legacyInner.counts(); visits != len(metas) {
		t.Fatalf("legacy index visited %d files, want %d", visits, len(metas))
	}
}

func indexFileSetSnapshot(
	t *testing.T,
	root string,
	base source.ContentSource,
	narrowed source.ContentSource,
	keep []string,
) fileSetGraphSnapshot {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "test", GraphID: "test-graph", LayerID: "test-layer",
		GenerationKind: "dirty", LowerViewFingerprint: "base", TreeOID: "tree",
		ConfigHash: "config", ExtractorVersions: "extractor-v1", ResolverVersion: "resolver-v1",
	})
	if err != nil {
		t.Fatalf("begin payload generation: %v", err)
	}
	if generationID == 0 {
		t.Fatal("begin payload generation returned generation zero")
	}

	registry := parser.NewRegistry()
	registry.Register(fileSetTestExtractor{})
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(handle, registry, cfg, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	idx.SetContentSource(narrowed)
	result, err := idx.IndexCtx(context.Background(), root)
	idx.Close()
	if err != nil {
		t.Fatalf("index narrowed source: %v", err)
	}

	report := BuildReport{}
	if result != nil {
		report.NodeCount = result.NodeCount
		report.EdgeCount = result.EdgeCount
	}
	builder := &SparseGenerationBuilder{Store: store, Registry: registry, Config: cfg, Logger: zap.NewNop()}
	req := BuildRequest{
		Base: store, Target: base, RootPath: root, RepoPrefix: "repo",
		WorkspaceID: "workspace", ProjectID: "project",
	}
	plan := buildPlan{indexed: append([]string(nil), keep...), deleted: []string{"removed.sparse"}}
	sort.Strings(plan.indexed)
	if err := builder.writeMasks(req, plan, handle, &report); err != nil {
		t.Fatalf("write masks: %v", err)
	}

	nodePtrs := handle.AllNodes()
	nodes := make([]graph.Node, 0, len(nodePtrs))
	for _, node := range nodePtrs {
		if node != nil {
			nodes = append(nodes, *node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	edgePtrs := handle.AllEdges()
	edges := make([]graph.Edge, 0, len(edgePtrs))
	for _, edge := range edgePtrs {
		if edge != nil {
			edges = append(edges, *edge)
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		left := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", edges[i].From, edges[i].To, edges[i].Kind, edges[i].FilePath, edges[i].Line)
		right := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", edges[j].From, edges[j].To, edges[j].Kind, edges[j].FilePath, edges[j].Line)
		return left < right
	})
	fileMasks, err := handle.FileMasks()
	if err != nil {
		t.Fatalf("read file masks: %v", err)
	}
	nodeTombstones, err := handle.NodeTombstones()
	if err != nil {
		t.Fatalf("read node tombstones: %v", err)
	}
	edgeMasks, err := handle.EdgeSourceMasks()
	if err != nil {
		t.Fatalf("read edge-source masks: %v", err)
	}
	return fileSetGraphSnapshot{
		Nodes: nodes, Edges: edges, FileMasks: fileMasks,
		NodeTombstones: nodeTombstones, EdgeSourceMasks: edgeMasks,
		ReplaceMaskCount: report.ReplaceMasks, DeleteMaskCount: report.DeleteMasks,
		NodeTombstoneCount: report.NodeTombstones, EdgeSourceMaskCount: report.EdgeSourceMarkers,
	}
}

func fileSetBenchmarkPaths(corpus, kept int) []string {
	paths := make([]string, kept)
	for i := range paths {
		index := corpus - 1 - i*(corpus/kept)
		paths[i] = fmt.Sprintf("pkg/file-%06d.sparse", index)
	}
	return paths
}

var fileSetWalkBenchmarkSink int

func BenchmarkFileSetSourceWalk(b *testing.B) {
	for _, corpus := range []int{1_000, 10_000, 100_000} {
		inner := newFileSetCorpusSource(corpus)
		for _, kept := range []int{1, 10} {
			keep := fileSetBenchmarkPaths(corpus, kept)
			b.Run(fmt.Sprintf("corpus=%d/keep=%d/direct", corpus, kept), func(b *testing.B) {
				narrowed := newFileSetSource(inner, keep)
				ctx := context.Background()
				seen := 0
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := narrowed.Walk(ctx, func(source.FileMeta) error {
						seen++
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				}
				fileSetWalkBenchmarkSink = seen
			})
			b.Run(fmt.Sprintf("corpus=%d/keep=%d/legacy", corpus, kept), func(b *testing.B) {
				narrowed := newLegacyFilteringFileSetSource(inner, keep)
				ctx := context.Background()
				seen := 0
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := narrowed.Walk(ctx, func(source.FileMeta) error {
						seen++
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				}
				fileSetWalkBenchmarkSink = seen
			})
		}
	}
}
