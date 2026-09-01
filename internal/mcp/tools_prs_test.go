package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm/svc"
	"github.com/zzet/gortex/internal/query"
)

// prToolsTestServer builds a server over a synthetic graph: a security-
// sensitive hub function (internal/auth/login.go) with many inbound
// callers and no covering test, so the file→symbol join and PR-risk score
// have real signal. Returns the server and the changed file path.
func prToolsTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	// `file` is the forge/git spelling a caller supplies; `stored` is what the
	// indexer writes, which uses native separators even with no repo prefix.
	// Hard-coding the '/' form as the graph key describes an index a Windows
	// daemon never produces, and the join contract then cannot be observed.
	file := "internal/auth/login.go"
	stored := filepath.FromSlash(file)
	hubID := stored + "::ValidateToken"
	g.AddNode(&graph.Node{ID: hubID, Kind: graph.KindFunction, Name: "ValidateToken", FilePath: stored, StartLine: 1, EndLine: 10})
	callerFile := filepath.FromSlash("pkg/c.go")
	for i := 0; i < 12; i++ {
		cid := callerFile + "::caller" + strconv.Itoa(i)
		g.AddNode(&graph.Node{ID: cid, Kind: graph.KindFunction, Name: "caller" + strconv.Itoa(i), FilePath: callerFile})
		g.AddEdge(&graph.Edge{From: cid, To: hubID, Kind: graph.EdgeCalls})
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	return srv, file
}

// withSeams swaps the forge func-var seam for the duration of the test so
// no network call is made; the originals are restored on cleanup.
func withSeams(t *testing.T, list func(context.Context, string, forge.ListOpts) ([]forge.PR, error), files func(context.Context, string, int) ([]string, error)) {
	t.Helper()
	origList, origFiles := forgeList, forgeFiles
	if list != nil {
		forgeList = list
	}
	if files != nil {
		forgeFiles = files
	}
	t.Cleanup(func() { forgeList, forgeFiles = origList, origFiles })
}

func callPRTool(t *testing.T, srv *Server, name string, h func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error), args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := h(t.Context(), req)
	require.NoError(t, err)
	return res
}

func unmarshalPRResult(t *testing.T, res *mcplib.CallToolResult, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), v))
}

// --- get_pr_impact: supplied files, no forge call --------------------------

