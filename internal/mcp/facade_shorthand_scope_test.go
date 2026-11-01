package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// shorthandNameReads records the name reads a shorthand resolution makes:
// the every-repository FindNodesByName, and the repositories the
// repository-scoped finder was asked about.
type shorthandNameReads struct {
	mu        sync.Mutex
	storeWide int
	repos     []string
}

func (r *shorthandNameReads) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storeWide, r.repos = 0, nil
}

// shorthandLegacyStore has no repository-scoped name finder, so the
// resolver's answer over it is the one every session got before.
type shorthandLegacyStore struct {
	graph.Store
	reads *shorthandNameReads
}

func (s shorthandLegacyStore) FindNodesByName(name string) []*graph.Node {
	s.reads.mu.Lock()
	s.reads.storeWide++
	s.reads.mu.Unlock()
	return s.Store.FindNodesByName(name)
}

// shorthandScopedStore is shorthandLegacyStore with the finder.
type shorthandScopedStore struct{ shorthandLegacyStore }

func (s shorthandScopedStore) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*graph.Node {
	s.reads.mu.Lock()
	s.reads.repos = append(s.reads.repos, repoPrefix)
	s.reads.mu.Unlock()
	return s.Store.(graph.RepoNamesNodeFinder).FindNodesByNamesInRepo(names, repoPrefix)
}

type shorthandScopeFixture struct {
	g             *graph.Graph
	mi            *indexer.MultiIndexer
	cm            *config.ConfigManager
	parent, repoA string
}

// newShorthandScopeFixture tracks repo-a (workspace alpha, Twin defined
// twice) and repo-b (workspace beta) side by side under one untracked
// parent, and adds forty repositories no session here can see, each holding
// every name the test resolves, plus one synthetic global external.
func newShorthandScopeFixture(t *testing.T) shorthandScopeFixture {
	t.Helper()
	write := func(path, body string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	repoB := filepath.Join(parent, "repo-b")
	write(filepath.Join(repoA, ".gortex.yaml"), "workspace: alpha\n")
	write(filepath.Join(repoA, "main.go"), "package main\n\nfunc Needle() {}\n\nfunc Twin() {}\n")
	write(filepath.Join(repoA, "sub", "sub.go"), "package sub\n\nfunc Twin() {}\n")
	write(filepath.Join(repoB, ".gortex.yaml"), "workspace: beta\n")
	write(filepath.Join(repoB, "main.go"), "package main\n\nfunc Needle() {}\n\nfunc Twin() {}\n")

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoA, Name: "repo-a"}, {Path: repoB, Name: "repo-b"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)
	for i := range 40 {
		repo := fmt.Sprintf("far-%02d", i)
		for _, name := range []string{"Needle", "Twin", "Shared"} {
			g.AddNode(&graph.Node{ID: repo + "/x.go::" + name, Name: name, Kind: graph.KindFunction,
				RepoPrefix: repo, WorkspaceID: repo, FilePath: repo + "/x.go"})
		}
	}
	g.AddNode(&graph.Node{ID: "ext::go:dep::Shared", Name: "Shared", Kind: graph.KindFunction, FilePath: "external::go:dep"})
	return shorthandScopeFixture{g: g, mi: mi, cm: cm, parent: parent, repoA: repoA}
}

// server reads through store. wrapped puts the contract-core wrapper the
// daemon installs in front of every request reader.
func (f shorthandScopeFixture) server(store graph.Store, wrapped bool) *Server {
	eng := query.NewEngine(store)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: f.mi, ConfigManager: f.cm})
	if wrapped {
		srv.contractAnalysisRuntime = &ContractAnalysisRuntime{}
	}
	return srv
}

// A bare-name shorthand from a bound session reads only the session's
// repositories (and the unowned bucket every scope sees) and answers exactly
// what the every-repository read answered: same canonical id, same ambiguous
// list. Unbound sessions keep the every-repository read.
func TestFacadeShorthandReadsOnlyTheSessionRepositories(t *testing.T) {
	f := newShorthandScopeFixture(t)
	cases := []struct {
		name, cwd string
		// candidates is how many definitions each name matches in scope:
		// one resolves to its id, more come back as the ambiguous list.
		candidates map[string]int
		// repos are the repositories read by name; nil means the
		// every-repository read.
		repos []string
	}{
		{"single repository", f.repoA, map[string]int{"Needle": 1, "Twin": 2, "Shared": 1, "Absent": 0}, []string{"", "repo-a"}},
		{"two-repository ceiling", f.parent, map[string]int{"Needle": 2, "Twin": 3, "Shared": 1, "Absent": 0}, []string{"", "repo-a", "repo-b"}},
		{"unbound", "", map[string]int{"Needle": 42, "Twin": 43, "Shared": 41, "Absent": 0}, nil},
	}
	for _, tc := range cases {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%v", tc.name, wrapped), func(t *testing.T) {
				ctx := context.Background()
				if tc.cwd != "" {
					ctx = sessionCtx("s-shorthand", tc.cwd)
				}
				legacy := f.server(shorthandLegacyStore{f.g, &shorthandNameReads{}}, wrapped)
				reads := &shorthandNameReads{}
				scoped := f.server(shorthandScopedStore{shorthandLegacyStore{f.g, reads}}, wrapped)
				for name, count := range tc.candidates {
					wantID, wantAmbiguous := legacy.resolveFacadeSymbolShorthand(ctx, name)
					switch count {
					case 0:
						require.Equal(t, name, wantID)
						require.Empty(t, wantAmbiguous)
					case 1:
						require.Truef(t, strings.HasSuffix(wantID, "::"+name), "%s resolves to its definition, got %q", name, wantID)
						require.Empty(t, wantAmbiguous)
					default:
						require.Equal(t, name, wantID)
						require.Lenf(t, wantAmbiguous, count, "%s", name)
					}

					reads.reset()
					gotID, gotAmbiguous := scoped.resolveFacadeSymbolShorthand(ctx, name)
					require.Equalf(t, wantID, gotID, "%s", name)
					require.Equalf(t, wantAmbiguous, gotAmbiguous, "%s", name)
					if tc.repos == nil {
						require.Equalf(t, 1, reads.storeWide, "%s: an unbound session keeps the every-repository read", name)
						require.Emptyf(t, reads.repos, "%s", name)
						continue
					}
					require.Zerof(t, reads.storeWide, "%s: a bound session read every repository's nodes of that name", name)
					require.Equalf(t, tc.repos, reads.repos, "%s", name)
				}
			})
		}
	}
}
