package hooks

import (
	"strings"
	"testing"
)

// newIndexedBridge stubs the daemon file-indexed probe so every queried
// file looks indexed with the given symbol count, for the duration of the
// test. Used to exercise enrichBash's ReadSource path without a real daemon.
// Returns a dummy port (0) so the legacy `port := newIndexedBridge(...)`
// call sites still compile; the value is unused now that the indexed check
// routes through the stubbed fileIndexScopeFn seam rather than an HTTP port.
func newIndexedBridge(t *testing.T, symbols int) int {
	t.Helper()
	prev := fileIndexScopeFn
	t.Cleanup(func() { fileIndexScopeFn = prev })
	fileIndexScopeFn = func(_, _ string) fileIndexStatus {
		return fileIndexStatus{Indexed: symbols > 0, Count: symbols, ProbeOK: true}
	}
	return 0
}

func TestEnrichBash_GrepHit_Denies(t *testing.T) {
	redirectTelemetry(t)
	stubProbe(t, []grepSymbolHit{
		{Name: "handleFoo", Kind: "function", FilePath: "internal/a.go", Line: 42},
	}, nil)

	r := enrichBash(map[string]any{"command": `grep -rn "handleFoo" .`}, "")
	if !r.deny {
		t.Fatalf("expected deny on grep hit, got %+v", r)
	}
	if !strings.Contains(r.reason, "handleFoo") {
		t.Error("deny reason should mention the pattern")
	}
	if !strings.Contains(r.reason, "internal/a.go:42") {
		t.Error("deny reason should list the hit")
	}
}

func TestEnrichBash_GrepMiss_SoftGuidance(t *testing.T) {
	redirectTelemetry(t)
	stubProbe(t, nil, nil) // daemon reachable, no hits

	r := enrichBash(map[string]any{"command": `grep -rn "handleFoo" .`}, "")
	if r.deny {
		t.Fatal("miss should not deny")
	}
	if !strings.Contains(r.context, `search(operation:"symbols"`) || !strings.Contains(r.context, "operation `text`") {
		t.Error("miss should return soft guidance mentioning public search operations")
	}
}

func TestEnrichBash_GrepPiped_PassesThrough(t *testing.T) {
	// grep after | is a filter on upstream output — not a codebase search.
	rec := stubProbe(t, nil, nil)
	r := enrichBash(map[string]any{"command": `go test ./... | grep FAIL`}, "")
	if r.deny || r.context != "" {
		t.Errorf("piped grep should pass through, got %+v", r)
	}
	if len(rec.calls) != 0 {
		t.Errorf("piped grep should not probe daemon, got calls %v", rec.calls)
	}
}

func TestEnrichBash_RgBare_Denies(t *testing.T) {
	redirectTelemetry(t)
	stubProbe(t, []grepSymbolHit{
		{Name: "MyType", Kind: "type", FilePath: "a.go", Line: 5},
	}, nil)

	r := enrichBash(map[string]any{"command": `rg MyType`}, "")
	if !r.deny {
		t.Fatalf("expected deny, got %+v", r)
	}
}

func TestEnrichBash_FindName_Denies(t *testing.T) {
	redirectTelemetry(t)
	stubProbe(t, []grepSymbolHit{
		{Name: "Handler", Kind: "type", FilePath: "x.go", Line: 10},
	}, nil)

	r := enrichBash(map[string]any{"command": `find . -name "Handler*"`}, "")
	if !r.deny {
		t.Fatalf("expected deny for find -name with symbol-shaped root, got %+v", r)
	}
}

func TestEnrichBash_FindNameGoFiles_NoProbe(t *testing.T) {
	// `-name "*.go"` reduces to ".go" which is not symbol-shaped — no probe,
	// no deny. Returns soft guidance because the pattern is >2 chars.
	rec := stubProbe(t, nil, nil)
	r := enrichBash(map[string]any{"command": `find . -name "*.go"`}, "")
	if r.deny {
		t.Fatal("find -name *.go should not deny")
	}
	if len(rec.calls) != 0 {
		t.Errorf("non-symbol-shaped name should not probe, got %v", rec.calls)
	}
}

func TestEnrichBash_FindTypeD_Passthrough(t *testing.T) {
	rec := stubProbe(t, nil, nil)
	r := enrichBash(map[string]any{"command": `find . -maxdepth 3 -type d`}, "")
	if r.deny || r.context != "" {
		t.Errorf("find -type d should pass through, got %+v", r)
	}
	if len(rec.calls) != 0 {
		t.Error("find without -name should not probe")
	}
}

