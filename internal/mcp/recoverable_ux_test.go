package mcp

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestRecoverableNotTrackedReturnsSuccessGuidance proves the F3 guardrail: a
// recoverable condition (repo not tracked, symbol not found, file not indexed)
// returns a NON-error result carrying machine-readable guidance — never a
// session-abandoning isError — and the guidance routes to Gortex's richer
// escape hatches plus the structured `gortex track` affordance.
func TestRecoverableNotTrackedReturnsSuccessGuidance(t *testing.T) {
	decode := func(t *testing.T, body string) RecoverableGuidance {
		t.Helper()
		var g RecoverableGuidance
		if err := json.Unmarshal([]byte(body), &g); err != nil {
			t.Fatalf("guidance body is not JSON: %v\n%s", err, body)
		}
		return g
	}

	t.Run("repo_not_tracked", func(t *testing.T) {
		res := repoNotTrackedGuidance("/work/newrepo")
		if res.IsError {
			t.Fatal("repo-not-tracked must NOT be an isError result (recoverable)")
		}
		g := decode(t, toolResultText(res))
		if !g.Recoverable || g.Condition != ErrCodeRepoNotTracked {
			t.Errorf("guidance = %+v, want recoverable repo_not_tracked", g)
		}
		if g.TrackCommand != "gortex track /work/newrepo" {
			t.Errorf("track_command = %q, want the path-specific gortex track", g.TrackCommand)
		}
		if !containsString(g.SuggestedTools, "find_files") || !containsString(g.SuggestedTools, "search_text") {
			t.Errorf("suggested_tools = %v, want the content-search escape hatches", g.SuggestedTools)
		}
	})

	t.Run("symbol_not_found", func(t *testing.T) {
		res := symbolNotFoundGuidance("pkg/foo.go::Bar")
		if res.IsError {
			t.Fatal("symbol-not-found must NOT be an isError result")
		}
		g := decode(t, toolResultText(res))
		if g.Condition != ErrCodeSymbolNotFound {
			t.Errorf("condition = %q, want symbol_not_found", g.Condition)
		}
		// Routes to Gortex's richer locators, not just three tools.
		if !containsString(g.SuggestedTools, "search_symbols") || !containsString(g.SuggestedTools, "find_usages") {
			t.Errorf("suggested_tools = %v, want search_symbols + find_usages", g.SuggestedTools)
		}
		if g.Data["id"] != "pkg/foo.go::Bar" {
			t.Errorf("data.id = %v, want the queried id", g.Data["id"])
		}
	})

	t.Run("file_not_indexed", func(t *testing.T) {
		res := fileNotIndexedGuidance("internal/new.go", fileNotIndexedState{})
		if res.IsError {
			t.Fatal("file-not-indexed must NOT be an isError result")
		}
		g := decode(t, toolResultText(res))
		if g.Condition != ErrCodeFileNotIndexed {
			t.Errorf("condition = %q, want file_not_indexed", g.Condition)
		}
		// A file that may yet be indexed keeps the full discovery triple.
		if !containsString(g.SuggestedTools, "find_files") || !containsString(g.SuggestedTools, "read_file") {
			t.Errorf("suggested_tools = %v, want the discovery tools plus read_file", g.SuggestedTools)
		}
		if g.Data["excluded"] != false || g.Data["unindexable"] != false || g.Data["indexed"] != false {
			t.Errorf("data = %v, want all scope flags false for an indexable file", g.Data)
		}
	})

	// find_files and search_text are both graph-backed. For a path the graph
	// will NEVER hold they return zero rows, so naming them costs the caller
	// two dead round-trips before it reaches the only tool that can answer.
	// An INDEXED file is the opposite case: the graph holds it, so both
	// locators have rows and only the symbol lookup comes up empty.
	for _, tc := range []struct {
		name      string
		path      string
		state     fileNotIndexedState
		want      map[string]bool
		wantTools []string
	}{{
		name:      "file_not_indexed_excluded",
		path:      "node_modules/x.js",
		state:     fileNotIndexedState{Unindexable: true, Excluded: true},
		want:      map[string]bool{"excluded": true, "unindexable": true, "indexed": false},
		wantTools: []string{"read_file"},
	}, {
		name:      "file_not_indexed_unindexable",
		path:      "db/migrations/0042.sql",
		state:     fileNotIndexedState{Unindexable: true},
		want:      map[string]bool{"excluded": false, "unindexable": true, "indexed": false},
		wantTools: []string{"read_file"},
	}, {
		name:      "file_indexed_but_symbolless",
		path:      "internal/doc.go",
		state:     fileNotIndexedState{Indexed: true},
		want:      map[string]bool{"excluded": false, "unindexable": false, "indexed": true},
		wantTools: []string{"find_files", "search_text", "read_file"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			g := decode(t, toolResultText(fileNotIndexedGuidance(tc.path, tc.state)))
			for key, want := range tc.want {
				if g.Data[key] != want {
					t.Errorf("data.%s = %v, want %v", key, g.Data[key], want)
				}
			}
			if !slices.Equal(g.SuggestedTools, tc.wantTools) {
				t.Errorf("suggested_tools = %v, want %v", g.SuggestedTools, tc.wantTools)
			}
			if tc.state.Unindexable && containsString(g.SuggestedTools, "search_text") {
				t.Error("search_text is graph-backed and cannot answer for a path the graph will never hold")
			}
		})
	}
}

func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
