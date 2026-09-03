package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubProbe replaces grepProbe for the duration of the test and records
// each call. Restored automatically via t.Cleanup.
func stubProbe(t *testing.T, hits []grepSymbolHit, err error) *probeRecorder {
	t.Helper()
	rec := &probeRecorder{hits: hits, err: err}
	prev := grepProbe
	grepProbe = rec.probe
	t.Cleanup(func() { grepProbe = prev })
	return rec
}

type probeRecorder struct {
	hits  []grepSymbolHit
	err   error
	calls []string
	// scopes records the location each probe was issued from, so a test can
	// assert the daemon is told which working copy is asking.
	scopes []string
}

func (r *probeRecorder) probe(pattern, scope string, _ time.Duration) ([]grepSymbolHit, error) {
	r.calls = append(r.calls, pattern)
	r.scopes = append(r.scopes, scope)
	return r.hits, r.err
}

// redirectTelemetry points the JSONL writer at a scratch file for this test.
func redirectTelemetry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-decisions.jsonl")
	t.Setenv("GORTEX_HOOK_LOG", path)
	return path
}

// readDecisions returns the decoded JSONL telemetry records from path.
func readDecisions(t *testing.T, path string) []hookDecision {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read telemetry: %v", err)
	}
	var out []hookDecision
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec hookDecision
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal telemetry line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestEnrichGrepProbesFromTheCallersLocation pins that the search's own
// working directory rides on the probe. Without it the daemon answers a
// worktree's grep from the family primary's corpus and the deny cites another
// working copy's code.
func TestEnrichGrepProbesFromTheCallersLocation(t *testing.T) {
	redirectTelemetry(t)
	rec := stubProbe(t, nil, nil)

	enrichGrep(map[string]any{"pattern": "handleFoo"}, 0, "/wt/project")

	if len(rec.scopes) != 1 || rec.scopes[0] != "/wt/project" {
		t.Fatalf("probe scopes = %v, want the caller's working directory once", rec.scopes)
	}
}

func TestEnrichGrep_SymbolHit_Denies(t *testing.T) {
	logPath := redirectTelemetry(t)
	hits := []grepSymbolHit{
		{Name: "handleFoo", Kind: "function", FilePath: "internal/a.go", Line: 42},
		{Name: "handleFoo", Kind: "method", FilePath: "internal/b.go", Line: 128},
	}
	stubProbe(t, hits, nil)

	result := enrichGrep(map[string]any{"pattern": "handleFoo"}, 0)
	if !result.deny {
		t.Fatalf("expected deny on symbol hit, got context=%q", result.context)
	}
	if !strings.Contains(result.reason, "handleFoo") {
		t.Error("deny reason should mention the pattern")
	}
	if !strings.Contains(result.reason, "internal/a.go:42") {
		t.Error("deny reason should list top hit with file:line")
	}
	if !strings.Contains(result.reason, "quote the pattern") && !strings.Contains(result.reason, "regex metachar") {
		t.Error("deny reason should include the bypass hint")
	}

	recs := readDecisions(t, logPath)
	if len(recs) != 1 || recs[0].Decision != DecisionProbedHit {
		t.Fatalf("expected one probed_hit record, got %+v", recs)
	}
	if recs[0].Hits != 2 {
		t.Errorf("expected hits=2 in telemetry, got %d", recs[0].Hits)
	}
}

func TestEnrichGrep_SymbolMiss_FallsThrough(t *testing.T) {
	logPath := redirectTelemetry(t)
	stubProbe(t, nil, nil)

	result := enrichGrep(map[string]any{"pattern": "nonexistentSymbol"}, 0)
	if result.deny {
		t.Fatal("miss should not deny")
	}
	if !strings.Contains(result.context, `search(operation:"symbols"`) {
		t.Error("miss should return soft guidance")
	}
	recs := readDecisions(t, logPath)
	if len(recs) != 1 || recs[0].Decision != DecisionProbedMiss {
		t.Fatalf("expected probed_miss, got %+v", recs)
	}
}

func TestEnrichGrep_NonSymbolPattern_Skipped(t *testing.T) {
	logPath := redirectTelemetry(t)
	rec := stubProbe(t, nil, nil)

	result := enrichGrep(map[string]any{"pattern": "hand.*"}, 0)
	if result.deny {
		t.Fatal("non-symbol pattern should not deny")
	}
	if result.context == "" {
		t.Error("non-symbol pattern >2 chars should still get soft guidance")
	}
	if len(rec.calls) != 0 {
		t.Errorf("non-symbol pattern should not call probe, got %v", rec.calls)
	}
	recs := readDecisions(t, logPath)
	if len(recs) != 1 || recs[0].Decision != DecisionSkippedNonSymbol {
		t.Fatalf("expected skipped_non_symbol, got %+v", recs)
	}
}

func TestEnrichGrep_ProbeTimeout_FallsThrough(t *testing.T) {
	logPath := redirectTelemetry(t)
	stubProbe(t, nil, errProbeTimeout)

	result := enrichGrep(map[string]any{"pattern": "handleFoo"}, 0)
	if result.deny {
		t.Fatal("timeout should not deny")
	}
	recs := readDecisions(t, logPath)
	if len(recs) != 1 || recs[0].Decision != DecisionTimedOut {
		t.Fatalf("expected timed_out, got %+v", recs)
	}
}

func TestEnrichGrep_DaemonUnreachable_NoTelemetry(t *testing.T) {
	logPath := redirectTelemetry(t)
	stubProbe(t, nil, errDaemonUnreachable)

	result := enrichGrep(map[string]any{"pattern": "handleFoo"}, 0)
	if result.deny {
		t.Fatal("daemon unreachable should not deny")
	}
	if !strings.Contains(result.context, `search(operation:"symbols"`) {
		t.Error("daemon unreachable should still return soft guidance")
	}
	if recs := readDecisions(t, logPath); len(recs) != 0 {
		t.Errorf("daemon-unreachable should not emit telemetry, got %+v", recs)
	}
}

func TestEnrichGrep_ShortPattern_NoTelemetry(t *testing.T) {
	logPath := redirectTelemetry(t)
	rec := stubProbe(t, nil, nil)

	result := enrichGrep(map[string]any{"pattern": "ab"}, 0)
	if result.context != "" || result.deny {
		t.Errorf("short pattern should be silent, got context=%q deny=%v", result.context, result.deny)
	}
	if len(rec.calls) != 0 {
		t.Errorf("short pattern should not call probe, got %v", rec.calls)
	}
	if recs := readDecisions(t, logPath); len(recs) != 0 {
		t.Errorf("short pattern should not emit telemetry, got %+v", recs)
	}
}
