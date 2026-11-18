package agents

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// TestWriteIfNotExistsCreatesAndSkips covers both the create and
// skip branches of the helper plus the DryRun prediction. Golden
// fixtures don't exercise DryRun, so we test it explicitly here.
func TestWriteIfNotExistsCreatesAndSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	var buf bytes.Buffer

	// 1. First call creates the file.
	a, err := WriteIfNotExists(&buf, path, "hello", ApplyOpts{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Action != ActionCreate {
		t.Fatalf("expected create, got %q", a.Action)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content: got %q want %q", got, "hello")
	}

	// 2. Second call finds the file and skips.
	a, err = WriteIfNotExists(&buf, path, "different", ApplyOpts{})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if a.Action != ActionSkip {
		t.Fatalf("expected skip, got %q", a.Action)
	}
	// Content must be unchanged — skip is never overwrite.
	got, _ = os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("skip must not overwrite: got %q", got)
	}

	// 3. DryRun on a missing file reports would-create, doesn't write.
	missing := filepath.Join(dir, "new.txt")
	a, err = WriteIfNotExists(&buf, missing, "x", ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if a.Action != ActionWouldCreate {
		t.Fatalf("expected would-create, got %q", a.Action)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote file: %v", err)
	}
}

// TestMergeJSONCreatesMergesAndSkipsIdempotent covers the three
// transitions the MCP installer relies on: fresh file, merge into
// existing, and no-op on re-run. This is the behavioural contract
// golden tests will compare against byte-for-byte.
// TestMergeJSON_DryRunNeverWritesBackup guards the regression where a
// dry-run over a malformed (or empty) existing file still dropped a
// .bak sibling on disk — dry-run must touch nothing.
func TestMergeJSON_DryRunNeverWritesBackup(t *testing.T) {
	dir := t.TempDir()
	add := func(root map[string]any, _ bool) (bool, error) {
		return UpsertMCPServer(root, "gortex", DefaultGortexMCPEntry(), ApplyOpts{}), nil
	}

	// A malformed existing file under dry-run.
	mal := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(mal, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeJSON(io.Discard, mal, add, ApplyOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mal + ".bak"); !os.IsNotExist(err) {
		t.Errorf("dry-run must not write a .bak (err=%v)", err)
	}

	// An empty file is treated as an empty object, not malformed: no
	// backup even on a real write.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeJSON(io.Discard, empty, add, ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty + ".bak"); !os.IsNotExist(err) {
		t.Errorf("an empty file must not be treated as malformed / backed up (err=%v)", err)
	}

	// A genuinely malformed file on the real write path still gets a .bak.
	mal2 := filepath.Join(dir, "malformed2.json")
	if err := os.WriteFile(mal2, []byte("garbage{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeJSON(io.Discard, mal2, add, ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mal2 + ".bak"); err != nil {
		t.Errorf("a real merge over a malformed file should keep a .bak: %v", err)
	}
}

func TestMergeJSONCreatesMergesAndSkipsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")

	var buf bytes.Buffer

	addGortex := func(root map[string]any, existed bool) (bool, error) {
		return UpsertMCPServer(root, "gortex", DefaultGortexMCPEntry(), ApplyOpts{}), nil
	}

	// 1. Missing file -> create.
	a, err := MergeJSON(&buf, path, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Action != ActionCreate {
		t.Fatalf("expected create, got %q", a.Action)
	}

	// 2. Re-run -> skip (idempotent).
	a, err = MergeJSON(&buf, path, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if a.Action != ActionSkip {
		t.Fatalf("expected skip, got %q", a.Action)
	}

	// 3. Pre-populate with an unrelated MCP server, merge adds ours
	//    without clobbering theirs.
	existing := map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "other"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	fresh := filepath.Join(dir, "mcp-pre.json")
	if err := os.WriteFile(fresh, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err = MergeJSON(&buf, fresh, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if a.Action != ActionMerge {
		t.Fatalf("expected merge, got %q", a.Action)
	}
	content, _ := os.ReadFile(fresh)
	var out map[string]any
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatal(err)
	}
	servers := out["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("merge clobbered existing 'other' server: %v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("merge didn't add 'gortex': %v", servers)
	}
}

// TestStripJSONComments covers the JSONC sanitiser: comments and
// trailing commas are removed, but `//`, `/* */`, and commas inside
// string literals are preserved.
func TestStripJSONComments(t *testing.T) {
	in := `{
  // a line comment
  "url": "https://example.com/path", /* block */
  "note": "a, b, c // not a comment",
  "list": [1, 2, 3,],
  "obj": { "k": "v", },
}`
	got := stripJSONComments([]byte(in))
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("sanitised JSONC did not parse: %v\n---\n%s", err, got)
	}
	if out["url"] != "https://example.com/path" {
		t.Fatalf("`//` inside a string was stripped: %v", out["url"])
	}
	if out["note"] != "a, b, c // not a comment" {
		t.Fatalf("comment/comma inside a string was altered: %v", out["note"])
	}
	if list, ok := out["list"].([]any); !ok || len(list) != 3 {
		t.Fatalf("trailing comma handling broke the array: %v", out["list"])
	}
}

// TestMergeJSON_JSONCMergesInsteadOfClobbering guards the OpenCode
// path: merging into an existing `.jsonc` with comments must preserve
// the user's data keys (not back the file up as malformed and start
// fresh).
func TestMergeJSON_JSONCMergesInsteadOfClobbering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.jsonc")
	if err := os.WriteFile(path, []byte(`{
  // user config
  "theme": "dark",
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	add := func(root map[string]any, _ bool) (bool, error) {
		root["added"] = true
		return true, nil
	}
	a, err := MergeJSON(io.Discard, path, add, ApplyOpts{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if a.Action != ActionMerge {
		t.Fatalf("expected merge, got %q", a.Action)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatalf("a valid commented .jsonc must not be backed up as malformed")
	}
	var out map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("re-marshalled output not valid JSON: %v", err)
	}
	if out["theme"] != "dark" {
		t.Fatalf("merge dropped the user's data key: %v", out)
	}
	if out["added"] != true {
		t.Fatalf("merge didn't apply the mutation: %v", out)
	}
}

// TestRegistryFilterValidatesNames ensures we hard-error on typos
// rather than silently dropping them — a key UX requirement from the
// init plan.
func TestRegistryFilterValidatesNames(t *testing.T) {
	r := NewRegistry()
	r.Register(stubAdapter{name: "alpha"})
	r.Register(stubAdapter{name: "beta"})

	if _, err := r.Filter("alpha,gamma", ""); err == nil {
		t.Fatal("expected error on unknown 'gamma', got nil")
	}
	got, err := r.Filter("alpha", "beta")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "alpha" {
		t.Fatalf("filter returned %v, want [alpha]", names(got))
	}
	// auto + skip should yield everything minus the skipped.
	got, err = r.Filter("auto", "alpha")
	if err != nil {
		t.Fatalf("auto+skip: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "beta" {
		t.Fatalf("auto+skip returned %v, want [beta]", names(got))
	}
}

type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                          { return s.name }
func (s stubAdapter) DocsURL() string                       { return "" }
func (s stubAdapter) Detect(Env) (bool, error)              { return true, nil }
func (s stubAdapter) Plan(Env) (*Plan, error)               { return &Plan{}, nil }
func (s stubAdapter) Apply(Env, ApplyOpts) (*Result, error) { return &Result{Name: s.name}, nil }

func names(as []Adapter) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}

// TestAtomicWriteFileCreatesAndOverwrites guards the core write path every
// MCP file tool funnels through (write_file, edit_file, move, ...): a
// fresh write lands the content and a second write atomically replaces it,
// leaving no stray *.gortex.tmp-* file behind. This is also the
// happy path for platform.ReplaceFile — its retry loop must be
// transparent when the rename succeeds first try.
func TestAtomicWriteFileCreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")

	if err := AtomicWriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("content: got %q want %q", got, "first")
	}

	// Overwrite must replace, not append or corrupt.
	if err := AtomicWriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "second" {
		t.Fatalf("overwrite content: got %q want %q", got, "second")
	}

	// Success must not leave the sibling temp file behind.
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.gortex.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("leftover temp files: %v", leftovers)
	}
}

// TestAtomicWriteFileReturnsNonRetryableRenameErr checks that a rename failure
// which isn't a transient sharing violation (here: a missing source) is
// surfaced immediately rather than retried — the retry budget is reserved
// for the Windows lock race, not for masking genuine errors. On every
// platform ERROR_FILE_NOT_FOUND / ENOENT is non-retryable, so this holds
// cross-platform.
func TestAtomicWriteFileReturnsNonRetryableRenameErr(t *testing.T) {
	dir := t.TempDir()
	if err := platform.ReplaceFile(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "dest")); err == nil {
		t.Fatal("expected an error renaming a missing source, got nil")
	}
}

// TestAtomicWriteFileReapsStaleTempOrphans guards the cleanup of temp
// files a crashed write orphaned ("not cleared"): a *.gortex.tmp-*
// stranded when a process died between CreateTemp and the rename must be
// reaped by a later AtomicWriteFile — but a temp still in flight (fresh
// mtime) and any unrelated file must be left untouched.
func TestAtomicWriteFileReapsStaleTempOrphans(t *testing.T) {
	dir := t.TempDir()

	// A stale orphan from a write that crashed long ago.
	stale := filepath.Join(dir, "config.json"+tempInfix+"123456")
	if err := os.WriteFile(stale, []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	// A temp another write is still filling in — fresh mtime, must survive.
	fresh := filepath.Join(dir, "other.json"+tempInfix+"987654")
	if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real, unrelated file must never be swept.
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Any AtomicWriteFile into the directory sweeps it first.
	target := filepath.Join(dir, "config.json")
	if err := AtomicWriteFile(target, []byte("new content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp orphan must be reaped (stat err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("in-flight temp (fresh mtime) must be preserved: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated file must be untouched: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new content" {
		t.Errorf("target content: got %q want %q", got, "new content")
	}
}
