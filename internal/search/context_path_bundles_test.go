package search

import (
	"context"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type pathGraphSearcherStub struct {
	contextGraphSearcherStub
	paths, repos []string
	limit, calls int
}

func (s *pathGraphSearcherStub) SearchSymbolBundlesPathScopedContext(_ context.Context, _ string, repos, paths []string, limit int) ([]graph.SymbolBundle, error) {
	s.calls++
	s.paths = paths
	s.repos = repos
	s.limit = limit
	return s.bundles, s.contextErr
}

func TestPathBundleScopeSurvivesProductionBackendWrappers(t *testing.T) {
	source := &pathGraphSearcherStub{}
	chain := NewSwappable(NewHybrid(NewSymbolSearcherBackend(source), nil, nil))
	got, handled := chain.SearchSymbolBundlesPathScopedContext(context.Background(), "needle", []string{"repo"}, []string{"a%_[/目录"}, 17)
	if !handled || got == nil || len(got) != 0 || source.calls != 1 || source.limit != 17 || !reflect.DeepEqual(source.paths, []string{"a%_[/目录"}) || !reflect.DeepEqual(source.repos, []string{"repo"}) {
		t.Fatalf("result%v handled%v source%+v", got, handled, source)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, handled = chain.SearchSymbolBundlesPathScopedContext(ctx, "needle", nil, nil, 1)
	if !handled || got != nil || source.calls != 1 {
		t.Fatalf("cancel=%v/%v calls%d", got, handled, source.calls)
	}
	unsupported := NewSwappable(NewHybrid(NewSymbolSearcherBackend(&contextGraphSearcherStub{}), nil, nil))
	_, handled = unsupported.SearchSymbolBundlesPathScopedContext(context.Background(), "needle", nil, []string{"a"}, 1)
	if handled {
		t.Fatal("unsupported graph claimed path-scoped answer")
	}
}
