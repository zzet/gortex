package indexer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// Parsing is deliberately outside the timer. This measures the new foreground
// accepted-file collector, independently of cold indexing and background work.
func BenchmarkContractBoundaryReceipt(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("functions_%d", count), func(b *testing.B) {
			var src strings.Builder
			src.WriteString("package fixture\n")
			for i := 0; i < count; i++ {
				fmt.Fprintf(&src, "func Value%d() int { return %d }\n", i, i)
			}
			benchmarkBoundaryAcceptedParse(b, []byte(src.String()))
		})
	}
	b.Run("indexer_go", func(b *testing.B) {
		src, err := os.ReadFile("indexer.go")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkBoundaryAcceptedParse(b, src)
	})
}
func benchmarkBoundaryAcceptedParse(b *testing.B, src []byte) {
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(graph.New(), registry, config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	b.Cleanup(idx.Close)
	result, err := languages.NewGoExtractor().Extract("fixture/accepted.go", src)
	if err != nil {
		b.Fatal(err)
	}
	if result.Tree != nil {
		b.Cleanup(result.Tree.Release)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.collectContractBoundaryReceipt(context.Background(), "fixture/accepted.go", "go", src, result); err != nil {
			b.Fatal(err)
		}
	}
}
