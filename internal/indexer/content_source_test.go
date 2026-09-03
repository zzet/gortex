package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// writeContentSourceFixture lays down a small multi-language,
// multi-directory tree: enough that the walk has a subdirectory to
// descend, a file no extractor claims, and a cross-file reference for the
// resolver to bind.
func writeContentSourceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(Helper()) }\n",
		"util.go": "package main\n\nfunc Helper() string { return \"hi\" }\n",
		"lib/lib.go": "package lib\n\n// Exported does a thing.\nfunc Exported(n int) int { return n * 2 }\n" +
			"\nfunc unexported() int { return Exported(2) }\n",
		"lib/notes.txt": "not source\n",
		"README.md":     "# fixture\n\nA fixture tree.\n",
	}
	for rel, body := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
	return dir
}

func newContentSourceIndexer(t *testing.T, g graph.Store) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, reg, cfg, zap.NewNop())
}

// graphShape renders a graph as two sorted, comparable projections. Node
// identity plus span plus file is what an equality check on "the same
// parse" needs; edge identity carries the resolver's binding.
func graphShape(g graph.Store) (nodes, edges []string) {
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		nodes = append(nodes, fmt.Sprintf("%s|%s|%s|%s|%d-%d",
			n.ID, n.Kind, n.Language, n.FilePath, n.StartLine, n.EndLine))
	}
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		edges = append(edges, fmt.Sprintf("%s|%s|%s|%s|%d", e.From, e.Kind, e.To, e.FilePath, e.Line))
	}
	sort.Strings(nodes)
	sort.Strings(edges)
	return nodes, edges
}

// A single-file index that reads through a FilesystemSource must produce
// exactly the graph the os-backed read produces. This is the guard on the
// content seam: same bytes, same parse, same commit, and a read receipt
// that degrades without failing the mutation.
//
// The file is rewritten before the single-file pass so the re-index
// actually reads it. Re-indexing an untouched file is answered by the
// mtime + content receipt without a read at all, which would make this
// comparison pass no matter what the seam did.
func TestContentSourceSingleFileIndexMatchesOSRead(t *testing.T) {
	const edited = "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(Helper(), Extra()) }\n" +
		"\nfunc Extra() int { return 7 }\n"

	indexEdited := func(useSource bool) graph.Store {
		dir := writeContentSourceFixture(t)
		target := filepath.Join(dir, "main.go")

		g := graph.New()
		idx := newContentSourceIndexer(t, g)
		_, err := idx.Index(dir)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(target, []byte(edited), 0o644))
		if useSource {
			fsSource, err := source.NewFilesystemSource(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = fsSource.Close() })
			idx.SetContentSource(fsSource)
		}
		require.NoError(t, idx.IndexFile(target))
		return g
	}

	// Node ids and file paths are repo-relative, so two runs over two
	// temp directories are directly comparable.
	osNodes, osEdges := graphShape(indexEdited(false))
	srcNodes, srcEdges := graphShape(indexEdited(true))
	require.Contains(t, strings.Join(osNodes, "\n"), "main.go::Extra",
		"precondition: the single-file pass picked up the edited content")
	require.Equal(t, osNodes, srcNodes, "source-backed read must mint the same nodes as the os read")
	require.Equal(t, osEdges, srcEdges, "source-backed read must mint the same edges as the os read")
}

// The read helper itself: identical bytes either way, and a receipt that
// says "snapshot" only when a source served it.
func TestContentSourceReadFileWithVersion(t *testing.T) {
	dir := writeContentSourceFixture(t)
	target := filepath.Join(dir, "lib", "lib.go")
	want, err := os.ReadFile(target)
	require.NoError(t, err)

	idx := newContentSourceIndexer(t, graph.New())
	_, err = idx.Index(dir)
	require.NoError(t, err)

	osSrc, osVersion, err := idx.readFileWithVersion(target)
	require.NoError(t, err)
	require.Equal(t, want, osSrc)
	require.True(t, osVersion.valid, "an os read of a quiet file yields a valid receipt")
	require.False(t, osVersion.snapshot, "an os read is not a snapshot read")

	fsSource, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx.SetContentSource(fsSource)

	srcBytes, srcVersion, err := idx.readFileWithVersion(target)
	require.NoError(t, err)
	require.Equal(t, want, srcBytes, "the source must serve the same bytes as the os read")
	require.True(t, srcVersion.valid)
	require.True(t, srcVersion.snapshot, "a source read is valid by construction, not by restat")
	require.Nil(t, srcVersion.info, "a snapshot receipt carries no FileInfo to restat")

	// A snapshot receipt is accepted without a stat and leaves the
	// working-tree staleness ledger exactly where the os walk left it.
	relPath := idx.relKey(target)
	idx.mtimeMu.RLock()
	before, hadBefore := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	require.True(t, idx.recordFileReadVersion(relPath, target, srcVersion))
	idx.mtimeMu.RLock()
	after, hadAfter := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	require.Equal(t, hadBefore, hadAfter)
	require.Equal(t, before, after, "a snapshot read must not move the working-tree mtime ledger")

	fresh, stale := idx.recordFileReadVersionsBatched([]fileReadReceipt{{
		absPath: target, mtimeKey: relPath, readVersion: srcVersion,
	}})
	require.Equal(t, []string{target}, fresh, "the batched restat loop accepts a snapshot receipt")
	require.Empty(t, stale)

	// A path outside the source root is refused rather than silently
	// falling back to the filesystem.
	_, _, err = idx.readFileWithVersion(filepath.Join(filepath.Dir(dir), "elsewhere.go"))
	require.ErrorIs(t, err, source.ErrOutsideRoot)
}

