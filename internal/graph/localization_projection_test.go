package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestFindNodesByNameBoundedCapsAndCancels(t *testing.T) {
	graph := New()
	for index := 0; index < 128; index++ {
		graph.AddNode(&Node{
			ID:       fmt.Sprintf("repo/file-%03d.go::handle", index),
			Name:     "handle",
			Kind:     KindFunction,
			FilePath: fmt.Sprintf("repo/file-%03d.go", index),
		})
	}

	page, err := graph.FindNodesByNameBounded(
		context.Background(), "handle",
		LocalizationNodeScope{Kinds: map[NodeKind]bool{KindFunction: true}},
		8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want threshold total 9, truncated, cap 8", page)
	}
	if !sort.SliceIsSorted(page.Nodes, func(i, j int) bool { return page.Nodes[i].ID < page.Nodes[j].ID }) {
		t.Fatalf("nodes are not deterministic: %#v", page.Nodes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := graph.FindNodesByNameBounded(ctx, "handle", LocalizationNodeScope{}, 8); err == nil || len(cancelled.Nodes) != 0 {
		t.Fatalf("cancelled lookup = %#v, %v; want empty error result", cancelled, err)
	}
}

func TestFindNodesByNameBoundedWeakSnapshotKeepsPageInvariants(t *testing.T) {
	graph := New()
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := 0; index < 2_000; index++ {
			graph.AddNode(&Node{
				ID:       fmt.Sprintf("repo/live-%04d.go::handle", index),
				Name:     "handle",
				Kind:     KindFunction,
				FilePath: fmt.Sprintf("repo/live-%04d.go", index),
			})
		}
	}()

	for attempt := 0; attempt < 100; attempt++ {
		page, err := graph.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 16)
		if err != nil {
			t.Fatalf("bounded lookup: %v", err)
		}
		if len(page.Nodes) > 16 || page.Total < len(page.Nodes) {
			t.Fatalf("weak snapshot violated bounds: %#v", page)
		}
		for index := 1; index < len(page.Nodes); index++ {
			if page.Nodes[index-1].ID >= page.Nodes[index].ID {
				t.Fatalf("weak snapshot is not sorted/deduplicated: %#v", page.Nodes)
			}
		}
	}
	writer.Wait()
}

func TestOverlaidViewFindNodesByNameBoundedRefillsAfterFileTombstone(t *testing.T) {
	base := New()
	for index := 0; index < 32; index++ {
		base.AddNode(&Node{
			ID: fmt.Sprintf("repo/a-hidden.go::handle:%02d", index), Name: "handle",
			Kind: KindFunction, FilePath: "repo/a-hidden.go",
		})
	}
	visible := &Node{
		ID: "repo/z-visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/z-visible.go",
	}
	base.AddNode(visible)

	layer := NewOverlayLayer()
	layer.MarkFile("repo/a-hidden.go", true)
	view := NewOverlaidView(base, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("page = %#v, want the later visible homonym after file tombstone", page)
	}
}

type recordingBoundedExactNameReader struct {
	Reader
	bounded BoundedExactNameReader
	limits  []int
}

func (reader *recordingBoundedExactNameReader) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	reader.limits = append(reader.limits, limit)
	return reader.bounded.FindNodesByNameBounded(ctx, name, scope, limit)
}

func TestOverlaidViewFindNodesByNameBoundedDoesNotInflateWholeFileShadows(t *testing.T) {
	base := New()
	visible := &Node{
		ID: "repo/visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/visible.go",
	}
	base.AddNode(visible)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}

	layer := NewOverlayLayer()
	layer.MarkFile("repo/generated.go", false)
	for index := 0; index < overlayExactNameInspectionLimit; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/generated.go::handle:%04d", index))
	}
	view := NewOverlaidView(recording, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 8 {
		t.Fatalf("base limits = %v, want unchanged request limit 8", recording.limits)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("page = %#v, want the one visible base homonym", page)
	}
}

func TestOverlaidViewFindNodesByNameBoundedFailsClosedAboveInspectionLimit(t *testing.T) {
	base := New()
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}
	layer := NewOverlayLayer()
	layer.MarkFile("repo/generated.go", false)
	for index := 0; index <= overlayExactNameInspectionLimit; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/generated.go::handle:%04d", index))
	}

	page, err := NewOverlaidView(recording, layer).FindNodesByNameBounded(
		context.Background(), "handle", LocalizationNodeScope{}, 8,
	)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "overlay exact-name entries" ||
		limitErr.Limit != overlayExactNameInspectionLimit {
		t.Fatalf("error = %v, want typed inspection limit error", err)
	}
	if len(page.Nodes) != 0 || page.Total != 0 || page.Truncated {
		t.Fatalf("overflow returned a partial page: %#v", page)
	}
	if len(recording.limits) != 0 {
		t.Fatalf("overflow called base with limits %v", recording.limits)
	}
}