func TestGetPRImpact_SuppliedFilesNoForge(t *testing.T) {
	srv, file := prToolsTestServer(t)

	// Both seams panic if hit — the supplied-files path must not touch them.
	seamHit := false
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { seamHit = true; return nil, nil },
		func(context.Context, string, int) ([]string, error) { seamHit = true; return nil, nil },
	)

	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number": float64(7),
		"files":  string(filesJSON),
	})
	require.False(t, res.IsError, "errored: %v", res)
	require.False(t, seamHit, "the forge seam must NOT be called when files are supplied")

	var out struct {
		Number         int      `json:"number"`
		Risk           string   `json:"risk"`
		Score          float64  `json:"score"`
		ChangedFiles   []string `json:"changed_files"`
		ChangedSymbols []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"changed_symbols"`
		Blast            map[string]any `json:"blast"`
		ReviewPriorities []struct {
			Axis  string  `json:"axis"`
			Score float64 `json:"score"`
		} `json:"review_priorities"`
	}
	unmarshalPRResult(t, res, &out)

	require.Equal(t, 7, out.Number)
	require.Contains(t, out.ChangedFiles, file)
	// The file→symbol join found the hub.
	require.NotEmpty(t, out.ChangedSymbols)
	foundHub := false
	for _, s := range out.ChangedSymbols {
		if s.Name == "ValidateToken" {
			foundHub = true
		}
	}
	require.True(t, foundHub, "expected ValidateToken in the changed-symbol join")
	// Security path + 12-caller untested hub scores at least HIGH.
	require.Contains(t, []string{"HIGH", "CRITICAL"}, out.Risk)
	// Blast grouping is present (callers grouped by file).
	require.NotNil(t, out.Blast)
	require.Contains(t, out.Blast, "callers_by_file")
	// review_priorities sorted descending.
	for i := 1; i < len(out.ReviewPriorities); i++ {
		require.GreaterOrEqual(t, out.ReviewPriorities[i-1].Score, out.ReviewPriorities[i].Score)
	}
}

func TestGetPRImpact_ReceiptEmitted(t *testing.T) {
	srv, file := prToolsTestServer(t)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)
	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number":  float64(7),
		"files":   string(filesJSON),
		"receipt": true,
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Receipt *struct {
			ReceiptVersion int    `json:"receipt_version"`
			RiskTier       string `json:"risk_tier"`
			NextSafeAction string `json:"next_safe_action"`
		} `json:"receipt"`
	}
	unmarshalPRResult(t, res, &out)
	require.NotNil(t, out.Receipt, "receipt:true must emit a receipt block")
	require.Equal(t, 1, out.Receipt.ReceiptVersion)
	require.NotEmpty(t, out.Receipt.RiskTier)
	require.NotEmpty(t, out.Receipt.NextSafeAction)

	// receipt absent by default.
	res2 := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number": float64(7),
		"files":  string(filesJSON),
	})
	var out2 map[string]any
	unmarshalPRResult(t, res2, &out2)
	_, has := out2["receipt"]
	require.False(t, has, "receipt must be absent when not requested")
}

func TestGetPRImpact_RequiresNumber(t *testing.T) {
	srv, file := prToolsTestServer(t)
	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"files": string(filesJSON),
	})
	require.True(t, res.IsError, "expected an error when number is missing")
}

// --- multi-repo prefix join --------------------------------------------------

// TestChangedSymbolsForFiles_RepoPrefixJoin covers the multi-repo daemon
// shape: indexed file paths carry the repo prefix ("myrepo/<rel>") while
// forge file lists are repo-relative. The join must retry with the prefix,
// stay raw-first for unprefixed graphs, and never double-prefix.
func TestChangedSymbolsForFiles_RepoPrefixJoin(t *testing.T) {
	g := graph.New()
	// The graph keys a file as "<prefix>/" + the remainder in the indexing
	// machine's native separators (see internal/graphpath), so the fixture is
	// built that way. Hard-coding the '/'-joined form describes a key a
	// Windows daemon never produces.
	prefixedFile := "myrepo/" + filepath.FromSlash("internal/auth/login.go")
	prefixedID := prefixedFile + "::ValidateToken"
	g.AddNode(&graph.Node{ID: prefixedID, Kind: graph.KindFunction, Name: "ValidateToken", FilePath: prefixedFile, StartLine: 1, EndLine: 10})
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)

	files, nodes := srv.changedSymbolsForFiles("myrepo", []string{"internal/auth/login.go"})
	require.Equal(t, []string{"internal/auth/login.go"}, files,
		"the reported changed files keep the forge-relative paths")
	require.Len(t, nodes, 1, "the prefixed retry must find the symbol")
	require.Equal(t, prefixedID, nodes[0].ID)

	// Without a prefix the relative path misses — the unprefixed single-repo
	// graph shape keeps its exact-match behavior.
	_, nodes = srv.changedSymbolsForFiles("", []string{"internal/auth/login.go"})
	require.Empty(t, nodes)

	// A forge file list is repo-relative, so a path that merely LOOKS
	// prefixed is still prefixed: it names the nested file, not the
	// same-named top-level one. Treating it as already-keyed used to
	// resolve the wrong file — or, with no shadow present, nothing at all.
	shadowFile := "myrepo/" + filepath.FromSlash("myrepo/internal/auth/login.go")
	g.AddNode(&graph.Node{ID: shadowFile + "::Nested", Kind: graph.KindFunction, Name: "Nested", FilePath: shadowFile, StartLine: 1, EndLine: 10})
	_, nodes = srv.changedSymbolsForFiles("myrepo", []string{"myrepo/internal/auth/login.go"})
	require.Len(t, nodes, 1)
	require.Equal(t, shadowFile+"::Nested", nodes[0].ID,
		"a repo-relative path that looks prefixed must resolve to the nested file")
}

// --- list_prs: classification ----------------------------------------------

func TestListPRs_ClassifiesSupplied(t *testing.T) {
	srv, _ := prToolsTestServer(t)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		nil,
	)

	prs := []forge.PR{
		{Number: 1, Title: "draft work", Author: "a", BaseRef: "main", IsDraft: true},
		{Number: 2, Title: "ready", Author: "b", BaseRef: "main", ReviewDecision: "APPROVED", CIRollup: "SUCCESS"},
		{Number: 3, Title: "needs work", Author: "c", BaseRef: "main", ReviewDecision: "CHANGES_REQUESTED", CIRollup: "FAILURE"},
	}
	prsJSON, _ := json.Marshal(prs)
	res := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON)})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total int `json:"total"`
		PRs   []struct {
			Number   int      `json:"number"`
			CI       string   `json:"ci"`
			State    string   `json:"state"`
			Blockers []string `json:"blockers"`
		} `json:"prs"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 3, out.Total)
	byNum := map[int]string{}
	ciByNum := map[int]string{}
	for _, p := range out.PRs {
		byNum[p.Number] = p.State
		ciByNum[p.Number] = p.CI
	}
	require.Equal(t, "DRAFT", byNum[1])
	require.Equal(t, "APPROVED", byNum[2])
	require.Equal(t, "SUCCESS", ciByNum[2])
	require.Equal(t, "CHANGES_REQUESTED", byNum[3])
	require.Equal(t, "FAILURE", ciByNum[3])
}

