package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// The #846 read-path contract: the indexer decodes UTF-16 sources before
// extraction, so a symbol declared in a UTF-16 file is discoverable. The
// read tools must serve the same decoded text — not the raw NUL-interleaved
// bytes — and the edit tools must refuse the file with a message that names
// the encoding, instead of "old_string not found" pointing at the reads.

const utf16FixtureSrc = "public class Legacy\n{\n    public int B() { return 1; }\n}\n"

// utf16ServerWith indexes one UTF-16LE file and one UTF-8 control file,
// returning the server and the temp dir root.
func utf16ServerWith(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, "Legacy.cs")
	require.NoError(t, os.WriteFile(full, utf16LEWithBOM(t, utf16FixtureSrc), 0o644))
	plain := filepath.Join(dir, "Utf8.cs")
	require.NoError(t, os.WriteFile(plain, []byte("public class Utf8Only\n{\n}\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil), dir
}

// utf16LEWithBOM encodes s as UTF-16LE with the byte-order mark a Windows
// tooling writer puts on the file.
func utf16LEWithBOM(t *testing.T, s string) []byte {
	t.Helper()
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+len(units)*2)
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// utf16SymbolNamed looks up a node by name among the file's symbols.
func utf16SymbolNamed(t *testing.T, srv *Server, file, name string) *graph.Node {
	t.Helper()
	sg := srv.engineFor(context.Background()).GetFileSymbols(file)
	require.NotNil(t, sg, "file %s must be indexed", file)
	for _, n := range sg.Nodes {
		if n != nil && n.Name == name {
			return n
		}
	}
	t.Fatalf("symbol %s not found in %s", name, file)
	return nil
}

func utf16JSON(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "%+v", res.Content)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

func TestGetSymbolSource_ServesDecodedUTF16Source(t *testing.T) {
	srv, dir := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "Legacy")
	require.True(t, fileLooksUTF16(filepath.Join(dir, "Legacy.cs")))

	out := utf16JSON(t, callTool(t, srv, "get_symbol_source", map[string]any{"id": node.ID}))

	source, _ := out["source"].(string)
	require.NotContains(t, source, "\x00", "the source must not be NUL-interleaved raw bytes")
	require.Contains(t, source, "public class Legacy")
	require.Contains(t, source, "return 1;")

	omissions, _ := out["omissions"].([]any)
	var disclosed bool
	for _, o := range omissions {
		if m, ok := o.(map[string]any); ok && m["kind"] == "utf16_decoded" {
			disclosed = true
		}
	}
	require.True(t, disclosed, "a decoded UTF-16 source must carry the utf16_decoded omission")
}

func TestGetEditingContext_ServesDecodedUTF16Source(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	out := utf16JSON(t, callTool(t, srv, "get_editing_context", map[string]any{"path": "Legacy.cs"}))

	raw, _ := json.Marshal(out)
	require.NotContains(t, string(raw), "\u0000", "the editing context must not carry NUL-interleaved text")
}

func TestReadFile_ServesDecodedUTF16Source(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	out := utf16JSON(t, callTool(t, srv, "read_file", map[string]any{"path": "Legacy.cs"}))

	content, _ := out["content"].(string)
	require.NotContains(t, content, "\x00")
	require.Contains(t, content, "public class Legacy")
}

func TestEditFile_RefusesUTF16SourceWithEncodingMessage(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	res := callTool(t, srv, "edit_file", map[string]any{
		"path": "Legacy.cs", "old_string": "return 1;", "new_string": "return 2;",
	})
	require.True(t, res.IsError, "a UTF-16 source must be refused, not spliced")
	msg := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
	require.NotContains(t, msg, "not found", "the old misleading failure mode must be gone")
}

func TestEditSymbol_RefusesUTF16SourceWithEncodingMessage(t *testing.T) {
	srv, _ := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "B")

	res := callTool(t, srv, "edit_symbol", map[string]any{
		"id": node.ID, "old_source": "return 1;", "new_source": "return 2;",
	})
	require.True(t, res.IsError, "a UTF-16 source must be refused, not spliced")
	msg := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
	require.NotContains(t, msg, "not found", "the old misleading failure mode must be gone")
}