func TestOverlaidViewFindNodesByNameBoundedAllowsDetachedShadowLimit(t *testing.T) {
	base := New()
	layer := NewOverlayLayer()
	for index := 0; index < overlayDetachedShadowLimit; index++ {
		stale := &Node{
			ID: fmt.Sprintf("repo/a-old-%03d.go::handle", index), Name: "handle",
			Kind: KindFunction, FilePath: fmt.Sprintf("repo/a-old-%03d.go", index),
		}
		base.AddNode(stale)
		layer.MarkRemoved(stale.Name, stale.ID)
	}
	visible := &Node{
		ID: "repo/z-visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/z-visible.go",
	}
	base.AddNode(visible)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}

	page, err := NewOverlaidView(recording, layer).FindNodesByNameBounded(
		context.Background(), "handle", LocalizationNodeScope{}, 8,
	)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 8+overlayDetachedShadowLimit {
		t.Fatalf("base limits = %v, want exact detached-shadow compensation", recording.limits)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("page = %#v, want only the visible base homonym", page)
	}
	if cap(page.Nodes) != len(page.Nodes) {
		t.Fatalf("page capacity = %d, want exact returned length %d", cap(page.Nodes), len(page.Nodes))
	}
}

func TestOverlaidViewFindNodesByNameBoundedFailsClosedAboveDetachedShadowLimit(t *testing.T) {
	base := New()
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}
	layer := NewOverlayLayer()
	for index := 0; index <= overlayDetachedShadowLimit; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/old-%03d.go::handle", index))
	}

	page, err := NewOverlaidView(recording, layer).FindNodesByNameBounded(
		context.Background(), "handle", LocalizationNodeScope{}, 8,
	)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "detached overlay shadow identities" ||
		limitErr.Limit != overlayDetachedShadowLimit {
		t.Fatalf("error = %v, want typed detached-shadow limit error", err)
	}
	if len(page.Nodes) != 0 || page.Total != 0 || page.Truncated {
		t.Fatalf("overflow returned a partial page: %#v", page)
	}
	if len(recording.limits) != 0 {
		t.Fatalf("overflow called base with limits %v", recording.limits)
	}
}

func TestOverlaidViewFindNodesByNameBoundedCancelsDuringOverlayInspection(t *testing.T) {
	base := New()
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}
	layer := NewOverlayLayer()
	layer.MarkFile("repo/generated.go", false)
	for index := 0; index < 512; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/generated.go::handle:%04d", index))
	}
	ctx := &cancelAfterLocalizationChecksContext{
		Context: context.Background(), remaining: 3, done: make(chan struct{}),
	}

	page, err := NewOverlaidView(recording, layer).FindNodesByNameBounded(
		ctx, "handle", LocalizationNodeScope{}, 8,
	)
	if err != context.Canceled {
		t.Fatalf("error = %v, want context cancellation during overlay inspection", err)
	}
	if len(page.Nodes) != 0 || page.Total != 0 || page.Truncated {
		t.Fatalf("cancelled inspection returned a partial page: %#v", page)
	}
	if len(recording.limits) != 0 {
		t.Fatalf("cancelled inspection called base with limits %v", recording.limits)
	}
}

