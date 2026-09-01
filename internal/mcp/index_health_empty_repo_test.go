package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// Issue #624: a repo whose ignore rules matched every path indexed zero
// files and reported success. index_health is the call an agent makes
// before trusting a whole-workspace answer, and nothing in it could see
// that state — every other signal describes what IS in the graph, so an
// index holding nothing looks exactly like a repo that has no code.

// healthServerWithIndexedRepos indexes each supplied directory through a
// real MultiIndexer and returns a server wired to the result.
func healthServerWithIndexedRepos(t *testing.T, named map[string]string) *Server {
	t.Helper()
	srv, _ := setupTestServer(t)

	entries := make([]config.RepoEntry, 0, len(named))
	for name, path := range named {
		entries = append(entries, config.RepoEntry{Path: path, Name: name})
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: entries}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(graph.New(), reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv.configManager = cm
	srv.multiIndexer = mi
	return srv
}

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
	return root
}

func TestIndexHealth_FlagsRepoThatIndexedNoFiles(t *testing.T) {
	healthy := writeRepo(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	// An ignore rule that swallows the whole tree — the shape the report
	// hit, reached here through a per-directory ignore file so the test
	// needs no daemon config.
	swallowed := writeRepo(t, map[string]string{
		"pkg/frame.go":  "package pkg\n\nfunc DataFrame() {}\n",
		".gortexignore": "*\n",
	})

	srv := healthServerWithIndexedRepos(t, map[string]string{
		"healthy":   healthy,
		"swallowed": swallowed,
	})

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	assert.Equal(t, false, payload["repos_hold_files_ok"])
	assert.Equal(t, []string{"swallowed"}, payload["empty_repos"],
		"only the repo that indexed nothing is reported")

	rec, _ := payload["recommendation"].(string)
	assert.Contains(t, rec, "swallowed")
	assert.Contains(t, rec, "ignore rule",
		"health must point at the cause an operator can act on")
}

func TestIndexHealth_EveryRepoHoldsFilesStaysGreen(t *testing.T) {
	srv := healthServerWithIndexedRepos(t, map[string]string{
		"a": writeRepo(t, map[string]string{"a.go": "package a\n"}),
		"b": writeRepo(t, map[string]string{"b.go": "package b\n"}),
	})

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	assert.Equal(t, true, payload["repos_hold_files_ok"])
	assert.NotContains(t, payload, "empty_repos")
}

// A server with no multi-repo registry (an embedded single-repo server)
// must report no empty repos rather than panicking.
func TestIndexHealth_NoMultiIndexerReportsNoEmptyRepos(t *testing.T) {
	srv, _ := setupTestServer(t)
	assert.Empty(t, srv.emptyTrackedRepos())

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)
	assert.Equal(t, true, payload["repos_hold_files_ok"])
}
