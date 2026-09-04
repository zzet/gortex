package hooks

import (
	"strings"
	"testing"
	"time"
)

// stubFileIndexScopeBy stubs the file-verdict seam with a function of the
// probed path. The timeout the production seam carries is dropped: no stub
// waits on anything.
func stubFileIndexScopeBy(t *testing.T, fn func(cwd, filePath string) fileIndexStatus) {
	t.Helper()
	old := fileIndexScopeFn
	fileIndexScopeFn = func(cwd, filePath string, _ time.Duration) fileIndexStatus {
		return fn(cwd, filePath)
	}
	t.Cleanup(func() { fileIndexScopeFn = old })
}

// stubFileIndexScope stubs the file-verdict seam with a full status, for tests
// that must exercise the excluded / probe-unavailable branches the boolean
// stubIndexedFile helper cannot express.
func stubFileIndexScope(t *testing.T, st fileIndexStatus) {
	t.Helper()
	stubFileIndexScopeBy(t, func(string, string) fileIndexStatus { return st })
}

// indexedStatus is the answered verdict for a TRACKED path holding symbols, or
// not holding them yet at zero. Tracked is not optional: an answered verdict
// that tracks nothing is the one state the read doors go silent for, so a stub
// omitting it silences every advisory it means to assert.
func indexedStatus(symbols int) fileIndexStatus {
	return fileIndexStatus{Indexed: symbols > 0, Count: symbols, Tracked: true, ProbeOK: true}
}

func stubIndexedFile(t *testing.T, indexed bool, symbols int) {
	t.Helper()
	st := indexedStatus(symbols)
	st.Indexed = indexed
	stubFileIndexScope(t, st)
}

func stubScopeTracked(t *testing.T, hasSource, probeOK bool) {
	t.Helper()
	old := scopeTrackedFn
	scopeTrackedFn = func(string, string) (bool, bool) { return hasSource, probeOK }
	t.Cleanup(func() { scopeTrackedFn = old })
}

// stubTrackedScope is the pre-existing single-bool seam. It conflated "the
// daemon proved this scope holds no indexed source" with "the daemon could not
// be asked", and both produced the Unproven posture — so `false` maps to the
// unprovable half here, preserving every caller's behaviour. Tests that mean
// the proven-empty half call stubScopeTracked directly.
func stubTrackedScope(t *testing.T, tracked bool) {
	t.Helper()
	stubScopeTracked(t, tracked, tracked)
}

func TestEnrichReadBlocksIndexedRangedRead(t *testing.T) {
	stubIndexedFile(t, true, 7)

	result := enrichRead(map[string]any{
		"file_path": "internal/hooks/pretooluse.go",
		"offset":    float64(10),
		"limit":     float64(20),
	}, "/repo")

	if !result.deny {
		t.Fatalf("indexed ranged Read was not denied: %#v", result)
	}
	if !strings.Contains(result.reason, "7 symbols indexed") {
		t.Fatalf("deny reason does not retain indexed-file evidence: %q", result.reason)
	}
}

func TestEnrichReadUnindexedRangedReadFallsBackSoftly(t *testing.T) {
	stubIndexedFile(t, false, 0)

	result := enrichRead(map[string]any{
		"file_path": "internal/hooks/pretooluse.go",
		"offset":    float64(10),
		"limit":     float64(20),
	}, "/repo")

	if result.deny || result.context == "" {
		t.Fatalf("unindexed ranged Read should remain soft: %#v", result)
	}
}

func TestEnrichGrepDeniesAnyPatternInTrackedScope(t *testing.T) {
	stubTrackedScope(t, true)

	for _, pattern := range []string{"e.x|ex", "ca740d9"} {
		t.Run(pattern, func(t *testing.T) {
			result := enrichGrep(map[string]any{"pattern": pattern}, 0, "/repo")
			if !result.deny {
				t.Fatalf("tracked Grep %q was not denied: %#v", pattern, result)
			}
			if !strings.Contains(result.reason, "search(operation:\"text\"") {
				t.Fatalf("tracked Grep lacks indexed-search redirect: %q", result.reason)
			}
		})
	}
}

