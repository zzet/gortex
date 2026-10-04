package indexer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type closureExactNamesBase struct {
	LayerBase
	nodes          []*graph.Node
	err            error
	batchCalls     int
	singleCalls    int
	visits         int
	names          []string
	seenContext    context.Context
	adjacencyCalls int
	keys           []string
	entered        chan struct{}
}

func (b *closureExactNamesBase) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	b.batchCalls++
	b.names = append([]string(nil), names...)
	b.seenContext = ctx
	if b.entered != nil {
		close(b.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	for _, n := range b.nodes {
		b.visits++
		if !yield(n) {
			return ctx.Err()
		}
	}
	return b.err
}
func (b *closureExactNamesBase) VisitNodesByNameContext(context.Context, string, func(*graph.Node) bool) error {
	b.singleCalls++
	return errors.New("unexpected single-name fallback")
}
func (b *closureExactNamesBase) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	b.adjacencyCalls++
	b.keys = append([]string(nil), ids...)
	return map[string][]*graph.Edge{ids[0]: {{FilePath: "caller.go"}}}
}
func (b *closureExactNamesBase) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	b.adjacencyCalls++
	return nil
}

func TestCollectPlaceholderReferrersBatchDefinitionPredicate(t *testing.T) {
	base := &closureExactNamesBase{nodes: []*graph.Node{
		nil,
		{Name: "EmptyPath", Kind: graph.KindFunction},
		{Name: "OtherRepo", Kind: graph.KindFunction, FilePath: "other.go", RepoPrefix: "other"},
		{Name: "FileKind", Kind: graph.KindFile, FilePath: "file.go"},
		{Name: "Defined", Kind: graph.KindFunction, FilePath: "repo/defined.go", RepoPrefix: "repo"},
		{Name: "LegacyRepo", Kind: graph.KindFunction, FilePath: "repo/legacy.go"},
	}}
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, req: BuildRequest{RepoPrefix: "repo", Base: commitLayerBase{Reader: base}}}
	defines := map[string]struct{}{"": {}, "EmptyPath": {}, "OtherRepo": {}, "FileKind": {}, "Defined": {}, "LegacyRepo": {}, "Missing": {}, "Extra1": {}, "Extra2": {}, "Extra3": {}}
	out := map[string]struct{}{}
	walk.collectPlaceholderReferrers(defines, out)
	var want []string
	for _, name := range []string{"EmptyPath", "OtherRepo", "FileKind", "Missing", "Extra1", "Extra2", "Extra3"} {
		want = append(want, closurePlaceholderIDs("repo", name)...)
	}
	sort.Strings(want)
	if walk.err != nil || base.batchCalls != 1 || base.singleCalls != 0 || !reflect.DeepEqual(base.keys, want) || len(out) != 1 {
		t.Fatalf("collector batch=%d single=%d keys=%v want=%v out=%v err=%v", base.batchCalls, base.singleCalls, base.keys, want, out, walk.err)
	}
}

func TestCollectPlaceholderReferrersBatchDiscardsPartialOnError(t *testing.T) {
	for _, lookupErr := range []error{errors.New("late close failure"), context.Canceled, context.DeadlineExceeded} {
		base := &closureExactNamesBase{nodes: []*graph.Node{{Name: "Defined", Kind: graph.KindFunction, FilePath: "defined.go"}}, err: lookupErr}
		walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, req: BuildRequest{Base: commitLayerBase{Reader: base}}}
		out := map[string]struct{}{}
		walk.collectPlaceholderReferrers(map[string]struct{}{"Defined": {}, "Missing": {}, "Extra1": {}, "Extra2": {}, "Extra3": {}, "Extra4": {}, "Extra5": {}, "Extra6": {}, "Extra7": {}}, out)
		if !errors.Is(walk.err, lookupErr) || base.batchCalls != 1 || base.singleCalls != 0 || base.adjacencyCalls != 0 || len(out) != 0 {
			t.Fatalf("partial batch err=%v adjacency=%d out=%v", walk.err, base.adjacencyCalls, out)
		}
	}
}

