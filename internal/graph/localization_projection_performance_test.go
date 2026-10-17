//go:build performance

package graph

import (
	"context"
	"fmt"
	"testing"
)

func TestPerformanceOverlaidViewFindNodesByNameBoundedAllocationDoesNotScaleWithCoveredFiles(t *testing.T) {
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
