package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

type findFilesResult struct {
	Files []struct {
		Path     string `json:"path"`
		Language string `json:"language"`
		ID       string `json:"id"`
	} `json:"files"`
	Count int `json:"count"`
}

func setupFindFilesServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	write("main.go", "package app\n\nfunc Main() {}\n")
	write("internal/handler.go", "package internal\n\nfunc Handle() {}\n")
	write("internal/sub/handler_test.go", "package sub\n\nfunc TestHandle() {}\n")

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

func decodeFindFiles(t *testing.T, res *mcplib.CallToolResult) findFilesResult {
	t.Helper()
	require.False(t, res.IsError)
	var out findFilesResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

func TestFindFiles_ByName(t *testing.T) {
	srv := setupFindFilesServer(t)

	// A basename substring matches both handler files; the shallower
	// one ranks first (same score, fewer path segments).
	resp := decodeFindFiles(t, callTool(t, srv, "find_files", map[string]any{"query": "handler"}))
	require.GreaterOrEqual(t, resp.Count, 2)
	require.Equal(t, "internal/handler.go", resp.Files[0].Path)

	// An exact basename only matches the one file.
	exact := decodeFindFiles(t, callTool(t, srv, "find_files", map[string]any{"query": "handler.go"}))
	require.Equal(t, 1, exact.Count)
	require.Equal(t, "internal/handler.go", exact.Files[0].Path)

	// main.go is reachable by name.
	main := decodeFindFiles(t, callTool(t, srv, "find_files", map[string]any{"query": "main"}))
	require.Equal(t, 1, main.Count)
	require.Equal(t, "main.go", main.Files[0].Path)
}

func TestFindFiles_Glob(t *testing.T) {
	srv := setupFindFilesServer(t)

	resp := decodeFindFiles(t, callTool(t, srv, "find_files", map[string]any{"glob": "*_test.go"}))
	require.Equal(t, 1, resp.Count)
	require.Equal(t, "internal/sub/handler_test.go", resp.Files[0].Path)
}

func TestFindFiles_Fuzzy(t *testing.T) {
	srv := setupFindFilesServer(t)

	// "hndlr" is a subsequence of "handler.go" but not a substring.
	none := decodeFindFiles(t, callTool(t, srv, "find_files", map[string]any{"query": "hndlr"}))
	require.Equal(t, 0, none.Count)

	fuzzy := decodeFindFiles(t, callTool(t, srv, "find_files",
		map[string]any{"query": "hndlr", "fuzzy": true}))
	require.GreaterOrEqual(t, fuzzy.Count, 1)
	require.Equal(t, "internal/handler.go", fuzzy.Files[0].Path)
}

func TestFindFiles_PathScoping(t *testing.T) {
	srv := setupFindFilesServer(t)

	scoped := decodeFindFiles(t, callTool(t, srv, "find_files",
		map[string]any{"query": "handler", "path": "internal/sub"}))
	require.Equal(t, 1, scoped.Count)
	require.Equal(t, "internal/sub/handler_test.go", scoped.Files[0].Path)
}

func TestFindFiles_RequiresArg(t *testing.T) {
	srv := setupFindFilesServer(t)
	bad := callTool(t, srv, "find_files", map[string]any{})
	require.True(t, bad.IsError)
}

func TestScoreFilenameMatch(t *testing.T) {
	cases := []struct {
		query, base, rel string
		fuzzy            bool
		want             int
		ok               bool
	}{
		{"handler.go", "handler.go", "internal/handler.go", false, 100, true},
		{"hand", "handler.go", "internal/handler.go", false, 70, true},
		{"ndl", "handler.go", "internal/handler.go", false, 50, true},
		{"internal", "handler.go", "internal/handler.go", false, 30, true},
		{"nomatch", "handler.go", "internal/handler.go", false, 0, false},
		{"hndlr", "handler.go", "internal/handler.go", true, 10, true},
		{"hndlr", "handler.go", "internal/handler.go", false, 0, false},
	}
	for _, c := range cases {
		got, ok := scoreFilenameMatch(c.query, c.base, c.rel, c.fuzzy)
		require.Equal(t, c.ok, ok, "match? query=%q", c.query)
		require.Equal(t, c.want, got, "score query=%q", c.query)
	}
}

func TestIsSubsequence(t *testing.T) {
	require.True(t, isSubsequence("", "anything"))
	require.True(t, isSubsequence("abc", "aXbYcZ"))
	require.True(t, isSubsequence("hndlr", "handler.go"))
	require.False(t, isSubsequence("abc", "acb"))
	require.False(t, isSubsequence("xyz", "abc"))
}

// setupFindFilesGlobstarServer indexes one file at each depth the
// find_files schema's own example — `internal/**/*_test.go` — has to
// reach, plus the two shapes it must not reach. It is separate from
// setupFindFilesServer because the tests there assert exact counts.
func setupFindFilesGlobstarServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	write("internal/direct_test.go", "package internal\n\nfunc TestDirect() {}\n")
	write("internal/sub/one_test.go", "package sub\n\nfunc TestOne() {}\n")
	write("internal/a/b/deep_test.go", "package b\n\nfunc TestDeep() {}\n")
	write("internal/sub/production.go", "package sub\n\nfunc Produce() {}\n")
	write("cmd/outside_test.go", "package cmd\n\nfunc TestOutside() {}\n")
	// For the trailing-subtree composition: a globbed directory prefix
	// (`src/**/internal`) followed by `/*`. None of these end in
	// _test.go, so the counts asserted above are unaffected.
	write("src/a/internal/x.go", "package internal\n\nfunc X() {}\n")
	write("src/a/internal/sub/deep.go", "package sub\n\nfunc Deep() {}\n")
	write("src/a/other/deep.go", "package other\n\nfunc Other() {}\n")

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

// TestFindFiles_GlobstarCrossesSegmentsAtAnyDepth holds the tool to the
// contract its own schema advertises: `**` crosses segments, in the
// middle of a pattern as much as at either end.
//
// The example in the description is `internal/**/*_test.go`, and before
// the globstar matcher a middle `**` fell through to path.Match and came
// back segment-bounded — it matched exactly one directory level and
// silently missed everything deeper. On Windows the older filepath.Match
// happened to match the deep paths, because '/' is an ordinary character
// there, so this is also the case that must not quietly change verdict
// between platforms.
func TestFindFiles_GlobstarCrossesSegmentsAtAnyDepth(t *testing.T) {
	srv := setupFindFilesGlobstarServer(t)

	resp := decodeFindFiles(t, callTool(t, srv, "find_files",
		map[string]any{"glob": "internal/**/*_test.go"}))

	got := map[string]bool{}
	for _, f := range resp.Files {
		got[f.Path] = true
	}

	for _, want := range []string{
		"internal/direct_test.go",   // zero segments between: the direct child
		"internal/sub/one_test.go",  // one level down
		"internal/a/b/deep_test.go", // two levels down — the case that regressed
	} {
		require.Truef(t, got[want], "%q should match internal/**/*_test.go; got %v", want, resp.Files)
	}
	for _, unwanted := range []string{
		"internal/sub/production.go", // right subtree, wrong basename
		"cmd/outside_test.go",        // right basename, wrong subtree
	} {
		require.Falsef(t, got[unwanted], "%q must not match internal/**/*_test.go", unwanted)
	}
	require.Equal(t, 3, resp.Count, "exactly the three test files under internal/")
}

// TestFindFiles_GlobstarComposesWithTrailingSubtree is the handler-level
// half of the composition contract: `**` crosses directories and a
// trailing `/*` covers a subtree, and the two have to work in one
// pattern. `src/**/internal/*` resolved neither rule before — the segment
// walk spent the final `*` on a single segment, and the legacy prefix
// fallback reads `src/**/internal` literally.
func TestFindFiles_GlobstarComposesWithTrailingSubtree(t *testing.T) {
	srv := setupFindFilesGlobstarServer(t)

	resp := decodeFindFiles(t, callTool(t, srv, "find_files",
		map[string]any{"glob": "src/**/internal/*"}))

	got := map[string]bool{}
	for _, f := range resp.Files {
		got[f.Path] = true
	}

	require.Truef(t, got["src/a/internal/x.go"],
		"a direct child of the globbed prefix; got %v", resp.Files)
	require.Truef(t, got["src/a/internal/sub/deep.go"],
		"a deeply nested child — the case that regressed; got %v", resp.Files)
	require.Falsef(t, got["src/a/other/deep.go"],
		"a sibling directory that is not `internal` must not match")
	require.Equal(t, 2, resp.Count, "exactly the two files under src/a/internal")
}

// TestFindFiles_GlobOversizedIsRejectedBeforeScanning is the consumer-side
// half of the resource bound. The matcher runs once per candidate file,
// ahead of `limit`, so the size of a user-supplied glob multiplies across
// the whole scan rather than costing one call. The handler therefore has to
// refuse an oversized pattern before it walks anything — a bound that only
// existed inside the matcher would still have paid for the walk.
//
// The counts are taken off the NORMALISED pattern. Reading them off the raw
// string was a bypass: on Windows a native-separator glob counts as one
// segment before filepath.ToSlash and as many after it, so a 130-byte
// pattern was admitted and then expanded to 65 segments at match time.
func TestFindFiles_GlobOversizedIsRejectedBeforeScanning(t *testing.T) {
	srv := setupFindFilesServer(t)

	refused := func(t *testing.T, glob string) {
		t.Helper()
		res := callTool(t, srv, "find_files", map[string]any{"glob": glob})
		require.True(t, res.IsError, "an oversized glob must be refused")
		require.Contains(t, res.Content[0].(mcplib.TextContent).Text, "too large")
	}
	served := func(t *testing.T, glob string) {
		t.Helper()
		res := callTool(t, srv, "find_files", map[string]any{"glob": glob})
		require.False(t, res.IsError, "a glob inside the bound must still be served")
	}

	// Exactly at each limit is served; one past it is refused. The
	// previous fixture built 63 segments and never touched the boundary.
	atSegmentLimit := strings.Repeat("x/", maxGlobSegments-1) + "**"
	require.Equal(t, maxGlobSegments, strings.Count(atSegmentLimit, "/")+1)
	served(t, atSegmentLimit)
	refused(t, strings.Repeat("x/", maxGlobSegments)+"**")

	atByteLimit := strings.Repeat("a", maxGlobBytes)
	require.Len(t, atByteLimit, maxGlobBytes)
	served(t, atByteLimit)
	refused(t, strings.Repeat("a", maxGlobBytes+1))

	// Native and mixed separators are counted after normalisation, which
	// is the only reading that agrees with the matcher.
	t.Run("native separators", func(t *testing.T) {
		glob := strings.Repeat(`x\`, maxGlobSegments) + `**`
		require.Equal(t, 1, strings.Count(glob, "/")+1,
			"the fixture must look small before normalisation, or it proves nothing")

		if runtime.GOOS != "windows" {
			// filepath.ToSlash is a no-op on POSIX, where a backslash is an
			// ordinary filename byte. There is no expansion here, so there
			// is no bypass to guard and this is a legitimate one-segment
			// pattern. Asserting the expansion outside this branch would be
			// asserting a Windows fact on a platform that lacks it.
			served(t, glob)
			return
		}
		require.Greater(t, strings.Count(filepath.ToSlash(glob), "/")+1, maxGlobSegments,
			"on Windows the fixture must expand past the bound, or it proves nothing")
		refused(t, glob)
	})
	t.Run("mixed separators", func(t *testing.T) {
		// Refused on both platforms, for different reasons: POSIX counts the
		// forward slashes alone and is already past the bound, Windows
		// counts twice as many after normalisation. An assertion that only
		// fires on one platform would leave an empty passing subtest on the
		// other.
		glob := strings.Repeat(`a/b\`, maxGlobSegments) + `**`
		require.Greater(t, strings.Count(glob, "/")+1, maxGlobSegments,
			"the forward slashes alone must already exceed the bound")
		refused(t, glob)
	})
}
