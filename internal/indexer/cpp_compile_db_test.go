package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compileDBDir renders an absolute directory the way a compile_commands.json
// carries it: forward-separated, so the JSON body stays valid. A Windows
// t.TempDir() is "C:\Users\...\001", and pasting those backslashes into a JSON
// string literal makes "\U" / "\A" invalid escapes — the whole database then
// fails to parse and every translation unit disappears. CMake and Ninja emit
// forward slashes on Windows for the same reason, and filepath.IsAbs / Join /
// Rel all accept that spelling.
func compileDBDir(root string) string { return filepath.ToSlash(root) }

// toolchainIncludeDir is an absolute include directory outside the repository,
// the shape a `-isystem` flag carries. Prefixing the root's volume is what makes
// it absolute on Windows: filepath.IsAbs("/usr/include") is false there, so the
// bare POSIX spelling would be joined onto the build directory and land back
// INSIDE the repo instead of being dropped. VolumeName is "" on POSIX, so the
// spelling is unchanged there.
func toolchainIncludeDir(root string) string {
	return filepath.ToSlash(filepath.VolumeName(root)) + "/usr/include"
}

// writeCompileDB writes a compile_commands.json at path and registers cleanup of
// the per-root include-dir cache so each test starts cold.
func writeCompileDB(t *testing.T, repoRoot, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	t.Cleanup(func() { clearCppIncludeDirCache(repoRoot) })
}

func TestLoadCompileCommands_CommandString(t *testing.T) {
	root := t.TempDir()
	dbDir := compileDBDir(root)
	writeCompileDB(t, root, filepath.Join(root, "compile_commands.json"), `[
	  {
	    "directory": "`+dbDir+`/build",
	    "file": "../src/main.c",
	    "command": "clang -I../gen/include -isystem `+toolchainIncludeDir(root)+` -c ../src/main.c -o main.o"
	  }
	]`)

	tus := loadCompileCommands(newDiskManifestTree(root))
	tu, ok := tus["src/main.c"]
	require.True(t, ok, "main.c TU resolved repo-relative from build/ directory")
	assert.Equal(t, "src/main.c", tu.file)
	// `-I../gen/include` becomes repo-relative; the toolchain `-isystem` path is
	// outside the repo and dropped.
	assert.Equal(t, []string{"gen/include"}, tu.includeDirs)
}

func TestLoadCompileCommands_ArgumentsArray(t *testing.T) {
	root := t.TempDir()
	dbDir := compileDBDir(root)
	writeCompileDB(t, root, filepath.Join(root, "compile_commands.json"), `[
	  {
	    "directory": "`+dbDir+`",
	    "file": "src/main.cpp",
	    "arguments": ["clang++", "-Igen/include", "-I", "vendor/inc", "-iquote", "local", "-c", "src/main.cpp"]
	  }
	]`)

	tus := loadCompileCommands(newDiskManifestTree(root))
	tu, ok := tus["src/main.cpp"]
	require.True(t, ok)
	assert.Equal(t, []string{"gen/include", "vendor/inc", "local"}, tu.includeDirs)
}

func TestLoadCompileCommands_BuildDirGlob(t *testing.T) {
	root := t.TempDir()
	dbDir := compileDBDir(root)
	// No root compile_commands.json — only build-debug/compile_commands.json.
	writeCompileDB(t, root, filepath.Join(root, "build-debug", "compile_commands.json"), `[
	  {
	    "directory": "`+dbDir+`/build-debug",
	    "file": "../src/app.c",
	    "command": "cc -I../include -c ../src/app.c"
	  }
	]`)

	tus := loadCompileCommands(newDiskManifestTree(root))
	tu, ok := tus["src/app.c"]
	require.True(t, ok, "TU discovered via build*/compile_commands.json glob")
	assert.Equal(t, []string{"include"}, tu.includeDirs)
}

func TestLoadCompileCommands_None(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { clearCppIncludeDirCache(root) })
	assert.Empty(t, loadCompileCommands(newDiskManifestTree(root)), "no compile_commands.json yields empty map")
}