func TestCollectPlaceholderReferrersBatchCancelsInflight(t *testing.T) {
	base := &closureExactNamesBase{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: ctx, req: BuildRequest{Base: commitLayerBase{Reader: base}}}
	out := map[string]struct{}{}
	done := make(chan struct{})
	go func() {
		walk.collectPlaceholderReferrers(map[string]struct{}{"One": {}, "Two": {}, "Extra1": {}, "Extra2": {}, "Extra3": {}, "Extra4": {}, "Extra5": {}, "Extra6": {}, "Extra7": {}}, out)
		close(done)
	}()
	select {
	case <-base.entered:
	case <-time.After(time.Second):
		t.Fatal("batch collector did not reach the wrapped visitor")
	}
	cancel()
	select {
	case <-done:
		if !errors.Is(walk.err, context.Canceled) || base.batchCalls != 1 || base.singleCalls != 0 || base.seenContext != ctx || base.adjacencyCalls != 0 || len(out) != 0 {
			t.Fatalf("cancel err=%v adjacency=%d out=%v", walk.err, base.adjacencyCalls, out)
		}
	case <-time.After(time.Second):
		t.Fatal("batch collector remained blocked")
	}
}

func TestCollectPlaceholderReferrersCommitLayer7168NamesBatchAndEarlyStop(t *testing.T) {
	defines := make(map[string]struct{}, 7168)
	base := &closureExactNamesBase{}
	for i := 0; i < 7168; i++ {
		name := fmt.Sprintf("introduced%d", i)
		defines[name] = struct{}{}
		base.nodes = append(base.nodes, &graph.Node{Name: name, Kind: graph.KindFunction, FilePath: "repo/defined.go", RepoPrefix: "repo"})
	}
	base.nodes = append(base.nodes, &graph.Node{Name: "late", Kind: graph.KindFunction, FilePath: "repo/late.go", RepoPrefix: "repo"})
	ctx := context.WithValue(context.Background(), commitLayerNameContextKey{}, "introduced-names")
	walk := &closureWalk{
		b: &SparseGenerationBuilder{Logger: zap.NewNop()}, ctx: ctx,
		req: BuildRequest{RepoPrefix: "repo", Base: commitLayerBase{Reader: base}},
	}
	out := map[string]struct{}{}
	walk.collectPlaceholderReferrers(defines, out)
	if walk.err != nil || base.batchCalls != 1 || base.singleCalls != 0 || base.visits != len(defines) ||
		len(base.names) != len(defines) || base.seenContext != ctx || base.adjacencyCalls != 0 || len(out) != 0 {
		t.Fatalf("wrapped collector batch=%d single=%d visits=%d names=%d adjacency=%d out=%v err=%v",
			base.batchCalls, base.singleCalls, base.visits, len(base.names), base.adjacencyCalls, out, walk.err)
	}
}

func TestCollectPlaceholderReferrersTwoCommonNamesRetainsEarlyStops(t *testing.T) {
	base := &closureSmallNamesBase{closureExactVisitorBase: closureExactVisitorBase{nodes: []*graph.Node{{Name: "Run", Kind: graph.KindFunction, FilePath: "run.go"}, {Name: "Run", Kind: graph.KindFunction, FilePath: "late-run.go"}, {Name: "New", Kind: graph.KindFunction, FilePath: "new.go"}, {Name: "New", Kind: graph.KindFunction, FilePath: "late-new.go"}}}}
	walk := &closureWalk{b: &SparseGenerationBuilder{Logger: zap.NewNop()}, req: BuildRequest{Base: base}}
	out := map[string]struct{}{}
	walk.collectPlaceholderReferrers(map[string]struct{}{"Run": {}, "New": {}}, out)
	if walk.err != nil || base.visits != 2 || len(out) != 0 {
		t.Fatalf("small edit visits=%d out=%v err=%v", base.visits, out, walk.err)
	}
}

type closureSmallNamesBase struct{ closureExactVisitorBase }

func (b *closureSmallNamesBase) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	for _, n := range b.nodes {
		if n.Name != name {
			continue
		}
		b.visits++
		if !yield(n) {
			return ctx.Err()
		}
	}
	return nil
}
