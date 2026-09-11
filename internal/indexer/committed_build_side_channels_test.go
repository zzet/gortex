package indexer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// A build that indexes a committed tree reads its payload through the
// installed content source, but four readers sat outside that seam and went
// straight to the working copy: the C/C++ compile database, the include-root
// heuristic behind it, the npm / workspace manifests, and the tsconfig /
// jsconfig path-alias scan. A generation for tree T then resolved its
// includes and its aliases from whatever the checkout happened to hold, so
// the same tree produced different edges depending on when it was built.
//
// Each test here writes ONE answer into the checkout and a DIFFERENT answer
// into the snapshot, then drives the production entry point and asserts the
// snapshot's answer won. Reverting the routing makes every one of them fail
// with the checkout's answer.

// sideChannelIndexer is a C/C++ + TypeScript indexer over an in-memory graph.
// It is built here rather than borrowed from another test file so this file's
// fixtures do not move when another test's do.
func sideChannelIndexer(t *testing.T, g graph.Store, root string) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewCExtractor())
	reg.Register(languages.NewCppExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	idx := New(g, reg, cfg, zap.NewNop())
	t.Cleanup(func() { idx.Close() })
	idx.storeRootPath(root)
	t.Cleanup(func() { clearCppIncludeDirCache(root) })
	return idx
}

// seedCFile plants the one C file node that makes an include search path
// worth reconstructing at all. populateCppIncludeDirs skips a source-backed
// probe for a graph with no C-family file, so a fixture that wants the probe
// to run has to hold one.
func seedCFile(g graph.Store, id string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id, Language: "c"})
}

// snapshotOf opens a real filesystem source over dir. A committed build's
// source is a git tree, but what matters to these readers is only that the
// source's bytes are not the checkout's — which a second directory models
// without a git fixture, through a production ContentSource implementation.
func snapshotOf(t *testing.T, dir string) source.ContentSource {
	t.Helper()
	src, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// writeSideChannelTree writes a repo-relative path → content map under root.
func writeSideChannelTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
}

// compileDBFor renders a one-TU compile database whose include search path is
// the given repo-relative directory.
func compileDBFor(dir, includeDir string) string {
	return `[{"directory": "` + dir + `", "file": "src/main.c",` +
		` "arguments": ["cc", "-I` + includeDir + `", "-c", "src/main.c"]}]`
}

// cppIncludeDirsCached reports what the process-wide, root-keyed compile-DB
// cache holds for a checkout root. It is the observable that says which tree
// populateCppIncludeDirs read: the working-copy loader fills this cache, and
// the source-backed loader must not touch it — the cache cannot tell two
// snapshots of one checkout apart, so an entry landed by a committed build
// would be a wrong answer handed to the live index.
func cppIncludeDirsCached(root string) (map[string]cppTU, bool) {
	return cppIncludeDirCache.get(root, compileDBMtime(root))
}

// TestCommittedBuildReadsTheSnapshotCompileDB drives populateCppIncludeDirs —
// the production entry point, called from Indexer.ResolveAll and from the
// deferred resolve phase of every index pass — with a content source installed
// and a compile database on the checkout that names a different include root.
func TestCommittedBuildReadsTheSnapshotCompileDB(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	// Both databases record the checkout as their build directory, which is
	// what a database generated in a working tree and then committed holds.
	// Only the include root differs.
	writeSideChannelTree(t, checkout, map[string]string{
		"compile_commands.json":    compileDBFor(checkout, "dirty/include"),
		"dirty/include/proj/api.h": "// dirty\n",
		"src/main.c":               "#include <proj/api.h>\n",
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"compile_commands.json":        compileDBFor(checkout, "committed/include"),
		"committed/include/proj/api.h": "// committed\n",
		"src/main.c":                   "#include <proj/api.h>\n",
	})

	g := graph.New()
	seedCFile(g, "src/main.c")
	idx := sideChannelIndexer(t, g, checkout)
	idx.SetContentSource(snapshotOf(t, snapshot))

	idx.populateCppIncludeDirs(true)

	_, cached := cppIncludeDirsCached(checkout)
	assert.False(t, cached,
		"a build under a content source must not have gone through the working-copy loader")

	tus := loadCompileCommands(idx.manifestTree())
	require.Contains(t, tus, "src/main.c")
	assert.Equal(t, []string{"committed/include"}, tus["src/main.c"].includeDirs,
		"the include search path must be reconstructed from the snapshot's compile database")

	onDisk := loadCompileCommands(newDiskManifestTree(checkout))
	require.Contains(t, onDisk, "src/main.c")
	require.Equal(t, []string{"dirty/include"}, onDisk["src/main.c"].includeDirs,
		"the checkout really does hold a different answer")
}

// TestWorkingCopyBuildReadsTheCheckoutCompileDB is the other half of the
// contract: with no source installed the read is the checkout's, through the
// same cache it always used.
func TestWorkingCopyBuildReadsTheCheckoutCompileDB(t *testing.T) {
	checkout := t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{
		"compile_commands.json":    compileDBFor(checkout, "dirty/include"),
		"dirty/include/proj/api.h": "// dirty\n",
		"src/main.c":               "#include <proj/api.h>\n",
	})

	g := graph.New()
	seedCFile(g, "src/main.c")
	idx := sideChannelIndexer(t, g, checkout)
	idx.populateCppIncludeDirs(true)

	tus, cached := cppIncludeDirsCached(checkout)
	require.True(t, cached, "the working-copy loader caches per repo root")
	require.Contains(t, tus, "src/main.c")
	assert.Equal(t, []string{"dirty/include"}, tus["src/main.c"].includeDirs)
}

// TestCommittedBuildHeuristicIncludeDirsComeFromTheSnapshot covers the
// no-compile-database fallback: the conventional include roots are probed in
// the snapshot's directory layout, not the checkout's.
func TestCommittedBuildHeuristicIncludeDirsComeFromTheSnapshot(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	// The checkout's only conventional root is lib/; the snapshot's is
	// include/, which outranks it in the heuristic's priority order. Neither
	// tree holds both, so the two layouts disagree on the answer.
	writeSideChannelTree(t, checkout, map[string]string{
		"lib/proj/api.h": "// dirty\n",
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"include/proj/api.h": "// committed\n",
		"src/main.c":         "#include <proj/api.h>\n",
	})

	g := graph.New()
	seedCFile(g, "src/main.c")
	idx := sideChannelIndexer(t, g, checkout)
	idx.SetContentSource(snapshotOf(t, snapshot))

	idx.populateCppIncludeDirs(true)

	_, cached := cppIncludeDirsCached(checkout)
	assert.False(t, cached, "no compile database means no cached answer either, under a source")

	assert.Equal(t, []string{"include", "src"}, heuristicIncludeDirs(idx.manifestTree()),
		"the include-root heuristic must probe the snapshot's layout")
	assert.Equal(t, []string{"lib"}, heuristicIncludeDirs(newDiskManifestTree(checkout)),
		"the checkout really does hold a different layout")
}