func TestEnrichBash_CatIndexedSource_Denies(t *testing.T) {
	newIndexedBridge(t, 17)
	r := enrichBash(map[string]any{"command": `cat /repo/handler.go`}, "")
	if !r.deny {
		t.Fatalf("expected deny for cat of indexed source, got %+v", r)
	}
	if !strings.Contains(r.reason, "/repo/handler.go") {
		t.Error("deny reason should mention the file path")
	}
	if !strings.Contains(r.reason, "17 symbols") {
		t.Error("deny reason should include the symbol count")
	}
	if !strings.Contains(r.reason, `read(operation:"summary"`) {
		t.Error("deny reason should point to read(operation=summary)")
	}
}

func TestEnrichBash_CatUnindexedSource_SoftGuidance(t *testing.T) {
	// probe not stubbed → file treated as not indexed.
	r := enrichBash(map[string]any{"command": `head -20 /tmp/foo.go`}, "")
	if r.deny {
		t.Fatal("unindexed source should not deny")
	}
	if !strings.Contains(r.context, "Use `read` instead") || !strings.Contains(r.context, `read(target:{symbol:`) {
		t.Error("soft guidance should show the selector-driven public symbol read")
	}
}

func TestEnrichBash_CatLogfile_Passthrough(t *testing.T) {
	r := enrichBash(map[string]any{"command": `cat /tmp/app.log`}, "")
	if r.deny || r.context != "" {
		t.Errorf("cat of non-source file should pass through, got %+v", r)
	}
}

func TestEnrichBash_EmptyCommand(t *testing.T) {
	r := enrichBash(map[string]any{"command": ""}, "")
	if r.deny || r.context != "" {
		t.Errorf("empty command should pass through, got %+v", r)
	}
}

