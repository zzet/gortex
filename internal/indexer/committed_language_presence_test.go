package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type checkedLanguageStore struct {
	graph.Store
	called bool
}

func (s *checkedLanguageStore) RepoHasLanguageContext(ctx context.Context, repo, language string) (bool, error) {
	s.called = true
	return false, context.Canceled
}

func TestCommittedGoPresenceUsesCheckedReaderAndGraphFallback(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "go", Kind: graph.KindFunction, RepoPrefix: "go", FilePath: "file", Language: "go"},
		{ID: "module", Kind: graph.KindModule, RepoPrefix: "module", FilePath: "file", Language: "go"},
		{ID: "doc", Kind: graph.KindDoc, RepoPrefix: "content", FilePath: "file", Language: "go", Meta: map[string]any{"data_class": "content"}},
		{ID: "symbol", Kind: graph.KindDoc, RepoPrefix: "symbol", FilePath: "file", Language: "go", Meta: map[string]any{"data_class": "symbol"}},
	}, nil)
	for _, repo := range []string{"go", "module", "content", "symbol", "missing"} {
		want := repo == "go" || repo == "symbol"
		got, err := carriesGoFilesContext(nil, g, repo)
		if got != want || err != nil {
			t.Fatalf("fallback%q got%t/%v want%t", repo, got, err, want)
		}
	}
	checked := &checkedLanguageStore{Store: g}
	if got, err := carriesGoFilesContext(context.Background(), checked, "go"); got || !errors.Is(err, context.Canceled) || !checked.called {
		t.Fatalf("checked cancellation got%t/%v called%t", got, err, checked.called)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checked.called = false
	if got, err := carriesGoFilesContext(ctx, checked, "go"); got || !errors.Is(err, context.Canceled) || checked.called {
		t.Fatalf("pre-cancel got%t/%v called%t", got, err, checked.called)
	}
}
