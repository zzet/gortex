# Testing Patterns

**Analysis Date:** 2026-07-24

## Test Framework

**Runner:**
- Go's built-in `testing` package (standard library)
- Version: Go 1.26.5
- CI: golangci-lint v2.11.4 enforces quality gates

**Assertion Library:**
- `github.com/stretchr/testify` v1.11.1
- Imports: `testify/assert`, `testify/require`

**Run Commands:**
```bash
go test -race ./...              # Run all tests with race detector
go test -bench=. -benchmem -count=1 -benchtime=1s ./internal/...  # Benchmarks
go test -v ./...                 # Verbose output
```

**Makefile Targets:**
- `make test` — runs `go test -race ./...`
- `make bench` — runs benchmarks on performance-critical packages

## Test File Organization

**Location:**
- **Co-located:** Test files live in the same package and directory as source — `config_cmd.go` paired with `config_cmd_test.go`
- **Package scope:** Tests in the same package can access unexported helpers and functions

**Naming:**
- `*_test.go` suffix for unit and integration tests
- `*_bench_test.go` suffix for benchmarks
- Examples: `config_cmd_test.go`, `coverage_test.go`, `metadata_normalize_test.go`

**Structure:**
```
internal/indexer/
├── indexer.go
├── indexer_test.go          # Tests for indexer.go
├── metadata_normalize.go
├── metadata_normalize_test.go
├── test_edges.go            # Test utilities (exported, used by other tests)
├── test_edges_test.go       # Tests for test_edges.go
├── testpattern.go           # Test framework utilities
├── bench_test.go            # Benchmarks
└── bench_lowmem_test.go     # Low-memory benchmarks
```

## Test Structure

**Basic Test Function:**
```go
func TestLooksLikeGlob(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"pkg/foo", false},
		{"pkg/*.go", true},
		{"*.tmp", true},
	}
	for _, tc := range cases {
		if got := looksLikeGlob(tc.in); got != tc.want {
			t.Errorf("looksLikeGlob(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
```

**Table-Driven Tests:**
- Preferred pattern: struct-based cases with `in` / `want` (or descriptive field names)
- Iterate over cases, test each one
- Report failures with context (input, expected, actual)
- Examples: `TestLooksLikeGlob()`, `TestWireContractFingerprint()`

**Test Helpers:**
- Mark helpers with `t.Helper()` on the first line
- Helpers don't count as test depth in error reporting
- Example:
  ```go
  func resolvePath(t *testing.T, p string) string {
  	t.Helper()
  	resolved, err := filepath.EvalSymlinks(p)
  	require.NoError(t, err)
  	return resolved
  }
  ```

**Setup and Teardown:**
- Use `t.Cleanup()` to register cleanup functions
- Called in LIFO order when the test completes
- Example:
  ```go
  func TestSomething(t *testing.T) {
  	orig := someGlobal
  	t.Cleanup(func() { someGlobal = orig })
  	
  	someGlobal = newValue
  	// test logic
  }
  ```

## Assertion Patterns

**Using testify/require:**
- `require.NoError(t, err)` — fail immediately if error (precondition check)
- `require.NotNil(t, value)` — fail immediately if nil
- `require.Equal(t, expected, actual)` — strong assertion for critical expectations
- `require.Contains(t, haystack, needle)` — string/slice contains check

**Using testify/assert:**
- `assert.Equal(t, expected, actual)` — check equality without failing immediately
- `assert.False(t, condition)` — soft assertion for validation
- `assert.Contains(t, slice, element)` — loose assertion

**Guidelines:**
- Use `require.*` for precondition checks (nil guards, setup validation)
- Use `assert.*` for validation checks within the test logic
- Multiple assertions per test are fine (good practice)

**Example from `analyze_test.go`:**
```go
func TestAnalyze_KindArgAndUniversalFlags(t *testing.T) {
	cap, _, err := runAnalyzeCmd(t,
		"--kind", "hotspots", "--arg", "threshold:=0.8", "--limit", "5")
	require.NoError(t, err)           // Precondition
	require.NotNil(t, cap)            // Precondition
	require.Equal(t, "analyze", cap.tool)  // Expected behavior
	require.Equal(t, "hotspots", cap.args["kind"])
	require.Equal(t, 0.8, cap.args["threshold"]) // walrus raw-JSON number
	require.Equal(t, 5, cap.args["limit"])
	require.Equal(t, "json", cap.args["format"])
}
```

## Subtests (t.Run)

**Pattern:**
```go
func TestWireContractFingerprint(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"graph.Node", reflect.TypeOf(graph.Node{}), ""},
		{"graph.Edge", reflect.TypeOf(graph.Edge{}), ""},
	}
	
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fingerprintType(c.typ)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
```

**Usage:**
- Used for table-driven tests with many cases
- Each case runs as a distinct subtest (visible in test output with `-v`)
- Allows parallel execution when cases are independent
- Not mandatory (some tests use table-driven without t.Run)

## Mocking

**Framework:** 
- No dedicated mocking library (like testify/mock)
- Mocking done via dependency injection and function-valued fields

