package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeFileOpsResult(t *testing.T, result *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.NotEmpty(t, result.Content)
	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	return resp
}

func TestWriteFile_CreatesNewFile(t *testing.T) {
	srv, dir := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "docs/intro.md",
		"content": "# Intro\n\nHello world.\n",
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "created", resp["status"])

	got, err := os.ReadFile(filepath.Join(dir, "docs", "intro.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Intro\n\nHello world.\n", string(got))
	assert.Equal(t, float64(len(got)), resp["bytes_written"])
}

func TestWriteFile_OverwritesExistingFile(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "notes.txt",
		"content": "new content",
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "overwritten", resp["status"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new content", string(got))
}

func TestWriteFile_AbsolutePath(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "absolute.txt")

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    target,
		"content": "abs",
	})
	assert.False(t, result.IsError)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "abs", string(got))
}

func TestWriteFile_ReindexesGoSource(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "main.go",
		"content": "package main\n\nfunc freshlyAdded() {}\n",
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, true, resp["reindexed"], "Go file inside repo must trigger reindex")

	search := callTool(t, srv, "search_symbols", map[string]any{"query": "freshlyAdded"})
	searchResp := decodeFileOpsResult(t, search)
	assert.Greater(t, searchResp["total"], float64(0), "new symbol should be searchable post-reindex")
}

func TestEditFile_UniqueReplacement(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "changelog.md")
	require.NoError(t, os.WriteFile(target, []byte("v0.1 released\nv0.2 pending\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "changelog.md",
		"old_string": "v0.2 pending",
		"new_string": "v0.2 shipped",
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	assert.Equal(t, float64(1), resp["replacements"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "v0.1 released\nv0.2 shipped\n", string(got))
}

func TestEditFile_AmbiguousMatchRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "dup.md")
	require.NoError(t, os.WriteFile(target, []byte("TODO\nTODO\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "dup.md",
		"old_string": "TODO",
		"new_string": "DONE",
	})
	assert.True(t, result.IsError, "multiple matches without replace_all must error")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "TODO\nTODO\n", string(got), "file must be untouched on error")
}

func TestEditFile_ReplaceAll(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "dup.md")
	require.NoError(t, os.WriteFile(target, []byte("TODO\nTODO\nTODO\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":        "dup.md",
		"old_string":  "TODO",
		"new_string":  "DONE",
		"replace_all": true,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, float64(3), resp["replacements"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "DONE\nDONE\nDONE\n", string(got))
}

func TestEditFile_MissingOldString(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "file.md")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "file.md",
		"old_string": "not present",
		"new_string": "x",
	})
	assert.True(t, result.IsError)
}

func TestEditFile_IdenticalStringsRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "file.md")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "file.md",
		"old_string": "content",
		"new_string": "content",
	})
	assert.True(t, result.IsError)
}

func TestEditFile_NonexistentFile(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "does-not-exist.md",
		"old_string": "a",
		"new_string": "b",
	})
	assert.True(t, result.IsError)
}

func TestWriteFile_PreservesExistingMode(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "exec.sh")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o755))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "exec.sh",
		"content": "new",
	})
	assert.False(t, result.IsError)

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestWriteFile_CreatesNestedDirs(t *testing.T) {
	srv, dir := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "deeply/nested/sub/dir/notes.md",
		"content": "hi",
	})
	assert.False(t, result.IsError, "atomic-write must mkdir -p parent dirs")

	got, err := os.ReadFile(filepath.Join(dir, "deeply", "nested", "sub", "dir", "notes.md"))
	require.NoError(t, err)
	assert.Equal(t, "hi", string(got))
}

func TestWriteFile_RejectsPathTraversal(t *testing.T) {
	srv, dir := setupTestServer(t)
	// Marker file outside the repo root that the traversal would target.
	parent := filepath.Dir(dir)
	probe := filepath.Join(parent, "escape.txt")
	t.Cleanup(func() { _ = os.Remove(probe) })

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "../escape.txt",
		"content": "should not land",
	})
	assert.True(t, result.IsError, "relative path with .. that escapes repo must be refused")

	_, err := os.Stat(probe)
	assert.True(t, os.IsNotExist(err), "no file may be written outside the repo root via traversal")
}

