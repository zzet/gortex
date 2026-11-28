package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// setupUntestedFixture builds an indexed repo with one covered symbol, one
// uncovered symbol, and a test file so reachableFromTests has something to
// walk. Isolated from setupTestServer because the base fixture has no test
// files — every symbol would be trivially uncovered there.
func setupUntestedFixture(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}

	write("lib.go", `package lib

// Covered is called by TestCovered — test-reachable.
func Covered() int { return 42 }

// Uncovered has no test reaching it; should be flagged.
func Uncovered() int { return 7 }

// FanInTarget is called by Covered and Uncovered; useful for fan_in assertions.
func FanInTarget() int { return 1 }
`)
	write("lib_helpers.go", `package lib

func caller1() int { return FanInTarget() }
func caller2() int { return FanInTarget() + Covered() }
`)
	write("lib_test.go", `package lib

import "testing"

func TestCovered(t *testing.T) {
	if Covered() != 42 {
		t.Fatal("bad")
	}
}
`)

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

type untestedEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	FanIn    int    `json:"fan_in"`
}

type untestedResp struct {
	Untested        []untestedEntry `json:"untested"`
	TotalCandidates int             `json:"total_candidates"`
	TotalUncovered  int             `json:"total_uncovered"`
	CoverageRatio   float64         `json:"coverage_ratio"`
	Truncated       bool            `json:"truncated"`
}

func decodeUntested(t *testing.T, result *mcplib.CallToolResult) untestedResp {
	t.Helper()
	var resp untestedResp
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

func TestGetUntestedSymbols_FlagsUncoveredSymbols(t *testing.T) {
	srv := setupUntestedFixture(t)

	result := callTool(t, srv, "get_untested_symbols", nil)
	require.False(t, result.IsError)
	resp := decodeUntested(t, result)

	// Uncovered must appear; Covered must NOT (TestCovered reaches it).
	names := make(map[string]bool)
	for _, e := range resp.Untested {
		names[e.Name] = true
	}
	assert.True(t, names["Uncovered"], "Uncovered function must be flagged; got: %+v", resp.Untested)
	assert.False(t, names["Covered"], "Covered function must NOT be flagged; got: %+v", resp.Untested)
}

func TestGetUntestedSymbols_ExcludesTestCodeItself(t *testing.T) {
	srv := setupUntestedFixture(t)

	result := callTool(t, srv, "get_untested_symbols", nil)
	resp := decodeUntested(t, result)

	// TestCovered is defined in a *_test.go file — it's test code, not a
	// candidate. It must never appear in the uncovered list.
	for _, e := range resp.Untested {
		assert.NotEqual(t, "TestCovered", e.Name,
			"test functions must be excluded from candidates")
		assert.NotContains(t, e.FilePath, "_test.go",
			"entries from test files leaked through: %+v", e)
	}
}

func TestGetUntestedSymbols_RankedByFanIn(t *testing.T) {
	srv := setupUntestedFixture(t)

	result := callTool(t, srv, "get_untested_symbols", nil)
	resp := decodeUntested(t, result)

	// Ranking contract: fan_in strictly non-increasing.
	for i := 0; i+1 < len(resp.Untested); i++ {
		assert.GreaterOrEqual(t,
			resp.Untested[i].FanIn,
			resp.Untested[i+1].FanIn,
			"entries must be sorted by fan_in descending")
	}
}

func TestGetUntestedSymbols_MinFanInFilter(t *testing.T) {
	srv := setupUntestedFixture(t)

	result := callTool(t, srv, "get_untested_symbols", map[string]any{
		"min_fan_in": 1,
	})
	resp := decodeUntested(t, result)

	for _, e := range resp.Untested {
		assert.GreaterOrEqual(t, e.FanIn, 1,
			"min_fan_in=1 must exclude symbols with fan_in=0")
	}
}

func TestGetUntestedSymbols_CoverageRatioInRange(t *testing.T) {
	srv := setupUntestedFixture(t)

	result := callTool(t, srv, "get_untested_symbols", nil)
	resp := decodeUntested(t, result)

	assert.GreaterOrEqual(t, resp.CoverageRatio, 0.0)
	assert.LessOrEqual(t, resp.CoverageRatio, 1.0)
	assert.Greater(t, resp.TotalCandidates, 0)
}

func TestGetUntestedSymbols_FilePrefix(t *testing.T) {
	srv := setupUntestedFixture(t)

	// Prefix that matches nothing should yield zero candidates and zero
	// uncovered — still a valid well-shaped response.
	result := callTool(t, srv, "get_untested_symbols", map[string]any{
		"file_prefix": "nonexistent/",
	})
	resp := decodeUntested(t, result)
	assert.Equal(t, 0, resp.TotalCandidates)
	assert.Equal(t, 0, resp.TotalUncovered)
	assert.Empty(t, resp.Untested)
}

// Test code is classified the way the indexer classifies it (graph.NodeIsTest:
// the is_test stamp, then testpath.IsTestFile), not by a substring of the path.
// So a pytest helper under tests/ is test code whose calls prove coverage, a
// production package whose name merely contains "test_" (a pytest plugin) is
// still a candidate, and a Rust #[test] function in a production file is a test
// rather than an untested candidate.
func TestGetUntestedSymbols_ClassifiesTestCodeLikeTheIndexer(t *testing.T) {
	cases := []struct {
		name           string
		files          map[string]string
		wantCandidates int
		wantFlagged    []string
	}{
		{
			name: "python tests/ helper and pytest_ package",
			files: map[string]string{
				"pytest_demo/__init__.py": "",
				"pytest_demo/plugin.py":   "def make_default():\n    return {}\n\n\ndef unused():\n    return 1\n",
				"tests/conftest.py":       "from pytest_demo.plugin import make_default\n\n\ndef default_model():\n    return make_default()\n",
				"tests/test_plugin.py":    "def test_model(default_model):\n    assert default_model == {}\n",
			},
			wantCandidates: 2,
			wantFlagged:    []string{"pytest_demo/plugin.py::unused"},
		},
		{
			name: "rust #[test] in a production file",
			files: map[string]string{
				"src/lib.rs": "pub fn parse() -> i32 {\n    1\n}\n\npub fn unused() -> i32 {\n    2\n}\n\n" +
					"#[cfg(test)]\nmod tests {\n    use super::*;\n\n    #[test]\n    fn parses_one() {\n        assert_eq!(parse(), 1);\n    }\n}\n",
			},
			wantCandidates: 2,
			wantFlagged:    []string{"src/lib.rs::unused"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				path := filepath.Join(dir, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			}
			g := graph.New()
			idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
			_, err := idx.Index(dir)
			require.NoError(t, err)
			srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

			resp := decodeUntested(t, callTool(t, srv, "get_untested_symbols", nil))
			var flagged []string
			for _, e := range resp.Untested {
				flagged = append(flagged, e.FilePath+"::"+e.Name)
			}
			assert.Equal(t, tc.wantCandidates, resp.TotalCandidates, "flagged: %v", flagged)
			assert.Equal(t, tc.wantFlagged, flagged)
		})
	}
}
