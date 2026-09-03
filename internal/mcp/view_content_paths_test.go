package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// Content and on-disk paths under a view.
//
// The search lane already proved a routed request searches the checkout's own
// bytes. These are the same claim for the content doors: the file a request
// reads, the source a symbol slices, and the absolute path an answer reports
// all belong to the checkout the view routes to — not to the canonical one.
// The last test here is the other side of it: a view with no working copy must
// report no on-disk path at all rather than one in the canonical checkout.
//
// Every worktree assertion is paired with the same call from the primary's
// working directory, which is served by the base corpus. Without the pair a
// passing read proves nothing: the two checkouts would agree on any file the
// test never made differ.

const (
	// worktreeMarker is a body line only the worktree's copy of keep.go
	// carries — invisible to any door that elides bodies.
	worktreeMarker = "zephyr-worktree-content-marker"
	// worktreeDecl is a declaration only the worktree's copy carries, so a
	// body-eliding door still has something to disagree about.
	worktreeDecl = "ZephyrWorktreeOnly"
)

// divergeWorktree rewrites keep.go in the worktree so its bytes, its line
// count and its symbol range all differ from the primary's, then waits for the
// edit to reach a routed generation.
func (w *worktreeSearchStack) divergeWorktree(t *testing.T) {
	t.Helper()
	previous := w.dirtyGeneration(t)
	refWriteFiles(t, w.worktree, map[string]string{
		"keep.go": "package repo\n\nfunc Keeper() {\n\t// " + worktreeMarker +
			"\n\tprintln(\"worktree\")\n}\n\nfunc " + worktreeDecl + "() {}\n",
	})
	w.awaitDirtyGenerationAfter(t, previous)
}

// callFrom runs one tool through the request middleware with the session bound
// to cwd, which is what decides whether the routed view or the base answers.
func (w *worktreeSearchStack) callFrom(
	t *testing.T,
	cwd, tool string,
	args map[string]any,
	handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), cwd)
	res, err := w.srv.wrapToolHandler(handler)(ctx, req)
	if err != nil {
		t.Fatalf("%s from %s: %v", tool, cwd, err)
	}
	if res.IsError {
		t.Fatalf("%s from %s was refused: %s", tool, cwd, viewResultText(t, res))
	}
	return res
}

// TestReadFileRoutedWorktreeViewServesTheCheckoutBytes is the defining claim:
// read_file through a routed worktree view returns that worktree's content, and
// the same call outside the view still returns the canonical checkout's.
func TestReadFileRoutedWorktreeViewServesTheCheckoutBytes(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"repo prefixed", refTestPrefix + "/keep.go"},
		{"repo relative", "keep.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routed := readFileContent(t, stack.callFrom(t, stack.worktree, "read_file",
				map[string]any{"path": tc.path}, stack.srv.handleReadFile))
			if !strings.Contains(routed, worktreeMarker) {
				t.Errorf("the routed read served the canonical checkout's bytes:\n%s", routed)
			}

			base := readFileContent(t, stack.callFrom(t, stack.primary, "read_file",
				map[string]any{"path": tc.path}, stack.srv.handleReadFile))
			if strings.Contains(base, worktreeMarker) {
				t.Errorf("the canonical checkout carries the marker, so the hit above proves nothing:\n%s", base)
			}
		})
	}
}

// TestReadFileRoutedWorktreeViewServesAnAbsoluteWorktreePath pins the other
// spelling a caller reaches a routed file by: the absolute path inside the
// checkout, which must not be re-rooted onto the canonical one.
func TestReadFileRoutedWorktreeViewServesAnAbsoluteWorktreePath(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	abs := filepath.Join(stack.worktree, "keep.go")
	got := readFileContent(t, stack.callFrom(t, stack.worktree, "read_file",
		map[string]any{"path": abs}, stack.srv.handleReadFile))
	if !strings.Contains(got, worktreeMarker) {
		t.Errorf("an absolute worktree path did not serve the worktree's bytes:\n%s", got)
	}
}

// TestSearchSymbolsRoutedWorktreeViewRootsAbsolutePaths is the path half of the
// claim: an answer served through the view reports paths inside the checkout
// the view reads, so an agent that opens one gets the bytes it was shown.
func TestSearchSymbolsRoutedWorktreeViewRootsAbsolutePaths(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	routed := searchSymbolAbsPath(t, stack.callFrom(t, stack.worktree, "search_symbols",
		map[string]any{"query": "Keeper"}, stack.srv.handleSearchSymbols), "Keeper")
	if want := filepath.Join(stack.worktree, "keep.go"); routed != want {
		t.Errorf("absolute_file_path is %q, want the worktree's %q", routed, want)
	}

	base := searchSymbolAbsPath(t, stack.callFrom(t, stack.primary, "search_symbols",
		map[string]any{"query": "Keeper"}, stack.srv.handleSearchSymbols), "Keeper")
	if want := filepath.Join(stack.primary, "keep.go"); base != want {
		t.Errorf("outside the view absolute_file_path is %q, want the canonical %q", base, want)
	}
}

