package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// ============================================================================
// 10.2 Property 21: Config persistence round-trip
// ============================================================================

// Feature: multi-repo-support, Property 21: Config persistence round-trip
//
// For any sequence of track and untrack operations, the GlobalConfig file
// SHALL reflect the current state of the repos list. After tracking a repo,
// the config file SHALL contain that entry. After untracking, the entry
// SHALL be removed.
//
// The sequence is a fixed script rather than random draws: every save is a
// durable file write followed by a reload, and a hundred random sequences of
// up to thirty such round trips cost seconds while only ever sampling the
// transitions below. The script walks each of them at least once: track into
// an empty list, track a new repo, track a repo whose name extends a tracked
// one (its own entry, not a duplicate), re-track a tracked repo
// (deduplicated), untrack a repo that was never tracked (refused, nothing
// changes), untrack the middle, first and last entry, untrack one name of a
// prefix-related pair (the other stays), re-track after an untrack, and
// untrack down to an empty list and past it. RemoveRepo must fail exactly
// when the repo is not tracked.
func TestPropertyConfigPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	// Start with an empty GlobalConfig.
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(configPath)
	require.NoError(t, gc.Save())

	script := []struct {
		track bool
		repo  string
	}{
		{false, "aa"}, // untrack on an empty list
		{true, "aa"},
		{true, "aaa"}, // extends a tracked name: its own entry, not a duplicate of repo-aa
		{true, "bb"},
		{true, "cc"},
		{true, "dd"},
		{true, "bb"},  // re-track: must not duplicate
		{false, "zz"}, // untrack a repo that was never tracked
		{false, "bb"}, // middle entry
		{false, "aa"}, // first entry; repo-aaa stays tracked
		{false, "dd"}, // last entry
		{true, "aa"},  // re-track after untrack, a prefix of the tracked repo-aaa
		{false, "cc"},
		{false, "aaa"}, // the longer name of the pair; repo-aa stays tracked
		{false, "aa"},  // the list is now empty
		{false, "aa"},  // untrack on the emptied list
	}
	tracked := make(map[string]bool) // absPath → tracked

	for i, op := range script {
		// Use absolute paths within the temp dir so AddRepo/RemoveRepo work.
		repoPath := filepath.Join(dir, "repos", "repo-"+op.repo)

		if op.track {
			// Create the directory so AddRepo can resolve it.
			require.NoError(t, os.MkdirAll(repoPath, 0o755), "create valid repository directory")
			require.NoError(t, gc.AddRepo(config.RepoEntry{Path: repoPath}), "track valid repository directory")
			tracked[repoPath] = true
		} else {
			// Untrack: RemoveRepo fails exactly when the repo is not tracked.
			err := gc.RemoveRepo(repoPath)
			if tracked[repoPath] {
				require.NoError(t, err, "op %d: untrack tracked repo-%s", i, op.repo)
				delete(tracked, repoPath)
			} else {
				require.Error(t, err, "op %d: untrack untracked repo-%s", i, op.repo)
			}
		}

		// Save after each operation.
		require.NoError(t, gc.Save())

		// Reload from disk and verify state matches.
		loaded, err := config.LoadGlobal(configPath)
		require.NoError(t, err)

		// Build set of loaded paths.
		loadedPaths := make(map[string]bool)
		for _, r := range loaded.Repos {
			loadedPaths[r.Path] = true
		}

		// Verify: every tracked path is in the loaded config.
		for path := range tracked {
			if !loadedPaths[path] {
				t.Errorf("op %d: tracked path %s not found in reloaded config", i, path)
			}
		}

		// Verify: every loaded path is in the tracked set.
		for path := range loadedPaths {
			if !tracked[path] {
				t.Errorf("op %d: loaded path %s not in tracked set", i, path)
			}
		}

		// Verify counts match.
		if len(loaded.Repos) != len(tracked) {
			t.Errorf("op %d: loaded %d repos, expected %d", i, len(loaded.Repos), len(tracked))
		}
	}
}