// TestBuilderManifestSourceIsTheWholeTargetNotThePlan pins the seam the sparse
// builder installs at builder_generation.go: parsing is narrowed to the plan,
// while the manifests the resolver reads stay the whole selected target. A
// compile database outside the plan's file set is still the tree's compile
// database, and reading it through the narrowed source would find nothing.
func TestBuilderManifestSourceIsTheWholeTargetNotThePlan(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{
		"compile_commands.json": compileDBFor(checkout, "dirty/include"),
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"compile_commands.json": compileDBFor(checkout, "committed/include"),
		"src/main.c":            "#include <proj/api.h>\n",
	})

	target := snapshotOf(t, snapshot)
	idx := sideChannelIndexer(t, graph.New(), checkout)
	// Exactly what SparseGenerationBuilder.runPass installs: the plan for
	// parsing, the whole target for manifests.
	idx.setContentSourceWithManifests(newFileSetSource(target, []string{"src/main.c"}), target)

	tree := idx.manifestTree()
	require.True(t, tree.sourced(), "a build with a source installed must not fall back to the checkout")
	tus := loadCompileCommands(tree)
	require.Contains(t, tus, "src/main.c")
	assert.Equal(t, []string{"committed/include"}, tus["src/main.c"].includeDirs)

	// And the narrowed source really is narrow, so the manifest read could not
	// have come through it.
	narrowed := sourceManifestTree{rootPath: checkout, src: newFileSetSource(target, []string{"src/main.c"})}
	assert.False(t, narrowed.isFile("compile_commands.json"),
		"the plan's file set holds only what the pass parses")
}

// npmIndexer returns an indexer whose npm-alias resolution is the production
// callback, over the given checkout and optional snapshot.
func npmIndexer(t *testing.T, checkout string, snapshot source.ContentSource) *Indexer {
	t.Helper()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(func() { idx.Close() })
	idx.storeRootPath(checkout)
	if snapshot != nil {
		idx.SetContentSource(snapshot)
	}
	return idx
}

// TestCommittedBuildResolvesNpmAliasFromTheSnapshotManifest drives the
// resolver callback the Indexer installs (resolveNpmAliasImport) with a
// package.json that disagrees between the checkout and the snapshot.
func TestCommittedBuildResolvesNpmAliasFromTheSnapshotManifest(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{
		"package.json": `{"dependencies": {"shared": "npm:@dirty/lib@1.0.0"}}`,
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"package.json": `{"dependencies": {"shared": "npm:@committed/lib@1.0.0"}}`,
	})

	sourced := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	assert.Equal(t, "@committed/lib", sourced.resolveNpmAliasImport("app/main.ts", "shared"),
		"an npm alias must be read out of the tree the generation describes")

	live := npmIndexer(t, checkout, nil)
	assert.Equal(t, "@dirty/lib", live.resolveNpmAliasImport("app/main.ts", "shared"),
		"with no content source the checkout's manifest is the right one to read")
}

// TestCommittedBuildDeclaredDependencyComesFromTheSnapshotManifest covers the
// second manifest reader: the lookup that refuses to bind a bare specifier
// declared as an external dependency.
func TestCommittedBuildDeclaredDependencyComesFromTheSnapshotManifest(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{
		"package.json": `{"dependencies": {"ui": "^1.0.0"}}`,
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"package.json": `{"dependencies": {"ui": "workspace:*"}}`,
	})

	sourced := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	assert.False(t, sourced.declaresExternalNpmDep("app/main.ts", "ui"),
		"the snapshot declares ui as a workspace member, which resolves in-repo")

	live := npmIndexer(t, checkout, nil)
	assert.True(t, live.declaresExternalNpmDep("app/main.ts", "ui"),
		"the checkout declares ui as a registry range, which resolves outside the repo")
}

// TestCommittedBuildWorkspaceRewriteProbesTheSnapshot covers the three
// remaining manifest reads in one path: the root manifest's `workspaces`
// globs, the per-package manifest name, and the module-file existence probe
// that gates the rewrite.
func TestCommittedBuildWorkspaceRewriteProbesTheSnapshot(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{
		"package.json": `{"name": "root"}`,
	})
	writeSideChannelTree(t, snapshot, map[string]string{
		"package.json":             `{"name": "root", "workspaces": ["packages/*"]}`,
		"packages/ui/package.json": `{"name": "@acme/ui"}`,
		"packages/ui/index.ts":     "export const ui = 1;\n",
	})

	sourced := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	assert.Equal(t, "../packages/ui", sourced.resolveNpmAliasImport("app/main.ts", "@acme/ui"),
		"a workspace rewrite must be discovered and probed in the snapshot")

	live := npmIndexer(t, checkout, nil)
	assert.Equal(t, "", live.resolveNpmAliasImport("app/main.ts", "@acme/ui"),
		"the checkout declares no workspaces, so nothing is rewritten")
}

// TestIndexerManifestTreeFollowsTheInstalledSource pins the switch itself: no
// source is the working copy, a source is that snapshot, and a source with no
// manifest view still refuses the working copy rather than falling back.
func TestIndexerManifestTreeFollowsTheInstalledSource(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, map[string]string{"package.json": `{"name": "dirty"}`})
	writeSideChannelTree(t, snapshot, map[string]string{"package.json": `{"name": "committed"}`})

	idx := npmIndexer(t, checkout, nil)
	live := idx.manifestTree()
	require.False(t, live.sourced())
	data, ok := live.readFile("package.json")
	require.True(t, ok)
	assert.Contains(t, string(data), "dirty")

	idx.SetContentSource(snapshotOf(t, snapshot))
	sourced := idx.manifestTree()
	require.True(t, sourced.sourced())
	data, ok = sourced.readFile("package.json")
	require.True(t, ok)
	assert.Contains(t, string(data), "committed")

	// Source authority with nothing behind it answers nothing. It must never
	// degrade into a working-copy read, which is the whole hazard.
	empty := sourceManifestTree{rootPath: checkout}
	assert.True(t, empty.sourced())
	_, ok = empty.readFile("package.json")
	assert.False(t, ok)
	assert.False(t, empty.isFile("package.json"))
	assert.Empty(t, empty.matchFiles("*.json"))
	assert.Empty(t, empty.matchDirs("*"))
	assert.Empty(t, empty.topLevelDirs(".h"))
}

