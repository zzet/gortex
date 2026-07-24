# Coding Conventions

**Analysis Date:** 2026-07-24

## Naming Patterns

**Files:**
- Source files: `lowercase_name.go` — descriptive package file names
- Test files: `*_test.go` suffixed to source file or concept name
- Benchmark tests: `*_bench_test.go` suffix
- Examples: `config_cmd.go`, `config_cmd_test.go`, `config_cmd_bench_test.go`

**Functions:**
- Exported (public): PascalCase — `AnalyzeResolutionOutcomes()`, `EnrichGraph()`
- Unexported (private): camelCase — `normalizeExtractionMetadata()`, `legacyLocalVariable()`
- Test functions: `TestFunctionName_ContextOrCase` pattern — `TestLooksLikeGlob()`, `TestAnalyze_KindArgAndUniversalFlags()`
- Benchmark functions: `BenchmarkFunctionName` pattern — `BenchmarkIndex_Self()`

**Variables:**
- Local/package-level: camelCase — `sourceLines`, `nodesByID`, `testNodes`
- Constants: PascalCase (typed constants) or UPPER_CASE (simple strings) — `KindFile`, `OutcomeAmbiguousMultiMatch`
- Interface/type variables: PascalCase — `EdgeKind`, `NodeKind`

**Types:**
- Exported: PascalCase — `Node`, `Edge`, `ResolutionRow`, `ExtractionResult`
- Unexported: camelCase — `snapshotHeader`, `classVal`
- Constants for kinds: PascalCase with descriptive prefix — `KindFunction`, `EdgeCalls`, `OutcomeStubOnly`

## Code Style

**Formatting:**
- Standard Go formatting via `gofmt` (implicit in build)
- Line length: typically 80–100 characters; longer lines acceptable for complex logic
- Indentation: tabs (Go standard)

**Linting:**
- Tool: golangci-lint v2.11.4 (pinned in CI via `.github/workflows/ci.yml`)
- Config: `.golangci.yaml` — enforces standard + custom exclusions
- Key rules enforced:
  - `errcheck`: Catches unchecked errors; excludes fmt.Print family and `io.Closer.Close()` (noise reduction)
  - `govet`: Built-in Go vet checks
  - `ineffassign`: Detects unused assignments
  - `staticcheck`: Static analysis
  - `unused`: Finds unused variables/functions
- Generated files: Excluded via `lax` preset (auto-detects generated code)

**Code Organization:**
- Package-level: Document the purpose and responsibility
- Types first, then functions that use them
- Exported symbols before unexported
- Related constants grouped with explanatory comments

## Import Organization

**Order:**
1. Standard library (`import ( "fmt" "strings" ... )`)
2. Third-party packages (`github.com/...`, `gopkg.in/...`, etc.)
3. Internal packages (`github.com/zzet/gortex/internal/...`)

**Path Aliases:**
- No aliases used; full import paths are explicit
- Internal packages always use the module path: `github.com/zzet/gortex/internal/config`

**Example:**
```go
import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)
```

## Comments

**Documentation Comments:**
- Every exported function must have a doc comment starting with the function name
- Format: `// FunctionName does X and returns Y` followed by details on purpose, parameters, and return values
- Multi-line comments explain edge cases, preconditions, and postconditions

**Special Comment Markers:**
- `PURPOSE —` explains what the function/constant/section solves
- `RATIONALE —` justifies design decisions
- `KEYWORDS —` lists searchable tags for finding related code
- Example from `resolution_outcomes.go`:
  ```go
  // PURPOSE — pure computation core for the resolution-outcomes analyzer:
  // classifies every unresolved call/reference edge by the structured reason
  // the resolver gave up and returns a per-reason rollup plus example rows.
  // RATIONALE — extracted from the MCP handler so the taxonomy logic is
  // independently testable and reusable across surfaces (MCP, CLI, etc.).
  // KEYWORDS — resolution_outcomes, unresolved, taxonomy, pure, calculation
  ```

