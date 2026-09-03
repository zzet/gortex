package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func TestReplaceIdentifierAll(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		old     string
		next    string
		want    string
		wantCnt int
	}{
		{
			// The bug that made writing these edits unsafe: a substring
			// replace renames inside a longer identifier.
			name: "leaves longer identifiers intact",
			line: "\tx := GetUser(Get())", old: "Get", next: "Fetch",
			want: "\tx := GetUser(Fetch())", wantCnt: 1,
		},
		{
			name: "replaces every occurrence on the line",
			line: "\treturn Get(Get(y))", old: "Get", next: "Fetch",
			want: "\treturn Fetch(Fetch(y))", wantCnt: 2,
		},
		{
			name: "does not match a suffix",
			line: "\tdoGet()", old: "Get", next: "Fetch",
			want: "\tdoGet()", wantCnt: 0,
		},
		{
			name: "does not match inside an underscore name",
			line: "\tmy_Get_helper()", old: "Get", next: "Fetch",
			want: "\tmy_Get_helper()", wantCnt: 0,
		},
		{
			name: "does not match a digit-adjacent name",
			line: "\tGet2()", old: "Get", next: "Fetch",
			want: "\tGet2()", wantCnt: 0,
		},
		{
			name: "renames a qualified selector",
			line: "\tpkg.Get(x)", old: "Get", next: "Fetch",
			want: "\tpkg.Fetch(x)", wantCnt: 1,
		},
		{
			// A sigil is not part of the name, so the identifier after it
			// must stay renamable.
			name: "renames past a PHP sigil",
			line: "\t$total = $total + 1;", old: "total", next: "sum",
			want: "\t$sum = $sum + 1;", wantCnt: 2,
		},
		{
			name: "handles a unicode identifier neighbour",
			line: "\tGetÜser(Get())", old: "Get", next: "Fetch",
			want: "\tGetÜser(Fetch())", wantCnt: 1,
		},
		{
			name: "whole line is the identifier",
			line: "Get", old: "Get", next: "Fetch",
			want: "Fetch", wantCnt: 1,
		},
		{
			name: "empty old name is a no-op",
			line: "anything", old: "", next: "x",
			want: "anything", wantCnt: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n := replaceIdentifierAll(tc.line, tc.old, tc.next)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantCnt, n)
		})
	}
}

func TestSplitLinesKeepEnds(t *testing.T) {
	assert.Equal(t, []string{"a\n", "b\n"}, splitLinesKeepEnds("a\nb\n"))
	assert.Equal(t, []string{"a\r\n", "b"}, splitLinesKeepEnds("a\r\nb"))
	assert.Equal(t, []string{"only"}, splitLinesKeepEnds("only"))
	assert.Nil(t, splitLinesKeepEnds(""))

	// Round-tripping must be byte-exact — a rename rewrites one line and
	// rejoins, so any normalisation here would silently rewrite the file.
	for _, content := range []string{"a\nb\n", "a\r\nb\r\n", "a\nb", "\n\n", ""} {
		var joined string
		for _, l := range splitLinesKeepEnds(content) {
			joined += l
		}
		assert.Equal(t, content, joined)
	}
}

func TestSplitLineTerminator(t *testing.T) {
	body, term := splitLineTerminator("x\r\n")
	assert.Equal(t, "x", body)
	assert.Equal(t, "\r\n", term)

	body, term = splitLineTerminator("x\n")
	assert.Equal(t, "x", body)
	assert.Equal(t, "\n", term)

	body, term = splitLineTerminator("x")
	assert.Equal(t, "x", body)
	assert.Equal(t, "", term)
}

// setupRenameServer indexes a two-file repo where caller.go calls a symbol
// defined in target.go, so a rename has to reach across files.
func setupRenameServer(t *testing.T, targetSrc, callerSrc string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.go"), []byte(targetSrc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "caller.go"), []byte(callerSrc), 0o644))

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv, dir
}

