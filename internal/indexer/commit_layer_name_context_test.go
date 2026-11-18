package indexer

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type commitLayerNameContextKey struct{}

type commitLayerNameContextReader struct {
	LayerBase
	nodes         []*graph.Node
	findErr       error
	findEntered   chan struct{}
	blockFind     bool
	seen          any
	findCalls     int
	visitorCalls  int
	visitorVisits int
	legacyCalls   int
}

func (r *commitLayerNameContextReader) FindNodesByName(string) []*graph.Node {
	r.legacyCalls++
	panic("commitLayerBase used legacy exact-name lookup")
}

func (r *commitLayerNameContextReader) FindNodesByNameContext(ctx context.Context, _ string) ([]*graph.Node, error) {
	r.findCalls++
	r.seen = ctx.Value(commitLayerNameContextKey{})
	if r.findEntered != nil {
		close(r.findEntered)
	}
	if r.blockFind {
		<-ctx.Done()
		return r.nodes, ctx.Err()
	}
	return r.nodes, r.findErr
}

func (r *commitLayerNameContextReader) VisitNodesByNameContext(ctx context.Context, _ string, yield func(*graph.Node) bool) error {
	r.visitorCalls++
	r.seen = ctx.Value(commitLayerNameContextKey{})
	for _, node := range r.nodes {
		r.visitorVisits++
		if !yield(node) {
			return ctx.Err()
		}
	}
	return r.findErr
}

func TestCommitLayerBaseForwardsFullNameLookupCancellation(t *testing.T) {
	reader := &commitLayerNameContextReader{
		nodes:       []*graph.Node{{ID: "partial", Name: "Shared", FilePath: "partial.go", Kind: graph.KindFunction}},
		findEntered: make(chan struct{}),
		blockFind:   true,
	}
	base := commitLayerBase{Reader: reader}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), commitLayerNameContextKey{}, "dirty-build"))
	defer cancel()
	walk := &closureWalk{
		b:   &SparseGenerationBuilder{Logger: zap.NewNop()},
		ctx: ctx,
		req: BuildRequest{RepoPrefix: "repo", Base: base},
	}
	done := make(chan []*graph.Node, 1)
	go func() { done <- walk.findNodesByName("Shared") }()
	select {
	case <-reader.findEntered:
	case <-time.After(time.Second):
		t.Fatal("full exact-name lookup did not reach contextual reader")
	}
	cancel()
	select {
	case got := <-done:
		if got != nil || !errors.Is(walk.err, context.Canceled) {
			t.Fatalf("canceled full lookup = %#v, %v", got, walk.err)
		}
		if reader.seen != "dirty-build" || reader.findCalls != 1 || reader.legacyCalls != 0 {
			t.Fatalf("forwarding = value %v, contextual %d, legacy %d", reader.seen, reader.findCalls, reader.legacyCalls)
		}
	case <-time.After(time.Second):
		t.Fatal("full exact-name lookup remained blocked after cancellation")
	}
}

func TestCommitLayerBaseVisitorPreservesComposedMasksAndOrder(t *testing.T) {
	underlying := &commitLayerNameContextReader{nodes: []*graph.Node{
		{ID: "covered.go::old", Name: "Shared", FilePath: "covered.go", Kind: graph.KindFunction},
		{ID: "visible.go::base", Name: "Shared", FilePath: "visible.go", Kind: graph.KindFunction},
	}}
	layer := graph.NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.AddNode("overlay.go", &graph.Node{ID: "overlay.go::new", Name: "Shared", FilePath: "overlay.go", Kind: graph.KindFile})
	view := graph.NewOverlaidView(underlying, layer)
	base := commitLayerBase{Reader: view}
	ctx := context.WithValue(context.Background(), commitLayerNameContextKey{}, "composed")
	var got []string
	err := graph.VisitNodesByNameContext(ctx, base, "Shared", func(node *graph.Node) bool {
		got = append(got, node.ID)
		return node.ID != "visible.go::base"
	})
	if err != nil || !reflect.DeepEqual(got, []string{"overlay.go::new", "visible.go::base"}) {
		t.Fatalf("composed visitor = %v, %v", got, err)
	}
	if underlying.seen != "composed" || underlying.visitorCalls != 1 || underlying.visitorVisits != 2 || underlying.legacyCalls != 0 {
		t.Fatalf("underlying = value %v, calls %d, visits %d, legacy %d", underlying.seen, underlying.visitorCalls, underlying.visitorVisits, underlying.legacyCalls)
	}

	before := underlying.visitorCalls
	err = graph.VisitNodesByNameContext(ctx, base, "Shared", func(*graph.Node) bool { return false })
	if err != nil || underlying.visitorCalls != before {
		t.Fatalf("overlay early stop reached base: err %v, calls %d -> %d", err, before, underlying.visitorCalls)
	}
}