// --- triage_prs: sorted by score desc --------------------------------------

func TestTriagePRs_SortedDescending(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	// PR #1 touches a low-risk file with no symbols; PR #2 touches the
	// security hub. Supplied via the files map so no forge call happens.
	prs := []forge.PR{
		{Number: 1, Title: "low", Author: "a"},
		{Number: 2, Title: "high", Author: "b"},
	}
	prsJSON, _ := json.Marshal(prs)
	filesMap := map[string][]string{
		"1": {"pkg/unrelated.go"},
		"2": {hubFile},
	}
	filesJSON, _ := json.Marshal(filesMap)

	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":   string(prsJSON),
		"files": string(filesJSON),
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total  int `json:"total"`
		Ranked []struct {
			Number int     `json:"number"`
			Score  float64 `json:"score"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 2, out.Total)
	require.Len(t, out.Ranked, 2)
	// Sorted by score descending: the security hub PR (#2) ranks first.
	for i := 1; i < len(out.Ranked); i++ {
		require.GreaterOrEqual(t, out.Ranked[i-1].Score, out.Ranked[i].Score, "ranked must be score-descending")
	}
	require.Equal(t, 2, out.Ranked[0].Number, "the higher-risk PR must rank first")
}

// TestTriagePRs_SecondRunHitsCacheNoRefetch proves the per-(repo,number) PR
// cache actually saves a forge round-trip: two successive self-served triage
// runs within the TTL fetch each PR's files exactly once. The first run
// self-serves the list and fetches files per PR (stamping the hydrated PR
// into the cache); the second run lists again but resolves every PR's files
// from the cache, so the file-fetch seam is NOT hit a second time.
func TestTriagePRs_SecondRunHitsCacheNoRefetch(t *testing.T) {
	// A resolvable token so forge.Available == true and the handler self-serves.
	t.Setenv("GH_TOKEN", "x-fake-token")

	srv, hubFile := prToolsTestServer(t)

	// ListPRs returns PRs with EMPTY Files (the forge contract) so the per-PR
	// file fetch is the only path that hydrates them — and thus the only thing
	// the cache can save on the second run.
	listHits := 0
	fileHits := 0
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			listHits++
			return []forge.PR{
				{Number: 1, Title: "low", Author: "a"},
				{Number: 2, Title: "high", Author: "b"},
			}, nil
		},
		func(_ context.Context, _ string, number int) ([]string, error) {
			fileHits++
			if number == 2 {
				return []string{hubFile}, nil
			}
			return []string{"pkg/unrelated.go"}, nil
		},
	)

	// First run: self-serve everything. The list seam fires once and the file
	// seam fires once per PR.
	res1 := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{})
	require.False(t, res1.IsError, "first run errored: %v", res1)
	require.Equal(t, 1, listHits, "first run lists once")
	require.Equal(t, 2, fileHits, "first run fetches files for both PRs")

	// Second run within the TTL on the SAME server: the list is re-served, but
	// every PR's files are resolved from the cache the first run hydrated — so
	// the file-fetch seam must NOT fire again.
	res2 := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{})
	require.False(t, res2.IsError, "second run errored: %v", res2)
	require.Equal(t, 2, fileHits, "second run must reuse cached PR files — the file-fetch seam stays at 2")

	// The cached result is equivalent: both runs rank the security-hub PR first.
	var out1, out2 struct {
		Ranked []struct {
			Number int `json:"number"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res1, &out1)
	unmarshalPRResult(t, res2, &out2)
	require.Len(t, out2.Ranked, 2)
	require.Equal(t, out1.Ranked[0].Number, out2.Ranked[0].Number,
		"the cache-served run ranks identically to the fetched run")
	require.Equal(t, 2, out2.Ranked[0].Number, "the higher-risk PR ranks first on the cached run")
}

