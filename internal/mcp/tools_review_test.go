package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/review"
)

// siblingDiffGitRepo creates a git repo with a base commit and a HEAD commit
// that mutates three Go files in two packages, so the changeset has several
// changed files. Returns the repo root and the relative paths of the changed
// files (focus + two siblings).
func siblingDiffGitRepo(t *testing.T) (root, fileA, fileB, fileC string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	// '/'-spelled on every platform: these are repo-relative paths, which is
	// what git prints, what the graph keys file nodes under, and what the
	// review handlers compare. filepath.Join here would build a third
	// spelling that matches neither on Windows.
	fileA = "internal/alpha/a.go"
	fileB = "internal/alpha/b.go"
	fileC = "internal/beta/c.go"
	write := func(rel, src string) {
		abs := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}
	write(fileA, "package alpha\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	write(fileB, "package alpha\n\nfunc Beta() int {\n\treturn 2\n}\n")
	write(fileC, "package beta\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	run("add", ".")
	run("commit", "-m", "base")
	run("tag", "base-ref")

	// HEAD commit mutates the body of every function so all three files change.
	write(fileA, "package alpha\n\nfunc Alpha() int {\n\tx := 1\n\treturn x\n}\n")
	write(fileB, "package alpha\n\nfunc Beta() int {\n\ty := 2\n\treturn y\n}\n")
	write(fileC, "package beta\n\nfunc Gamma() int {\n\tz := 3\n\treturn z\n}\n")
	run("add", ".")
	run("commit", "-m", "change")
	return dir, fileA, fileB, fileC
}

// indexedSiblingServer indexes the repo and builds a server over it.
func indexedSiblingServer(t *testing.T, dir string) *Server {
	t.Helper()
	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

func callSiblingDiff(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "sibling_diff_context"
	req.Params.Arguments = args
	res, err := srv.handleSiblingDiffContext(t.Context(), req)
	require.NoError(t, err)
	return res
}

type siblingDiffOut struct {
	Focus     []string `json:"focus"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
	Siblings  []struct {
		File     string  `json:"file"`
		Relation string  `json:"relation"`
		Score    float64 `json:"score"`
		Diff     string  `json:"diff"`
	} `json:"siblings"`
}

func decodeSiblingDiff(t *testing.T, res *mcplib.CallToolResult) siblingDiffOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %v", res)
	var out siblingDiffOut
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

// TestSiblingDiffContext_ExcludesFocusReturnsSiblings asserts the focus file is
// excluded and the other changed files come back with their raw diffs.
func TestSiblingDiffContext_ExcludesFocusReturnsSiblings(t *testing.T) {
	dir, fileA, fileB, fileC := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeSiblingDiff(t, callSiblingDiff(t, srv, map[string]any{
		"base":        "base-ref",
		"focus_files": fileA,
	}))

	require.Equal(t, []string{fileA}, out.Focus)
	require.Equal(t, 2, out.Total, "two siblings expected (b, c)")

	got := map[string]string{}
	for _, sib := range out.Siblings {
		got[sib.File] = sib.Diff
		require.NotEqual(t, fileA, sib.File, "focus file must be excluded")
		require.NotEmpty(t, sib.Relation, "every sibling carries a relation tag")
	}
	require.Contains(t, got, fileB)
	require.Contains(t, got, fileC)

	// Each sibling carries the RAW unified diff text for that file only.
	require.Contains(t, got[fileB], "+++ b/"+filepath.ToSlash(fileB))
	require.Contains(t, got[fileB], "@@")
	require.Contains(t, got[fileB], "y := 2")
	require.NotContains(t, got[fileB], "x := 1", "sibling b's diff must not include focus a's hunks")

	require.Contains(t, got[fileC], "+++ b/"+filepath.ToSlash(fileC))
	require.Contains(t, got[fileC], "z := 3")
}

// TestSiblingDiffContext_FocusSymbolID resolves the focus file from a changed
// symbol's ID and excludes that file.
func TestSiblingDiffContext_FocusSymbolID(t *testing.T) {
	dir, fileA, fileB, fileC := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	// Alpha lives in fileA — find its node ID from the graph.
	var alphaID string
	for _, n := range srv.graph.GetFileNodes(fileA) {
		if n.Name == "Alpha" {
			alphaID = n.ID
		}
	}
	require.NotEmpty(t, alphaID, "Alpha symbol must be indexed")

	out := decodeSiblingDiff(t, callSiblingDiff(t, srv, map[string]any{
		"base":            "base-ref",
		"focus_symbol_id": alphaID,
	}))
	require.Equal(t, []string{fileA}, out.Focus)
	require.Equal(t, 2, out.Total)
	for _, sib := range out.Siblings {
		require.NotEqual(t, fileA, sib.File)
	}
	_ = fileB
	_ = fileC
}

// TestSiblingDiffContext_Relation asserts same-package siblings outrank a
// cross-package sibling (directory proximity), so the ranking is deterministic.
func TestSiblingDiffContext_Relation(t *testing.T) {
	dir, fileA, fileB, fileC := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeSiblingDiff(t, callSiblingDiff(t, srv, map[string]any{
		"base":        "base-ref",
		"focus_files": fileA,
	}))
	require.Equal(t, 2, out.Total)

	score := map[string]float64{}
	for _, sib := range out.Siblings {
		score[sib.File] = sib.Score
	}
	// b.go shares the alpha directory with the focus a.go; c.go lives in beta.
	require.Greater(t, score[fileB], score[fileC],
		"same-directory sibling must outrank the cross-directory sibling")
	// Ranking is highest-score-first.
	require.Equal(t, fileB, out.Siblings[0].File)
}

// TestSiblingDiffContext_EmptyChangeset returns total:0 with no siblings.
func TestSiblingDiffContext_EmptyChangeset(t *testing.T) {
	dir, fileA, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	// Compare HEAD against itself — no changes.
	out := decodeSiblingDiff(t, callSiblingDiff(t, srv, map[string]any{
		"scope":       "compare",
		"base_ref":    "HEAD",
		"focus_files": fileA,
	}))
	require.Equal(t, 0, out.Total)
	require.Empty(t, out.Siblings)
}

// TestSiblingDiffContext_GCXAndTOONAndBudget covers the wire-format + budget
// contract.
func TestSiblingDiffContext_GCXAndTOONAndBudget(t *testing.T) {
	dir, fileA, _, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	base := map[string]any{"base": "base-ref", "focus_files": fileA}

	// GCX round-trip: section headers must appear.
	gcxArgs := map[string]any{}
	for k, v := range base {
		gcxArgs[k] = v
	}
	gcxArgs["format"] = "gcx"
	gcx := callSiblingDiff(t, srv, gcxArgs)
	require.False(t, gcx.IsError)
	gtext := gcx.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "sibling_diff_context.summary")
	require.Contains(t, gtext, "sibling_diff_context.siblings")

	// max_bytes budget is honoured (response stays bounded vs the full diff).
	budgetArgs := map[string]any{}
	for k, v := range base {
		budgetArgs[k] = v
	}
	budgetArgs["format"] = "gcx"
	budgetArgs["max_bytes"] = float64(140)
	budgeted := callSiblingDiff(t, srv, budgetArgs)
	require.False(t, budgeted.IsError)
	require.LessOrEqual(t, len(budgeted.Content[0].(mcplib.TextContent).Text), 600)

	// TOON round-trip: still carries the total key.
	toonArgs := map[string]any{}
	for k, v := range base {
		toonArgs[k] = v
	}
	toonArgs["format"] = "toon"
	toon := callSiblingDiff(t, srv, toonArgs)
	require.False(t, toon.IsError)
	require.Contains(t, toon.Content[0].(mcplib.TextContent).Text, "total")
}

// reviewGitRepo creates a git repo whose HEAD commit introduces a function with
// a planted review-rule violation: an inverted error check
// (`if err == nil { return err }`) that the go-inverted-err-check detector flags
// at error severity. Returns the repo root and the changed file path.
func reviewGitRepo(t *testing.T) (root, file string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.noprefix", "false")

	file = "internal/svc/handler.go"
	write := func(rel, src string) {
		abs := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}
	write(file, "package svc\n\nfunc Load() error {\n\treturn nil\n}\n")
	run("add", ".")
	run("commit", "-m", "base")
	run("tag", "base-ref")

	// HEAD commit rewrites Load to carry the inverted err-check bug.
	write(file, "package svc\n\nimport \"errors\"\n\n"+
		"func Load() error {\n"+
		"\terr := errors.New(\"boom\")\n"+
		"\tif err == nil {\n"+
		"\t\treturn err\n"+
		"\t}\n"+
		"\treturn nil\n"+
		"}\n")
	run("add", ".")
	run("commit", "-m", "change")
	return dir, file
}

func callReview(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "review"
	req.Params.Arguments = args
	res, err := srv.handleReview(t.Context(), req)
	require.NoError(t, err)
	return res
}

type reviewOut struct {
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Total    int    `json:"total"`
	Comments []struct {
		File        string `json:"file"`
		Line        int    `json:"line"`
		Severity    string `json:"severity"`
		Message     string `json:"message"`
		Rule        string `json:"rule"`
		Category    string `json:"category"`
		Source      string `json:"source"`
		IdentityKey string `json:"identity_key"`
	} `json:"comments"`
	FileRisk []struct {
		File     string `json:"file"`
		Risk     string `json:"risk"`
		Findings int    `json:"findings"`
	} `json:"file_risk"`
	Depth string `json:"depth"`
	Gate  struct {
		Input           int `json:"input"`
		Kept            int `json:"kept"`
		BelowConfidence int `json:"below_confidence"`
		BelowSeverity   int `json:"below_severity"`
	} `json:"gate"`
	Cost *struct {
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		USD          float64 `json:"usd"`
		Estimated    bool    `json:"estimated"`
		ElapsedMs    int64   `json:"elapsed_ms"`
	} `json:"cost"`
}

func decodeReview(t *testing.T, res *mcplib.CallToolResult) reviewOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %v", res)
	var out reviewOut
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

// TestReview_RulepackFindingAndVerdict asserts the review tool runs the
// deterministic rulepack over the changeset, returns a line-anchored finding for
// the planted inverted-err-check, and reports a BLOCK verdict (error severity).
func TestReview_RulepackFindingAndVerdict(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"base": "base-ref",
	}))

	require.Equal(t, "BLOCK", out.Verdict, "an error-severity finding must block")
	require.GreaterOrEqual(t, out.Total, 1, "the planted inverted-err-check must be flagged")

	var found bool
	for _, c := range out.Comments {
		if c.Rule == "go-inverted-err-check" {
			found = true
			require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(c.File))
			require.Greater(t, c.Line, 0, "the finding must be anchored to a real line")
			require.Equal(t, "error", c.Severity)
			require.Equal(t, "rulepack", c.Source)
		}
	}
	require.True(t, found, "expected a go-inverted-err-check finding; got %+v", out.Comments)

	// The file carries a risk row.
	require.NotEmpty(t, out.FileRisk)
}

// TestReview_UseLLMAddsFinding drives the LLM phase through the test-only seam:
// a stubbed gen returns a candidate whose snippet appears verbatim in the
// change, so it relocates to a real line and joins the report as an LLM finding.
func TestReview_UseLLMAddsFinding(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	// The stub gen returns one candidate anchored to a verbatim change line.
	srv.reviewLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[{"file":"` + filepath.ToSlash(file) + `",` +
				`"snippet":"err := errors.New(\"boom\")",` +
				`"message":"prefer fmt.Errorf for wrapping","severity":"warning","category":"idiom"}]`, nil
		}
	}

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"base":    "base-ref",
		"use_llm": true,
	}))

	var llmFound bool
	for _, c := range out.Comments {
		if c.Source == "llm" {
			llmFound = true
			require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(c.File))
			require.Greater(t, c.Line, 0, "LLM finding must relocate to a real line")
			require.Equal(t, "prefer fmt.Errorf for wrapping", c.Message)
		}
	}
	require.True(t, llmFound, "the stubbed LLM finding must join the report; got %+v", out.Comments)
}