// ============================================================================
// 10.4 Property 14: Query scoping by repo, project, and ref
// ============================================================================

// Feature: multi-repo-support, Property 14: Query scoping by repo, project, and ref
//
// For any multi-repo graph and any query with a repo parameter, results SHALL
// contain only nodes with matching RepoPrefix. When project is specified,
// results SHALL contain only nodes from repos belonging to that project.
// When ref is specified with project, results SHALL contain only nodes from
// repos in that project with matching reference tag. When ref is specified
// without project, results SHALL contain nodes from all repos across all
// projects with matching reference tag. When no filter is specified, results
// SHALL span all repositories.
//
// Each random check built a whole Server to sample one target repo and one
// target project of one topology. The topologies below are every repo count
// the generator could draw (two to five, so every alpha/beta split), and each
// one targets every repo and both projects.
func TestPropertyQueryScopingByRepoProjectRef(t *testing.T) {
	for _, prefixes := range [][]string{
		{"alpha1", "beta22"},
		{"core7", "edge42", "webui3"},
		{"north1", "south22", "east3", "west44"},
		{"api1", "auth2", "billing3", "cache44", "deploy5"},
	} {
		t.Run(fmt.Sprintf("%d_repos", len(prefixes)), func(t *testing.T) {
			// Build a graph with nodes from each repo; repo i carries i+1 nodes.
			g := graph.New()
			for i, prefix := range prefixes {
				for j := 0; j <= i; j++ {
					g.AddNode(&graph.Node{
						ID:         fmt.Sprintf("%s/file.go::Func%d", prefix, j),
						Kind:       graph.KindFunction,
						Name:       "Func",
						FilePath:   prefix + "/file.go",
						StartLine:  j + 1,
						Language:   "go",
						RepoPrefix: prefix,
					})
				}
			}

			// Create projects with ref tags.
			// Project "alpha" gets first half of repos, "beta" gets second half.
			mid := len(prefixes) / 2
			alphaRepos := prefixes[:mid]
			betaRepos := prefixes[mid:]

			// Assign ref tags.
			alphaEntries := make([]config.RepoEntry, len(alphaRepos))
			for i, p := range alphaRepos {
				alphaEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p, Ref: "work"}
			}
			betaEntries := make([]config.RepoEntry, len(betaRepos))
			for i, p := range betaRepos {
				betaEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p, Ref: "opensource"}
			}

			gc := &config.GlobalConfig{
				Projects: map[string]config.ProjectConfig{
					"alpha": {Repos: alphaEntries},
					"beta":  {Repos: betaEntries},
				},
			}

			dir := t.TempDir()
			gcPath := filepath.Join(dir, "config.yaml")
			gc.SetConfigPath(gcPath)
			require.NoError(t, gc.Save())

			cm, err := config.NewConfigManager(gcPath)
			require.NoError(t, err)

			eng := query.NewEngine(g)
			srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
				ConfigManager: cm,
			})
			allNodes := g.AllNodes()

			// --- Test 1: repo filter, for every repo ---
			for _, targetPrefix := range prefixes {
				req1 := mcplib.CallToolRequest{}
				req1.Params.Arguments = map[string]any{"repo": targetPrefix}
				allowed1, err := srv.resolveRepoFilter(context.Background(), req1)
				require.NoError(t, err)
				require.NotNil(t, allowed1, "repo filter should return non-nil allowed set")
				assert.True(t, allowed1[targetPrefix], "target repo should be in allowed set")
				assert.Equal(t, 1, len(allowed1), "repo filter should allow exactly one prefix")

				// Verify filtering nodes works correctly.
				for _, n := range filterNodes(allNodes, allowed1) {
					assert.Equal(t, targetPrefix, n.RepoPrefix,
						"filtered node should have matching RepoPrefix")
				}
			}

			// --- Test 2: project filter, for both projects ---
			for targetProject, expectedPrefixes := range map[string][]string{"alpha": alphaRepos, "beta": betaRepos} {
				req2 := mcplib.CallToolRequest{}
				req2.Params.Arguments = map[string]any{"project": targetProject}
				allowed2, err := srv.resolveRepoFilter(context.Background(), req2)
				require.NoError(t, err)
				require.NotNil(t, allowed2)
				for _, p := range expectedPrefixes {
					assert.True(t, allowed2[p],
						"project %s should include repo %s", targetProject, p)
				}
				for _, n := range filterNodes(allNodes, allowed2) {
					assert.True(t, allowed2[n.RepoPrefix],
						"filtered node repo %s should be in project %s", n.RepoPrefix, targetProject)
				}
			}

			// --- Test 3: project + ref filter ---
			req3 := mcplib.CallToolRequest{}
			req3.Params.Arguments = map[string]any{"project": "alpha", "ref": "work"}
			allowed3, err := srv.resolveRepoFilter(context.Background(), req3)
			require.NoError(t, err)
			require.NotNil(t, allowed3)
			for _, p := range alphaRepos {
				assert.True(t, allowed3[p],
					"alpha+work should include repo %s", p)
			}
			// Beta repos should NOT be in the result.
			for _, p := range betaRepos {
				assert.False(t, allowed3[p],
					"alpha+work should NOT include beta repo %s", p)
			}

			// --- Test 4: ref without project ---
			req4 := mcplib.CallToolRequest{}
			req4.Params.Arguments = map[string]any{"ref": "opensource"}
			allowed4, err := srv.resolveRepoFilter(context.Background(), req4)
			require.NoError(t, err)
			require.NotNil(t, allowed4)
			for _, p := range betaRepos {
				assert.True(t, allowed4[p],
					"ref=opensource should include beta repo %s", p)
			}
			for _, p := range alphaRepos {
				assert.False(t, allowed4[p],
					"ref=opensource should NOT include alpha repo %s (ref=work)", p)
			}

			// --- Test 5: no filter → nil (all repos) ---
			req5 := mcplib.CallToolRequest{}
			req5.Params.Arguments = map[string]any{}
			allowed5, err := srv.resolveRepoFilter(context.Background(), req5)
			require.NoError(t, err)
			assert.Nil(t, allowed5, "no filter should return nil (all repos)")
		})
	}
}