// TestManifestTreesAgreeAcrossDiskAndSnapshot pins that the two
// implementations answer the same questions the same way for one layout. The
// routing is only safe if swapping the reader does not change the answer for
// the tree both are looking at.
func TestManifestTreesAgreeAcrossDiskAndSnapshot(t *testing.T) {
	root := t.TempDir()
	writeSideChannelTree(t, root, map[string]string{
		"compile_commands.json":             "[]",
		"build-debug/compile_commands.json": "[]",
		"build/compile_commands.json":       "[]",
		"include/proj/api.h":                "// nested, not direct\n",
		"thirdparty/lib.hpp":                "// direct header\n",
		"docs/readme.md":                    "x\n",
		"packages/ui/package.json":          `{"name": "@acme/ui"}`,
		"packages/api/package.json":         `{"name": "@acme/api"}`,
	})
	disk := newDiskManifestTree(root)
	snap := sourceManifestTree{rootPath: root, src: snapshotOf(t, root)}

	assert.Equal(t,
		[]string{"build-debug/compile_commands.json", "build/compile_commands.json"},
		disk.matchFiles("build*/compile_commands.json"))
	assert.Equal(t, disk.matchFiles("build*/compile_commands.json"),
		snap.matchFiles("build*/compile_commands.json"))

	assert.Equal(t, []string{"packages/api", "packages/ui"}, disk.matchDirs("packages/*"))
	assert.Equal(t, disk.matchDirs("packages/*"), snap.matchDirs("packages/*"))

	assert.Equal(t, []string{"include"}, disk.matchDirs("include"))
	assert.Equal(t, disk.matchDirs("include"), snap.matchDirs("include"))
	assert.Empty(t, snap.matchDirs("absent"))

	assert.Equal(t,
		map[string]bool{"build": false, "build-debug": false, "docs": false,
			"include": false, "packages": false, "thirdparty": true},
		disk.topLevelDirs(cppHeaderExts...))
	assert.Equal(t, disk.topLevelDirs(cppHeaderExts...), snap.topLevelDirs(cppHeaderExts...))

	assert.True(t, disk.isFile("include/proj/api.h"))
	assert.True(t, snap.isFile("include/proj/api.h"))
	assert.False(t, disk.isFile("include"), "a directory is not content")
	assert.False(t, snap.isFile("include"))

	assert.Equal(t,
		[]string{"compile_commands.json", "build-debug/compile_commands.json", "build/compile_commands.json"},
		compileDBLocations(disk))
	assert.Equal(t, compileDBLocations(disk), compileDBLocations(snap))
}

// countingSource records how many entries a Walk actually visited.
type countingSource struct {
	source.ContentSource
	visited atomic.Int64
}

func (c *countingSource) Walk(ctx context.Context, fn func(source.FileMeta) error) error {
	return c.ContentSource.Walk(ctx, func(meta source.FileMeta) error {
		c.visited.Add(1)
		return fn(meta)
	})
}

func (c *countingSource) Open(p string) (io.ReadCloser, source.FileMeta, error) {
	return c.ContentSource.Open(p)
}

// TestSnapshotScanStopsAtTheGlobLiteralPrefix pins the cost bound the routing
// depends on. A source cannot list a directory, so a glob is answered by a
// walk — and a walk of the whole tree on every index pass would be a new cost
// the working-copy reader never paid. Walk's lexicographic order is part of
// the ContentSource contract, so the scan stops as soon as it passes the
// glob's literal prefix.
func TestSnapshotScanStopsAtTheGlobLiteralPrefix(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"build/compile_commands.json": "[]"}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		files["zzz/"+name+".c"] = "// filler\n"
	}
	writeSideChannelTree(t, root, files)

	counting := &countingSource{ContentSource: snapshotOf(t, root)}
	snap := sourceManifestTree{rootPath: root, src: counting}

	require.Equal(t, []string{"build/compile_commands.json"},
		snap.matchFiles("build*/compile_commands.json"))
	visited := counting.visited.Load()
	assert.LessOrEqual(t, visited, int64(2),
		"the scan must stop once it has passed the \"build\" block, not walk the whole tree")

	// A glob with no literal prefix is a whole walk, which is what the
	// top-level probe asks for — the bound is the prefix, not the method.
	counting.visited.Store(0)
	snap.topLevelDirs(".c")
	assert.Equal(t, int64(len(files)), counting.visited.Load())
}

// TestSourceConfigNarrowingNamesOnlyTheAdmissionReaders keeps the
// generation's own account of itself truthful in BOTH directions. The two
// admission rules that really are inert under a content source are named. The
// four build-configuration readers this item routed are not — and naming one
// of them would be worse than silence, because each decides where an import or
// include BINDS, so a reader off the wrong tree is a wrong edge, which is a
// resolution claim, not a configuration one.
func TestSourceConfigNarrowingNamesOnlyTheAdmissionReaders(t *testing.T) {
	assert.Contains(t, sourceConfigNarrowingReason, "ignore files")
	assert.Contains(t, sourceConfigNarrowingReason, "untracked-asset gate")
	for _, routed := range []string{"tsconfig", "jsconfig", "compile", "package.json", "alias"} {
		assert.NotContains(t, strings.ToLower(sourceConfigNarrowingReason), routed,
			"a routed reader must not be declared narrowed")
	}
}

// --- the working copy's own answers must not move -------------------------

// TestHeuristicIncludeRootFollowsASymlinkOnTheWorkingCopy pins the live
// index's side of the include-root heuristic. A conventional root has always
// been "does this NAME resolve to a directory" — os.Stat, which follows a
// symlink — while the "any other top-level directory holding a header" clause
// reads a directory listing, which calls a symlink a symlink. A repo that
// symlinks include/ at a generated or vendored tree is ordinary, and dropping
// that root costs the resolver the first entry of its ordered -I probe: a
// quoted or angle include then falls through to the suffix-unique fallback and
// either binds elsewhere or refuses on ambiguity.
func TestHeuristicIncludeRootFollowsASymlinkOnTheWorkingCopy(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	writeSideChannelTree(t, elsewhere, map[string]string{"proj/api.h": "int api(void);\n"})
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(root, "include")))
	writeSideChannelTree(t, root, map[string]string{"src/main.c": "#include <proj/api.h>\n"})

	disk := newDiskManifestTree(root)
	require.True(t, disk.isDir("include"), "os.Stat follows the link, which is the probe this clause always used")
	_, listed := disk.topLevelDirs(cppHeaderExts...)["include"]
	require.False(t, listed, "a directory listing reports the link as a link — this is why the second probe exists")

	assert.Equal(t, []string{"include", "src"}, heuristicIncludeDirs(disk),
		"a symlinked conventional root must stay on the ordered include path")
}