func decodeRenameResp(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "%v", res.Content)
	require.NotEmpty(t, res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

const renameTargetSrc = `package main

func Target() string {
	return "t"
}
`

const renameCallerSrc = `package main

func Caller() string {
	return Target()
}
`

// TestRenameSymbol_AppliesToDisk is the regression test for issue #463:
// rename_symbol returned an edit list and wrote nothing.
func TestRenameSymbol_AppliesToDisk(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id":       "target.go::Target",
		"new_name": "Renamed",
	})
	resp := decodeRenameResp(t, res)

	assert.Equal(t, "applied", resp["status"])
	assert.Equal(t, false, resp["dry_run"])
	assert.Equal(t, true, resp["written"])
	require.NotNil(t, resp["files"], "per-file apply results expected: %v", resp)

	target, err := os.ReadFile(filepath.Join(dir, "target.go"))
	require.NoError(t, err)
	assert.Contains(t, string(target), "func Renamed() string")
	assert.NotContains(t, string(target), "func Target()")

	caller, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Contains(t, string(caller), "return Renamed()")
	assert.NotContains(t, string(caller), "return Target()")

	for _, f := range resp["files"].([]any) {
		entry := f.(map[string]any)
		assert.Equal(t, "applied", entry["status"], "%v", entry)
		assert.NotEmpty(t, entry["new_sha"], "%v", entry)
	}
}

// TestRenameSymbol_DryRunWritesNothing pins the other half of #463: a dry run
// must be distinguishable from an apply, and must leave the tree untouched.
func TestRenameSymbol_DryRunWritesNothing(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id":       "target.go::Target",
		"new_name": "Renamed",
		"dry_run":  true,
	})
	resp := decodeRenameResp(t, res)

	assert.Equal(t, "would_apply", resp["status"])
	assert.Equal(t, true, resp["dry_run"])
	assert.Equal(t, false, resp["written"])

	target, err := os.ReadFile(filepath.Join(dir, "target.go"))
	require.NoError(t, err)
	assert.Equal(t, renameTargetSrc, string(target), "dry run must not write")
	caller, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Equal(t, renameCallerSrc, string(caller), "dry run must not write")
}

// TestRenameSymbol_DryRunDiffersFromApply is the reporter's exact check: the
// two responses used to be byte-identical, so a caller could not tell whether
// anything had been written.
func TestRenameSymbol_DryRunDiffersFromApply(t *testing.T) {
	srvDry, _ := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	srvApply, _ := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	args := func(dry bool) map[string]any {
		return map[string]any{"id": "target.go::Target", "new_name": "Renamed", "dry_run": dry}
	}
	dry := decodeRenameResp(t, callToolByName(t, srvDry, context.Background(), "rename_symbol", args(true)))
	applied := decodeRenameResp(t, callToolByName(t, srvApply, context.Background(), "rename_symbol", args(false)))

	assert.NotEqual(t, dry["status"], applied["status"])
	assert.NotEqual(t, dry["written"], applied["written"])
	assert.Equal(t, dry["edits"], applied["edits"], "the planned edits themselves should match")
}

// TestRenameSymbol_LeavesLongerIdentifiersIntact guards the boundary fix on
// the real write path. The call site is deliberately written so the old name
// appears twice on one line — first inside a longer identifier. A
// first-occurrence substring replace rewrites GetUserName instead of the call,
// which is exactly the corruption that made these edits unsafe to write.
func TestRenameSymbol_LeavesLongerIdentifiersIntact(t *testing.T) {
	srv, dir := setupRenameServer(t,
		`package main

func Get() string {
	return "g"
}

func GetUserName(s string) string {
	return s
}
`,
		`package main

func Caller() string {
	return GetUserName(Get())
}
`)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id":       "target.go::Get",
		"new_name": "Fetch",
	})
	resp := decodeRenameResp(t, res)
	assert.Equal(t, "applied", resp["status"])

	caller, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Equal(t, "\treturn GetUserName(Fetch())",
		callerCallLine(t, string(caller)), "only the Get call may be renamed")

	target, err := os.ReadFile(filepath.Join(dir, "target.go"))
	require.NoError(t, err)
	assert.Contains(t, string(target), "func Fetch() string")
	assert.Contains(t, string(target), "func GetUserName(s string) string",
		"GetUserName must survive renaming Get")
}

