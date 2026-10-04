package mcp

import (
	"time"

	"github.com/zzet/gortex/internal/search/rerank"
)

func symbolRerankTimingFields(t rerank.Timing) map[string]any {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return map[string]any{
		"prepare_ms": ms(t.Prepare), "metrics_ms": ms(t.Metrics), "bookkeeping_ms": ms(t.Bookkeeping),
		"outgoing_ms": ms(t.Outgoing), "incoming_ms": ms(t.Incoming), "merge_fan_ms": ms(t.MergeFan),
		"centrality_ms": ms(t.Centrality), "scoring_ms": ms(t.Scoring),
		"prepare_calls": t.PrepareCalls, "scoring_calls": t.ScoringCalls, "candidates": t.Candidates,
		"missing_out": t.MissingOut, "missing_in": t.MissingIn, "out_rows": t.OutRows, "in_rows": t.InRows,
	}
}
