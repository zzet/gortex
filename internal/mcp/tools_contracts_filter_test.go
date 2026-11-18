package mcp

import (
	"context"
	"encoding/json"
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
	"github.com/zzet/gortex/internal/search"
)

// TestHandleContracts_FiltersByProjectAndRef is the regression guard for
// the D fix: the contracts tool previously only accepted `repo`, so there
// was no way to ask "contracts for project X" or "contracts in repos
// tagged <ref>" without listing every repo by hand. After the fix, the
// handler shares `resolveRepoFilter` with the rest of the query tools
// and the three axes behave as you'd expect.
//
// Scenario: two repos ("alpha-svc", "beta-svc") grouped under project
// "backend"; a third repo ("orphan-svc") at the top level. `ref` tags
// pin alpha as "prod" and beta as "staging". The test exercises:
//
//   - project=backend: both alpha + beta, orphan excluded.
//   - project=backend + ref=prod: only alpha.
//   - ref=prod (no project): only alpha (ref narrows across all repos).
//   - repo=orphan-svc: just orphan, unaffected by project/ref axes.
func TestHandleContracts_FiltersByProjectAndRef(t *testing.T) {
	root := t.TempDir()
	alphaRepo := setupRepoWithHTTPProvider(t, root, "alpha-svc")
	betaRepo := setupRepoWithHTTPProvider(t, root, "beta-svc")
	orphanRepo := setupRepoWithHTTPConsumer(t, root, "orphan-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	// Top-level Repos selects the startup registration fixtures; projects
	// and refs are metadata used for filter
	// resolution only. Listing a repo in both places is valid — the
	// prefix resolves identically (it's derived from Name), so the
	// allow-set never double-counts.
	gc := &config.GlobalConfig{
		Projects: map[string]config.ProjectConfig{
			"backend": {
				Repos: []config.RepoEntry{
					{Path: alphaRepo, Name: "alpha-svc", Ref: "prod"},
					{Path: betaRepo, Name: "beta-svc", Ref: "staging"},
				},
			},
		},
		Repos: []config.RepoEntry{
			{Path: alphaRepo, Name: "alpha-svc"},
			{Path: betaRepo, Name: "beta-svc"},
			{Path: orphanRepo, Name: "orphan-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	preg := testRegistry()

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, preg, search.NewNull(), cm, zap.NewNop())
	registerMCPFixtureRepos(t, mi, cm)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	// Sanity: each of the three repos contributes at least one contract.
	alphaCount := contractsTotal(t, srv, map[string]any{"repo": "alpha-svc"})
	betaCount := contractsTotal(t, srv, map[string]any{"repo": "beta-svc"})
	orphanCount := contractsTotal(t, srv, map[string]any{"repo": "orphan-svc"})
	require.Greater(t, alphaCount, 0, "alpha must produce contracts")
	require.Greater(t, betaCount, 0, "beta must produce contracts")
	require.Greater(t, orphanCount, 0, "orphan must produce contracts")

	// project=backend should equal alpha + beta exactly, and exclude
	// orphan — if the filter leaks across projects, this assertion
	// fails by a margin large enough to diagnose on sight.
	projectTotal := contractsTotal(t, srv, map[string]any{"project": "backend"})
	assert.Equal(t, alphaCount+betaCount, projectTotal,
		"project=backend must return exactly alpha+beta contracts")
	contractRepos := contractsRepoSet(t, srv, map[string]any{"project": "backend"})
	assert.Contains(t, contractRepos, "alpha-svc")
	assert.Contains(t, contractRepos, "beta-svc")
	assert.NotContains(t, contractRepos, "orphan-svc",
		"project=backend must not leak orphan-svc contracts")

	// project + ref narrows further within the project.
	prodInBackend := contractsTotal(t, srv, map[string]any{"project": "backend", "ref": "prod"})
	assert.Equal(t, alphaCount, prodInBackend,
		"project=backend + ref=prod must return only alpha's contracts")

	// ref alone scopes across all repos (top-level + every project).
	// Here only alpha carries ref=prod, so the count must match alpha.
	prodAny := contractsTotal(t, srv, map[string]any{"ref": "prod"})
	assert.Equal(t, alphaCount, prodAny,
		"ref=prod without project must match repos tagged prod anywhere")

	// repo= still works alongside the new axes — it's the most specific
	// filter and short-circuits project/ref resolution.
	orphanByRepo := contractsTotal(t, srv, map[string]any{"repo": "orphan-svc"})
	assert.Equal(t, orphanCount, orphanByRepo,
		"repo filter must still pin-point a single repo regardless of projects")
}

// contractsRepoSet calls contracts list and returns the set of repo
// prefixes that appeared in the response. Used to prove which repos
// contributed to a filtered result, not just how many.
func contractsRepoSet(t *testing.T, srv *Server, args map[string]any) map[string]bool {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := srv.handleContracts(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)

	payload := extractTextFromContent(t, res.Content)
	var parsed struct {
		ByRepo map[string]any `json:"by_repo"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &parsed),
		"contracts payload is not JSON: %s", payload)
	out := make(map[string]bool, len(parsed.ByRepo))
	for repo := range parsed.ByRepo {
		out[repo] = true
	}
	return out
}