**Constant and Type Comments:**
- Every exported const block must explain what each constant represents
- Example from `graph/node.go`:
  ```go
  // KindParam represents a single function/method parameter. ID
  // convention: `<func-id>#param:<name>`. EdgeParamOf links the param
  // node back to its owner; EdgeTypedAs binds it to its declared
  // type. Created when index.function_shape.enabled is true.
  KindParam NodeKind = "param"
  ```

**Inline Comments:**
- Explain complex logic, non-obvious optimizations, and invariants
- Used sparingly; clear code is better than commented code

## Error Handling

**Patterns:**
- Standard Go: check `if err != nil { return ..., err }` immediately after error-prone calls
- Return errors explicitly, don't ignore them
- Use `fmt.Errorf()` to add context: `fmt.Errorf("no repo entry named %q in global config", name)`
- When ignoring errors is intentional, document why in a comment

**Example:**
```go
if err != nil {
	return nil, err
}
cfg, err := loadConfig(path)
if err != nil {
	return fmt.Errorf("failed to load config: %w", err)
}
```

**Critical Pattern:**
- Errors bubble up; don't suppress them silently
- Use `%w` wrapper in `fmt.Errorf()` to preserve error chains for debugging

## Function Design

**Size:** 
- Functions typically 15–50 lines (most < 100 lines)
- When logic exceeds 50 lines, consider extracting helpers

**Parameters:**
- Keep parameter count ≤ 5; use struct if more needed
- Clear, descriptive names: `sourceLines`, `changedFiles`, `reasonFilter`
- No generic single-letter names except loop counters (`i`, `j`) and well-known patterns (`b` for `*testing.B`)

**Return Values:**
- Exported functions return `(result T, err error)` — error last
- Helpers often return `(count int, err error)` or just `T` for simple operations
- Multiple return values acceptable for (value, error) pairs

## Module Design

**Exports:**
- Package exposes only public API; helpers are unexported (lowercase)
- Each package is a cohesive unit: `indexer`, `parser`, `resolver`, `graph`, etc.

**Package Structure Example:**
- `internal/indexer/` — index construction
  - `indexer.go` — main entry point (Index function)
  - `metadata_normalize.go` — helpers for metadata normalization
  - `test_edges.go` — test symbol and edge marking
  - `*_test.go` — unit tests

**Barrel Files:**
- Not used; each package is imported by full path
- No `__init__.go` pattern or re-export files

## Type Definitions

**Structs:**
- Fields are exported (PascalCase) when needed by consumers
- JSON field tags map to external formats: 
  ```go
  type ResolutionRow struct {
  	From       string `json:"from"`
  	To         string `json:"to"`
  	Kind       string `json:"edge_kind"`
  	Name       string `json:"name"`
  	Reason     string `json:"reason"`
  	Candidates int    `json:"candidates"`
  }
  ```

**Interfaces:**
- Receiver-specific methods (pointer vs. value receivers chosen based on mutability)
- Methods grouped by interface they satisfy

**Constants and Enums:**
- String constants for enums (NodeKind, EdgeKind) — checked at compile time via type system
- Example:
  ```go
  type NodeKind string
  const (
      KindFile     NodeKind = "file"
      KindFunction NodeKind = "function"
      KindMethod   NodeKind = "method"
  )
  ```

## Cross-Cutting Concerns

**Logging:**
- Uses Go's built-in `log` package or custom `conversationlog` package (for LLM conversation recording)
- No centralized logging framework; each package manages its own observability

**Validation:**
- Input validation happens at package boundaries (public functions)
- nil checks for critical pointers with early returns or defensive defaults

**Authentication & Authorization:**
- Configuration handled via `.gortex.yaml` and environment variables
- No in-code secrets; all credentials external

---

*Convention analysis: 2026-07-24*
