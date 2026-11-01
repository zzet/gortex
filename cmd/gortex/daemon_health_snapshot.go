package main

import (
	"runtime/metrics"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/sqliteconn"
)

var daemonHealthMetricNames = [...]string{
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/total:bytes",
	"/sched/goroutines:goroutines",
	"/gc/cycles/total:gc-cycles",
}

// daemonHealthRuntimeStats reads the non-STW runtime/metrics surface. Health
// ticks are telemetry, not an exact accounting boundary, so they must not run
// runtime.ReadMemStats while the indexing pipeline is allocation-heavy.
func daemonHealthRuntimeStats() daemon.RuntimeStats {
	samples := make([]metrics.Sample, len(daemonHealthMetricNames))
	for i, name := range daemonHealthMetricNames {
		samples[i].Name = name
	}
	metrics.Read(samples)
	values := make(map[string]uint64, len(samples))
	for _, sample := range samples {
		if sample.Value.Kind() == metrics.KindUint64 {
			values[sample.Name] = sample.Value.Uint64()
		}
	}
	heapObjects := values["/memory/classes/heap/objects:bytes"]
	heapUnused := values["/memory/classes/heap/unused:bytes"]
	heapFree := values["/memory/classes/heap/free:bytes"]
	heapReleased := values["/memory/classes/heap/released:bytes"]
	return withProcessGauges(daemon.RuntimeStats{
		Alloc:        heapObjects,
		Sys:          values["/memory/classes/total:bytes"],
		HeapInuse:    heapObjects + heapUnused,
		HeapIdle:     heapFree + heapReleased,
		HeapReleased: heapReleased,
		StackInuse:   values["/memory/classes/heap/stacks:bytes"],
		NumGC:        uint32(values["/gc/cycles/total:gc-cycles"]),
		NumGoroutine: int(values["/sched/goroutines:goroutines"]),
	})
}

// withProcessGauges adds what the Go runtime cannot see to one of its samples:
// the operating system's resident accounting and SQLite's page caches. Status
// and health read them only when someone asks, so a daemon nobody watches
// pays nothing for them.
func withProcessGauges(stats daemon.RuntimeStats) daemon.RuntimeStats {
	mem := platform.ReadProcessMemory()
	stats.RSS, stats.RSSPeak, stats.PSS, stats.PSSFile = mem.RSSBytes, mem.PeakRSSBytes, mem.PSSBytes, mem.PSSFileBytes
	stats.Footprint, stats.FootprintPeak = mem.FootprintBytes, mem.PeakFootprintBytes
	pageCache, pageCachePeak := sqliteconn.PageCacheBytes()
	stats.SQLitePageCache, stats.SQLitePageCachePeak = uint64(max(pageCache, 0)), uint64(max(pageCachePeak, 0))
	return stats
}

// buildDaemonHealthSnapshot is intentionally query-free. Health subscribers
// receive an immediate sample and another every 15 seconds; issuing Status or
// graph counts here used all four SQLite readers for corpus-wide scans during
// warmup. RepoMetadata is advisory but maintained without touching SQLite.
func buildDaemonHealthSnapshot(
	daemonStart time.Time,
	controller *realController,
	state *daemonState,
	srv *daemon.Server,
) map[string]any {
	now := time.Now()
	runtimeStats := daemonHealthRuntimeStats()
	out := map[string]any{
		"uptime_seconds":      int64(now.Sub(daemonStart).Seconds()),
		"ready":               false,
		"enriched":            false,
		"warmup_seconds":      int64(0),
		"tracked_repos":       0,
		"graph_nodes":         0,
		"graph_edges":         0,
		"alloc_bytes":         runtimeStats.Alloc,
		"sys_bytes":           runtimeStats.Sys,
		"heap_inuse_bytes":    runtimeStats.HeapInuse,
		"heap_idle_bytes":     runtimeStats.HeapIdle,
		"heap_released_bytes": runtimeStats.HeapReleased,
		"stack_inuse_bytes":   runtimeStats.StackInuse,
		"num_goroutine":       runtimeStats.NumGoroutine,
		"num_gc":              runtimeStats.NumGC,

		"sqlite_page_cache_bytes":      runtimeStats.SQLitePageCache,
		"sqlite_page_cache_peak_bytes": runtimeStats.SQLitePageCachePeak,
	}
	// A figure the platform has no reader for is left out, not reported as 0.
	for key, value := range map[string]uint64{
		"rss_bytes":            runtimeStats.RSS,
		"rss_peak_bytes":       runtimeStats.RSSPeak,
		"pss_bytes":            runtimeStats.PSS,
		"pss_file_bytes":       runtimeStats.PSSFile,
		"footprint_bytes":      runtimeStats.Footprint,
		"footprint_peak_bytes": runtimeStats.FootprintPeak,
	} {
		if value > 0 {
			out[key] = value
		}
	}
	if controller != nil {
		out["ready"] = controller.IsReady()
		out["enriched"] = controller.IsEnriched()
		out["warmup_seconds"] = controller.warmupSeconds.Load()
		out["coordinators"] = controller.lifecycle.LiveCoordinators("")
		if lspStatus := controller.collectLSPRouterStatus(); lspStatus != nil {
			out["lsp_alive"] = len(lspStatus.ActiveProviders)
			out["lsp_specs_registered"] = len(lspStatus.EnabledSpecs)
		}
	}
	if state != nil && state.multiIndexer != nil {
		metadata := state.multiIndexer.AllMetadata()
		nodes, edges := 0, 0
		for _, meta := range metadata {
			if meta == nil {
				continue
			}
			nodes += meta.NodeCount
			edges += meta.EdgeCount
		}
		out["tracked_repos"] = len(metadata)
		out["graph_nodes"] = nodes
		out["graph_edges"] = edges
	}
	activity := runtimeactivity.Current()
	out["activity_active"] = activity.Active
	out["activity_epoch"] = activity.Epoch
	out["activity_quiet_ms"] = activity.QuietFor(now).Milliseconds()
	if len(activity.ByKind) > 0 {
		out["activity_by_kind"] = activity.ByKind
	}
	if srv != nil {
		if sessions := srv.Sessions(); sessions != nil {
			out["sessions"] = sessions.Count()
		}
	}
	if state != nil && state.graph != nil {
		if integrity := daemon.GraphIntegrityStatusFor(state.graph); integrity != nil {
			out["graph_integrity"] = integrity
		}
		if reporter, ok := state.graph.(graph.DBStatReporter); ok {
			dbBytes, walBytes := reporter.DBStats()
			if dbBytes > 0 || walBytes > 0 {
				out["db_bytes"] = dbBytes
				out["wal_bytes"] = walBytes
				if dbBytes > 0 {
					out["wal_db_ratio"] = float64(walBytes) / float64(dbBytes)
				}
			}
		}
	}
	return out
}