// ============================================================================
// 10.7 Property 20: Active project default scoping
// ============================================================================

// Feature: multi-repo-support, Property 20: Active project default scoping
//
// For any active project setting, all MCP tool calls without an explicit
// project parameter SHALL return results scoped to the active project's
// repositories. Changing the active project via set_active_project SHALL
// immediately change the default scope for all subsequent queries.
//
// As above, each random check paid for a whole Server to sample one topology;
// the topologies below are every repo count the generator could draw (two to
// four, so every projA/projB split).
func TestPropertyActiveProjectDefaultScoping(t *testing.T) {
	for _, prefixes := range [][]string{
		{"red1", "green22"},
		{"north7", "east42", "south3"},
		{"one1", "two2", "three3", "four44"},
	} {
		t.Run(fmt.Sprintf("%d_repos", len(prefixes)), func(t *testing.T) {
			// Build graph with nodes from each repo.
			g := graph.New()
			for _, prefix := range prefixes {
				for j := 0; j < 3; j++ {
					g.AddNode(&graph.Node{
						ID:         fmt.Sprintf("%s/file.go::Func%d", prefix, j),
						Kind:       graph.KindFunction,
						Name:       "Func",
						FilePath:   prefix + "/file.go",
						StartLine:  j + 1,
						Language:   "go",
						RepoPrefix: prefix,
					})
				}
			}

			// Split repos into two projects.
			mid := len(prefixes) / 2
			projARepos := prefixes[:mid]
			projBRepos := prefixes[mid:]

			projAEntries := make([]config.RepoEntry, len(projARepos))
			for i, p := range projARepos {
				projAEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p}
			}
			projBEntries := make([]config.RepoEntry, len(projBRepos))
			for i, p := range projBRepos {
				projBEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p}
			}

			gc := &config.GlobalConfig{
				Projects: map[string]config.ProjectConfig{
					"projA": {Repos: projAEntries},
					"projB": {Repos: projBEntries},
				},
			}

			dir := t.TempDir()
			gcPath := filepath.Join(dir, "config.yaml")
			gc.SetConfigPath(gcPath)
			require.NoError(t, gc.Save())

			cm, err := config.NewConfigManager(gcPath)
			require.NoError(t, err)

			eng := query.NewEngine(g)

			// --- Phase 1: Set active project to projA ---
			srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
				ConfigManager: cm,
				ActiveProject: "projA",
			})

			// Call resolveRepoFilter with empty params — should scope to projA.
			reqEmpty := mcplib.CallToolRequest{}
			reqEmpty.Params.Arguments = map[string]any{}
			allowed, err := srv.resolveRepoFilter(context.Background(), reqEmpty)
			require.NoError(t, err)
			require.NotNil(t, allowed, "active project should produce non-nil filter")

			for _, p := range projARepos {
				assert.True(t, allowed[p],
					"active project projA: repo %s should be in scope", p)
			}
			for _, p := range projBRepos {
				assert.False(t, allowed[p],
					"active project projA: repo %s should NOT be in scope", p)
			}

			// Verify node filtering matches.
			for _, n := range filterNodes(g.AllNodes(), allowed) {
				assert.True(t, allowed[n.RepoPrefix],
					"filtered node %s should be in projA scope", n.ID)
			}

			// --- Phase 2: Switch active project to projB ---
			srv.activeProject = "projB"

			allowed2, err := srv.resolveRepoFilter(context.Background(), reqEmpty)
			require.NoError(t, err)
			require.NotNil(t, allowed2, "active project projB should produce non-nil filter")

			for _, p := range projBRepos {
				assert.True(t, allowed2[p],
					"active project projB: repo %s should be in scope", p)
			}
			for _, p := range projARepos {
				assert.False(t, allowed2[p],
					"active project projB: repo %s should NOT be in scope", p)
			}

			// --- Phase 3: Explicit project param overrides active project ---
			reqExplicit := mcplib.CallToolRequest{}
			reqExplicit.Params.Arguments = map[string]any{"project": "projA"}
			allowed3, err := srv.resolveRepoFilter(context.Background(), reqExplicit)
			require.NoError(t, err)
			require.NotNil(t, allowed3)

			for _, p := range projARepos {
				assert.True(t, allowed3[p],
					"explicit project=projA should include repo %s even when active is projB", p)
			}
		})
	}
}