// reviewServerWithConfig is indexedSiblingServer plus a ConfigManager whose
// workspace config (keyed by the empty repo prefix the review handlers query
// with no `repo` arg) carries the given `review:` block. It writes a
// .gortex.yaml into the repo and loads it, so the same path the live daemon
// uses (GetRepoConfig → cfg.Review) is exercised.
func reviewServerWithConfig(t *testing.T, dir, reviewYAML string) *Server {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(reviewYAML), 0o644))

	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "global.yaml"))
	require.NoError(t, err)
	cm.LoadWorkspaceConfig("", dir)

	srv := indexedSiblingServer(t, dir)
	srv.configManager = cm
	return srv
}

// TestReview_GateDropsBelowSeverity proves the repo's `review:` config is now
// LIVE on the handler path: with min_severity: warning, a planted info-severity
// LLM finding is dropped by the gate while the error-severity rulepack finding
// survives, and the gate summary records the suppression.
func TestReview_GateDropsBelowSeverity(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := reviewServerWithConfig(t, dir, "review:\n  min_severity: warning\n")

	// The stub gen returns one INFO-severity candidate anchored to a verbatim
	// change line. With no gate it would join the report; min_severity:warning
	// must drop it.
	srv.reviewLLMGenWithUsageOverride = func() review.LLMGenWithUsage {
		return func(_ context.Context, _ string, _ int) (string, llm.TokenUsage, error) {
			out := `[{"file":"` + filepath.ToSlash(file) + `",` +
				`"snippet":"err := errors.New(\"boom\")",` +
				`"message":"style nit","severity":"info","category":"idiom"}]`
			return out, llm.TokenUsage{}, nil
		}
	}

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"base":    "base-ref",
		"use_llm": true,
	}))

	// The info-severity LLM finding must be gated out.
	for _, c := range out.Comments {
		require.NotEqual(t, "info", c.Severity, "info finding must be dropped by min_severity:warning; got %+v", c)
		require.NotEqual(t, "llm", c.Source, "the only LLM finding was info-severity and must be suppressed")
	}
	// The error-severity rulepack finding survives the floor.
	require.GreaterOrEqual(t, out.Total, 1, "the error-severity rulepack finding must survive")
	// The gate summary reports the suppression.
	require.GreaterOrEqual(t, out.Gate.BelowSeverity, 1, "gate must count one below-severity drop")
	require.GreaterOrEqual(t, out.Gate.Input, out.Gate.Kept+1, "gate input must exceed kept by the dropped finding")
}