// TestHeuristicIncludeRootsSurviveAnUnlistableRoot covers the other half of the
// same divergence: the conventional-root clause never needed a listing of the
// repo root, so a root that can be traversed but not enumerated still answers.
func TestHeuristicIncludeRootsSurviveAnUnlistableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory read bit")
	}
	root := t.TempDir()
	writeSideChannelTree(t, root, map[string]string{
		"include/proj/api.h": "int api(void);\n",
		"src/main.c":         "#include <proj/api.h>\n",
	})
	require.NoError(t, os.Chmod(root, 0o111))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	disk := newDiskManifestTree(root)
	require.Nil(t, disk.topLevelDirs(cppHeaderExts...), "the root cannot be listed")
	assert.Equal(t, []string{"include", "src"}, heuristicIncludeDirs(disk),
		"the conventional roots are still reachable by name")
}

// TestSnapshotIncludeRootProbeCostsNoExtraWalk pins that the second probe is
// the working copy's alone. For a snapshot the enumeration is already complete
// — a source holds no directory entries, so a directory exists exactly when
// something in it does — and asking per absent root would cost a walk each.
func TestSnapshotIncludeRootProbeCostsNoExtraWalk(t *testing.T) {
	root := t.TempDir()
	writeSideChannelTree(t, root, map[string]string{
		"src/main.c":         "#include <proj/api.h>\n",
		"include/proj/api.h": "int api(void);\n",
	})
	counting := &countingSource{ContentSource: snapshotOf(t, root)}
	snap := sourceManifestTree{rootPath: root, src: counting}

	require.Equal(t, []string{"include", "src"}, heuristicIncludeDirs(snap))
	assert.Equal(t, int64(2), counting.visited.Load(),
		"one enumeration answers every conventional root; inc/api/lib are absent and must cost nothing")
}

// TestSnapshotMatchFilesAndIsFileAgreeOnASymlink pins that the snapshot's two
// content probes answer the same question. A source hands back a symlink's
// link text rather than the file's bytes, so isFile rejects one; matching it
// anyway would feed a reader a path string — a symlinked
// build/compile_commands.json would be "found" and then silently dropped by
// the JSON decode, which reads as "this tree has no compile database".
func TestSnapshotMatchFilesAndIsFileAgreeOnASymlink(t *testing.T) {
	root := t.TempDir()
	writeSideChannelTree(t, root, map[string]string{
		"real/compile_commands.json": "[]",
	})
	require.NoError(t, os.MkdirAll(filepath.Join(root, "build"), 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join(root, "real", compileDBName),
		filepath.Join(root, "build", compileDBName)))

	snap := sourceManifestTree{rootPath: root, src: snapshotOf(t, root)}
	require.False(t, snap.isFile("build/"+compileDBName), "a link's bytes are not the file's")
	assert.Empty(t, snap.matchFiles("build*/"+compileDBName),
		"matchFiles must not offer what isFile refuses")
	assert.Empty(t, compileDBLocations(snap))
}

// TestMultiIndexerManifestReadKeepsTheMetadataRoot pins which root an
// out-of-payload read for a tracked repo is addressed against. It has always
// been the tracked-repo metadata's root, and only an indexer that has declared
// SOURCE authority takes over — that is the single case where reading the
// working copy would hand a committed build the checkout's bytes. An indexer
// with no source installed must not silently move the read root, even though
// the two roots agree today.
func TestMultiIndexerManifestReadKeepsTheMetadataRoot(t *testing.T) {
	metaRoot, idxRoot, snapshot := t.TempDir(), t.TempDir(), t.TempDir()
	writeSideChannelTree(t, metaRoot, map[string]string{"package.json": `{"name":"from-metadata-root"}`})
	writeSideChannelTree(t, idxRoot, map[string]string{"package.json": `{"name":"from-indexer-root"}`})
	writeSideChannelTree(t, snapshot, map[string]string{"package.json": `{"name":"from-snapshot"}`})

	idx := sideChannelIndexer(t, graph.New(), idxRoot)
	mi := &MultiIndexer{
		repos:    map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: metaRoot}},
		indexers: map[string]*Indexer{"repo": idx},
	}

	data, ok := mi.readFileFromAnyRepo("repo/package.json")
	require.True(t, ok)
	assert.Contains(t, string(data), "from-metadata-root",
		"with no source installed the read stays on the metadata root it always used")

	idx.SetContentSource(snapshotOf(t, snapshot))
	data, ok = mi.readFileFromAnyRepo("repo/package.json")
	require.True(t, ok)
	assert.Contains(t, string(data), "from-snapshot",
		"source authority is the one thing that takes the read off the checkout")
}

// --- the built generation, end to end -------------------------------------

// sideChannelAliasEdge returns the target of the npm-alias-rewritten import
// edge out of src/app.ts in a reader. The rewrite is the observable: an
// aliased bare specifier is re-pointed at the package the manifest names
// before the import falls through to an external stub, so the stub's name IS
// the manifest the build read.
func sideChannelAliasEdge(t *testing.T, r graph.Reader) string {
	t.Helper()
	from := builderRepoPrefix + "/src/app.ts"
	for _, e := range r.AllEdges() {
		if e == nil || e.From != from || e.Kind != graph.EdgeImports {
			continue
		}
		if strings.HasPrefix(e.To, "external::@acme/") {
			return e.To
		}
	}
	t.Fatalf("no rewritten import edge out of %s; the fixture stopped exercising the npm alias", from)
	return ""
}

// sideChannelAliasTree is one tree of the npm-alias fixture: a root manifest
// that aliases the bare specifier "shared" onto a package name, and the one
// module that imports it.
func sideChannelAliasTree(pkg, marker string) map[string]string {
	return map[string]string{
		"package.json": `{"name":"root","dependencies":{"shared":"npm:` + pkg + `@1.0.0"}}`,
		"src/app.ts":   "import { x } from 'shared';\nexport const y = x + " + marker + ";\n",
	}
}