**Pattern — Stub/Spy Pattern:**
```go
func runAnalyzeCmd(t *testing.T, argv ...string) (*capturedAnalyze, *bytes.Buffer, error) {
	t.Helper()
	
	// Stub the daemon seam
	orig := analyzeDaemonTool
	t.Cleanup(func() { analyzeDaemonTool = orig })
	
	var cap *capturedAnalyze
	analyzeDaemonTool = func(repo, tool string, args map[string]any) (json.RawMessage, error) {
		cap = &capturedAnalyze{repo: repo, tool: tool, args: args}
		return json.RawMessage(`{"status":"ok"}`), nil
	}
	
	// Run command
	rootCmd.SetArgs(append([]string{"analyze"}, argv...))
	err := rootCmd.Execute()
	return cap, buf, err
}
```

**What to Mock:**
- External service calls (HTTP, database, filesystem)
- Global state that's difficult to reset (CLI flags, package-level vars)
- Clock/time for time-dependent logic (use `time.Now` injection when possible)

**What NOT to Mock:**
- Core business logic (test the real implementation)
- Standard library functions (filesystem I/O, etc.)
- Internal helper functions (test at a higher level)

## Test Data Utilities

**Location:**
- `testpattern.go` — framework utilities for test construction
- `test_edges.go` — graph-specific test helpers
- `test_runner.go` — test discovery and execution utilities

**Example from `indexer/test_edges.go`:**
```go
func markTestSymbolsAndEmitEdges(g graph.Store) (markedTests int, edgesEmitted int) {
	// ... implementation ...
}
```

**Test Fixtures:**
- Most tests construct data in-place (minimal setup)
- Temporary directories created via `t.TempDir()` for filesystem tests
- Example:
  ```go
  root := resolvePath(t, t.TempDir())
  configPath := filepath.Join(root, ".gortex.yaml")
  require.NoError(t, os.WriteFile(configPath, []byte(""), 0644))
  ```

## Coverage

**Requirements:** 
- None explicitly enforced per package (linter doesn't check coverage)
- Focus is on meaningful tests rather than line coverage

**View Coverage:**
```bash
go test -cover ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Benchmark Tests

**Structure:**
```go
func BenchmarkIndex_Self(b *testing.B) {
	for b.Loop() {
		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		idx := New(g, reg, config.IndexConfig{}, zap.NewNop())
		_, err := idx.Index("../..")
		if err != nil {
			b.Fatal(err)
		}
	}
}
```

**Pattern:**
- Use `b.Loop()` to iterate the benchmark iterations
- Setup before the loop, measured code inside
- Use `b.Fatal()` on errors (stops the benchmark)

**Where Benchmarks Live:**
- `internal/indexer/bench_test.go` — full indexing performance
- `internal/indexer/bench_lowmem_test.go` — memory-constrained scenarios
- `internal/resolver/resolver_incremental_bench_test.go` — incremental resolution
- `cmd/gortex/bench_test.go` — CLI and command performance

**Running:**
```bash
go test -bench=. -benchmem -count=1 -benchtime=1s ./internal/parser/languages/
```

## Common Test Types

**Unit Tests:**
- Single function or method in isolation
- Fast (< 1ms typical)
- Example: `TestLooksLikeGlob()` tests one small utility

**Integration Tests:**
- Multiple components working together
- May use temporary directories, temp databases
- Example: `TestAnalyze_KindArgAndUniversalFlags()` tests the full command→daemon flow

**Snapshot Tests:**
- Compare against a golden file or hard-coded string
- Example: `TestWireContractFingerprint()` compares struct fingerprints
- Used for schema stability and compatibility checks

## Async Testing

**Pattern:**
- Go's `testing` package doesn't have built-in async helpers
- Goroutines tested via channels and `select` with timeout
- Example (hypothetical):
  ```go
  done := make(chan error, 1)
  go func() {
      // async operation
      done <- someOp()
  }()
  
  select {
  case err := <-done:
      require.NoError(t, err)
  case <-time.After(100 * time.Millisecond):
      t.Fatal("operation timed out")
  }
  ```

## Error Testing

**Pattern:**
```go
func TestNormalizePattern_OutsideRepoRejected(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	outside := resolvePath(t, t.TempDir())
	t.Chdir(outside)
	_, err := normalizePattern(outside, root)
	require.Error(t, err)  // Assert error is non-nil
}

func TestAnalyze_BogusKindClientSideError(t *testing.T) {
	_, _, err := runAnalyzeCmd(t, "--kind", "bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown analyze kind")
	require.Contains(t, err.Error(), "hotspots", "list valid kinds")
}
```

**Guidelines:**
- Test both success and failure paths
- Assert on error message content for clarity
- Use `require.Error()` to fail if error is nil (when expecting error)

## Test Comments

**Test Documentation:**
- Start with a sentence: `// TestX asserts Y` or `// TestX validates Z`
- Include preconditions, expected behavior, and any non-obvious setup
- Example from `wire_contract_test.go`:
  ```go
  // TestWireContractFingerprint is the schema-stability guard for the
  // daemon's snapshot wire format. It fingerprints the exported fields of
  // every struct that gets gob-encoded into the snapshot and compares the
  // hash against a checked-in golden value...
  ```

## Test Discipline

**What Breaks Tests:**
- Changing exported function signatures (breaking API)
- Changing type definitions (struct fields, constants)
- Changes to error messages (if tests check them)
- Global state mutations (use `t.Cleanup()`)

**Safe to Change Without Breaking Tests:**
- Internal implementation (helper functions, logic rewrites)
- Unexported type fields
- Function ordering within a package

---

*Testing analysis: 2026-07-24*
