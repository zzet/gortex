package mcp

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"
)

const indexHealthSnapshotTTL = 30 * time.Second

type indexHealthCache struct {
	mu         sync.RWMutex
	payload    map[string]any
	updatedAt  time.Time
	refreshing bool
}

func (s *Server) indexHealthSnapshot() (map[string]any, time.Time, bool) {
	s.indexHealth.mu.RLock()
	defer s.indexHealth.mu.RUnlock()
	if s.indexHealth.payload == nil {
		return nil, time.Time{}, s.indexHealth.refreshing
	}
	return s.indexHealth.payload, s.indexHealth.updatedAt, s.indexHealth.refreshing
}

func (s *Server) refreshIndexHealthInBackground() {
	s.indexHealth.mu.Lock()
	if s.indexHealth.refreshing {
		s.indexHealth.mu.Unlock()
		return
	}
	s.indexHealth.refreshing = true
	s.indexHealth.mu.Unlock()

	go func() {
		payload, err := s.buildIndexHealthBasePayloadCtx(context.Background())
		s.indexHealth.mu.Lock()
		defer s.indexHealth.mu.Unlock()
		s.indexHealth.refreshing = false
		if err == nil && payload != nil {
			s.indexHealth.payload = payload
			s.indexHealth.updatedAt = time.Now()
		}
	}()
}

func (s *Server) indexHealthNeedsRefresh(updatedAt time.Time) bool {
	return updatedAt.IsZero() || time.Since(updatedAt) >= indexHealthSnapshotTTL
}

// indexHealthScopeField is the payload key that names the corpus an
// index_health answer describes. It is deliberately the same key and the same
// vocabulary the view rider uses (view_request.go, viewRiderFields), because
// the statement is the same statement: part of this answer was produced by an
// engine that read the indexed corpus itself rather than the view the request
// selected.
const indexHealthScopeField = "base_scoped"

// withIndexHealthCorpusScope states, inside the payload, the corpus the
// payload describes.
//
// index_health answers out of a whole-daemon probe — s.graph.Stats() plus a
// NodesByKind(KindFile) walk of the corpus and the indexer's own mtime ledger
// (tools_enhancements.go, buildIndexHealthBasePayloadCtx) — so under a routed
// view the health score, the node count, the stale-file list and the path
// audit all describe the base corpus and not the checkout the caller selected.
// The TOOL already says so on its rider (view_capabilities.go,
// baseScopedEngineCapabilities). The gortex://index-health RESOURCE carries no
// rider at all (tool_deadline.go, requestScoped: "no rider to report a
// fallback on"), so the payload is the only channel it has, and until this it
// had none: a session bound to a worktree read corpus-wide numbers presented
// as its own.
//
// The predicate is view.routed(), byte for byte the one annotateBaseScoped
// uses, so the resource and the tool fire on exactly the same requests. Both
// surfaces are stamped, which keeps the PAYLOAD equality the resource's own
// description promises ("Same payload as the `index_health` tool") — the two
// results are not byte-equal, because a routed tool result also carries the
// view rider a resources/read has no channel for.
//
// Two properties this must not cost:
//
//   - the probe stays cheap. It reads the request's view and nothing else — no
//     NodeCount()/EdgeCount() (a whole-generation COUNT(*) on the SQL backend)
//     and no second Stats(). Re-scoping the counts themselves to the view is
//     the larger change this deliberately is not; what ships here is the
//     honest label on the corpus-scoped numbers.
//   - the shared cache stays unstamped. The snapshot in indexHealthCache is
//     built once for every session, so the stamp is applied at READ time on a
//     clone — a routed session can never leave its label on the payload an
//     unrouted one then reads.
func withIndexHealthCorpusScope(ctx context.Context, payload map[string]any) map[string]any {
	if payload == nil || !requestViewFromContext(ctx).routed() {
		return payload
	}
	scoped := maps.Clone(payload)
	scoped[indexHealthScopeField] = sortedCapabilityNames(baseScopedEngineCapabilities["index_health"])
	return scoped
}

func compactIndexHealth(payload map[string]any, updatedAt time.Time, refreshing bool) string {
	stale, _ := payload["stale_files"].([]string)
	parseFailureCount := indexHealthParseFailureCount(payload["parse_failures"])
	failedFiles, _ := payload["failed_file_count"].(int)
	unreadableFiles, _ := payload["unreadable_file_count"].(int)
	status := "ready"
	if refreshing {
		status = "refreshing"
	}
	if failedFiles > 0 || payload["status"] == "degraded" {
		status = "degraded"
	}
	return fmt.Sprintf("health=%v nodes=%v stale=%d failures=%d status=%s age=%ds failed_files=%d unreadable_files=%d\n",
		payload["health_score"], payload["node_count"], len(stale), parseFailureCount, status, int(time.Since(updatedAt).Seconds()), failedFiles, unreadableFiles)
}