func TestEnrichBash_UnrelatedCommand(t *testing.T) {
	rec := stubProbe(t, nil, nil)
	for _, cmd := range []string{
		`ls /repo`,
		`go build ./...`,
		`git status`,
		`echo hello`,
	} {
		r := enrichBash(map[string]any{"command": cmd}, "")
		if r.deny || r.context != "" {
			t.Errorf("%q should pass through, got %+v", cmd, r)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("unrelated commands should not probe, got %v", rec.calls)
	}
}

// probedIndexedBridge stubs the daemon file-indexed probe and records every
// path it was asked about, so a test can assert the write path stays within
// its probe budget.
func probedIndexedBridge(t *testing.T, indexed map[string]bool) *[]string {
	t.Helper()
	probes := &[]string{}
	prev := fileIndexScopeFn
	t.Cleanup(func() { fileIndexScopeFn = prev })
	fileIndexScopeFn = func(_, filePath string) fileIndexStatus {
		*probes = append(*probes, filePath)
		if indexed[filePath] {
			return fileIndexStatus{Indexed: true, Count: 4, ProbeOK: true}
		}
		return fileIndexStatus{ProbeOK: true}
	}
	return probes
}

func TestEnrichBash_WriteIndexedSource_AdvisesWhenBlockingOff(t *testing.T) {
	redirectTelemetry(t)
	withEditBlocking(t, false)
	newIndexedBridge(t, 4)

	r := enrichBash(map[string]any{"command": `sed -i 's/a/b/' internal/x.go`}, "")
	if r.deny {
		t.Fatalf("shell write must not hard-block without the env gate, got %+v", r)
	}
	for _, want := range []string{"internal/x.go", "parse gate", `edit(operation:"file"`} {
		if !strings.Contains(r.context, want) {
			t.Errorf("context missing %q:\n%s", want, r.context)
		}
	}
	if strings.Contains(r.context, "BLOCKED") {
		t.Error("advisory context must not claim the command was blocked")
	}
}

func TestEnrichBash_WriteIndexedSource_DeniesUnderEnvGate(t *testing.T) {
	redirectTelemetry(t)
	withEditBlocking(t, true)
	newIndexedBridge(t, 4)

	r := enrichBash(map[string]any{"command": "cat > internal/x.go <<'EOF'\npackage x\nEOF"}, "")
	if !r.deny {
		t.Fatalf("expected deny for a shell write to indexed source, got %+v", r)
	}
	for _, want := range []string{"BLOCKED", "internal/x.go", `edit(operation:"write"`, "GORTEX_HOOK_BLOCK_EDIT"} {
		if !strings.Contains(r.reason, want) {
			t.Errorf("reason missing %q:\n%s", want, r.reason)
		}
	}
}

func TestEnrichBash_WriteRecordsRedirectDecision(t *testing.T) {
	logPath := redirectTelemetry(t)
	withEditBlocking(t, true)
	newIndexedBridge(t, 4)

	_ = enrichBash(map[string]any{"command": `sed -i 's/a/b/' internal/x.go`}, "")

	recs := readDecisions(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("expected 1 telemetry record, got %d", len(recs))
	}
	if recs[0].Tool != "Bash" || recs[0].Decision != DecisionRedirectedWrite {
		t.Errorf("record = %+v, want Bash/redirected_write", recs[0])
	}
}

func TestEnrichBash_WriteNewFile_PassesThrough(t *testing.T) {
	// A file the graph does not know is a new file; enrichWrite lets that
	// through for Write and the shell door has to agree.
	withEditBlocking(t, true)
	fakeIndexedBridge(t, map[string]bool{"internal/existing.go": true})

	r := enrichBash(map[string]any{"command": `cat > internal/new.go`}, "")
	if r.deny || r.context != "" {
		t.Errorf("write to an unindexed path should pass through, got %+v", r)
	}
}

func TestEnrichBash_WriteNonSourceTarget_NoProbe(t *testing.T) {
	withEditBlocking(t, true)
	probes := probedIndexedBridge(t, map[string]bool{})

	for _, cmd := range []string{
		`go test -race ./... > /tmp/test.log 2>&1`,
		`go build -o gortex ./cmd/gortex/`,
		`gortex guide > docs/notes.md`,
	} {
		if r := enrichBash(map[string]any{"command": cmd}, ""); r.deny || r.context != "" {
			t.Errorf("%q should pass through, got %+v", cmd, r)
		}
	}
	if len(*probes) != 0 {
		t.Errorf("non-source write targets must not probe the daemon, got %v", *probes)
	}
}

func TestEnrichBash_WriteAfterPipe_IsGuarded(t *testing.T) {
	redirectTelemetry(t)
	withEditBlocking(t, true)
	probes := probedIndexedBridge(t, map[string]bool{"internal/x.go": true})

	r := enrichBash(map[string]any{"command": `go run ./gen | tee internal/x.go`}, "")
	if !r.deny {
		t.Fatalf("a write after a pipe is still a write, got %+v", r)
	}
	if len(*probes) != 1 || (*probes)[0] != "internal/x.go" {
		t.Errorf("probes = %v, want exactly internal/x.go", *probes)
	}
}

func TestEnrichBash_WriteProbesBoundedByCandidateLimit(t *testing.T) {
	redirectTelemetry(t)
	withEditBlocking(t, true)
	probes := probedIndexedBridge(t, map[string]bool{"internal/c.go": true})

	r := enrichBash(map[string]any{
		"command": `echo a > internal/a.go; echo b > internal/b.go; echo c > internal/c.go; echo d > internal/d.go`,
	}, "")
	if !r.deny {
		t.Fatalf("expected deny once a candidate is indexed, got %+v", r)
	}
	if !strings.Contains(r.reason, "internal/c.go") {
		t.Errorf("reason should name the indexed target:\n%s", r.reason)
	}
	if len(*probes) > bashWriteProbeLimit {
		t.Errorf("probed %d paths, budget is %d: %v", len(*probes), bashWriteProbeLimit, *probes)
	}
}

func TestEnrichBash_UnindexedWriteFallsBackToReadAnswer(t *testing.T) {
	// The write pre-pass claims this command, but /tmp/scratch.go is not in
	// the graph. The indexed read in the same command must still be answered.
	redirectTelemetry(t)
	withEditBlocking(t, true)
	probedIndexedBridge(t, map[string]bool{"internal/x.go": true})

	r := enrichBash(map[string]any{"command": `sed -i 's/a/b/' /tmp/scratch.go && cat internal/x.go`}, "")
	if !r.deny {
		t.Fatalf("expected the read deny to survive an inapplicable write shape, got %+v", r)
	}
	if !strings.Contains(r.reason, "reads indexed source") {
		t.Errorf("expected the read answer, got:\n%s", r.reason)
	}
}

func TestEnrichBash_WriteDispatchedFromEnrich(t *testing.T) {
	redirectTelemetry(t)
	withEditBlocking(t, true)
	newIndexedBridge(t, 4)

	input := HookInput{ToolName: "Bash", ToolInput: map[string]any{"command": `sed -i 's/a/b/' internal/x.go`}}
	if r := enrich(input, 0); !r.deny {
		t.Errorf("dispatcher must route a Bash write through enrichBash; got %+v", r)
	}
}

func TestEnrichBash_TelemetryTaggedAsBash(t *testing.T) {
	logPath := redirectTelemetry(t)
	stubProbe(t, []grepSymbolHit{
		{Name: "handleFoo", Kind: "function", FilePath: "a.go", Line: 1},
	}, nil)

	_ = enrichBash(map[string]any{"command": `grep -rn handleFoo .`}, "")

	recs := readDecisions(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("expected 1 telemetry record, got %d", len(recs))
	}
	if recs[0].Tool != "Bash" {
		t.Errorf("tool = %q, want %q", recs[0].Tool, "Bash")
	}
	if recs[0].Decision != DecisionProbedHit {
		t.Errorf("decision = %v, want probed_hit", recs[0].Decision)
	}
}