// TestCommitLayerAliasResolutionComesFromTheCommittedTree is the gate-1 oracle
// for this item: a commit layer for tree T, built against a working copy that
// has DIVERGED from T, must resolve its npm aliases exactly as a fresh
// isolated index of T does — not as the checkout on disk would.
//
// The discriminator is the root package.json, which the npm-alias index reads
// outside the indexed payload. The alias rewrite runs inside the ordinary
// import resolution (resolver.go resolveImport), so the manifest the build
// read is legible in the built generation's own edges: the rewritten
// specifier IS the package name the manifest declared.
func TestCommitLayerAliasResolutionComesFromTheCommittedTree(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	treeAFiles := sideChannelAliasTree("@acme/checkout", "0")
	treeBFiles := sideChannelAliasTree("@acme/committed", "1")

	builderWriteTree(t, repoDir, treeAFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	builderWriteTree(t, repoDir, treeBFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	// The working copy the build is handed still holds tree A's manifest.
	checkout := builderTempDir(t, "checkout")
	builderWriteTree(t, checkout, treeAFiles)

	store := builderOpenStore(t, "base")
	builderIndex(t, store, checkout)
	require.Equal(t, "external::@acme/checkout",
		sideChannelAliasEdge(t, store.AtGeneration(0)),
		"the checkout on disk must genuinely answer something else")

	generationID, _, err := builderNewBuilder(store).BuildCommitLayer(
		context.Background(), CommitLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
				LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture",
			},
			Base:          store,
			RepoDir:       repoDir,
			BaseTreeOID:   treeA,
			TargetTreeOID: treeB,
			RootPath:      checkout,
			RepoPrefix:    builderRepoPrefix,
			WorkspaceID:   builderRepoPrefix,
			ProjectID:     builderRepoPrefix,
		})
	require.NoError(t, err)

	// The reference: tree T indexed on its own, with nothing else on disk.
	fresh := builderTempDir(t, "fresh")
	builderWriteTree(t, fresh, treeBFiles)
	freshStore := builderOpenStore(t, "fresh")
	builderIndex(t, freshStore, fresh)
	want := sideChannelAliasEdge(t, freshStore.AtGeneration(0))
	require.Equal(t, "external::@acme/committed", want,
		"the reference index of the tree reads the tree's own manifest")

	got := sideChannelAliasEdge(t, builderComposed(t, store, generationID))
	assert.Equal(t, want, got,
		"the commit layer must resolve the alias the way an isolated index of its tree does")
	assert.NotEqual(t, "external::@acme/checkout", got,
		"@acme/checkout is what the divergent working copy on disk would have answered")
}

// TestBuiltGenerationDeclaresTheSourceConfigNarrowing reads the declaration
// back off a published generation rather than off the constant. The narrowing
// is only worth anything if a reader of the generation can see it — and only
// truthful if it stops at the readers that really are narrowed: the two
// admission rules, and none of the four build-configuration readers this item
// routed through the content source.
func TestBuiltGenerationDeclaresTheSourceConfigNarrowing(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	checkout := builderTempDir(t, "checkout")
	builderWriteTree(t, checkout, builderTreeA())

	builderWriteTree(t, repoDir, builderTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, checkout)

	generationID, _, err := builderNewBuilder(store).BuildCommitLayer(
		context.Background(), CommitLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
				LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture",
			},
			Base:          store,
			RepoDir:       repoDir,
			BaseTreeOID:   treeA,
			TargetTreeOID: treeB,
			RootPath:      checkout,
			RepoPrefix:    builderRepoPrefix,
			WorkspaceID:   builderRepoPrefix,
			ProjectID:     builderRepoPrefix,
		})
	require.NoError(t, err)

	rows, err := store.AtGeneration(generationID).ProducerStates()
	require.NoError(t, err)
	var config store_sqlite.ProducerCompleteness
	var found bool
	for _, row := range rows {
		if row.Producer == string(graphview.CapSourceConfig) {
			config, found = row, true
		}
	}
	require.True(t, found, "the generation says nothing about source.config")
	assert.Equal(t, sourceConfigNarrowingReason, config.Reason,
		"the stored row must carry the narrowing, not only the constant")
	assert.Contains(t, config.Reason, "untracked-asset gate",
		"an admission rule that is inert under a content source must be named")
	for _, routed := range []string{"tsconfig", "jsconfig", "compile", "package.json"} {
		assert.NotContains(t, strings.ToLower(config.Reason), routed,
			"a routed reader must not be declared narrowed on the stored row either")
	}

	// The other half of the claim: nothing moved the narrowing onto
	// resolution, because there is no residual reader to declare there.
	var resolution store_sqlite.ProducerCompleteness
	for _, row := range rows {
		if row.Producer == string(graphview.CapResolutionLocal) {
			resolution = row
		}
	}
	assert.Equal(t, store_sqlite.ProducerStateComplete, resolution.State)
	assert.Empty(t, resolution.Reason)
}

// --- the tsconfig / jsconfig path-alias channel ---------------------------

// tsAliasTree is one tree of the path-alias fixture: a tsconfig whose "@x"
// alias names ONE of two really-present modules, plus both modules and the
// file that imports the alias. Which module the edge lands on is therefore
// decided by the config alone — which is exactly the question "whose tsconfig
// did this build read".
func tsAliasTree(target, marker string) map[string]string {
	return map[string]string{
		"tsconfig.json": `{"compilerOptions":{"baseUrl":".","paths":{"@x":["` + target + `"]}}}`,
		// Every file carries the marker so the whole fixture is in the
		// tree-to-tree diff. A file the commit layer does not re-derive keeps
		// the BASE layer's payload, and the base was indexed off the checkout:
		// an unchanged app.ts would keep the checkout's edge, and an unchanged
		// alias TARGET would not be a node the build's own graph can bind to.
		"src/a/x.ts": "export const x = 1 + " + marker + ";\n",
		"src/b/x.ts": "export const x = 2 + " + marker + ";\n",
		"src/app.ts": "import { x } from '@x';\nexport const y = x + " + marker + ";\n",
	}
}

// tsAliasImportEdge returns the target of the import edge out of src/app.ts
// that the path-alias expansion resolved. A resolved alias lands on the
// indexed FILE NODE it names — not on an external stub — so the edge is a
// direct statement about which tsconfig the build read.
func tsAliasImportEdge(t *testing.T, r graph.Reader) string {
	t.Helper()
	from := builderRepoPrefix + "/src/app.ts"
	for _, e := range r.AllEdges() {
		if e == nil || e.From != from || e.Kind != graph.EdgeImports {
			continue
		}
		if strings.Contains(e.To, "/src/a/x") || strings.Contains(e.To, "/src/b/x") {
			return e.To
		}
	}
	t.Fatalf("no path-alias import edge out of %s; the fixture stopped exercising the alias", from)
	return ""
}