func TestUTF16ControlFileStillEdits(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	res := callTool(t, srv, "edit_file", map[string]any{
		"path": "Utf8.cs", "old_string": "}\n", "new_string": "} // touched\n",
	})
	require.False(t, res.IsError, "a plain UTF-8 file must still edit: %+v", res.Content)
}

func TestFileLooksUTF16_BoundedProbe(t *testing.T) {
	_, dir := utf16ServerWith(t)
	require.True(t, fileLooksUTF16(filepath.Join(dir, "Legacy.cs")))
	require.False(t, fileLooksUTF16(filepath.Join(dir, "Utf8.cs")))
	require.False(t, fileLooksUTF16(filepath.Join(dir, "missing.cs")))
}

// utf16FileBytes reads the UTF-16 fixture from disk for byte-unchanged
// assertions.
func utf16FileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "Legacy.cs"))
	require.NoError(t, err)
	return b
}

// TestRenameSymbol_RefusesUTF16SourceAndLeavesFileUntouched pins the #846
// round-2 blocker: rename_symbol used to splice UTF-8 identifier text into
// the NUL-interleaved bytes and report written:true. The refusal must come
// from the write planner (before any plan is materialised) and the file must
// keep its exact bytes.
// TestScaffold_RefusesUTF16SourceAndLeavesFileUntouched pins the scaffold
// write path (#846): scaffold's non-dry-run apply used to read, splice and
// write with os.WriteFile directly — outside commitFileMutation — so a
// UTF-16 source came back "applied": true with the file grown by an odd
// byte count, corrupting everything after the insertion point. The write
// now funnels through commitFileMutation, whose UTF-16 guard refuses it.
func TestScaffold_RefusesUTF16SourceAndLeavesFileUntouched(t *testing.T) {
	srv, dir := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "B")
	original := utf16FileBytes(t, dir)

	// findAndCallHandler's eager map does not carry the deferred
	// enhancement tools, so the handler is invoked directly — the same
	// pattern the safe_delete refusal test below uses.
	req := mcplib.CallToolRequest{}
	req.Params.Name = "scaffold"
	req.Params.Arguments = map[string]any{
		"id": node.ID, "new_name": "Cee", "dry_run": false,
	}
	res, err := srv.handleScaffold(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "a UTF-16 source must be refused, not scaffolded into: %+v", res.Content)
	msg := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
	require.Contains(t, msg, "scaffold", "the refusal must name the tool that refused")

	after := utf16FileBytes(t, dir)
	require.Equal(t, original, after, "the refused scaffold must not touch the file's bytes")
}

func TestRenameSymbol_RefusesUTF16SourceAndLeavesFileUntouched(t *testing.T) {
	srv, dir := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "B")
	original := utf16FileBytes(t, dir)

	res := callTool(t, srv, "rename_symbol", map[string]any{
		"id": node.ID, "new_name": "Cee",
	})
	require.True(t, res.IsError, "a UTF-16 source must be refused, not renamed: %+v", res.Content)
	msg := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
	require.Contains(t, msg, "rename_symbol", "the refusal must name the tool that refused")

	after := utf16FileBytes(t, dir)
	require.Equal(t, original, after, "the refused rename must not touch the file's bytes")
}

// TestSafeDeleteSymbol_RefusesUTF16SourceAndLeavesFileUntouched covers the
// other writer the round-2 review flagged: safe_delete_symbol used to
// survive on UTF-16LE + whole-line deletion by accident. It must refuse
// explicitly, for preview and apply alike.
func TestSafeDeleteSymbol_RefusesUTF16SourceAndLeavesFileUntouched(t *testing.T) {
	srv, dir := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "B")
	original := utf16FileBytes(t, dir)

	for _, dryRun := range []bool{false, true} {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "safe_delete_symbol"
		req.Params.Arguments = map[string]any{"id": node.ID, "dry_run": dryRun, "force": true}
		res, err := srv.handleSafeDeleteSymbol(context.Background(), req)
		require.NoError(t, err)
		require.True(t, res.IsError, "dry_run=%v: a UTF-16 source must be refused: %+v", dryRun, res.Content)
		msg := res.Content[0].(mcplib.TextContent).Text
		require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
	}
	after := utf16FileBytes(t, dir)
	require.Equal(t, original, after, "the refused delete must not touch the file's bytes")
}

