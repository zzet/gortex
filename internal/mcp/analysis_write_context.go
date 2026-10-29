package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func bindAnalysisWriteContext(writer graph.AnalysisGenerationStore, ctx context.Context) graph.AnalysisGenerationStore {
	// Bind only the concrete handle. An embedding wrapper may inherit the
	// method while overriding publication or writes; its promoted method would
	// return the raw Store and erase those semantics. Unknown/decorated writers
	// retain their existing owned fallback, without an interruptibility claim.
	if store, ok := writer.(*store_sqlite.Store); ok {
		return store.BindAnalysisWriteContext(ctx)
	}
	return writer // Legacy writer remains actually owned, without a bound claim.
}

// This is an admission/interruptible-SQL budget, not a whole rollback bound.
// The actual physical cleanup remains part of the caller's completion.
const analysisAbortCleanupBudget = 2 * time.Second

func abortAnalysisGeneration(ctx context.Context, writer graph.AnalysisGenerationStore, id int64) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analysisAbortCleanupBudget)
	defer cancel()
	return bindAnalysisWriteContext(writer, cleanupCtx).AbortAnalysisGeneration(id)
}
