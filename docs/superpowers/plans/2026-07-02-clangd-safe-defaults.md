# Clangd Safe Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Gortex's built-in clangd semantic-enrichment launch safe by default by disabling clang-tidy, disabling background indexing, and limiting clangd worker concurrency.

**Architecture:** Keep the existing LSP registry and provider override architecture. Pin the built-in clangd args with a focused registry test, update only the clangd spec defaults, and document the new default command and rationale.

**Tech Stack:** Go, standard `testing` package, Gortex LSP registry, Markdown docs.

---

## File Structure

- Modify: `internal/semantic/lsp/registry_test.go`
  - Add a focused unit test that asserts the built-in `clangd` spec has the exact safe default args.
- Modify: `internal/semantic/lsp/registry.go`
  - Change only the built-in `clangd` `Args` slice and its adjacent explanatory comment.
- Modify: `docs/lsp.md`
  - Update the clangd command table row and add a short note explaining why clang-tidy and background indexing are off by default for enrichment.

### Task 1: Pin clangd safe default args with a failing test

**Files:**
- Modify: `internal/semantic/lsp/registry_test.go`

- [ ] **Step 1: Write the failing test**

Add this test after `TestPyreflyAndTsgoSpecs` and before `TestSpecWithOverrides`:

```go
// TestClangdSpecUsesSafeEnrichmentDefaults pins clangd's built-in
// enrichment argv. Gortex needs request-driven graph signal from
// clangd, not repo clang-tidy diagnostics or a persistent background
// project index.
func TestClangdSpecUsesSafeEnrichmentDefaults(t *testing.T) {
	clangd := SpecByName("clangd")
	if clangd == nil {
		t.Fatal("clangd spec not registered")
	}

	want := []string{
		"--background-index=false",
		"--clang-tidy=false",
		"--header-insertion=never",
		"-j=1",
	}
	if got := clangd.Args; !slices.Equal(got, want) {
		t.Fatalf("clangd args = %v, want %v", got, want)
	}
}
```

Also add the `slices` import to the import block:

```go
import (
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/semantic"
)
```

- [ ] **Step 2: Run the focused test to verify it fails**

Run:

```bash
go test ./internal/semantic/lsp -run TestClangdSpecUsesSafeEnrichmentDefaults -count=1
```

Expected: FAIL. The failure should show the current args are:

```text
[--background-index --header-insertion=never]
```

- [ ] **Step 3: Commit the failing test**

Run:

```bash
git add internal/semantic/lsp/registry_test.go
git commit -m "test: pin clangd safe enrichment defaults"
```

Expected: commit succeeds with only the failing test change.

### Task 2: Change clangd built-in args

**Files:**
- Modify: `internal/semantic/lsp/registry.go`

- [ ] **Step 1: Update the clangd spec comment and args**

Replace the current clangd comment and `Args` value with:

```go
	{
		Name:    "clangd",
		Command: "clangd",
		// Gortex uses clangd for request-driven graph evidence during
		// enrichment, not lint diagnostics or a persistent project index.
		// Keep clang-tidy and background indexing off by default so broad
		// repo .clang-tidy configs and large C++ trees do not dominate CPU
		// or crash-loop the enrichment subprocess. Users can opt back in via
		// a semantic.providers override in .gortex.yaml.
		Args: []string{
			"--background-index=false",
			"--clang-tidy=false",
			"--header-insertion=never",
			"-j=1",
		},
```

Leave the existing `Languages`, `Extensions`, `LanguageIDs`, `Priority`, `Daemon`, and `MaxParallel` values unchanged.

- [ ] **Step 2: Run the focused test to verify it passes**

Run:

```bash
go test ./internal/semantic/lsp -run TestClangdSpecUsesSafeEnrichmentDefaults -count=1
```

Expected: PASS.

- [ ] **Step 3: Commit the implementation**

Run:

```bash
git add internal/semantic/lsp/registry.go
git commit -m "fix(lsp): use safe clangd enrichment defaults"
```

Expected: commit succeeds with only the registry implementation change.

### Task 3: Update LSP documentation

**Files:**
- Modify: `docs/lsp.md`

- [ ] **Step 1: Update the clangd command table row**

Change the `clangd` row in the server registry table from:

```markdown
| `clangd`                     | `clangd --background-index`      | c, c++, objc, objc++        | 5                |
```

to:

```markdown
| `clangd`                     | `clangd --background-index=false --clang-tidy=false --header-insertion=never -j=1` | c, c++, objc, objc++ | 5                |
```

- [ ] **Step 2: Add a short note below the fallback list**

After the fallback list ending with:

```markdown
- `phpactor` → falls back to `intelephense --stdio`.
```

add:

```markdown
The built-in `clangd` spec disables background indexing and clang-tidy by
default because semantic enrichment needs request-driven graph evidence,
not lint diagnostics or a persistent project index. Repositories that want
those clangd features can override the `clangd` provider args in
`.gortex.yaml`.
```

- [ ] **Step 3: Commit the docs update**

Run:

```bash
git add docs/lsp.md
git commit -m "docs: document clangd safe defaults"
```

Expected: commit succeeds with only the documentation change.

### Task 4: Verify and prepare final summary

**Files:**
- No file edits expected.

- [ ] **Step 1: Run the relevant package tests**

Run:

```bash
go test ./internal/semantic/lsp -count=1
```

Expected: PASS.

- [ ] **Step 2: Run the broader required test command if time permits**

Run:

```bash
go test -race ./...
```

Expected: PASS. If this is too slow or fails for an unrelated pre-existing issue, capture the failing package and error for the final summary.

- [ ] **Step 3: Inspect git history and working tree**

Run:

```bash
git status --short --branch
git log --oneline -n 5
```

Expected: branch is `fix/issue-220-clangd-tidy-high-cpu`; working tree is clean except any intentional plan/spec files; recent commits include the design, test, implementation, and docs commits.

- [ ] **Step 4: Final response**

Summarize:

- The clangd default args now disable background indexing and clang-tidy and use `-j=1`.
- The focused registry test pins those args.
- `docs/lsp.md` documents the new default and opt-in override path.
- Verification commands run and their outcomes.