func TestCommitLayerBaseVisitorDrivesClosureEarlyStop(t *testing.T) {
	reader := &commitLayerNameContextReader{nodes: []*graph.Node{
		{ID: "valid", Name: "Shared", FilePath: "repo/valid.go", RepoPrefix: "repo", Kind: graph.KindFunction},
		{ID: "late", Name: "Shared", FilePath: "repo/late.go", RepoPrefix: "repo", Kind: graph.KindFunction},
	}}
	base := commitLayerBase{Reader: reader}
	ctx := context.WithValue(context.Background(), commitLayerNameContextKey{}, "closure")
	walk := &closureWalk{
		b:   &SparseGenerationBuilder{Logger: zap.NewNop()},
		ctx: ctx,
		req: BuildRequest{RepoPrefix: "repo", Base: base},
	}
	if !walk.baseDefinesName("Shared") || walk.err != nil {
		t.Fatalf("baseDefinesName = false, err %v", walk.err)
	}
	if reader.seen != "closure" || reader.visitorCalls != 1 || reader.visitorVisits != 1 || reader.legacyCalls != 0 {
		t.Fatalf("early stop = value %v, calls %d, visits %d, legacy %d", reader.seen, reader.visitorCalls, reader.visitorVisits, reader.legacyCalls)
	}
}

func TestCommitLayerBaseBatchVisitorPreservesComposedMasksAndOrder(t *testing.T) {
	underlying := &closureExactNamesBase{nodes: []*graph.Node{
		{ID: "covered.go::old", Name: "Shared", FilePath: "covered.go", Kind: graph.KindFunction},
		{ID: "visible.go::base", Name: "Other", FilePath: "visible.go", Kind: graph.KindFunction},
	}}
	layer := graph.NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.AddNode("overlay.go", &graph.Node{ID: "overlay.go::new", Name: "Shared", FilePath: "overlay.go", Kind: graph.KindFunction})
	view := graph.NewOverlaidView(underlying, layer)
	base := commitLayerBase{Reader: view}
	ctx := context.WithValue(context.Background(), commitLayerNameContextKey{}, "composed-batch")
	names := []string{"Shared", "Other"}
	collect := func(reader graph.Reader) []string {
		var got []string
		err := graph.VisitNodesByNamesContext(ctx, reader, names, func(node *graph.Node) bool {
			got = append(got, node.ID)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := collect(view)
	got := collect(base)
	if !reflect.DeepEqual(want, []string{"overlay.go::new", "visible.go::base"}) || !reflect.DeepEqual(got, want) ||
		underlying.batchCalls != 2 || underlying.singleCalls != 0 || underlying.seenContext != ctx {
		t.Fatalf("composed batch got=%v want=%v batch=%d single=%d", got, want, underlying.batchCalls, underlying.singleCalls)
	}
	before := underlying.batchCalls
	err := graph.VisitNodesByNamesContext(ctx, base, names, func(*graph.Node) bool { return false })
	if err != nil || underlying.batchCalls != before {
		t.Fatalf("overlay batch early stop reached base: err=%v calls=%d -> %d", err, before, underlying.batchCalls)
	}
}
