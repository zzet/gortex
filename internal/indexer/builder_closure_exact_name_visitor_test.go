package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type closureExactVisitorBase struct {
	LayerBase
	nodes       []*graph.Node
	err         error
	visits      int
	legacyCalls int
	ignoreStop  bool
	entered     chan struct{}
	block       bool
}

func (b *closureExactVisitorBase) FindNodesByName(string) []*graph.Node {
	b.legacyCalls++
	panic("materialized exact-name lookup used")
}
func (b *closureExactVisitorBase) VisitNodesByNameContext(ctx context.Context, _ string, yield func(*graph.Node) bool) error {
	if b.entered != nil {
		close(b.entered)
	}
	if b.block {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, node := range b.nodes {
		b.visits++
		if !yield(node) && !b.ignoreStop {
			return ctx.Err()
		}
	}
	return b.err
}

func TestBaseDefinesNameVisitorPreservesFiltersAndStops(t *testing.T) {
	base := &closureExactVisitorBase{nodes: []*graph.Node{
		{ID: "empty-path", Kind: graph.KindFunction},
		{ID: "other-repo", Kind: graph.KindFunction, FilePath: "other.go", RepoPrefix: "other"},
		{ID: "file", Kind: graph.KindFile, FilePath: "repo/file.go", RepoPrefix: "repo"},
		{ID: "valid", Kind: graph.KindFunction, FilePath: "repo/valid.go", RepoPrefix: "repo"},
		{ID: "late", Kind: graph.KindFunction, FilePath: "repo/late.go", RepoPrefix: "repo"},
	}}
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: context.Background(), req: BuildRequest{RepoPrefix: "repo", Base: base}}
	if !walk.baseDefinesName("NewName") || walk.err != nil || base.visits != 4 || base.legacyCalls != 0 {
		t.Fatalf("defined=%v err=%v visits=%d legacy=%d", walk.baseDefinesName("NewName"), walk.err, base.visits, base.legacyCalls)
	}
}

func TestCollectPlaceholderReferrersVisitorPreservesFiltersAndStops(t *testing.T) {
	base := &closureExactVisitorBase{nodes: []*graph.Node{
		{ID: "empty-path", Kind: graph.KindFunction},
		{ID: "other-repo", Kind: graph.KindFunction, FilePath: "other.go", RepoPrefix: "other"},
		{ID: "file", Kind: graph.KindFile, FilePath: "repo/file.go", RepoPrefix: "repo"},
		{ID: "valid", Kind: graph.KindFunction, FilePath: "repo/valid.go", RepoPrefix: "repo"},
		{ID: "late", Kind: graph.KindFunction, FilePath: "repo/late.go", RepoPrefix: "repo"},
	}}
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: context.Background(), req: BuildRequest{RepoPrefix: "repo", Base: base}}
	out := map[string]struct{}{}
	walk.collectPlaceholderReferrers(map[string]struct{}{"NewName": {}}, out)
	if walk.err != nil || base.visits != 4 || base.legacyCalls != 0 || len(out) != 0 {
		t.Fatalf("collector err=%v visits=%d legacy=%d out=%#v", walk.err, base.visits, base.legacyCalls, out)
	}
}

func TestCollectPlaceholderReferrersVisitorCancelsInflight(t *testing.T) {
	base := &closureExactVisitorBase{entered: make(chan struct{}), block: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: ctx, req: BuildRequest{RepoPrefix: "repo", Base: base}}
	out := map[string]struct{}{}
	done := make(chan struct{})
	go func() {
		walk.collectPlaceholderReferrers(map[string]struct{}{"NewName": {}}, out)
		close(done)
	}()
	select {
	case <-base.entered:
	case <-time.After(time.Second):
		t.Fatal("collector did not enter visitor")
	}
	cancel()
	select {
	case <-done:
		if !errors.Is(walk.err, context.Canceled) || len(out) != 0 {
			t.Fatalf("collector cancellation = err %v, out %#v", walk.err, out)
		}
	case <-time.After(time.Second):
		t.Fatal("collector remained blocked after cancellation")
	}
}

func TestBaseDefinesNameVisitorErrorSuppressesPartialMatch(t *testing.T) {
	for _, lookupErr := range []error{context.Canceled, fmt.Errorf("wrapped: %w", context.DeadlineExceeded), errors.New("scan failed")} {
		base := &closureExactVisitorBase{
			nodes:      []*graph.Node{{ID: "partial", Kind: graph.KindFunction, FilePath: "partial.go"}},
			err:        lookupErr,
			ignoreStop: true, // Model a cursor close/backend error after a matching row.
		}
		walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: context.Background(), req: BuildRequest{RepoPrefix: "repo", Base: base}}
		if walk.baseDefinesName("NewName") || !errors.Is(walk.err, lookupErr) {
			t.Fatalf("lookup error %v = found true or recorded %v", lookupErr, walk.err)
		}
	}
}

type collectDependentsContextBase struct {
	LayerBase
	entered chan struct{}
	err     error
	facts   map[string][]graph.RefFact
}

func (b *collectDependentsContextBase) FindNodesByName(string) []*graph.Node { return nil }
func (b *collectDependentsContextBase) GetInEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	return nil
}
func (b *collectDependentsContextBase) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	return nil
}
func (b *collectDependentsContextBase) LoadRefFactsByFiles(string, []string) ([]graph.RefFact, error) {
	return nil, nil
}
func (b *collectDependentsContextBase) LoadRefFactsByTargets(string, []string) (map[string][]graph.RefFact, error) {
	panic("legacy reverse-fact lookup used")
}
func (b *collectDependentsContextBase) LoadRefFactsByTargetsContext(ctx context.Context, _ string, _ []string) (map[string][]graph.RefFact, error) {
	if b.entered != nil {
		close(b.entered)
		<-ctx.Done()
		return b.facts, ctx.Err()
	}
	return b.facts, b.err
}

func TestCollectDependentsUsesContextAndSuppressesCanceledPartialFacts(t *testing.T) {
	base := &collectDependentsContextBase{
		entered: make(chan struct{}),
		facts:   map[string][]graph.RefFact{"partial.go": {{FilePath: "partial.go"}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := map[string]struct{}{}
	done := make(chan error, 1)
	go func() {
		done <- (&SparseGenerationBuilder{Logger: zap.NewNop()}).collectDependents(ctx, BuildRequest{Base: base}, []string{"target"}, out)
	}()
	<-base.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("collectDependents error = %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("canceled partial facts escaped: %#v", out)
	}
}

func TestCollectDependentsPropagatesTypedBackendCancellationWithLiveParent(t *testing.T) {
	backendErr := fmt.Errorf("backend stopped: %w", context.DeadlineExceeded)
	base := &collectDependentsContextBase{
		err:   backendErr,
		facts: map[string][]graph.RefFact{"partial.go": {{FilePath: "partial.go"}}},
	}
	out := map[string]struct{}{}
	err := (&SparseGenerationBuilder{Logger: zap.NewNop()}).collectDependents(context.Background(), BuildRequest{Base: base}, []string{"target"}, out)
	if !errors.Is(err, context.DeadlineExceeded) || len(out) != 0 {
		t.Fatalf("typed cancellation = err %v, out %#v", err, out)
	}
}