// TestReview_DepthSkipsLLM proves the adaptive-depth thresholds from config are
// now LIVE: with quick_max_lines high enough that the small changeset classifies
// as quick, the LLM MAIN phase is skipped entirely — the usage seam is never
// invoked — and the report records depth: quick.
func TestReview_DepthSkipsLLM(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := reviewServerWithConfig(t, dir, "review:\n  quick_max_lines: 1000\n")

	called := false
	srv.reviewLLMGenWithUsageOverride = func() review.LLMGenWithUsage {
		return func(_ context.Context, _ string, _ int) (string, llm.TokenUsage, error) {
			called = true
			out := `[{"file":"` + filepath.ToSlash(file) + `",` +
				`"snippet":"err := errors.New(\"boom\")",` +
				`"message":"should not appear","severity":"warning","category":"idiom"}]`
			return out, llm.TokenUsage{InputTokens: 10, OutputTokens: 5}, nil
		}
	}

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"base":    "base-ref",
		"use_llm": true,
	}))

	require.Equal(t, "quick", out.Depth, "a small change under quick_max_lines must classify quick")
	require.False(t, called, "the quick depth must skip the LLM MAIN phase — the usage seam must not be called")
	for _, c := range out.Comments {
		require.NotEqual(t, "llm", c.Source, "no LLM finding may appear when the MAIN phase is skipped")
	}
}