// TestCommitLayerPathAliasResolutionComesFromTheCommittedTree is the gate-1
// oracle for the tsconfig / jsconfig channel: a commit layer for tree T, built
// against a working copy that has DIVERGED from T, must expand its path
// aliases exactly as a fresh isolated index of T does.
//
// Unlike the npm-alias oracle below it, the observable here is a RESOLVED
// edge: the alias expansion runs inside resolveImport (resolveJSTSImportTarget
// → the installed PathAliasResolver), and its target is the indexed file node
// the config names. A build that read the checkout's tsconfig lands the edge
// on the other file — a present, wrong edge.
func TestCommitLayerPathAliasResolutionComesFromTheCommittedTree(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	treeAFiles := tsAliasTree("src/a/x.ts", "0")
	treeBFiles := tsAliasTree("src/b/x.ts", "1")

	builderWriteTree(t, repoDir, treeAFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	builderWriteTree(t, repoDir, treeBFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	// The working copy the build is handed still holds tree A's tsconfig.
	checkout := builderTempDir(t, "checkout")
	builderWriteTree(t, checkout, treeAFiles)

	store := builderOpenStore(t, "base")
	builderIndex(t, store, checkout)
	require.Equal(t, builderRepoPrefix+"/src/a/x.ts",
		tsAliasImportEdge(t, store.AtGeneration(0)),
		"the checkout on disk must genuinely answer something else")

	generationID, _, err := builderNewBuilder(store).BuildCommitLayer(
		context.Background(), CommitLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
				LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture",
			},
			Base:          store,
			RepoDir:       repoDir,
			BaseTreeOID:   treeA,
			TargetTreeOID: treeB,
			RootPath:      checkout,
			RepoPrefix:    builderRepoPrefix,
			WorkspaceID:   builderRepoPrefix,
			ProjectID:     builderRepoPrefix,
		})
	require.NoError(t, err)

	// The reference: tree T indexed on its own, with nothing else on disk.
	fresh := builderTempDir(t, "fresh")
	builderWriteTree(t, fresh, treeBFiles)
	freshStore := builderOpenStore(t, "fresh")
	builderIndex(t, freshStore, fresh)
	want := tsAliasImportEdge(t, freshStore.AtGeneration(0))
	require.Equal(t, builderRepoPrefix+"/src/b/x.ts", want,
		"the reference index of the tree reads the tree's own tsconfig")

	got := tsAliasImportEdge(t, builderComposed(t, store, generationID))
	assert.Equal(t, want, got,
		"the commit layer must expand the alias the way an isolated index of its tree does")
	assert.NotEqual(t, builderRepoPrefix+"/src/a/x.ts", got,
		"src/a/x.ts is the file the divergent working copy's tsconfig names")
}

// TestCommittedBuildPathAliasComesFromTheSnapshotConfig drives the resolver
// callback the Indexer installs (resolvePathAliasImport, wired at
// indexer.go New → SetPathAliasResolver) with a tsconfig that disagrees
// between the checkout and the snapshot, plus the unsourced control.
func TestCommittedBuildPathAliasComesFromTheSnapshotConfig(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, tsAliasTree("src/a/x.ts", "0"))
	writeSideChannelTree(t, snapshot, tsAliasTree("src/b/x.ts", "1"))
	t.Cleanup(func() { forgetTSAliasCache(checkout, snapshot) })

	live := npmIndexer(t, checkout, nil)
	assert.Equal(t, "src/a/x", live.resolvePathAliasImport("src/app.ts", "@x"),
		"with no source installed the alias comes from the checkout")

	committed := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	assert.Equal(t, "src/b/x", committed.resolvePathAliasImport("src/app.ts", "@x"),
		"a build that declared source authority expands the alias through its own tree")
}

// forgetTSAliasCache drops the process-wide working-copy entries a test
// planted, so one test's temp root cannot answer another's.
func forgetTSAliasCache(roots ...string) {
	tsAliasCacheMu.Lock()
	defer tsAliasCacheMu.Unlock()
	for _, root := range roots {
		delete(tsAliasCache, root)
	}
}

// tsAliasCacheEntry reports what the process-wide cache holds for a root.
func tsAliasCacheEntry(root string) (*tsalias.Collection, bool) {
	tsAliasCacheMu.Lock()
	defer tsAliasCacheMu.Unlock()
	c, ok := tsAliasCache[root]
	return c, ok
}

// TestSnapshotAliasScopesNeitherReadNorFillTheProcessCache pins the cache
// keying. tsAliasCache is keyed by repo ROOT and never invalidated, so a
// committed build and the live index at the same root would share one
// checkout-derived Collection. A sourced load must ignore a primed entry for
// its root and must not leave one behind.
func TestSnapshotAliasScopesNeitherReadNorFillTheProcessCache(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, tsAliasTree("src/a/x.ts", "0"))
	writeSideChannelTree(t, snapshot, tsAliasTree("src/b/x.ts", "1"))
	t.Cleanup(func() { forgetTSAliasCache(checkout, snapshot) })

	// Prime the root's entry with the checkout's answer, exactly as a live
	// index of the same checkout would have.
	live := npmIndexer(t, checkout, nil)
	require.Equal(t, "src/a/x", live.resolvePathAliasImport("src/app.ts", "@x"))
	primed, ok := tsAliasCacheEntry(checkout)
	require.True(t, ok, "the working-copy load must still use the shared cache")

	committed := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	assert.Equal(t, "src/b/x", committed.resolvePathAliasImport("src/app.ts", "@x"),
		"a primed root entry must not decide a snapshot's aliases")

	after, ok := tsAliasCacheEntry(checkout)
	assert.True(t, ok)
	assert.Same(t, primed, after, "a sourced load must not overwrite the root's entry")
	_, ok = tsAliasCacheEntry(snapshot)
	assert.False(t, ok, "a sourced load must not fill the cache under any root")
}

// TestIndexerAliasScopesFollowASwappedSource pins the memo's key. The scopes a
// sourced Indexer holds are keyed on the installed source ref, so swapping the
// source reloads them rather than serving the previous snapshot's answer.
func TestIndexerAliasScopesFollowASwappedSource(t *testing.T) {
	checkout, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	writeSideChannelTree(t, checkout, tsAliasTree("src/a/x.ts", "0"))
	writeSideChannelTree(t, first, tsAliasTree("src/a/x.ts", "0"))
	writeSideChannelTree(t, second, tsAliasTree("src/b/x.ts", "1"))
	t.Cleanup(func() { forgetTSAliasCache(checkout, first, second) })

	idx := npmIndexer(t, checkout, snapshotOf(t, first))
	require.Equal(t, "src/a/x", idx.resolvePathAliasImport("src/app.ts", "@x"))

	idx.SetContentSource(snapshotOf(t, second))
	assert.Equal(t, "src/b/x", idx.resolvePathAliasImport("src/app.ts", "@x"),
		"the memo is keyed on the installed source, not on the root")
}