// --- forge-unavailable: no token, no supplied data -------------------------

func TestPRTools_ForgeUnavailable(t *testing.T) {
	// Strip any token from the environment so forge.Available == false.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")

	srv, _ := prToolsTestServer(t)
	seamHit := false
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { seamHit = true; return nil, nil },
		func(context.Context, string, int) ([]string, error) { seamHit = true; return nil, nil },
	)

	cases := []struct {
		name string
		h    func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
		args map[string]any
	}{
		{"list_prs", srv.handleListPRs, map[string]any{}},
		{"get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(5)}},
		{"triage_prs", srv.handleTriagePRs, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callPRTool(t, srv, c.name, c.h, c.args)
			require.False(t, res.IsError, "must degrade, not error")
			var out struct {
				Error string `json:"error"`
				Hint  string `json:"hint"`
			}
			unmarshalPRResult(t, res, &out)
			require.Equal(t, "forge unavailable", out.Error)
			require.Contains(t, out.Hint, "GH_TOKEN")
		})
	}
	require.False(t, seamHit, "an unavailable forge must short-circuit before the seam")
}

// --- rate limited: the seam returns forge.ErrRateLimited -------------------

func TestPRTools_RateLimited(t *testing.T) {
	// A token must resolve so forge.Available == true and the handler
	// proceeds to call the seam, which simulates a rate-limit.
	t.Setenv("GH_TOKEN", "x-fake-token")

	srv, _ := prToolsTestServer(t)
	rlErr := fmt.Errorf("%w (retry after 42s)", forge.ErrRateLimited)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { return nil, rlErr },
		func(context.Context, string, int) ([]string, error) { return nil, rlErr },
	)

	cases := []struct {
		name string
		h    func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
		args map[string]any
	}{
		{"list_prs", srv.handleListPRs, map[string]any{}},
		{"get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(5)}},
		{"triage_prs", srv.handleTriagePRs, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callPRTool(t, srv, c.name, c.h, c.args)
			require.False(t, res.IsError, "must degrade, not error")
			var out struct {
				Error      string `json:"error"`
				RetryAfter int    `json:"retry_after_s"`
			}
			unmarshalPRResult(t, res, &out)
			require.Equal(t, "rate limited", out.Error)
			require.Equal(t, 42, out.RetryAfter, "retry_after_s parsed from the wrapped hint")
		})
	}
}

// --- gcx / toon / max_bytes round-trip -------------------------------------