// TestReview_CostBlockFromUsageSeam proves the usage-aware seam now feeds the
// response: a stubbed gen reporting token usage produces a cost block on the
// review response, priced against the (overridden) rate card.
func TestReview_CostBlockFromUsageSeam(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	srv.reviewLLMGenWithUsageOverride = func() review.LLMGenWithUsage {
		return func(_ context.Context, _ string, _ int) (string, llm.TokenUsage, error) {
			out := `[{"file":"` + filepath.ToSlash(file) + `",` +
				`"snippet":"err := errors.New(\"boom\")",` +
				`"message":"prefer fmt.Errorf","severity":"warning","category":"idiom"}]`
			return out, llm.TokenUsage{InputTokens: 1000, OutputTokens: 500}, nil
		}
	}
	// Deterministic rate card: $3/1M input, $6/1M output.
	srv.reviewPricingOverride = &llm.ProviderPricing{Input: 3.0, Output: 6.0}

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"base":    "base-ref",
		"use_llm": true,
	}))

	require.NotNil(t, out.Cost, "the usage-aware seam must populate a cost block")
	require.Equal(t, 1000, out.Cost.InputTokens)
	require.Equal(t, 500, out.Cost.OutputTokens)
	require.True(t, out.Cost.Estimated, "a non-zero usage report is a grounded (estimated) cost")
	// USD = 1000*3/1e6 + 500*6/1e6 = 0.003 + 0.003 = 0.006.
	require.InDelta(t, 0.006, out.Cost.USD, 1e-9)
}