// TestAliasScopesAgreeAcrossDiskAndSnapshot is the loader's own agreement
// pin: one layout, two readers, the same scopes. It covers the three places
// the two could diverge — which directories the walk refuses to descend,
// where a nested scope is rooted, and how a multi-target alias is grounded.
func TestAliasScopesAgreeAcrossDiskAndSnapshot(t *testing.T) {
	root := t.TempDir()
	writeSideChannelTree(t, root, map[string]string{
		"tsconfig.json": `{"compilerOptions":{"baseUrl":".","paths":` +
			`{"@x":["src/b/x.ts"],"@multi/*":["absent/*","src/b/*"]}}}`,
		"packages/web/tsconfig.json": `{"compilerOptions":{"paths":{"@x":["nested/x.ts"]}}}`,
		"node_modules/dep/tsconfig.json": `{"compilerOptions":{"paths":` +
			`{"@x":["should/not/be/used.ts"]}}}`,
		"src/b/x.ts":               "export const x = 1;\n",
		"packages/web/nested/x.ts": "export const x = 2;\n",
	})
	t.Cleanup(func() { forgetTSAliasCache(root) })

	disk := tsAliasCollectionForTree(newDiskManifestTree(root))
	snap := tsAliasCollectionForTree(sourceManifestTree{rootPath: root, src: snapshotOf(t, root)})
	require.NotNil(t, disk)
	require.NotNil(t, snap)
	require.Len(t, disk.Maps(), 2, "the node_modules config is not a scope")
	require.Equal(t, len(disk.Maps()), len(snap.Maps()))

	for _, probe := range []struct{ file, spec string }{
		{"src/app.ts", "@x"},
		{"src/app.ts", "@multi/x"},
		{"packages/web/src/app.ts", "@x"},
		{"src/app.ts", "react"},
	} {
		want := resolveTSPathAlias(disk, "", probe.file, probe.spec)
		got := resolveTSPathAlias(snap, "", probe.file, probe.spec)
		assert.Equal(t, want, got, "%s / %s", probe.file, probe.spec)
	}
	assert.Equal(t, "src/b/x", resolveTSPathAlias(snap, "", "src/app.ts", "@x"))
	assert.Equal(t, "src/b/x", resolveTSPathAlias(snap, "", "src/app.ts", "@multi/x"),
		"a multi-target alias is grounded in the tree it was loaded from")
	assert.Equal(t, "packages/web/nested/x", resolveTSPathAlias(snap, "", "packages/web/src/app.ts", "@x"))
}

// TestSourcedTreeThatCannotEnumerateAnswersNoScopes pins the refusal: a tree
// that declared source authority must never fall back to the checkout, even
// when the source behind it can answer nothing.
func TestSourcedTreeThatCannotEnumerateAnswersNoScopes(t *testing.T) {
	root := t.TempDir()
	writeSideChannelTree(t, root, tsAliasTree("src/a/x.ts", "0"))
	t.Cleanup(func() { forgetTSAliasCache(root) })

	require.NotNil(t, tsAliasCollectionForTree(newDiskManifestTree(root)))
	assert.Nil(t, tsAliasCollectionForTree(sourceManifestTree{rootPath: root}),
		"a sourced tree with nothing behind it answers no scopes, not the checkout's")
}

// TestMultiIndexerAliasScopesPreferASourcedIndexer covers the multi-repo
// entry point in both directions, through the function production actually
// installs (MultiIndexer.pathAliasResolver → tsAliasMapFor): the tracked-repo
// metadata's root answers unless the registered indexer has declared SOURCE
// authority — the one case where reading the working copy would hand a
// committed build the checkout's config.
func TestMultiIndexerAliasScopesPreferASourcedIndexer(t *testing.T) {
	metaRoot, snapshot := t.TempDir(), t.TempDir()
	writeSideChannelTree(t, metaRoot, tsAliasTree("src/a/x.ts", "0"))
	writeSideChannelTree(t, snapshot, tsAliasTree("src/b/x.ts", "1"))
	t.Cleanup(func() { forgetTSAliasCache(metaRoot, snapshot) })

	idx := sideChannelIndexer(t, graph.New(), metaRoot)
	mi := &MultiIndexer{
		repos:    map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: metaRoot}},
		indexers: map[string]*Indexer{"repo": idx},
	}
	resolve := mi.pathAliasResolver()

	assert.Equal(t, "repo/src/a/x", resolve("repo/src/app.ts", "@x"),
		"an indexer with no source installed leaves the metadata root in charge")

	idx.SetContentSource(snapshotOf(t, snapshot))
	assert.Equal(t, "repo/src/b/x", resolve("repo/src/app.ts", "@x"),
		"source authority takes over, through the owning indexer's own memo")
}

// --- the C/C++ compile database, end to end -------------------------------

// cppSearchPathRecord is one installation of a reconstructed C/C++ include
// search path onto a resolver: which root the Indexer was indexing, whether it
// was reading a snapshot, and the two halves of the path it installed.
type cppSearchPathRecord struct {
	root     string
	sourced  bool
	perFile  map[string][]string
	fallback []string
}

// recordCppSearchPaths swaps the install seam for the test's lifetime and
// returns a reader over everything a build installed while it was in place.
// This is how a REAL build's include search path is observed: the resolver
// exposes no reader for what it was handed, and — see
// TestCppIncludeEdgesStayStubsBeforeTheSearchPathIsRead — the pass that would
// consume it does not run in a whole index today, so the built generation's
// own include edges cannot tell two compile databases apart.
func recordCppSearchPaths(t *testing.T) func() []cppSearchPathRecord {
	t.Helper()
	var mu sync.Mutex
	var records []cppSearchPathRecord
	previous := installCppIncludeSearchPath
	installCppIncludeSearchPath = func(idx *Indexer, perFile map[string][]string, fallback []string) {
		mu.Lock()
		records = append(records, cppSearchPathRecord{
			root:     idx.rootPath,
			sourced:  idx.manifestTree().sourced(),
			perFile:  perFile,
			fallback: fallback,
		})
		mu.Unlock()
		previous(idx, perFile, fallback)
	}
	t.Cleanup(func() { installCppIncludeSearchPath = previous })
	return func() []cppSearchPathRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]cppSearchPathRecord(nil), records...)
	}
}

// lastCppSearchPath returns the last search path installed by a build that
// was (or was not) reading a snapshot.
func lastCppSearchPath(t *testing.T, records []cppSearchPathRecord, sourced bool) cppSearchPathRecord {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].sourced == sourced {
			return records[i]
		}
	}
	t.Fatalf("no include search path was installed by a sourced=%v build (%d records)", sourced, len(records))
	return cppSearchPathRecord{}
}