func TestEnrichGrepUntrackedRegexFallsBackSoftly(t *testing.T) {
	stubTrackedScope(t, false)
	// The probe must never reach a live daemon — a real index with hits
	// for a segment of the pattern would flip this soft path to a deny.
	stubProbe(t, nil, nil)
	redirectTelemetry(t)

	result := enrichGrep(map[string]any{"pattern": "e.x|ex"}, 0, "/untracked")
	if result.deny || result.context == "" {
		t.Fatalf("untracked regex Grep should remain soft: %#v", result)
	}
}

func TestEnrichGlobDeniesSourcePatternInTrackedScope(t *testing.T) {
	stubTrackedScope(t, true)

	result := enrichGlob(map[string]any{
		"pattern": "**/handler*.go",
		"path":    "internal",
	}, "/repo")
	if !result.deny {
		t.Fatalf("tracked source Glob was not denied: %#v", result)
	}
	if !strings.Contains(result.reason, "search(operation:\"files\"") {
		t.Fatalf("tracked Glob lacks indexed-file redirect: %q", result.reason)
	}
}

func TestEnrichGlobUntrackedSourcePatternFallsBackSoftly(t *testing.T) {
	stubTrackedScope(t, false)

	result := enrichGlob(map[string]any{
		"pattern": "**/handler*.go",
		"path":    "internal",
	}, "/untracked")
	if result.deny || result.context == "" {
		t.Fatalf("untracked source Glob should remain soft: %#v", result)
	}
}

func TestScopeTrackedViaDaemonUnavailable(t *testing.T) {
	old := daemonReachableFn
	daemonReachableFn = func() bool { return false }
	t.Cleanup(func() { daemonReachableFn = old })

	hasSource, probeOK := scopeTrackedViaDaemon("/repo", "internal")
	if hasSource {
		t.Fatal("unreachable daemon must not prove a tracked scope")
	}
	// And it must not prove an EMPTY one either: an unreachable daemon is no
	// evidence the scope holds no source, so the caller keeps probing rather
	// than going silent.
	if probeOK {
		t.Fatal("unreachable daemon must report the scope as unprovable, not proven-empty")
	}
}

func TestEnrichGrepExplicitNonSourceFileStaysSilent(t *testing.T) {
	stubTrackedScope(t, true)
	// A daemon that answers with hits is the case that used to hard-deny a
	// grep of a README because the pattern existed somewhere in the graph.
	stubProbe(t, []grepSymbolHit{{Name: "TODO", FilePath: "pkg/a.go", Line: 3}}, nil)

	result := enrichGrep(map[string]any{
		"pattern": "TODO",
		"path":    "/repo/README.md",
	}, 0, "/repo")
	if result.deny || result.context != "" {
		t.Fatalf("explicit non-source Grep must not fire at all: %#v", result)
	}
}

func TestEnrichGrepIndexedSourceFileDenies(t *testing.T) {
	stubIndexedFile(t, true, 4)
	stubTrackedScope(t, false)

	result := enrichGrep(map[string]any{
		"pattern": "e.x|ex",
		"path":    "pkg/matcher.go",
	}, 0, "/repo")
	if !result.deny {
		t.Fatalf("indexed source-file Grep was not denied: %#v", result)
	}
}

func TestEnrichGlobUntrackedDaemonUpGreedyPatternStaysSoft(t *testing.T) {
	stubTrackedScope(t, false)
	withDaemonReachable(t, true)

	result := enrichGlob(map[string]any{"pattern": "**/*.go"}, "/untracked")
	if result.deny || result.context == "" {
		t.Fatalf("daemon reachability alone must not deny an untracked Glob: %#v", result)
	}
}

func TestTrackedSearchDenyStillSoftensInEnrichMode(t *testing.T) {
	stubTrackedScope(t, true)
	raw := enrichGrep(map[string]any{"pattern": "ca740d9"}, 0, "/repo")
	result := applyMode(HookInput{ToolName: "Grep"}, false, ModeEnrich, raw)
	if result.deny || result.context == "" {
		t.Fatalf("ModeEnrich posture should soften the new tracked-scope deny: %#v", result)
	}
}
