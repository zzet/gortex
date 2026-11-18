package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

type workerSemanticInputProbe struct {
	rows  map[graph.SemanticBindingSite]string
	err   error
	calls int
}

func (p *workerSemanticInputProbe) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	p.calls++
	rows := make(map[graph.SemanticBindingSite]string)
	for _, site := range sites {
		if value := p.rows[site]; value != "" {
			rows[site] = value
		}
	}
	return rows, p.err
}

func (p *workerSemanticInputProbe) LookupTypeAtLine(string, int) (string, bool) {
	return "global-latest", true
}

func TestContractWorkerUsesSelectedSemanticInputsWithoutGlobalFallback(t *testing.T) {
	site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/handler.go", Line: 4, Name: "response"}
	missing := site
	missing.Name = "not-in-selected-view"
	global := &workerSemanticInputProbe{rows: map[graph.SemanticBindingSite]string{site: "global-latest", missing: "foreign-latest"}}
	previous := contracts.CurrentBindingResolver()
	contracts.SetBindingResolver(global)
	t.Cleanup(func() { contracts.SetBindingResolver(previous) })
	selected := &workerSemanticInputProbe{rows: map[graph.SemanticBindingSite]string{site: "SelectedResponse"}}
	idx := &Indexer{graph: graph.New(), contractAnalysisOnly: true, contractSemanticReader: selected}
	got := idx.readSemanticBindingTypes([]graph.SemanticBindingSite{site, missing})
	require.Equal(t, "SelectedResponse", got[site])
	require.NotContains(t, got, missing)
	require.Zero(t, global.calls, "historical worker cannot consult an actor-latest compiler")
	require.Nil(t, idx.contractInputError())
	legacy := &Indexer{graph: graph.New()}
	require.Equal(t, "global-latest", legacy.readSemanticBindingTypes([]graph.SemanticBindingSite{site})[site])
	require.Equal(t, 1, global.calls, "default behavior retains its existing resolver fallback")
}

func TestContractWorkerSelectedSemanticFailureCannotCertifyPartialFacts(t *testing.T) {
	failure := errors.New("selected semantic read failed")
	site := graph.SemanticBindingSite{FilePath: "repo/handler.go", Line: 4, Name: "response"}
	reader := &workerSemanticInputProbe{rows: map[graph.SemanticBindingSite]string{site: "partial"}, err: failure}
	idx := &Indexer{graph: graph.New(), contractAnalysisOnly: true, contractSemanticReader: reader}
	require.Empty(t, idx.readSemanticBindingTypes([]graph.SemanticBindingSite{site}))
	require.ErrorIs(t, idx.contractInputError(), failure)
}

func TestContractWorkerBodyFactsReadAcceptedSourceInsteadOfDisk(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "handler.go"), []byte("latest unaccepted bytes"), 0o600))
	accepted := []byte("accepted historical bytes")
	idx := &Indexer{graph: graph.New(), rootPath: root, repoPrefix: "repo", contractAnalysisOnly: true,
		contractAcceptedFileSource: func(path string) ([]byte, error) {
			require.Equal(t, "repo/handler.go", path)
			return accepted, nil
		}}
	cache := &bodyFactsCache{idx: idx}
	require.Equal(t, accepted, cache.readFile(&graph.Node{FilePath: "repo/handler.go"}))
	require.Nil(t, idx.contractInputError())
}

func TestContractWorkerMissingAcceptedSourceIsStickyIncomplete(t *testing.T) {
	idx := &Indexer{graph: graph.New(), contractAnalysisOnly: true}
	require.Nil(t, idx.contractFileSrc("repo/handler.go"))
	require.Error(t, idx.contractInputError())
	idx.clearContractInputError()
	failure := errors.New("accepted source moved")
	idx.contractAcceptedFileSource = func(string) ([]byte, error) { return []byte("partial"), failure }
	require.Nil(t, idx.contractFileSrc("repo/handler.go"))
	require.ErrorIs(t, idx.contractInputError(), failure)
	idx.contractAcceptedFileSource = func(string) ([]byte, error) { return []byte("later source"), nil }
	require.NotNil(t, idx.contractFileSrc("repo/handler.go"))
	require.ErrorIs(t, idx.contractInputError(), failure, "a later read cannot silently clear a failed snapshot")
}
