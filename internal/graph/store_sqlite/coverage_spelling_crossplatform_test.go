package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestOpenKeepsLiveRowsWhenAStoreCrossesPlatforms is the case that decides
// whether the purge may delete anything at all on a path-shape argument.
//
// A store written on Windows and then carried to POSIX - a synced home
// directory, a container mounting the host's store - keeps its Windows
// rows, because eviction is spelling-exact and that is this migration's
// own premise. Re-indexing there produces LIVE forward-slash rows for the
// same logical files, while the stale backslash rows remain to vouch for
// them: they set the repository's Windows verdict and they satisfy the
// native-twin test. At that point a live POSIX row and a stale Windows
// legacy row are identical in every path field.
//
// What separates them is not the spelling but whether anything else in the
// graph still claims that path. A legacy artifact is the only thing that
// ever carried its re-spelled path; a live file's path is also carried by
// its file node and by every symbol in it.
func TestOpenKeepsLiveRowsWhenAStorePlatformChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		staleWinFile = `r/a\b.go` // left over from the Windows run, never evicted
		liveFile     = `r/a/b.go` // the same logical file, re-indexed on POSIX
		liveSymbol   = `r/a/b.go::Fn`
		liveTodo     = `r/a/b.go::todo:3`
		liveLicense  = `r/license::MIT`

		// A genuine legacy row in the same store: its path is claimed by
		// nothing but itself, so it still heals.
		deadTodo = `r/c/d.go::todo:9`
		deadPath = `r/c/d.go`
		liveDead = `r/c\d.go`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: staleWinFile, Kind: graph.KindFile, Name: "b.go", FilePath: staleWinFile, RepoPrefix: "r"},
		{ID: liveFile, Kind: graph.KindFile, Name: "b.go", FilePath: liveFile, RepoPrefix: "r"},
		{ID: liveSymbol, Kind: graph.KindFunction, Name: "Fn", FilePath: liveFile, RepoPrefix: "r"},
		{ID: liveTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: liveFile, RepoPrefix: "r"},
		{ID: liveLicense, Kind: graph.KindLicense, Name: "MIT", FilePath: liveFile, RepoPrefix: "r"},
		{ID: liveDead, Kind: graph.KindFile, Name: "d.go", FilePath: liveDead, RepoPrefix: "r"},
		{ID: deadTodo, Kind: graph.KindTodo, Name: "todo:9", FilePath: deadPath, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: liveFile, To: liveSymbol, Kind: graph.EdgeDefines, FilePath: liveFile, Line: 1},
		{From: liveFile, To: liveTodo, Kind: graph.EdgeAnnotated, FilePath: liveFile, Line: 3},
		{From: liveFile, To: liveLicense, Kind: graph.EdgeLicensedAs, FilePath: liveFile},
		{From: deadPath, To: deadTodo, Kind: graph.EdgeAnnotated, FilePath: deadPath, Line: 9},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.NotNil(t, s2.GetNode(liveTodo),
		"a live POSIX todo must survive: its path is claimed by a real file node")
	require.NotNil(t, s2.GetNode(liveLicense),
		"and the license it points at must survive with it")
	require.Len(t, s2.GetOutEdges(liveFile), 3,
		"every live edge on that file must survive")

	require.Nil(t, s2.GetNode(deadTodo),
		"a genuine legacy row, whose path nothing else claims, still heals")
}

// TestOpenKeepsLiveRowsWhenAPosixFilenameContainsABackslash is the
// pathological but legal shape: a backslash is a valid character in a POSIX
// filename, so one such file makes its repository look Windows-written and
// doubles as the native twin of a genuinely different file.
func TestOpenKeepsLiveRowsWhenAPosixFilenameContainsABackslash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		oddFile  = `r/a\b.go` // ONE real POSIX file whose name contains a backslash
		realFile = `r/a/b.go` // a different real file
		realSym  = `r/a/b.go::Fn`
		realTodo = `r/a/b.go::todo:7`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: oddFile, Kind: graph.KindFile, Name: `a\b.go`, FilePath: oddFile, RepoPrefix: "r"},
		{ID: realFile, Kind: graph.KindFile, Name: "b.go", FilePath: realFile, RepoPrefix: "r"},
		{ID: realSym, Kind: graph.KindFunction, Name: "Fn", FilePath: realFile, RepoPrefix: "r"},
		{ID: realTodo, Kind: graph.KindTodo, Name: "todo:7", FilePath: realFile, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: realFile, To: realSym, Kind: graph.EdgeDefines, FilePath: realFile, Line: 1},
		{From: realFile, To: realTodo, Kind: graph.EdgeAnnotated, FilePath: realFile, Line: 7},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.NotNil(t, s2.GetNode(realTodo), "a live todo must survive")
	require.Len(t, s2.GetOutEdges(realFile), 2, "its live edges must survive")
}