// cppSideChannelTree is one tree of the compile-database fixture: two include
// roots that both really hold the header, and a compile database naming ONE of
// them. Which root the TU searches is therefore decided by the database alone.
func cppSideChannelTree(includeDir, marker string) map[string]string {
	return map[string]string{
		// No "directory": a database recorded with the absolute paths of the
		// machine that generated it describes no other checkout at all (every
		// path falls outside the repo root and is dropped). A committed
		// database that two trees can share is root-relative, which is also
		// what makes this fixture identical in both trees bar the include root.
		"build/compile_commands.json": `[{"file": "src/main.c",` +
			` "arguments": ["cc", "-I` + includeDir + `", "-c", "src/main.c"]}]`,
		"include/proj/api.h": "#define API 1\n",
		"inc/proj/api.h":     "#define API 2\n",
		"src/main.c":         "#include \"proj/api.h\"\nint main(void){return " + marker + ";}\n",
	}
}

// TestCommitLayerCppIncludeSearchPathComesFromTheCommittedTree is the gate-1
// oracle for the compile-database channel: a commit layer for tree T, built
// against a working copy whose compile_commands.json names a DIFFERENT include
// root, must reconstruct the same include search path a fresh isolated index
// of T reconstructs.
//
// The compile database records absolute paths under a build directory that is
// not the checkout, which is what a database generated in CI and committed
// holds; both trees' databases are identical apart from the include root, so
// the only thing that can decide the answer is which tree was read.
func TestCommitLayerCppIncludeSearchPathComesFromTheCommittedTree(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	treeAFiles := cppSideChannelTree("inc", "0")
	treeBFiles := cppSideChannelTree("include", "1")

	builderWriteTree(t, repoDir, treeAFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	builderWriteTree(t, repoDir, treeBFiles)
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	checkout := builderTempDir(t, "checkout")
	builderWriteTree(t, checkout, treeAFiles)
	t.Cleanup(func() { clearCppIncludeDirCache(checkout) })

	records := recordCppSearchPaths(t)

	store := builderOpenStore(t, "base")
	builderIndex(t, store, checkout)
	onDisk := lastCppSearchPath(t, records(), false)
	require.Equal(t,
		map[string][]string{builderRepoPrefix + "/src/main.c": {builderRepoPrefix + "/inc"}},
		onDisk.perFile,
		"the divergent working copy must genuinely reconstruct a different search path")

	generationID, _, err := builderNewBuilder(store).BuildCommitLayer(
		context.Background(), CommitLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind: "dedicated_graph", GraphID: "graph-fixture",
				LayerID: "layer-" + treeB, CheckoutID: "checkout-fixture",
			},
			Base:          store,
			RepoDir:       repoDir,
			BaseTreeOID:   treeA,
			TargetTreeOID: treeB,
			RootPath:      checkout,
			RepoPrefix:    builderRepoPrefix,
			WorkspaceID:   builderRepoPrefix,
			ProjectID:     builderRepoPrefix,
		})
	require.NoError(t, err)
	built := lastCppSearchPath(t, records(), true)
	require.Equal(t, checkout, built.root,
		"the build indexes the checkout root — which is why its READS have to come from the source")

	// The reference: tree T indexed on its own, with nothing else on disk.
	fresh := builderTempDir(t, "fresh")
	builderWriteTree(t, fresh, treeBFiles)
	t.Cleanup(func() { clearCppIncludeDirCache(fresh) })
	freshStore := builderOpenStore(t, "fresh")
	builderIndex(t, freshStore, fresh)
	want := lastCppSearchPath(t, records(), false)
	require.Equal(t, fresh, want.root)
	require.Equal(t,
		map[string][]string{builderRepoPrefix + "/src/main.c": {builderRepoPrefix + "/include"}},
		want.perFile,
		"the reference index of the tree reads the tree's own compile database")

	assert.Equal(t, want.perFile, built.perFile,
		"the commit layer must reconstruct the include search path an isolated index of its tree does")
	assert.Equal(t, want.fallback, built.fallback)
	assert.NotEqual(t, onDisk.perFile, built.perFile,
		"the checkout's compile database is what the build must NOT have read")

	// The generation's own include edges are equal too — but see
	// TestCppIncludeEdgesStayStubsBeforeTheSearchPathIsRead for why that
	// equality is not, today, evidence about the compile database.
	assert.Equal(t,
		cppIncludeEdgeTargets(t, builderComposed(t, store, generationID)),
		cppIncludeEdgeTargets(t, freshStore.AtGeneration(0)))
}

// cppIncludeEdgeTargets returns every C-include import edge out of src/main.c
// as "<target>|<include_dir>", so a resolved include and an external stub are
// distinguishable.
func cppIncludeEdgeTargets(t *testing.T, r graph.Reader) []string {
	t.Helper()
	from := builderRepoPrefix + "/src/main.c"
	var out []string
	for _, e := range r.AllEdges() {
		if e == nil || e.From != from || e.Kind != graph.EdgeImports {
			continue
		}
		dir, _ := e.Meta["include_dir"].(string)
		out = append(out, e.To+"|"+dir)
	}
	sort.Strings(out)
	return out
}

// TestCppIncludeEdgesStayStubsBeforeTheSearchPathIsRead is a CANARY, not an
// endorsement. It records a PRE-EXISTING defect this item measured and does
// not cause: in a whole index, a C-family `#include` edge is stubbed to
// `external::<path>` by resolveImport (resolver.go, the `import::` arm of the
// per-edge cascade) during the main resolve loop, which runs BEFORE
// resolveRelativeImports — and that pass's C branch only fires for a target
// still carrying the `unresolved::import::` prefix (relative_imports.go). The
// reconstructed `-I` search path therefore reaches no consumer in the whole
// index path, and no include edge is ever stamped with include_dir /
// resolved_via.
//
// The consequence for THIS item: a built generation's include edges cannot
// tell two compile databases apart, so the compile-database oracle above has
// to observe the search path the build installed instead.
//
// If this test fails, the include pass has become reachable — which is good
// news. Upgrade TestCommitLayerCppIncludeSearchPathComesFromTheCommittedTree
// to assert the composed generation's include_dir meta directly, and delete
// this canary.
func TestCppIncludeEdgesStayStubsBeforeTheSearchPathIsRead(t *testing.T) {
	dir := builderTempDir(t, "cstub")
	builderWriteTree(t, dir, cppSideChannelTree("include", "0"))
	t.Cleanup(func() { clearCppIncludeDirCache(dir) })
	store := builderOpenStore(t, "cstub")
	builderIndex(t, store, dir)

	assert.Equal(t, []string{"external::proj/api.h|"}, cppIncludeEdgeTargets(t, store.AtGeneration(0)),
		"a whole index still stubs C includes before the -I search path is consulted; "+
			"if this now resolves, upgrade the compile-database oracle and drop this canary")
}