// TestReview_PastedDiff reviews a pasted unified diff off-disk (no git command).
func TestReview_PastedDiff(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	diff := "diff --git a/x.go b/x.go\n" +
		"--- a/x.go\n+++ b/x.go\n" +
		"@@ -1,1 +1,2 @@\n package x\n+var Added = 1\n"
	out := decodeReview(t, callReview(t, srv, map[string]any{
		"diff": diff,
	}))
	// A pasted diff with no rule violation approves; the file appears in risk.
	require.Equal(t, "APPROVE", out.Verdict)
	require.NotNil(t, out.Comments)
}

// TestReview_GCXAndTOONAndBudget covers the wire-format + budget contract.
func TestReview_GCXAndTOONAndBudget(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	base := map[string]any{"base": "base-ref"}

	gcxArgs := map[string]any{"format": "gcx"}
	for k, v := range base {
		gcxArgs[k] = v
	}
	gcx := callReview(t, srv, gcxArgs)
	require.False(t, gcx.IsError)
	gtext := gcx.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "review.summary")
	require.Contains(t, gtext, "review.comments")

	budgetArgs := map[string]any{"format": "gcx", "max_bytes": float64(120)}
	for k, v := range base {
		budgetArgs[k] = v
	}
	budgeted := callReview(t, srv, budgetArgs)
	require.False(t, budgeted.IsError)
	require.LessOrEqual(t, len(budgeted.Content[0].(mcplib.TextContent).Text), 600)

	toonArgs := map[string]any{"format": "toon"}
	for k, v := range base {
		toonArgs[k] = v
	}
	toon := callReview(t, srv, toonArgs)
	require.False(t, toon.IsError)
	require.Contains(t, toon.Content[0].(mcplib.TextContent).Text, "verdict")
}

func callSuppressFinding(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "suppress_finding"
	req.Params.Arguments = args
	res, err := srv.handleSuppressFinding(t.Context(), req)
	require.NoError(t, err)
	return res
}

// TestSuppressFinding_SuppressesAcrossReviews is the end-to-end suppression
// proof: a review surfaces the planted inverted-err-check finding, suppress_finding
// records its identity, and a subsequent review of the same changeset no longer
// flags it (and the gate counts it as identity-suppressed).
func TestSuppressFinding_SuppressesAcrossReviews(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)
	// Wire a sidecar-backed suppression store at a temp cache dir.
	cacheDir := t.TempDir()
	srv.InitSuppressions(cacheDir, dir)
	require.NotNil(t, srv.suppressions)
	t.Cleanup(func() {
		sidecar, err := persistence.OpenSidecar(persistence.DefaultSidecarPath(cacheDir))
		require.NoError(t, err)
		require.NoError(t, sidecar.Close())
	})

	// First review: the finding is present and carries an identity key.
	out := decodeReview(t, callReview(t, srv, map[string]any{"base": "base-ref"}))
	require.Equal(t, "BLOCK", out.Verdict)

	var identityKey string
	for _, c := range out.Comments {
		if c.Rule == "go-inverted-err-check" {
			identityKey = c.IdentityKey
		}
	}
	require.NotEmpty(t, identityKey, "review must expose the finding's identity_key; got %+v", out.Comments)

	// Suppress it by identity key.
	supRes := callSuppressFinding(t, srv, map[string]any{
		"action":       "add",
		"identity_key": identityKey,
		"rule":         "go-inverted-err-check",
		"reason":       "intentional in this handler",
		"author":       "tester",
	})
	require.False(t, supRes.IsError, "suppress add errored: %v", supRes)

	// List shows the one suppression.
	listRes := callSuppressFinding(t, srv, map[string]any{"action": "list"})
	var listOut struct {
		Total        int `json:"total"`
		Suppressions []struct {
			IdentityKey string `json:"identity_key"`
		} `json:"suppressions"`
	}
	require.NoError(t, json.Unmarshal([]byte(listRes.Content[0].(mcplib.TextContent).Text), &listOut))
	require.Equal(t, 1, listOut.Total)
	require.Equal(t, identityKey, listOut.Suppressions[0].IdentityKey)

	// Second review: the suppressed finding is gone.
	out2 := decodeReview(t, callReview(t, srv, map[string]any{"base": "base-ref"}))
	for _, c := range out2.Comments {
		require.NotEqual(t, "go-inverted-err-check", c.Rule,
			"a suppressed finding must not reappear; got %+v", out2.Comments)
	}

	// Un-suppress and confirm the finding returns.
	rmRes := callSuppressFinding(t, srv, map[string]any{
		"action":       "remove",
		"identity_key": identityKey,
	})
	require.False(t, rmRes.IsError, "suppress remove errored: %v", rmRes)

	out3 := decodeReview(t, callReview(t, srv, map[string]any{"base": "base-ref"}))
	var back bool
	for _, c := range out3.Comments {
		if c.Rule == "go-inverted-err-check" {
			back = true
			require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(c.File))
		}
	}
	require.True(t, back, "un-suppressed finding must reappear; got %+v", out3.Comments)
}