func TestOverlaidViewFindNodesByNameBoundedComposesExclusionsWithoutMutatingCaller(t *testing.T) {
	base := New()
	for _, filePath := range []string{
		"repo/caller-hidden.go", "repo/inner-hidden.go", "repo/outer-hidden.go", "repo/visible.go",
	} {
		base.AddNode(&Node{
			ID: filePath + "::handle", Name: "handle", Kind: KindFunction, FilePath: filePath,
		})
	}
	innerLayer := NewOverlayLayer()
	innerLayer.MarkFile("repo/inner-hidden.go", true)
	outerLayer := NewOverlayLayer()
	outerLayer.MarkFile("repo/outer-hidden.go", true)
	view := NewOverlaidView(NewOverlaidView(base, innerLayer), outerLayer)
	callerFiles := map[string]bool{"repo/caller-hidden.go": true}

	page, err := view.FindNodesByNameBounded(
		context.Background(), "handle", LocalizationNodeScope{ExcludeFiles: callerFiles}, 8,
	)
	if err != nil {
		t.Fatalf("bounded nested-overlay lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].FilePath != "repo/visible.go" {
		t.Fatalf("page = %#v, want only the file outside all exclusion layers", page)
	}
	if len(callerFiles) != 1 || !callerFiles["repo/caller-hidden.go"] {
		t.Fatalf("caller exclusion map was mutated: %#v", callerFiles)
	}
	for _, filePath := range []string{"repo/inner-hidden.go", "repo/outer-hidden.go"} {
		if _, added := callerFiles[filePath]; added {
			t.Fatalf("overlay path %q leaked into caller exclusion map: %#v", filePath, callerFiles)
		}
	}
}

func TestOverlaidViewFindNodesByNameBoundedAllocationDoesNotScaleWithCoveredFiles(t *testing.T) {
	base := New()
	base.AddNode(&Node{
		ID: "repo/visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/visible.go",
	})
	layer := NewOverlayLayer()
	for index := 0; index < overlayExactNameInspectionLimit; index++ {
		layer.MarkFile(fmt.Sprintf("repo/covered-%04d.go", index), true)
	}
	view := NewOverlaidView(base, layer)

	result := testing.Benchmark(func(b *testing.B) {
		for iteration := 0; iteration < b.N; iteration++ {
			page, err := view.FindNodesByNameBounded(
				context.Background(), "handle", LocalizationNodeScope{}, 8,
			)
			if err != nil || len(page.Nodes) != 1 {
				b.Fatalf("bounded overlay lookup = %#v, %v", page, err)
			}
		}
	})
	if bytes := result.AllocedBytesPerOp(); bytes > 16<<10 {
		t.Fatalf("allocated %d bytes/op with many covered files, want request-bounded allocation", bytes)
	}
}

func TestOverlaidViewFindNodesByNameBoundedHonorsDetachedRemoval(t *testing.T) {
	base := New()
	stale := &Node{
		ID: "repo/old.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/old.go",
	}
	base.AddNode(stale)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}

	layer := NewOverlayLayer()
	// MarkRemoved is sufficient in the legacy overlay contract even when the
	// layer was assembled without MarkFile. The bounded path must keep parity.
	layer.MarkRemoved(stale.Name, stale.ID)
	view := NewOverlaidView(recording, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 9 {
		t.Fatalf("base limits = %v, want one detached-shadow refill slot", recording.limits)
	}
	if page.Total != 0 || len(page.Nodes) != 0 || page.Truncated {
		t.Fatalf("detached removal leaked stale base node: %#v", page)
	}
}

func TestFindFileNodesBoundedCapsScopeKindsAndCancels(t *testing.T) {
	graph := New()
	const filePath = "repo/dense.go"
	for index := 0; index < 32; index++ {
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/dense.go::fn-%03d", index), Name: fmt.Sprintf("fn%d", index),
			Kind: KindFunction, FilePath: filePath, RepoPrefix: "repo",
			WorkspaceID: "workspace", ProjectID: "project",
			Meta: map[string]any{"doc": "must not escape the summary projection"},
		})
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/dense.go::value-%03d", index), Name: fmt.Sprintf("value%d", index),
			Kind: KindVariable, FilePath: filePath, RepoPrefix: "repo",
			WorkspaceID: "workspace", ProjectID: "project",
		})
		graph.AddNode(&Node{
			ID: fmt.Sprintf("foreign/dense.go::fn-%03d", index), Name: fmt.Sprintf("foreign%d", index),
			Kind: KindFunction, FilePath: filePath, RepoPrefix: "foreign",
			WorkspaceID: "foreign", ProjectID: "foreign",
		})
	}

	page, err := graph.FindFileNodesBounded(
		context.Background(), filePath,
		LocalizationNodeScope{
			WorkspaceID: "workspace", ProjectID: "project",
			RepoAllow: map[string]bool{"repo": true},
			Kinds:     map[NodeKind]bool{KindFunction: true},
		},
		8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want threshold total 9, truncated, cap 8", page)
	}
	if !sort.SliceIsSorted(page.Nodes, func(i, j int) bool { return page.Nodes[i].ID < page.Nodes[j].ID }) {
		t.Fatalf("nodes are not deterministic: %#v", page.Nodes)
	}
	for _, node := range page.Nodes {
		if node.Kind != KindFunction || node.RepoPrefix != "repo" || node.WorkspaceID != "workspace" {
			t.Fatalf("scope or kind was applied after cap: %#v", node)
		}
		if node.Meta != nil {
			t.Fatalf("in-memory file summary retained metadata: %#v", node.Meta)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := graph.FindFileNodesBounded(ctx, filePath, LocalizationNodeScope{}, 8); err == nil || len(cancelled.Nodes) != 0 {
		t.Fatalf("cancelled file lookup = %#v, %v; want empty error result", cancelled, err)
	}
}

func TestFindFileNodesBoundedFiltersTestsBeforeCap(t *testing.T) {
	graph := New()
	const filePath = "repo/dense_test.go"
	for index := 0; index < 32; index++ {
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/dense_test.go::a-test-%03d", index), Name: "test",
			Kind: KindFunction, FilePath: filePath, Meta: map[string]any{"is_test": true},
		})
	}
	for index := 0; index < 10; index++ {
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/dense_test.go::z-prod-%03d", index), Name: "prod",
			Kind: KindFunction, FilePath: filePath,
		})
	}

	page, err := graph.FindFileNodesBounded(
		context.Background(), filePath, LocalizationNodeScope{ExcludeTests: true}, 8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want production sentinel after test rows", page)
	}
	for _, node := range page.Nodes {
		if node.Name != "prod" {
			t.Fatalf("test node consumed the production cap: %#v", node)
		}
	}
}