// callerCallLine returns the single return line from the caller fixture.
func callerCallLine(t *testing.T, content string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "return ") {
			return line
		}
	}
	t.Fatalf("no return line in %q", content)
	return ""
}

// TestRenameFacade_AppliesToDisk reproduces the reporter's call path in
// issue #463: refactor({operation:"rename", ..., dry_run:false}) over the
// compact facade surface, which returned an edit list and wrote nothing.
func TestRenameFacade_AppliesToDisk(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	call := func(dryRun bool) map[string]any {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "refactor"
		req.Params.Arguments = map[string]any{
			"operation": "rename",
			"target":    map[string]any{"symbol": "target.go::Target"},
			"new_name":  "Renamed",
			"dry_run":   dryRun,
		}
		res, err := srv.handleFacade(context.Background(), "refactor", req)
		require.NoError(t, err)
		return decodeRenameResp(t, res)
	}

	// Step 1-2 of the report: the dry run must leave the tree alone.
	preview := call(true)
	assert.Equal(t, "would_apply", preview["status"])
	onDisk, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Equal(t, renameCallerSrc, string(onDisk))

	// Step 3-4: the real call must differ from the preview and actually write.
	applied := call(false)
	assert.Equal(t, "applied", applied["status"])
	assert.NotEqual(t, preview["status"], applied["status"],
		"a dry run and an apply must be distinguishable")

	onDisk, err = os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "return Renamed()")
}

// TestRenameSymbol_PreservesCRLF checks the rewrite splices one line without
// normalising the file's line endings.
func TestRenameSymbol_PreservesCRLF(t *testing.T) {
	srv, dir := setupRenameServer(t,
		"package main\r\n\r\nfunc Target() string {\r\n\treturn \"t\"\r\n}\r\n",
		"package main\r\n\r\nfunc Caller() string {\r\n\treturn Target()\r\n}\r\n")

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id":       "target.go::Target",
		"new_name": "Renamed",
	})
	resp := decodeRenameResp(t, res)
	assert.Equal(t, "applied", resp["status"])

	caller, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	assert.Contains(t, string(caller), "return Renamed()")
	assert.NotContains(t, string(caller), "\n\n\r", "line endings must not be rewritten")
	assert.Equal(t, "package main\r\n\r\nfunc Caller() string {\r\n\treturn Renamed()\r\n}\r\n", string(caller))
}

// TestRenameSymbol_RefusesOnDiskDrift verifies the whole rename is refused —
// not partially applied — when a planned line no longer matches disk.
func TestRenameSymbol_RefusesOnDiskDrift(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	edits := []renameEdit{{
		File:    "caller.go",
		Line:    4,
		OldText: "\treturn SomethingElse()",
		NewText: "\treturn Renamed()",
	}}
	_, err := srv.planRenameWrites(context.Background(), edits, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed on disk")

	caller, err2 := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err2)
	assert.Equal(t, renameCallerSrc, string(caller), "a refused rename must write nothing")
}

// TestRenameSymbol_RefusesOutOfRangeLine covers a stale index pointing past
// the end of the file.
func TestRenameSymbol_RefusesOutOfRangeLine(t *testing.T) {
	srv, _ := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	_, err := srv.planRenameWrites(context.Background(), []renameEdit{{
		File: "caller.go", Line: 9999, OldText: "x", NewText: "y",
	}}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the file")
}

// TestRenameSymbol_ParseGateRefusesWholeRename ensures a rewrite that breaks
// one file aborts every file, so the tree never lands half-renamed.
func TestRenameSymbol_ParseGateRefusesWholeRename(t *testing.T) {
	if !parseGateEnabled() {
		t.Skip("parse gate disabled in this environment")
	}
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)

	// A replacement that is not a legal identifier breaks the file's syntax.
	_, err := srv.planRenameWrites(context.Background(), []renameEdit{{
		File:    "caller.go",
		Line:    4,
		OldText: "\treturn Target()",
		NewText: "\treturn ((((",
	}}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse gate")

	caller, err2 := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err2)
	assert.Equal(t, renameCallerSrc, string(caller))
}
