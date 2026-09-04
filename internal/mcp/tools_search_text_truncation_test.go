package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// searchTextServerWith indexes one file per given relative path, each holding
// the same literal, and returns a server over the result. Every file matches,
// so the number of matches is the number of paths — which is what lets these
// tests reason about the limit exactly.
func searchTextServerWith(t *testing.T, rels ...string) *Server {
	t.Helper()
	dir := t.TempDir()
	for i, rel := range rels {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full,
			[]byte("package app\n\nfunc Target"+string(rune('A'+i))+"() {}\n"), 0o644))
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
}

func searchTextResponse(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	res := callTool(t, srv, "search_text", args)
	require.False(t, res.IsError, "%+v", res.Content)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

func TestSearchText_LimitBoundResultDisclosesTruncation(t *testing.T) {
	srv := searchTextServerWith(t, "a.go", "b.go", "c.go")

	out := searchTextResponse(t, srv, map[string]any{"query": "package app", "limit": 2})

	require.Equal(t, float64(2), out["count"])
	require.Equal(t, true, out["_truncated_by_limit"],
		"a result bound by limit is byte-indistinguishable from a complete one without this")
	require.Equal(t, float64(2), out["_limit_applied"])
	require.Equal(t, false, out["count_is_exact"],
		"count is a floor here, and saying so is the half a caller can act on")
	note, _ := out["truncation_note"].(string)
	require.Contains(t, note, "floor")
	// The recovery that looks obvious and does not work: `path` filters run
	// over what survived truncation, so narrowing cannot recover the rest.
	require.Contains(t, note, "path")
	require.Contains(t, note, "NOT recover")
}

func TestSearchText_CompleteResultCarriesNoTruncationKeys(t *testing.T) {
	srv := searchTextServerWith(t, "a.go", "b.go", "c.go")

	out := searchTextResponse(t, srv, map[string]any{"query": "package app", "limit": 100})

	require.Equal(t, float64(3), out["count"])
	for _, key := range []string{"_truncated_by_limit", "_limit_applied", "count_is_exact", "truncation_note", "_limit_requested"} {
		_, present := out[key]
		require.False(t, present, "a complete result must not carry %q", key)
	}
}

func TestSearchText_DisclosesThatTheCeilingChoseTheLimit(t *testing.T) {
	// The ceiling, not the caller, decided the effective limit. Without
	// _limit_requested a caller who asked for 50 and got 2 cannot tell that
	// from a corpus holding exactly 2 matches.
	t.Setenv("GORTEX_SEARCH_TEXT_MAX_LIMIT", "2")
	srv := searchTextServerWith(t, "a.go", "b.go", "c.go")

	out := searchTextResponse(t, srv, map[string]any{"query": "package app", "limit": 50})

	require.Equal(t, true, out["_truncated_by_limit"])
	require.Equal(t, float64(2), out["_limit_applied"], "the ceiling override was not applied")
	require.Equal(t, float64(50), out["_limit_requested"],
		"the response does not say the clamp chose the limit")
}

func TestSearchText_RequestWithinTheCeilingReportsNoRequestedLimit(t *testing.T) {
	t.Setenv("GORTEX_SEARCH_TEXT_MAX_LIMIT", "10")
	srv := searchTextServerWith(t, "a.go", "b.go", "c.go")

	out := searchTextResponse(t, srv, map[string]any{"query": "package app", "limit": 2})

	require.Equal(t, true, out["_truncated_by_limit"])
	_, present := out["_limit_requested"]
	require.False(t, present, "the caller's own limit bound this result; nothing was clamped")
}

func TestSearchText_TruncationIsMeasuredBeforeThePathFilter(t *testing.T) {
	// The ordering this whole fix turns on. The searcher stops at `limit`,
	// and the path filter then runs over what survived — so the count can sit
	// below the limit while the result is still a truncated prefix. Measuring
	// after the filter would miss exactly the case a caller cannot detect.
	//
	// Four matching files, limit 3, filtered to one directory of two: the
	// searcher is bound whichever three it returns, and at most two survive
	// the filter.
	srv := searchTextServerWith(t, "a/1.go", "a/2.go", "b/1.go", "b/2.go")

	out := searchTextResponse(t, srv, map[string]any{
		"query": "package app", "limit": 3, "path": "a",
	})

	count, _ := out["count"].(float64)
	require.Less(t, count, float64(3),
		"the fixture must leave the count below the limit, or it proves nothing")
	require.Equal(t, true, out["_truncated_by_limit"],
		"a filtered result below the limit is still a truncated prefix of the corpus")
}

func TestSearchTextBoundByLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   int
		limit int
		want  bool
	}{
		{"corpus ran out first", 5, 10, false},
		{"landed exactly on the limit", 10, 10, true},
		// The fan-out searchers hand each repo the full limit, so the raw
		// count can exceed it before the final trim.
		{"searcher returned more than the limit", 25, 10, true},
		{"no limit in force", 100, 0, false},
		{"empty result", 0, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, searchTextBoundByLimit(tc.raw, tc.limit))
		})
	}
}

func TestSearchTextMaxLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want int
	}{
		{"unset keeps the historical ceiling", "", searchTextDefaultMaxLimit},
		{"an explicit ceiling is honored", "5000", 5000},
		{"surrounding space is tolerated", "  2000  ", 2000},
		// A typo must not turn into an unbounded scan, so every unusable
		// value falls back rather than lifting the bound.
		{"garbage falls back", "unlimited", searchTextDefaultMaxLimit},
		{"zero falls back", "0", searchTextDefaultMaxLimit},
		{"negative falls back", "-1", searchTextDefaultMaxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SEARCH_TEXT_MAX_LIMIT", tc.env)
			require.Equal(t, tc.want, searchTextMaxLimit())
		})
	}
}