func TestPRTools_GCXTOONBudget(t *testing.T) {
	srv, file := prToolsTestServer(t)
	filesJSON, _ := json.Marshal([]string{file})
	prsJSON, _ := json.Marshal([]forge.PR{{Number: 1, Title: "t", Author: "a", BaseRef: "main"}})

	// list_prs GCX.
	g1 := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON), "format": "gcx"})
	require.False(t, g1.IsError)
	require.Contains(t, g1.Content[0].(mcplib.TextContent).Text, "list_prs.summary")
	require.Contains(t, g1.Content[0].(mcplib.TextContent).Text, "list_prs.prs")

	// get_pr_impact GCX.
	g2 := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx"})
	require.False(t, g2.IsError)
	require.Contains(t, g2.Content[0].(mcplib.TextContent).Text, "get_pr_impact.summary")

	// triage_prs GCX.
	g3 := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{"prs": string(prsJSON), "files": "{\"1\":[\"" + file + "\"]}", "format": "gcx"})
	require.False(t, g3.IsError)
	require.Contains(t, g3.Content[0].(mcplib.TextContent).Text, "triage_prs.ranked")

	// TOON round-trip keeps a known key.
	tn := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON), "format": "toon"})
	require.False(t, tn.IsError)
	require.Contains(t, tn.Content[0].(mcplib.TextContent).Text, "total")

	// max_bytes budget honoured: the budgeted GCX response is trimmed
	// below the full one (row-tail trimming kicks in on opt-in).
	full := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx"})
	require.False(t, full.IsError)
	b := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx", "max_bytes": float64(160)})
	require.False(t, b.IsError)
	require.Less(t, len(b.Content[0].(mcplib.TextContent).Text), len(full.Content[0].(mcplib.TextContent).Text),
		"max_bytes must trim the response below the unbudgeted size")
}

// --- discoverability via tools_search --------------------------------------

func TestPRTools_DiscoverableViaToolsSearch(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotNil(t, srv.lazy)

	for _, name := range []string{"list_prs", "get_pr_impact", "triage_prs"} {
		hits := srv.lazy.Query("select:"+name, 1)
		require.Len(t, hits, 1, "%s must be discoverable by exact name", name)
		require.Equal(t, name, hits[0].tool.Name)
	}

	// All three are deferred (none in the hot eager set).
	for _, name := range []string{"list_prs", "get_pr_impact", "triage_prs"} {
		require.False(t, hotEagerTools[name], "%s must be a deferred tool", name)
	}
}

// --- retry-after parser ----------------------------------------------------

func TestRetryAfterSeconds(t *testing.T) {
	require.Equal(t, 30, retryAfterSeconds(fmt.Errorf("%w (retry after 30s)", forge.ErrRateLimited)))
	require.Equal(t, 90, retryAfterSeconds(fmt.Errorf("%w (retry after 1m30s)", forge.ErrRateLimited)))
	require.Equal(t, -1, retryAfterSeconds(fmt.Errorf("%w: secondary limit", forge.ErrRateLimited)))
	require.Equal(t, -1, retryAfterSeconds(nil))
}

// --- triage_prs: opt-in LLM re-rank ----------------------------------------

// withLLMRerank swaps the package-level llmRerank seam so the re-rank path
// runs with a fixed model response and no provider / network. usable=false
// simulates a disabled service; err simulates a failed Generate call.
func withLLMRerank(t *testing.T, text string, usable bool, err error) {
	t.Helper()
	orig := llmRerank
	llmRerank = func(context.Context, *svc.Service, string, int) (string, bool, error) {
		return text, usable, err
	}
	t.Cleanup(func() { llmRerank = orig })
}

