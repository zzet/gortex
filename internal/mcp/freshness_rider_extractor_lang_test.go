package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// riderIndexStateStore wraps the in-memory graph with a durable-index-state
// capability, so the rider has a stored extractor-version row to compare
// against. The in-memory graph implements neither half on its own.
type riderIndexStateStore struct {
	graph.Store
	st graph.RepoIndexState
}

func (s *riderIndexStateStore) GetRepoIndexState(string) (graph.RepoIndexState, bool, error) {
	return s.st, true, nil
}

func (s *riderIndexStateStore) SetRepoIndexState(st graph.RepoIndexState) error {
	s.st = st
	return nil
}

// A bumped extractor version is compared against the baseline every
// language implicitly carries, so ANY repository indexed by an older
// binary reports the bumped language behind — a Go-only repository
// included. Riding that on every file-scoped response would put a banner
// on every read in every repository, naming a language the reader has no
// file of and cannot act on. The advisory is narrowed to the language of
// the file being described.
func TestFreshnessRider_ExtractorStalenessIsScopedToTheFilesLanguage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "geom.jl"),
		[]byte("module Geom\nradius(c) = c.r\nend\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "repo-a"}}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewJuliaExtractor())
	g := graph.New()
	store := &riderIndexStateStore{Store: g}
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// A snapshot at the current global post-extraction policy epoch, with
	// every language it tracked except julia. Keeping the policy epoch
	// current isolates the per-language Julia bump; without it, the global
	// policy upgrade intentionally makes every real language stale.
	previous := map[string]int{"_post_extraction_policy": 2}
	for _, lang := range []string{"go", "python", "java", "ruby", "rust", "c", "cpp",
		"csharp", "php", "swift", "kotlin", "scala", "objc", "objcpp", "lua",
		"dart", "elixir", "bash", "javascript", "typescript"} {
		previous[lang] = 1
	}
	previous["c"], previous["cpp"], previous["csharp"] = 3, 2, 12
	previous["go"], previous["php"], previous["scala"], previous["swift"] = 3, 2, 2, 2
	encoded, err := json.Marshal(previous)
	require.NoError(t, err)
	store.st = graph.RepoIndexState{RepoPrefix: "repo-a", ExtractorVersions: string(encoded)}

	srv := NewServer(query.NewEngine(g), store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	read := func(path string) mcp.CallToolRequest {
		return freshReq(map[string]any{"path": path})
	}

	// julia IS stale against that snapshot...
	require.Contains(t, indexer.ExtractorVersionStaleLangs(string(encoded)), "julia")

	// ...but a fresh Go file must draw no rider at all. Before the
	// narrowing this returned {"extractor_stale_langs":["julia"], ...} on
	// every read, in every repository, permanently.
	require.Nil(t, srv.freshnessRiderFor(context.Background(), "read_file", read("repo-a/main.go")),
		"a fresh Go file must not be told that Julia's extractor moved")

	// The Julia file, whose language really is behind, still gets the
	// advisory and the actionable hint.
	jlRider := srv.freshnessRiderFor(context.Background(), "read_file", read("repo-a/geom.jl"))
	require.NotNil(t, jlRider, "a file of the stale language must still be flagged")
	require.Equal(t, []string{"julia"}, jlRider["extractor_stale_langs"])
	require.NotEmpty(t, jlRider["extractor_stale_hint"])
}