func TestEditFile_RejectsPathTraversal(t *testing.T) {
	srv, dir := setupTestServer(t)
	parent := filepath.Dir(dir)
	probe := filepath.Join(parent, "outside.txt")
	require.NoError(t, os.WriteFile(probe, []byte("untouched"), 0o644))
	t.Cleanup(func() { _ = os.Remove(probe) })

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "../outside.txt",
		"old_string": "untouched",
		"new_string": "TAMPERED",
	})
	assert.True(t, result.IsError, "edit_file must refuse to follow traversal out of the repo")

	got, err := os.ReadFile(probe)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got), "the file outside the repo must remain unchanged")
}

func TestWriteFile_AbsolutePathOutsideRepoIsRefused(t *testing.T) {
	// Confinement is universal (SECURITY.md): an absolute path that points
	// outside every indexed repository root is refused, not honoured. This
	// is the GHSA-w42c-h7hr-f67p arbitrary-write fix — previously such a
	// path was written verbatim, an arbitrary-write -> RCE primitive.
	srv, _ := setupTestServer(t)
	tmp := t.TempDir()
	target := filepath.Join(tmp, "external.txt")

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    target,
		"content": "abs allowed",
	})
	assert.True(t, result.IsError, "an absolute path outside the repo must be refused")

	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "the out-of-root file must not be created")
}

func TestEditFile_DryRunDoesNotWrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "preview.md")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "preview.md",
		"old_string": "original",
		"new_string": "preview-only",
		"dry_run":    true,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_apply", resp["status"])
	assert.Equal(t, true, resp["dry_run"])
	assert.Equal(t, float64(1), resp["replacements"])
	assert.Equal(t, false, resp["reindexed"], "dry_run must NOT reindex")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "original", string(got), "dry_run must NOT touch the file")
}

func TestWriteFile_DryRunReportsCreate(t *testing.T) {
	srv, dir := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "newfile.md",
		"content": "preview",
		"dry_run": true,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_create", resp["status"])
	assert.Equal(t, true, resp["dry_run"])

	_, err := os.Stat(filepath.Join(dir, "newfile.md"))
	assert.True(t, os.IsNotExist(err), "dry_run must NOT create the file")
}

func TestWriteFile_DryRunReportsOverwrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "existing.md")
	require.NoError(t, os.WriteFile(target, []byte("v1"), 0o644))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "existing.md",
		"content": "v2",
		"dry_run": true,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_overwrite", resp["status"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got), "dry_run must NOT overwrite")
}

func TestWriteFile_RejectsDirectoryTarget(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "is_a_dir"), 0o755))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "is_a_dir",
		"content": "should fail",
	})
	assert.True(t, result.IsError, "write_file must refuse to clobber a directory")
}

func TestEditFile_AmbiguousErrorIncludesLineNumbers(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "many.md")
	require.NoError(t, os.WriteFile(target, []byte("\nTODO\nTODO\nTODO\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "many.md",
		"old_string": "TODO",
		"new_string": "DONE",
	})
	assert.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "first match lines", "error must surface the line numbers so the agent can choose a unique fragment")
}

func TestEditFile_DryRunAmbiguousFailsBeforeWrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "amb.md")
	require.NoError(t, os.WriteFile(target, []byte("DUP\nDUP\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "amb.md",
		"old_string": "DUP",
		"new_string": "x",
		"dry_run":    true,
	})
	assert.True(t, result.IsError, "dry_run must still surface ambiguity errors")
}

func TestPathContainedIn(t *testing.T) {
	tests := []struct {
		abs, root string
		want      bool
	}{
		{"/repo/a/b.go", "/repo", true},
		{"/repo", "/repo", true},
		{"/repo/", "/repo", true},
		{"/repo-other/x.go", "/repo", false},
		{"/etc/passwd", "/repo", false},
		{"/repo/../etc/passwd", "/repo", false},
		{"/repo/..hidden/file.go", "/repo", true},
		{"", "/repo", false},
		{"/repo/x.go", "", false},
	}
	for _, tt := range tests {
		got := pathContainedIn(tt.abs, tt.root)
		assert.Equal(t, tt.want, got, "pathContainedIn(%q, %q)", tt.abs, tt.root)
	}
}