func TestBuildTriagePrompt_Stable(t *testing.T) {
	rows := []map[string]any{
		{"number": 7, "title": "fix auth", "author": "alice", "risk": "CRITICAL", "score": float64(9.5)},
		{"number": 3, "title": "tweak docs", "author": "bob", "risk": "LOW", "score": float64(1.0)},
	}
	p1 := buildTriagePrompt(rows)
	p2 := buildTriagePrompt(rows)
	require.Equal(t, p1, p2, "buildTriagePrompt must be deterministic")
	require.Contains(t, p1, "PR 7 | risk=CRITICAL score=9.5 | fix auth | by alice")
	require.Contains(t, p1, "PR 3 | risk=LOW score=1.0 | tweak docs | by bob")
}

func TestParseTriageRanking_WellFormed(t *testing.T) {
	resp := "PR 3: docs change, low risk but quick to land\n" +
		"PR 7 - auth fix, review carefully\n" +
		"#11 some other thing\n"
	order, rationales := parseTriageRanking(resp)
	require.Equal(t, []int{3, 7, 11}, order)
	require.Equal(t, "docs change, low risk but quick to land", rationales[3])
	require.Equal(t, "auth fix, review carefully", rationales[7])
	require.Equal(t, "some other thing", rationales[11])
}

func TestParseTriageRanking_OrdinalAndBareForms(t *testing.T) {
	resp := "1. PR 7: highest blast radius\n" +
		"2) PR 3 - secondary\n" +
		"42: bare number with rationale\n"
	order, rationales := parseTriageRanking(resp)
	require.Equal(t, []int{7, 3, 42}, order, "leading ordinals must not be mistaken for PR numbers")
	require.Equal(t, "highest blast radius", rationales[7])
	require.Equal(t, "secondary", rationales[3])
	require.Equal(t, "bare number with rationale", rationales[42])
}

func TestParseTriageRanking_GarbageEmpty(t *testing.T) {
	for _, in := range []string{"", "no numbers here at all", "I cannot rank these.\nSorry!"} {
		order, rationales := parseTriageRanking(in)
		require.Empty(t, order, "garbage %q must yield no order", in)
		require.Empty(t, rationales)
	}
}

func TestParseTriageRanking_DropsRepeats(t *testing.T) {
	order, _ := parseTriageRanking("PR 5: a\nPR 5: duplicate\nPR 8: b\n")
	require.Equal(t, []int{5, 8}, order, "a repeated number keeps only its first occurrence")
}

// triageRows builds two supplied PRs (low-risk #1, security-hub #2) plus
// the files map so triage_prs runs with no forge call.
func triageRows(t *testing.T, srv *Server, hubFile string) (prsJSON, filesJSON string) {
	t.Helper()
	prs := []forge.PR{
		{Number: 1, Title: "low", Author: "a"},
		{Number: 2, Title: "high", Author: "b"},
	}
	pj, _ := json.Marshal(prs)
	fj, _ := json.Marshal(map[string][]string{"1": {"pkg/unrelated.go"}, "2": {hubFile}})
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) {
			t.Fatal("list seam hit")
			return nil, nil
		},
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)
	return string(pj), string(fj)
}