// The shebang probe has to work off the snapshot too: a file whose
// extension nothing recognises still reaches the same language verdict.
func TestContentSourceSniffPrefix(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runme")
	require.NoError(t, os.WriteFile(script, []byte("#!/usr/bin/env python3\n\ndef main():\n    return 1\n"), 0o755))

	idx := newContentSourceIndexer(t, graph.New())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	osLang, osOK := idx.effectiveLanguage(script, nil)

	fsSource, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx.SetContentSource(fsSource)

	srcLang, srcOK := idx.effectiveLanguage(script, nil)
	require.Equal(t, osOK, srcOK, "the shebang probe must reach the same verdict off the snapshot")
	require.Equal(t, osLang, srcLang)
	require.True(t, osOK, "precondition: the shebang probe recognises the fixture script")

	prefix := idx.readSniffPrefix(script)
	require.GreaterOrEqual(t, len(prefix), 22, "the source prefix read must return the shebang line")
	require.Equal(t, []byte("#!/usr/bin/env python3"), prefix[:22])
}

// walkSource must admit exactly the set a filesystem walk through the
// same gate admits — the seam changes where entries come from, never
// which ones survive.
func TestWalkSourceAdmitsTheSameSetAsTheFilesystemWalk(t *testing.T) {
	dir := writeContentSourceFixture(t)

	osIdx := newContentSourceIndexer(t, graph.New())
	_, err := osIdx.Index(dir)
	require.NoError(t, err)

	var osAdmitted []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if osIdx.admitWalkEntry(dir, path, -1, true).pruneDir {
				return filepath.SkipDir
			}
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if osIdx.admitWalkEntry(dir, path, info.Size(), false).admit {
			rel, relErr := filepath.Rel(dir, path)
			require.NoError(t, relErr)
			osAdmitted = append(osAdmitted, filepath.ToSlash(rel))
		}
		return nil
	}))
	require.NotEmpty(t, osAdmitted, "precondition: the fixture admits files")

	srcIdx := newContentSourceIndexer(t, graph.New())
	_, err = srcIdx.Index(dir)
	require.NoError(t, err)
	fsSource, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	srcIdx.SetContentSource(fsSource)

	var srcAdmitted []string
	require.NoError(t, srcIdx.walkSource(context.Background(), fsSource, func(f walkedFile, adm walkAdmission) error {
		rel, relErr := filepath.Rel(dir, f.path)
		require.NoError(t, relErr)
		require.NotEmpty(t, f.lang, "an admitted entry carries the language the gate resolved")
		require.Zero(t, f.mtimeNano, "a snapshot entry has no modification time")
		require.True(t, adm.admit, "the fixture has no over-cap file, so every entry is an admission")
		srcAdmitted = append(srcAdmitted, filepath.ToSlash(rel))
		return nil
	}))

	sort.Strings(osAdmitted)
	sort.Strings(srcAdmitted)
	require.Equal(t, osAdmitted, srcAdmitted)
}

// The size cap is the one rejection the shared gate reports back instead
// of swallowing, because the bulk walk tells the user about it.
func TestAdmitWalkEntryReportsOversize(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n"), 0o644))

	idx := newContentSourceIndexer(t, graph.New())
	idx.config.MaxFileSize = 4

	adm := idx.admitWalkEntry(dir, target, 13, false)
	require.False(t, adm.admit)
	require.True(t, adm.oversize)
	require.Equal(t, "go", adm.lang)

	// A caller that does not know the size opts out of the cap entirely.
	adm = idx.admitWalkEntry(dir, target, -1, false)
	require.True(t, adm.admit)
	require.False(t, adm.oversize)
}

// An over-cap file has to leave the same trace whichever walk found it: the
// synthetic skip node, not silence. A sparse generation depends on it — the
// node is what puts the path in the payload, and a path with no payload earns
// no mask, so the layer below would keep showing its stale symbols through
// where a flat index of the same bytes shows a skip stub.
func TestSourceWalkEmitsTheSameSizeSkipNodeAsTheFilesystemWalk(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.go"),
		[]byte("package main\n\nfunc Big() {}\n"+strings.Repeat("// pad\n", 64)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.go"),
		[]byte("package main\n\nfunc Small() {}\n"), 0o644))

	index := func(withSource bool) graph.Store {
		g := graph.New()
		idx := newContentSourceIndexer(t, g)
		idx.config.MaxFileSize = 64
		if withSource {
			fsSource, err := source.NewFilesystemSource(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = fsSource.Close() })
			idx.SetContentSource(fsSource)
		}
		_, err := idx.Index(dir)
		require.NoError(t, err)
		return g
	}

	osGraph, srcGraph := index(false), index(true)
	osNodes, osEdges := graphShape(osGraph)
	srcNodes, srcEdges := graphShape(srcGraph)
	require.Equal(t, osNodes, srcNodes)
	require.Equal(t, osEdges, srcEdges)

	skip := srcGraph.GetNode("big.go")
	require.NotNil(t, skip, "the over-cap file must stay visible through the source walk")
	require.Equal(t, "size", skip.Meta["skip_reason"])
	require.NotNil(t, srcGraph.GetNode("small.go::Small"), "the under-cap file is indexed as usual")
}

