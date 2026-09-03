package hooks

import (
	"strings"
	"testing"
)

func stubIndexedFile(t *testing.T, indexed bool, symbols int) {
	t.Helper()
	old := fileIndexedFn
	fileIndexedFn = func(string, string) (bool, int) { return indexed, symbols }
	t.Cleanup(func() { fileIndexedFn = old })
}

func stubTrackedScope(t *testing.T, tracked bool) {
	t.Helper()
	old := scopeTrackedFn
	scopeTrackedFn = func(string, string) bool { return tracked }
	t.Cleanup(func() { scopeTrackedFn = old })
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

	if scopeTrackedViaDaemon("/repo", "internal") {
		t.Fatal("unreachable daemon must not prove a tracked scope")
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

func TestParseFindFilesHasSourceRequiresNonEmptyIndexedResult(t *testing.T) {
	withSource := []byte(`{"result":{"content":[{"text":"{\"count\":1,\"files\":[{\"path\":\"pkg/a.go\"}]}"}]}}`)
	if !parseFindFilesHasSource(withSource) {
		t.Fatal("non-empty find_files result should prove indexed source")
	}
	withoutSource := []byte(`{"result":{"content":[{"text":"{\"count\":0,\"files\":[]}"}]}}`)
	if parseFindFilesHasSource(withoutSource) {
		t.Fatal("empty find_files result must not prove indexed source")
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