// TestSuppressFinding_RegisteredEagerly asserts the suppress_finding tool is in
// the eager (hot) review-engine set.
func TestSuppressFinding_RegisteredEagerly(t *testing.T) {
	require.True(t, hotEagerTools["suppress_finding"],
		"suppress_finding must be eagerly registered (hot), not deferred")

	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "suppress_finding",
		"eager suppress_finding tool must appear in tools/list without tools_search expansion")
	require.False(t, srv.lazy.IsDeferred("suppress_finding"),
		"suppress_finding must not be deferred")
}

// TestReview_RegisteredEagerly asserts the review tool is in the eager set.
func TestReview_RegisteredEagerly(t *testing.T) {
	require.True(t, hotEagerTools["review"],
		"review must be eagerly registered (hot), not deferred")

	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "review",
		"eager review tool must appear in tools/list without tools_search expansion")
	require.False(t, srv.lazy.IsDeferred("review"),
		"review must not be deferred")
}

// TestSiblingDiffContext_RegisteredEagerly asserts the review-engine tool is in
// the eager (hot) set — published in tools/list at session start — unlike the
// deferred PR tools, so a reviewing agent does not pay a discovery round-trip.
func TestSiblingDiffContext_RegisteredEagerly(t *testing.T) {
	require.True(t, hotEagerTools["sibling_diff_context"],
		"sibling_diff_context must be eagerly registered (hot), not deferred")

	// And it is actually live in tools/list even with the lazy split enabled.
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "sibling_diff_context",
		"eager review tool must appear in tools/list without tools_search expansion")
	require.False(t, srv.lazy.IsDeferred("sibling_diff_context"),
		"sibling_diff_context must not be deferred")
}

func callReviewPack(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "review_pack"
	req.Params.Arguments = args
	res, err := srv.handleReviewPack(t.Context(), req)
	require.NoError(t, err)
	return res
}

type reviewPackOut struct {
	Verdict        string `json:"verdict"`
	Summary        string `json:"summary"`
	Total          int    `json:"total"`
	ChangedSymbols []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Class string `json:"class"`
		Risk  string `json:"risk"`
	} `json:"changed_symbols"`
	FileRisk []struct {
		File     string `json:"file"`
		Risk     string `json:"risk"`
		Findings int    `json:"findings"`
	} `json:"file_risk"`
	Findings []struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Severity string `json:"severity"`
		Rule     string `json:"rule"`
		Source   string `json:"source"`
	} `json:"findings"`
	Guards []struct {
		RuleName string `json:"rule_name"`
		Kind     string `json:"kind"`
	} `json:"guards"`
	TestTargets         []string `json:"test_targets"`
	VerificationCommand string   `json:"verification_command"`
	Receipt             struct {
		RiskTier       string `json:"risk_tier"`
		NextSafeAction string `json:"next_safe_action"`
		MergeBlocker   bool   `json:"merge_blocker"`
		BlockerReason  string `json:"blocker_reason"`
		AffectedCount  int    `json:"affected_count"`
		TopFactors     []struct {
			Axis  string  `json:"axis"`
			Score float64 `json:"score"`
		} `json:"top_factors"`
	} `json:"receipt"`
	Pack *struct {
		Changed []struct {
			ID   string `json:"id"`
			Diff string `json:"diff"`
		} `json:"changed"`
	} `json:"pack"`
}

func decodeReviewPack(t *testing.T, res *mcplib.CallToolResult) reviewPackOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %v", res)
	var out reviewPackOut
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

