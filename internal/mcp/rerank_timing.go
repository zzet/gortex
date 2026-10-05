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
		"centrality_ms": ms(t.Centrality), "centrality_work": symbolCentralityTimingFields(t.CentralityWork), "scoring_ms": ms(t.Scoring),
		"prepare_calls": t.PrepareCalls, "scoring_calls": t.ScoringCalls, "candidates": t.Candidates,
		"missing_out": t.MissingOut, "missing_in": t.MissingIn, "out_rows": t.OutRows, "in_rows": t.InRows,
	}
}

func symbolCentralityTimingFields(t rerank.CentralityTiming) map[string]any {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return map[string]any{
		"total_ms": ms(t.Total), "reader_setup_ms": ms(t.ReaderSetup),
		"snapshot_ms": ms(t.Snapshot), "node_read_ms": ms(t.NodeRead), "edge_read_ms": ms(t.EdgeRead), "snapshot_compute_ms": ms(t.SnapshotCompute),
		"core_edge_read": symbolCoreEdgeTimingFields(t.CoreEdges),
		"node_presence_read": map[string]any{
			"pre_driver_acquisition_setup_ms": float64(t.NodePresence.PreDriver) / float64(time.Millisecond),
			"gate_ms":                         float64(t.NodePresence.Gate) / float64(time.Millisecond),
			"query_start_including_gate_ms":   float64(t.NodePresence.QueryStart) / float64(time.Millisecond),
			"drain_scan_ms":                   float64(t.NodePresence.Drain) / float64(time.Millisecond),
			"total_ms":                        float64(t.NodePresence.Total) / float64(time.Millisecond),
			"batches":                         t.NodePresence.Batches, "input_ids": t.NodePresence.InputIDs,
			"rows": t.NodePresence.Rows, "errors": t.NodePresence.Errors,
			"driver_entries": t.NodePresence.DriverEntries,
		},
		"scope_key_ms": ms(t.ScopeKey), "cache_lookup_including_wait_ms": ms(t.CacheLookup), "walk_topk_ms": ms(t.Walk),
		"cache_store_including_wait_ms": ms(t.CacheStore), "bookkeeping_ms": ms(t.Bookkeeping),
		"calls": t.Calls, "node_reads": t.NodeReads, "edge_reads": t.EdgeReads, "node_ids": t.NodeIDs, "edge_ids": t.EdgeIDs,
		"node_rows": t.NodeRows, "edge_rows": t.EdgeRows, "memo_calls": t.MemoCalls, "cache_hits": t.CacheHits, "cache_misses": t.CacheMisses,
		"cache_disabled": t.CacheDisabled, "cache_uncacheable": t.CacheUncacheable,
		"snapshot_nodes": t.SnapshotNodes, "snapshot_edges": t.SnapshotEdges, "truncated_calls": t.Truncated,
		"max_nodes": rerankBoundedMaxNodes, "max_edges": rerankBoundedMaxEdges,
	}
}

func symbolCoreEdgeTimingFields(t rerank.CoreEdgeTiming) map[string]any {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return map[string]any{
		"raw_read_ms": ms(t.RawRead), "filter_total_ms": ms(t.Filter),
		"filter_endpoint_lookup_ms": ms(t.EndpointLookup), "filter_remaining_ms": ms(t.Filter - t.EndpointLookup),
		"raw_reader_type": t.ReaderType, "raw_reads": t.RawReads,
		"raw_returned_rows": t.RawRows, "kept_rows": t.KeptRows,
		"endpoint_reads": t.EndpointReads, "endpoint_input_ids": t.EndpointIDs, "endpoint_distinct_ids_per_read_sum": t.EndpointDistinctIDs,
		"endpoint_checked":    t.CheckedClassifiers > 0 && t.LegacyClassifiers == 0,
		"checked_classifiers": t.CheckedClassifiers, "legacy_classifiers": t.LegacyClassifiers,
	}
}