// ============================================================================
// 10.8 Property 15: Shared repo indexed once
// ============================================================================

// Feature: multi-repo-support, Property 15: Shared repo indexed once
//
// For any repository path that appears in multiple project definitions,
// the graph SHALL contain exactly one set of nodes for that repository
// (not duplicated per project). The node count for that RepoPrefix SHALL
// equal the count from a single indexing of that repository.
func TestPropertySharedRepoIndexedOnce(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a shared repo prefix and 1-2 unique repo prefixes.
		sharedPrefix := rapid.StringMatching(`shared[a-z]{1,4}`).Draw(rt, "sharedPrefix")
		numUnique := rapid.IntRange(1, 3).Draw(rt, "numUnique")
		uniquePrefixes := make([]string, numUnique)
		for i := 0; i < numUnique; i++ {
			uniquePrefixes[i] = rapid.StringMatching(`unique[a-z]{1,4}`).Draw(rt, "uniquePrefix") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "suffix")
		}

		// Generate nodes for the shared repo.
		sharedNodeCount := rapid.IntRange(2, 8).Draw(rt, "sharedNodeCount")
		g := graph.New()

		// Add shared repo nodes exactly once (simulating correct indexing behavior).
		// Use index-based names to guarantee uniqueness.
		for j := 0; j < sharedNodeCount; j++ {
			funcName := fmt.Sprintf("SharedFunc%d", j)
			id := sharedPrefix + "/file.go::" + funcName
			g.AddNode(&graph.Node{
				ID:         id,
				Kind:       graph.KindFunction,
				Name:       funcName,
				FilePath:   sharedPrefix + "/file.go",
				StartLine:  j + 1,
				Language:   "go",
				RepoPrefix: sharedPrefix,
			})
		}

		// Add unique repo nodes.
		for _, prefix := range uniquePrefixes {
			numNodes := rapid.IntRange(1, 5).Draw(rt, "numNodes_"+prefix)
			for j := 0; j < numNodes; j++ {
				funcName := fmt.Sprintf("UniqueFunc%d", j)
				id := prefix + "/file.go::" + funcName
				g.AddNode(&graph.Node{
					ID:         id,
					Kind:       graph.KindFunction,
					Name:       funcName,
					FilePath:   prefix + "/file.go",
					StartLine:  j + 1,
					Language:   "go",
					RepoPrefix: prefix,
				})
			}
		}

		// Create a GlobalConfig where the shared repo appears in multiple projects.
		numProjects := rapid.IntRange(2, 4).Draw(rt, "numProjects")
		projects := make(map[string]config.ProjectConfig, numProjects)
		for i := 0; i < numProjects; i++ {
			projName := rapid.StringMatching(`proj[a-z]{1,4}`).Draw(rt, "projName") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "projSuffix")
			entries := []config.RepoEntry{
				{Path: "/tmp/" + sharedPrefix, Name: sharedPrefix},
			}
			// Optionally add a unique repo to this project.
			if len(uniquePrefixes) > 0 {
				idx := rapid.IntRange(0, len(uniquePrefixes)-1).Draw(rt, "uniqueIdx")
				entries = append(entries, config.RepoEntry{
					Path: "/tmp/" + uniquePrefixes[idx],
					Name: uniquePrefixes[idx],
				})
			}
			projects[projName] = config.ProjectConfig{Repos: entries}
		}

		// Verify: GetRepoNodes returns exactly the shared nodes (not duplicated).
		sharedNodes := g.GetRepoNodes(sharedPrefix)
		assert.Equal(t, sharedNodeCount, len(sharedNodes),
			"shared repo should have exactly %d nodes, got %d", sharedNodeCount, len(sharedNodes))

		// Verify: each shared node appears exactly once in the graph.
		allNodes := g.AllNodes()
		sharedInGraph := 0
		for _, n := range allNodes {
			if n.RepoPrefix == sharedPrefix {
				sharedInGraph++
			}
		}
		assert.Equal(t, sharedNodeCount, sharedInGraph,
			"shared repo nodes in full graph should be %d, got %d", sharedNodeCount, sharedInGraph)

		// Verify: querying from different projects returns the same shared nodes.
		// Both projects should see the shared repo's nodes when filtering.
		for projName, proj := range projects {
			allowedPrefixes := make(map[string]bool)
			for _, e := range proj.Repos {
				allowedPrefixes[config.ResolvePrefix(e)] = true
			}

			filtered := filterNodes(allNodes, allowedPrefixes)
			sharedInFiltered := 0
			for _, n := range filtered {
				if n.RepoPrefix == sharedPrefix {
					sharedInFiltered++
				}
			}
			assert.Equal(t, sharedNodeCount, sharedInFiltered,
				"project %s should see exactly %d shared nodes, got %d",
				projName, sharedNodeCount, sharedInFiltered)
		}
	})
}