// TestHeuristicIncludeDirs pins the no-compile-DB fallback: conventional roots
// in priority order, followed by any other top-level dir directly containing a
// C/C++ header; dirs without headers and dotfile dirs are skipped.
func TestHeuristicIncludeDirs(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) {
		require.NoError(t, os.MkdirAll(filepath.Join(append([]string{root}, parts...)...), 0o755))
	}
	wr := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}

	mk("include", "proj") // conventional; header is nested, not direct
	wr("include/proj/api.h", "// h")
	mk("src") // conventional, exists
	mk("thirdparty")
	wr("thirdparty/lib.hpp", "// h") // non-conventional dir with a direct header
	mk("docs")
	wr("docs/readme.md", "x") // no header → ignored
	mk(".git")
	wr(".git/x.h", "x") // dotfile dir → skipped

	got := heuristicIncludeDirs(newDiskManifestTree(root))
	assert.Equal(t, []string{"include", "src", "thirdparty"}, got)
}

func TestHeuristicIncludeDirs_Empty(t *testing.T) {
	assert.Nil(t, heuristicIncludeDirs(newDiskManifestTree("")))
	assert.Empty(t, heuristicIncludeDirs(newDiskManifestTree(t.TempDir())), "no conventional roots and no header dirs")
}

// TestLoadCompileCommands_CacheAndClear pins the cache-and-invalidate contract:
// a second load returns the cached include dirs even after the file changes on
// disk, and clearing the cache (as an incremental reindex does) re-reads it —
// so an edited compile_commands.json takes effect without a daemon restart.
func TestLoadCompileCommands_CacheAndClear(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compile_commands.json")
	dbDir := compileDBDir(root)
	t.Cleanup(func() { clearCppIncludeDirCache(root) })

	// write replaces the DB body while preserving its modtime, so this test
	// isolates the explicit-clear path from the mtime-keyed reload (covered by
	// TestLoadCompileCommands_MtimeReload).
	write := func(incDir string) {
		var keep time.Time
		if fi, err := os.Stat(path); err == nil {
			keep = fi.ModTime()
		}
		body := `[{"directory":"` + dbDir + `","file":"src/main.c","command":"cc -I` + incDir + ` -c src/main.c"}]`
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		if !keep.IsZero() {
			require.NoError(t, os.Chtimes(path, keep, keep))
		}
	}

	write("inc1")
	require.Equal(t, []string{"inc1"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs)

	// Editing the DB on disk (without bumping its modtime) is not observed until
	// the cache is invalidated.
	write("inc2")
	assert.Equal(t, []string{"inc1"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs,
		"second load returns the cached result")

	// Clearing the cache (the incremental-reindex hook) re-reads the new dir.
	clearCppIncludeDirCache(root)
	assert.Equal(t, []string{"inc2"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs,
		"after clear the edited -I dir is picked up")
}

// TestLoadCompileCommands_MtimeReload pins the mtime-keyed invalidation: editing
// only compile_commands.json (changing an -I dir) and re-loading picks up the new
// include dir without any explicit cache clear / full reindex — the cache reloads
// because the file's modtime advanced.
func TestLoadCompileCommands_MtimeReload(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compile_commands.json")
	dbDir := compileDBDir(root)
	t.Cleanup(func() { clearCppIncludeDirCache(root) })

	write := func(incDir string, mtime time.Time) {
		body := `[{"directory":"` + dbDir + `","file":"src/main.c","command":"cc -I` + incDir + ` -c src/main.c"}]`
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}

	t0 := time.Now().Add(-time.Hour)
	write("inc1", t0)
	require.Equal(t, []string{"inc1"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs)

	// Edit only compile_commands.json, advancing its modtime. No clearCppIncludeDirCache
	// call — the next load must observe the newer file and re-read it.
	write("inc2", t0.Add(time.Minute))
	assert.Equal(t, []string{"inc2"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs,
		"a newer compile_commands.json is re-read without an explicit cache clear")

	// A subsequent load with no further edit is served from the (now-warm) cache.
	assert.Equal(t, []string{"inc2"}, loadCompileCommands(newDiskManifestTree(root))["src/main.c"].includeDirs,
		"unchanged compile_commands.json stays cached")
}