// The readers a pass makes outside the parse loop are the ones that quietly
// keep consulting the working tree: the module manifest and the go.mod
// contract extractor read the repo root by hand, and the embed chunker reads a
// symbol's body back to split it. All three must answer from the state the
// pass is describing. Pointing the source at one tree while the working tree
// holds another is the only way to tell which one they read.
func TestAuxiliaryReadersFollowTheContentSource(t *testing.T) {
	const body = `package main

func Long() int {
	a := 1
	b := 2
	c := 3
	return a + b + c
}
`
	checkout, snapshot := t.TempDir(), t.TempDir()
	for dir, marker := range map[string]string{checkout: "checkout", snapshot: "snapshot"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module example.com/fixture\n\ngo 1.22\n\nrequire "+marker+"dep.example/x v1.0.0\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte(strings.Replace(body, "a := 1", "a := 1 // "+marker, 1)), 0o644))
	}

	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	// One line is past the threshold for every symbol in the fixture, so the
	// chunker reads the body instead of settling for the metadata vector.
	idx.SetEmbeddingChunkOptions(embedding.ChunkOptions{ThresholdLines: 1})
	fsSource, err := source.NewFilesystemSource(snapshot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx.SetContentSource(fsSource)
	_, err = idx.Index(checkout)
	require.NoError(t, err)

	require.NotNil(t, g.GetNode("module::go:snapshotdep.example/x@v1.0.0"),
		"the module manifest reader answered from the working tree")
	require.NotNil(t, g.GetNode("dep::snapshotdep.example/x"),
		"the go.mod contract extractor answered from the working tree")
	require.Nil(t, g.GetNode("module::go:checkoutdep.example/x@v1.0.0"))

	texts, _, _, _ := idx.collectEmbedTexts(g.AllNodes())
	joined := strings.Join(texts, "\n")
	require.Contains(t, joined, "// snapshot", "the embed chunker answered from the working tree")
	require.NotContains(t, joined, "// checkout")
}

// The untracked-asset gate asks `git ls-files` of the checkout, which
// describes a tree a snapshot source is not serving. It goes inert rather than
// admitting from the wrong one; the index declares the omission in its
// producer state.
func TestUntrackedAssetGateIsInertUnderAContentSource(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")

	idx := newContentSourceIndexer(t, graph.New())
	idx.config.SkipUntrackedAssets = true
	require.NotNil(t, idx.newUntrackedAssetGate(context.Background(), dir),
		"precondition: a git checkout with asset extractors registered builds the gate")

	fsSource, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx.SetContentSource(fsSource)
	require.Nil(t, idx.newUntrackedAssetGate(context.Background(), dir),
		"the gate read the checkout's tracked set while a source was installed")
}

// contentFileVersion is what the per-file contract caches key on. Under a
// source it answers from the snapshot, which has one version for everything it
// holds, and it keeps "absent" apart from "unreadable" — a caller that evicts
// on absence must not evict on an I/O error.
func TestContentFileVersionAnswersFromTheSource(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "here.go")
	require.NoError(t, os.WriteFile(present, []byte("package main\n"), 0o644))

	idx := newContentSourceIndexer(t, graph.New())
	idx.storeRootPath(dir)

	mtime, exists, ok := idx.contentFileVersion(present)
	require.True(t, ok)
	require.True(t, exists)
	require.NotZero(t, mtime, "the os path keys the cache by the file's modification time")

	_, exists, ok = idx.contentFileVersion(filepath.Join(dir, "gone.go"))
	require.False(t, ok)
	require.False(t, exists, "a missing file is absent, not unreadable")

	fsSource, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx.SetContentSource(fsSource)

	mtime, exists, ok = idx.contentFileVersion(present)
	require.True(t, ok)
	require.True(t, exists)
	require.Zero(t, mtime, "a snapshot has one version, so it contributes no modification time")

	_, exists, ok = idx.contentFileVersion(filepath.Join(dir, "gone.go"))
	require.False(t, ok)
	require.False(t, exists)

	// A path the source cannot be addressed by says nothing about whether the
	// file is there, so it must not read as a deletion.
	_, exists, ok = idx.contentFileVersion(filepath.Join(filepath.Dir(dir), "outside.go"))
	require.False(t, ok)
	require.True(t, exists, "an unaddressable path is not a confirmed deletion")
}