// TestReviewPack_WorktreeViewRendersTheWorktreeHunk: under a linked worktree's
// view, the pack's changed-symbol hunk comes from the worktree's own diff, so it
// carries the removed line, not a fallback that reads the span as all-new.
func TestReviewPack_WorktreeViewRendersTheWorktreeHunk(t *testing.T) {
	// Given a tracked repo with a clean working tree, and a linked worktree
	// outside it whose working copy rewrites Beta's body.
	dir, _, fileB, _ := siblingDiffGitRepo(t)
	srv := indexedSiblingServer(t, dir)
	worktree := filepath.Join(t.TempDir(), "wt")
	runGitT(t, dir, "worktree", "add", "--detach", worktree, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, filepath.FromSlash(fileB)),
		[]byte("package alpha\n\nfunc Beta() int {\n\treturn 4\n}\n"), 0o644))

	// When review_pack renders the default unstaged changeset through that view.
	ctx := withRequestView(t.Context(), &requestView{kind: requestViewKindWorktree, viewRoot: worktree, reader: srv.graph})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "review_pack"
	req.Params.Arguments = map[string]any{"include_pack": true}
	res, err := srv.handleReviewPack(ctx, req)
	require.NoError(t, err)

	// Then Beta's hunk shows the worktree's removed line.
	out := decodeReviewPack(t, res)
	require.NotNil(t, out.Pack)
	diffs := map[string]string{}
	for _, entry := range out.Pack.Changed {
		diffs[entry.ID] = entry.Diff
	}
	require.Contains(t, diffs[fileB+"::Beta"], "-\ty := 2")
}

// TestReviewPack_Envelope asserts the packaged envelope carries the verdict,
// per-symbol classification, per-file risk, line-anchored findings, the
// impacted-test verification command, and the privacy-safe receipt on a staged
// change with a planted error-severity finding.
func TestReviewPack_Envelope(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"base": "base-ref",
	}))

	// Verdict reflects the error-severity rulepack finding.
	require.Equal(t, "BLOCK", out.Verdict, "an error-severity finding blocks")
	require.GreaterOrEqual(t, out.Total, 1)

	// The planted inverted-err-check finding rides on the envelope, anchored.
	var found bool
	for _, f := range out.Findings {
		if f.Rule == "go-inverted-err-check" {
			found = true
			require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(f.File))
			require.Greater(t, f.Line, 0)
			require.Equal(t, "error", f.Severity)
		}
	}
	require.True(t, found, "expected the planted finding; got %+v", out.Findings)

	// Per-symbol classification: the changed Load function is classified.
	require.NotEmpty(t, out.ChangedSymbols)
	var classified bool
	for _, cs := range out.ChangedSymbols {
		if cs.Name == "Load" {
			classified = true
			require.NotEmpty(t, cs.Class, "the changed symbol carries a class")
			require.NotEmpty(t, cs.Risk, "the changed symbol carries a risk tier")
		}
	}
	require.True(t, classified, "expected Load to be classified; got %+v", out.ChangedSymbols)

	// Per-file risk ranking.
	require.NotEmpty(t, out.FileRisk)

	// A concrete, runnable verification command derived from the toolchain.
	require.NotEmpty(t, out.VerificationCommand)
	require.Contains(t, out.VerificationCommand, "go test")

	// The receipt is populated (a real PR-risk projection).
	require.NotEmpty(t, out.Receipt.NextSafeAction)
	require.GreaterOrEqual(t, out.Receipt.AffectedCount, 0)
}

// TestReviewPack_GuardBreakBlocks plants a co-change guard rule the changeset
// violates (it touches internal/svc but not the required internal/audit), and
// asserts the verdict is driven to BLOCK and the violation rides on the envelope.
func TestReviewPack_GuardBreakBlocks(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	// A co-change rule: any change to internal/svc requires a matching change
	// to internal/audit. The changeset only touches internal/svc, so the rule
	// is violated.
	srv.guardRules = []config.GuardRule{{
		Name:    "svc-requires-audit",
		Kind:    "co-change",
		Source:  filepath.Join("internal", "svc"),
		Target:  filepath.Join("internal", "audit"),
		Message: "svc changes require an audit-log update",
	}}

	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"base": "base-ref",
	}))

	require.Equal(t, "BLOCK", out.Verdict, "a guard violation must drive the verdict to BLOCK")
	require.NotEmpty(t, out.Guards, "the guard violation rides on the envelope")
	var guarded bool
	for _, g := range out.Guards {
		if g.RuleName == "svc-requires-audit" {
			guarded = true
			require.Equal(t, "co-change", g.Kind)
		}
	}
	require.True(t, guarded, "expected the planted guard violation; got %+v", out.Guards)

	// The receipt's merge blocker reflects the out-of-band gate break.
	require.True(t, out.Receipt.MergeBlocker, "guard break flags the receipt merge_blocker")
}