func TestTriagePRs_LLMRerankReordersAndStampsRationale(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	prsJSON, filesJSON := triageRows(t, srv, hubFile)

	// The deterministic order ranks the security hub (#2) first. The fake
	// model flips it: #1 first, with a per-PR rationale.
	withLLMRerank(t, "PR 1: ship the trivial one first\nPR 2: bigger blast radius, slower review\n", true, nil)

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":     prsJSON,
		"files":   filesJSON,
		"use_llm": true,
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total   int  `json:"total"`
		LLMUsed bool `json:"llm_used"`
		Ranked  []struct {
			Number    int    `json:"number"`
			Rationale string `json:"rationale"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.True(t, out.LLMUsed, "llm_used must be true when the model returns a usable ranking")
	require.Len(t, out.Ranked, 2)
	require.Equal(t, 1, out.Ranked[0].Number, "LLM order must override deterministic order")
	require.Equal(t, 2, out.Ranked[1].Number)
	require.Equal(t, "ship the trivial one first", out.Ranked[0].Rationale)
	require.Equal(t, "bigger blast radius, slower review", out.Ranked[1].Rationale)
}

func TestTriagePRs_LLMDisabledFallsBackDeterministic(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	prsJSON, filesJSON := triageRows(t, srv, hubFile)

	// Service reports unusable (no provider configured).
	withLLMRerank(t, "", false, nil)

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":     prsJSON,
		"files":   filesJSON,
		"use_llm": true,
	})
	require.False(t, res.IsError)

	var out struct {
		LLMUsed bool `json:"llm_used"`
		Ranked  []struct {
			Number    int    `json:"number"`
			Rationale string `json:"rationale"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.False(t, out.LLMUsed, "a disabled service must fall back with llm_used:false")
	require.Equal(t, 2, out.Ranked[0].Number, "deterministic order ranks the security hub first")
	require.Empty(t, out.Ranked[0].Rationale, "no rationale on the fallback path")
}

func TestTriagePRs_LLMGarbageFallsBackDeterministic(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	prsJSON, filesJSON := triageRows(t, srv, hubFile)

	// Provider usable, but the response has no parseable PR numbers.
	withLLMRerank(t, "I am unable to rank these pull requests.", true, nil)

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":     prsJSON,
		"files":   filesJSON,
		"use_llm": true,
	})
	require.False(t, res.IsError)

	var out struct {
		LLMUsed bool `json:"llm_used"`
		Ranked  []struct {
			Number int `json:"number"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.False(t, out.LLMUsed, "an unparseable response must fall back with llm_used:false")
	require.Equal(t, 2, out.Ranked[0].Number, "deterministic order is preserved on fallback")
}

func TestTriagePRs_UseLLMFalseStaysDeterministic(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	prsJSON, filesJSON := triageRows(t, srv, hubFile)

	// The seam must not be consulted at all when use_llm is omitted.
	withLLMRerank(t, "PR 1: should be ignored\nPR 2: ignored\n", true, nil)
	llmRerank = func(context.Context, *svc.Service, string, int) (string, bool, error) {
		t.Fatal("llmRerank seam hit with use_llm=false")
		return "", false, nil
	}

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":   prsJSON,
		"files": filesJSON,
	})
	require.False(t, res.IsError)

	var out struct {
		LLMUsed bool `json:"llm_used"`
		Ranked  []struct {
			Number int `json:"number"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.False(t, out.LLMUsed)
	require.Equal(t, 2, out.Ranked[0].Number)
}

func TestLLMRankPRs_PreservesDroppedTail(t *testing.T) {
	rows := []map[string]any{
		{"number": 1, "title": "a", "author": "x", "risk": "LOW", "score": float64(1)},
		{"number": 2, "title": "b", "author": "y", "risk": "HIGH", "score": float64(5)},
		{"number": 3, "title": "c", "author": "z", "risk": "MEDIUM", "score": float64(3)},
	}
	withLLMRerank(t, "PR 3: most urgent\nPR 1: then this\n", true, nil)
	out, used := llmRankPRs(context.Background(), nil, rows, 256)
	require.True(t, used)
	require.Len(t, out, 3, "no PR may be dropped from the queue")
	nums := []int{out[0]["number"].(int), out[1]["number"].(int), out[2]["number"].(int)}
	require.Equal(t, []int{3, 1, 2}, nums, "model order first, then the deterministic tail (#2)")
}

func TestLLMRankPRs_InventedNumberIgnored(t *testing.T) {
	rows := []map[string]any{
		{"number": 1, "title": "a", "author": "x", "risk": "LOW", "score": float64(1)},
		{"number": 2, "title": "b", "author": "y", "risk": "HIGH", "score": float64(5)},
	}
	// 99 does not exist; it must be skipped, and both real PRs preserved.
	withLLMRerank(t, "PR 99: hallucinated\nPR 2: real\nPR 1: real\n", true, nil)
	out, used := llmRankPRs(context.Background(), nil, rows, 256)
	require.True(t, used)
	require.Len(t, out, 2)
	require.Equal(t, []int{2, 1}, []int{out[0]["number"].(int), out[1]["number"].(int)})
}