// utf16BatchCall invokes batch_edit the way the other batch tests do —
// through the handler, since findAndCallHandler's name map does not carry it.
func utf16BatchCall(t *testing.T, srv *Server, edits []map[string]any) *mcplib.CallToolResult {
	t.Helper()
	items := make([]any, 0, len(edits))
	for _, e := range edits {
		items = append(items, e)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = map[string]any{"edits": items}
	res, err := srv.handleBatchEdit(context.Background(), req)
	require.NoError(t, err)
	return res
}

// utf16BatchReceipt parses a batch_edit receipt and returns the result
// entry for the single submitted op.
func utf16BatchReceipt(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "batch_edit reports per-op failures in the receipt: %+v", res.Content)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	results, ok := out["results"].([]any)
	require.True(t, ok, "receipt must carry results: %v", out)
	require.Len(t, results, 1)
	entry, ok := results[0].(map[string]any)
	require.True(t, ok)
	return entry
}

// TestBatchEditFileOp_RefusesUTF16SourceWithEncodingMessage pins the gap the
// round-2 review found: the batch edit_file op used to fail with the
// misleading "old_string not found in file" against NUL-interleaved bytes.
// Removing the refusal makes this fail on "not found".
func TestBatchEditFileOp_RefusesUTF16SourceWithEncodingMessage(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	res := utf16BatchCall(t, srv, []map[string]any{{
		"op": "edit_file", "path": "Legacy.cs",
		"old_string": "return 1;", "new_string": "return 2;",
	}})
	entry := utf16BatchReceipt(t, res)
	require.Equal(t, "failed", entry["status"])
	errMsg, _ := entry["error"].(string)
	require.Contains(t, errMsg, "UTF-16", "the refusal must name the encoding")
	require.NotContains(t, errMsg, "not found", "the old misleading failure mode must be gone")
}

// TestBatchEditSymbolOp_RefusesUTF16SourceWithEncodingMessage pins the
// existing batch edit_symbol refusal the round-2 review said was untested:
// if the refusal is removed, the match below runs against NUL-interleaved
// bytes and this fails on "old_source not found".
func TestBatchEditSymbolOp_RefusesUTF16SourceWithEncodingMessage(t *testing.T) {
	srv, _ := utf16ServerWith(t)
	node := utf16SymbolNamed(t, srv, "Legacy.cs", "B")

	res := utf16BatchCall(t, srv, []map[string]any{{
		"op": "edit_symbol", "id": node.ID,
		"old_source": "return 1;", "new_source": "return 2;",
	}})
	entry := utf16BatchReceipt(t, res)
	require.Equal(t, "failed", entry["status"])
	errMsg, _ := entry["error"].(string)
	require.Contains(t, errMsg, "UTF-16", "the refusal must name the encoding")
	require.NotContains(t, errMsg, "not found", "the old misleading failure mode must be gone")
}

// TestWriteFile_RefusesUTF16Source pins the shared-write-path backstop:
// commitFileMutation guards every tool that writes a source file, so
// write_file — which never matches old_string — must still refuse.
func TestWriteFile_RefusesUTF16Source(t *testing.T) {
	srv, _ := utf16ServerWith(t)

	res := callTool(t, srv, "write_file", map[string]any{
		"path": "Legacy.cs", "content": "public class Legacy\n{\n}\n",
	})
	require.True(t, res.IsError, "a UTF-16 source must refuse even a full write: %+v", res.Content)
	msg := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, msg, "UTF-16", "the refusal must name the encoding")
}