// TestReviewPack_ScrubScrubsReceipt asserts scrub:true sanitizes the receipt's
// free-text fields while keeping the structural counts/tier/action.
func TestReviewPack_ScrubScrubsReceipt(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"base":  "base-ref",
		"scrub": true,
	}))

	// The structural fields survive scrub.
	require.NotEmpty(t, out.Receipt.RiskTier, "the risk tier is structurally safe and retained")
	// No field carries a path-like / symbol-ID-like / email-like value.
	require.NotContains(t, out.Receipt.NextSafeAction, "/")
	require.NotContains(t, out.Receipt.BlockerReason, "::")
	for _, f := range out.Receipt.TopFactors {
		require.NotContains(t, f.Axis, "/")
		require.NotContains(t, f.Axis, "::")
		require.NotContains(t, f.Axis, "@")
	}
}

// TestReviewPack_IncludePack renders the FG9 tiered pack when include_pack is set.
func TestReviewPack_IncludePack(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"base":         "base-ref",
		"include_pack": true,
	}))
	require.NotNil(t, out.Pack, "include_pack must render the tiered pack")
	require.NotEmpty(t, out.Pack.Changed, "the changed tier carries the changed symbols")
	var hasDiff bool
	for _, e := range out.Pack.Changed {
		if e.Diff != "" {
			hasDiff = true
		}
	}
	require.True(t, hasDiff, "the changed tier renders diff-hunk text")
}

// TestReviewPack_PastedDiffApproves reviews a pasted diff off-disk with no rule
// violation: it approves and skips the indexed-symbol gates gracefully.
func TestReviewPack_PastedDiffApproves(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	diff := "diff --git a/x.go b/x.go\n" +
		"--- a/x.go\n+++ b/x.go\n" +
		"@@ -1,1 +1,2 @@\n package x\n+var Added = 1\n"
	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"diff": diff,
	}))
	require.Equal(t, "APPROVE", out.Verdict)
	require.Empty(t, out.Guards)
	// A pasted diff has no runnable test targets — the command falls back to the
	// whole-tree run so it is always runnable.
	require.NotEmpty(t, out.VerificationCommand)
}

// TestReviewPack_GCXAndTOONAndBudget covers the wire-format + budget contract.
func TestReviewPack_GCXAndTOONAndBudget(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	base := map[string]any{"base": "base-ref"}

	gcxArgs := map[string]any{"format": "gcx"}
	for k, v := range base {
		gcxArgs[k] = v
	}
	gcx := callReviewPack(t, srv, gcxArgs)
	require.False(t, gcx.IsError)
	gtext := gcx.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "review_pack.summary")
	require.Contains(t, gtext, "review_pack.changed_symbols")
	require.Contains(t, gtext, "review_pack.findings")

	budgetArgs := map[string]any{"format": "gcx", "max_bytes": float64(120)}
	for k, v := range base {
		budgetArgs[k] = v
	}
	budgeted := callReviewPack(t, srv, budgetArgs)
	require.False(t, budgeted.IsError)
	require.LessOrEqual(t, len(budgeted.Content[0].(mcplib.TextContent).Text), 900)

	toonArgs := map[string]any{"format": "toon"}
	for k, v := range base {
		toonArgs[k] = v
	}
	toon := callReviewPack(t, srv, toonArgs)
	require.False(t, toon.IsError)
	require.Contains(t, toon.Content[0].(mcplib.TextContent).Text, "verdict")
}

// TestReviewPack_RegisteredEagerly asserts review_pack is in the eager (hot) set
// — published in tools/list at session start so a reviewing agent does not pay a
// discovery round-trip.
func TestReviewPack_RegisteredEagerly(t *testing.T) {
	require.True(t, hotEagerTools["review_pack"],
		"review_pack must be eagerly registered (hot), not deferred")

	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "review_pack",
		"eager review_pack tool must appear in tools/list without tools_search expansion")
	require.False(t, srv.lazy.IsDeferred("review_pack"),
		"review_pack must not be deferred")
}