// TestGetSymbolSourceRoutedWorktreeViewSlicesTheCheckoutContent proves the
// symbol door reads the same bytes the view's line ranges were computed from.
// The worktree's Keeper spans more lines than the canonical one, so slicing the
// canonical file with the view's range silently drops the marker.
func TestGetSymbolSourceRoutedWorktreeViewSlicesTheCheckoutContent(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	id := refTestPrefix + "/keep.go::Keeper"
	routed := symbolSourceText(t, stack.callFrom(t, stack.worktree, "get_symbol_source",
		map[string]any{"id": id, "context_lines": 0}, stack.srv.handleGetSymbolSource))
	if !strings.Contains(routed, worktreeMarker) {
		t.Errorf("the routed symbol source came out of the canonical checkout:\n%s", routed)
	}

	base := symbolSourceText(t, stack.callFrom(t, stack.primary, "get_symbol_source",
		map[string]any{"id": id, "context_lines": 0}, stack.srv.handleGetSymbolSource))
	if strings.Contains(base, worktreeMarker) {
		t.Errorf("the canonical symbol source carries the marker, so the hit above proves nothing:\n%s", base)
	}
}

// TestGetEditingContextRoutedWorktreeViewReadsTheCheckoutFile covers the third
// content door: the compressed whole-file view, which reads the file off disk
// and elides it. Bodies are gone by the time it answers, so the claim is made
// on a declaration only the worktree's copy carries.
func TestGetEditingContextRoutedWorktreeViewReadsTheCheckoutFile(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	args := map[string]any{"path": refTestPrefix + "/keep.go", "compress_bodies": true}
	routed := editingContextCompressed(t, stack.callFrom(t, stack.worktree, "get_editing_context",
		args, stack.srv.handleGetEditingContext))
	if !strings.Contains(routed, worktreeDecl) {
		t.Errorf("the routed editing context came out of the canonical checkout:\n%s", routed)
	}

	base := editingContextCompressed(t, stack.callFrom(t, stack.primary, "get_editing_context",
		args, stack.srv.handleGetEditingContext))
	if strings.Contains(base, worktreeDecl) {
		t.Errorf("the canonical editing context carries the declaration, so the hit above proves nothing:\n%s", base)
	}
}

// TestSearchSymbolsRefViewReportsNoAbsolutePath is the committed-tree half of
// the claim. A ref view's content lives in the object store and the canonical
// checkout has some other branch out, so an absolute path in the answer would
// name bytes the view does not serve. Reporting none is the honest answer.
func TestSearchSymbolsRefViewReportsNoAbsolutePath(t *testing.T) {
	stack := newRefStack(t)

	res, err := stack.call(t, "search_symbols", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"query": "New"}, stack.srv.handleSearchSymbols)
	if err != nil {
		t.Fatalf("search_symbols through the ref view: %v", err)
	}
	if res.IsError {
		t.Fatalf("search_symbols through the ref view was refused: %s", viewResultText(t, res))
	}
	if abs := searchSymbolAbsPath(t, res, "New"); abs != "" {
		t.Errorf("a view of a committed tree reported the on-disk path %q", abs)
	}
}

// readFileContent reads the content field out of a read_file answer.
func readFileContent(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	var obj struct {
		Content string `json:"content"`
	}
	text := viewResultText(t, res)
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("unmarshal read_file result: %v\n%s", err, text)
	}
	return obj.Content
}

// symbolSourceText reads the source field out of a get_symbol_source answer.
func symbolSourceText(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	var obj struct {
		Source string `json:"source"`
	}
	text := viewResultText(t, res)
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("unmarshal get_symbol_source result: %v\n%s", err, text)
	}
	return obj.Source
}

// editingContextCompressed reads the whole-file compressed view out of a
// get_editing_context answer.
func editingContextCompressed(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	var obj struct {
		SourceCompressed string `json:"source_compressed"`
	}
	text := viewResultText(t, res)
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("unmarshal get_editing_context result: %v\n%s", err, text)
	}
	if obj.SourceCompressed == "" {
		t.Fatalf("get_editing_context returned no compressed source:\n%s", text)
	}
	return obj.SourceCompressed
}

// searchSymbolAbsPath reads the absolute path search_symbols reported for one
// named symbol.
func searchSymbolAbsPath(t *testing.T, res *mcplib.CallToolResult, name string) string {
	t.Helper()
	var obj struct {
		Results []struct {
			Name             string `json:"name"`
			AbsoluteFilePath string `json:"absolute_file_path"`
		} `json:"results"`
	}
	text := viewResultText(t, res)
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("unmarshal search_symbols result: %v\n%s", err, text)
	}
	for _, r := range obj.Results {
		if r.Name == name {
			return r.AbsoluteFilePath
		}
	}
	t.Fatalf("search_symbols returned no %q:\n%s", name, text)
	return ""
}