func TestFindFileNodesBoundedExcludesKindsBeforeCap(t *testing.T) {
	graph := New()
	const filePath = "repo/generated.go"
	for index := 0; index < 32; index++ {
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/generated.go::a-param-%03d", index), Name: "arg",
			Kind: KindParam, FilePath: filePath,
		})
	}
	for index := 0; index < 10; index++ {
		graph.AddNode(&Node{
			ID: fmt.Sprintf("repo/generated.go::z-function-%03d", index), Name: "function",
			Kind: KindFunction, FilePath: filePath,
		})
	}

	page, err := graph.FindFileNodesBounded(
		context.Background(), filePath,
		LocalizationNodeScope{ExcludeKinds: map[NodeKind]bool{KindParam: true}}, 8,
	)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want definition sentinel behind excluded params", page)
	}
	for _, node := range page.Nodes {
		if node.Kind != KindFunction {
			t.Fatalf("excluded kind consumed the cap: %#v", node)
		}
	}
}

func TestOverlaidViewFindFileNodesBoundedReplacesAndTombstones(t *testing.T) {
	base := New()
	const filePath = "repo/handler.go"
	base.AddNode(&Node{ID: filePath + "::old", Name: "old", Kind: KindFunction, FilePath: filePath})

	layer := NewOverlayLayer()
	layer.MarkFile(filePath, false)
	layer.AddNode(filePath, &Node{
		ID: filePath + "::replacement", Name: "replacement", Kind: KindFunction, FilePath: filePath,
		Meta: map[string]any{"doc": "must not escape the overlay summary projection"},
	})
	layer.AddNode(filePath, &Node{ID: filePath + "::ignored", Name: "ignored", Kind: KindVariable, FilePath: filePath})
	view := NewOverlaidView(base, layer)

	page, err := view.FindFileNodesBounded(
		context.Background(), filePath,
		LocalizationNodeScope{ExcludeKinds: map[NodeKind]bool{KindVariable: true}}, 8,
	)
	if err != nil {
		t.Fatalf("bounded overlay file lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].Name != "replacement" {
		t.Fatalf("overlay page = %#v, want replacement only", page)
	}
	if page.Nodes[0].Meta != nil {
		t.Fatalf("overlay file summary retained metadata: %#v", page.Nodes[0].Meta)
	}

	tombstone := NewOverlayLayer()
	tombstone.MarkFile(filePath, true)
	deleted, err := NewOverlaidView(base, tombstone).FindFileNodesBounded(
		context.Background(), filePath, LocalizationNodeScope{}, 8,
	)
	if err != nil {
		t.Fatalf("bounded tombstone lookup: %v", err)
	}
	if deleted.Total != 0 || deleted.Truncated || len(deleted.Nodes) != 0 {
		t.Fatalf("tombstone leaked base nodes: %#v", deleted)
	}
}

func TestFindFileNodesBoundedAllocatesOnlyRetainedSummaries(t *testing.T) {
	const (
		filePath  = "repo/generated.go"
		nodeCount = 2_048
		limit     = 8
	)
	memory := New()
	layer := NewOverlayLayer()
	layer.MarkFile(filePath, false)
	for index := 0; index < nodeCount; index++ {
		node := &Node{
			ID:   fmt.Sprintf("%s::declaration-%04d", filePath, index),
			Name: fmt.Sprintf("declaration%d", index), Kind: KindFunction,
			FilePath: filePath, Meta: map[string]any{"doc": "large retrieval payload"},
		}
		memory.AddNode(node)
		layer.AddNode(filePath, node)
	}

	readers := []struct {
		name   string
		reader BoundedFileNodeReader
	}{
		{name: "graph", reader: memory},
		{name: "overlay", reader: NewOverlaidView(nil, layer)},
	}
	for _, test := range readers {
		t.Run(test.name, func(t *testing.T) {
			var page BoundedNodeProjection
			allocations := testing.AllocsPerRun(3, func() {
				var err error
				page, err = test.reader.FindFileNodesBounded(
					context.Background(), filePath, LocalizationNodeScope{}, limit,
				)
				if err != nil {
					panic(err)
				}
			})
			if allocations > 64 {
				t.Fatalf("allocations = %.0f, want response-bounded summary allocation", allocations)
			}
			if len(page.Nodes) != limit || page.Total != limit+1 || !page.Truncated {
				t.Fatalf("page = %#v, want bounded saturated projection", page)
			}
			if cap(page.Nodes) != len(page.Nodes) {
				t.Fatalf("page capacity = %d, want exact returned length %d", cap(page.Nodes), len(page.Nodes))
			}
			for _, node := range page.Nodes[:cap(page.Nodes)] {
				if node.Meta != nil {
					t.Fatalf("retained or reslice-reachable summary hydrated metadata: %#v", node.Meta)
				}
			}
		})
	}
}

type cancelAfterLocalizationChecksContext struct {
	context.Context
	remaining int
	done      chan struct{}
}

func (ctx *cancelAfterLocalizationChecksContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *cancelAfterLocalizationChecksContext) Err() error {
	if ctx.remaining > 0 {
		ctx.remaining--
		if ctx.remaining > 0 {
			return nil
		}
	}
	select {
	case <-ctx.done:
	default:
		close(ctx.done)
	}
	return context.Canceled
}

func TestOverlaidViewFindFileNodesBoundedCancelsDuringLargeScan(t *testing.T) {
	const filePath = "repo/generated.go"
	layer := NewOverlayLayer()
	layer.MarkFile(filePath, false)
	for index := 0; index < 1_024; index++ {
		layer.AddNode(filePath, &Node{
			ID:   fmt.Sprintf("%s::declaration-%04d", filePath, index),
			Name: "declaration", Kind: KindFunction, FilePath: filePath,
		})
	}
	ctx := &cancelAfterLocalizationChecksContext{
		Context: context.Background(), remaining: 3, done: make(chan struct{}),
	}
	page, err := NewOverlaidView(nil, layer).FindFileNodesBounded(
		ctx, filePath, LocalizationNodeScope{}, 8,
	)
	if err != context.Canceled {
		t.Fatalf("error = %v, want context cancellation during scan", err)
	}
	if len(page.Nodes) != 0 || page.Total != 0 {
		t.Fatalf("cancelled scan returned partial page: %#v", page)
	}
}

func TestOverlayLayerAddNodeReplacesDuplicateIdentity(t *testing.T) {
	const (
		filePath = "repo/handler.go"
		nodeID   = filePath + "::handler"
	)
	layer := NewOverlayLayer()
	layer.MarkFile(filePath, false)
	layer.AddNode(filePath, &Node{
		ID: nodeID, Name: "old", QualName: "repo.Old", Kind: KindFunction, FilePath: filePath,
	})
	layer.AddNode(filePath, &Node{
		ID: nodeID, Name: "handler", QualName: "repo.Handler", Kind: KindFunction,
		FilePath: filePath, StartLine: 2,
	})
	final := &Node{
		ID: nodeID, Name: "handler", QualName: "repo.Handler", Kind: KindFunction,
		FilePath: filePath, StartLine: 3,
	}
	layer.AddNode(filePath, final)

	if nodes := layer.FileNodes(filePath); len(nodes) != 1 || nodes[0] != final {
		t.Fatalf("file nodes = %#v, want one final replacement", nodes)
	}
	if old := layer.NodesByName("old"); len(old) != 0 {
		t.Fatalf("old name index retained replacement: %#v", old)
	}
	if current := layer.NodesByName("handler"); len(current) != 1 || current[0] != final {
		t.Fatalf("name index = %#v, want final replacement", current)
	}
	view := NewOverlaidView(nil, layer)
	if view.GetNode(nodeID) != final || view.GetNodeByQualName("repo.Handler") != final {
		t.Fatal("identity or qualified-name index did not retain final replacement")
	}
	if stale := view.GetNodeByQualName("repo.Old"); stale != nil {
		t.Fatalf("stale qualified name retained: %#v", stale)
	}
	page, err := view.FindFileNodesBounded(context.Background(), filePath, LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatalf("bounded file lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].StartLine != 3 {
		t.Fatalf("duplicate identity inflated bounded projection: %#v", page)
	}
}
