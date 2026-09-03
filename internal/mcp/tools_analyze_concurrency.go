// N11 concurrency analyzers — language-agnostic detectors that ride
// the substrate edges existing extractors already emit
// (`EdgeSpawns`, `EdgeSends`, `EdgeRecvs`, `EdgeWrites`, `EdgeCalls`)
// so no new parser work is required for the languages the spawn /
// channel / write edges already cover (Go full; TS / Python / Kotlin
// / Rust / C# for spawns + writes).
//
// Two analyzers ship in this file:
//
//   - `race_writes`: KindField writes from inside a goroutine-
//     reachable function whose writer holds no detectable
//     synchronisation primitive (Lock / RLock / Mutex.Do / RWMutex
//     siblings). Conservative: a writer that calls any of the known
//     lock methods is treated as guarded for the whole function.
//     Missed-lock false positives (lock in caller, not in writer)
//     are tolerated — the report is reviewed, not enforced.
//
//   - `unclosed_channels`: channels that have at least one
//     `EdgeSends` but no `close()` call anywhere in the codebase.
//     Detection rides the existing `EdgeCalls` edges whose target
//     resolves (or fails to resolve) to a function named `close`.
//     A channel with a single sender + no close that nobody ranges
//     over is fine and not flagged; the analyzer reports
//     `risk` levels so callers see which closures actually matter.

package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// ---------------------------------------------------------------------------
// race_writes — field writes from goroutine-reachable functions without
// synchronisation. Walks the spawn-closure once, then field writes once.
// ---------------------------------------------------------------------------

// handleAnalyzeRaceWrites lists potentially-racy field writes —
// every `EdgeWrites` to a KindField whose source function is
// reachable (via `EdgeCalls`) from at least one goroutine-spawn
// target AND whose writer has no detectable lock acquisition in
// its outgoing call set.
//
// Args:
//
//   - `path_prefix`: scope to writes originating in a directory
//     subtree.
//   - `limit`: max rows (default 50).
func (s *Server) handleAnalyzeRaceWrites(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// One reader for the whole pass: an overlay-active request walks the
	// pushed buffers, everyone else the base graph.
	g := s.readerFor(ctx)
	goroutineReachable := s.buildGoroutineReachableSet(g)
	guarded := map[string]bool{}

	type raceRow struct {
		Field     string   `json:"field"`
		Writer    string   `json:"writer"`
		FilePath  string   `json:"file_path"`
		Line      int      `json:"line"`
		Reason    string   `json:"reason"`
		SpawnPath []string `json:"spawn_path,omitempty"`
	}
	var rows []raceRow

	for e := range edgesByKinds(g, graph.EdgeWrites) {
		if !goroutineReachable[e.From] {
			continue
		}
		target := g.GetNode(e.To)
		if target == nil || target.Kind != graph.KindField {
			continue
		}
		if !graphpath.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		// Lock-guard check is cached per writer because it walks the
		// writer's out-edges once and the writer is reused across
		// every field it touches.
		if _, cached := guarded[e.From]; !cached {
			guarded[e.From] = s.writerHoldsLock(g, e.From)
		}
		if guarded[e.From] {
			continue
		}
		rows = append(rows, raceRow{
			Field:    e.To,
			Writer:   e.From,
			FilePath: e.FilePath,
			Line:     e.Line,
			Reason:   "field write inside goroutine-reachable function with no detected lock guard",
		})
	}

	// Scope filter: when the request narrows below the global graph
	// (workspace-bound session or repo allow-set), keep only races
	// whose field AND writer node are both visible. total/truncated
	// below recompute over the kept rows. No-op for an unbound request.
	if s.scopeFiltersActive(ctx) {
		kept := make([]raceRow, 0, len(rows))
		for _, r := range rows {
			if s.analyzeNodeVisible(ctx, g.GetNode(r.Field)) &&
				s.analyzeNodeVisible(ctx, g.GetNode(r.Writer)) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Field != rows[j].Field {
			return rows[i].Field < rows[j].Field
		}
		if rows[i].Writer != rows[j].Writer {
			return rows[i].Writer < rows[j].Writer
		}
		return rows[i].Line < rows[j].Line
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]raceWriteItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, raceWriteItem{
				Field:    r.Field,
				Writer:   r.Writer,
				FilePath: r.FilePath,
				Line:     r.Line,
				Reason:   r.Reason,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("race_writes", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s -> %s  %s:%d\n", r.Writer, r.Field, r.FilePath, r.Line)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no unguarded field writes from goroutine-reachable code\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"race_writes": rows,
		"total":       len(rows),
		"truncated":   truncated,
	})
}

// buildGoroutineReachableSet returns the closure of every function /
// method reachable via `EdgeCalls` (transitively) from any node that
// is the target of an `EdgeSpawns` edge. Membership in this set is
// the precondition for a write to be considered "happens on another
// goroutine"; an unguarded write to a shared field by such a writer
// is a data race candidate. The walk runs on the caller's reader so an
// overlay-active request sees the pushed buffers' spawn/call edges.
func (s *Server) buildGoroutineReachableSet(g graph.Reader) map[string]bool {
	reach := map[string]bool{}
	var roots []string
	for e := range edgesByKinds(g, graph.EdgeSpawns) {
		if !reach[e.To] {
			reach[e.To] = true
			roots = append(roots, e.To)
		}
	}
	// BFS over out-edges of every spawned root; only EdgeCalls and
	// EdgeSpawns (a goroutine spawning another goroutine extends the
	// closure too) advance the frontier.
	frontier := roots
	for len(frontier) > 0 {
		var next []string
		for _, id := range frontier {
			for _, e := range g.GetOutEdges(id) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeSpawns {
					continue
				}
				if reach[e.To] {
					continue
				}
				reach[e.To] = true
				next = append(next, e.To)
			}
		}
		frontier = next
	}
	return reach
}

// writerHoldsLock returns true when writer's outgoing call set
// touches any of the known synchronisation primitives. Heuristic
// list — covers stdlib sync.Mutex / RWMutex, the common pattern of
// `withLock(func() { ... })`, JS `Mutex.acquire`, and Rust's
// `lock()`. False positives are preferable to false negatives here
// because the analyzer is review-grade, not enforcement-grade. The
// out-edge walk runs on the caller's reader.
func (s *Server) writerHoldsLock(g graph.Reader, writerID string) bool {
	for _, e := range g.GetOutEdges(writerID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if name := callTargetName(e); isLockMethodName(name) {
			return true
		}
		target := g.GetNode(e.To)
		if target != nil && isLockMethodName(target.Name) {
			return true
		}
	}
	return false
}

// callTargetName extracts the method/function name from an edge's
// `To` — works for both resolved targets (`pkg/m.go::Mutex.Lock`)
// and pre-resolution `unresolved::*.Lock` / `unresolved::Lock`
// shapes so the lock-guard check is robust against partial graphs.
func callTargetName(e *graph.Edge) string {
	target := e.To
	if i := strings.LastIndex(target, "*."); i >= 0 {
		return target[i+2:]
	}
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	if i := strings.LastIndex(target, "::"); i >= 0 {
		return target[i+2:]
	}
	return target
}

// isLockMethodName recognises the names that the standard Go / Rust /
// TS / Python / Java sync libraries use to acquire a lock. Matching
// is case-insensitive because some libraries shout-case their APIs.
func isLockMethodName(name string) bool {
	switch strings.ToLower(name) {
	case "lock", "rlock", "trylock", "trylockcontext",
		"acquire", "tryacquire",
		"withlock", "withreadlock", "withwritelock",
		"runwithlock",
		"do",           // sync.Once
		"synchronized": // Java-flavoured wrapper helpers
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// unclosed_channels — channels with at least one EdgeSends but
// no `close()` call site reachable from any sender or receiver.
// ---------------------------------------------------------------------------

// handleAnalyzeUnclosedChannels surfaces channel variables that are
// sent to but never closed. Two failure modes the heuristic catches:
//
//  1. Multiple goroutines write to a channel and rely on a `for v
//     := range ch` consumer to terminate when the channel closes —
//     if nobody closes the channel, the receiver hangs forever.
//  2. A worker pool publishes results via a channel and the
//     coordinator forgets to close after all workers return.
//
// Detection: a channel is "closed somewhere" if any of its senders
// or receivers is in the set of functions that contain a `close()`
// call. Conservative — a channel closed by an unrelated function
// counts as "not flagged" too, because the analyzer cannot prove
// arg identity from the static graph alone.
func (s *Server) handleAnalyzeUnclosedChannels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// One reader for the whole pass: an overlay-active request walks the
	// pushed buffers, everyone else the base graph.
	g := s.readerFor(ctx)

	// Pass 1: build the set of functions that contain a `close()`
	// call. Indirect proxy for "this function probably closes a
	// channel"; the channel arg isn't tracked so the membership test
	// is per-function, not per-channel.
	closesIn := map[string]bool{}
	for e := range edgesByKinds(g, graph.EdgeCalls) {
		if callTargetName(e) != "close" {
			continue
		}
		closesIn[e.From] = true
	}

	// Pass 2: aggregate channels by sender/receiver sets.
	type channelInfo struct {
		Channel   string
		Senders   map[string]bool
		Receivers map[string]bool
		Sends     int
		Recvs     int
		FilePath  string
		Line      int
	}
	byChannel := map[string]*channelInfo{}
	for e := range edgesByKinds(g, graph.EdgeSends, graph.EdgeRecvs) {
		info := byChannel[e.To]
		if info == nil {
			info = &channelInfo{
				Channel:   e.To,
				Senders:   map[string]bool{},
				Receivers: map[string]bool{},
				FilePath:  e.FilePath,
				Line:      e.Line,
			}
			byChannel[e.To] = info
		}
		if e.Kind == graph.EdgeSends {
			info.Sends++
			info.Senders[e.From] = true
		} else {
			info.Recvs++
			info.Receivers[e.From] = true
		}
	}

	type unclosedRow struct {
		Channel  string `json:"channel"`
		FilePath string `json:"file_path"`
		Line     int    `json:"line"`
		Sends    int    `json:"sends"`
		Recvs    int    `json:"recvs"`
		Senders  int    `json:"senders"`
		Risk     string `json:"risk"`
		Reason   string `json:"reason"`
	}
	var rows []unclosedRow

	for _, info := range byChannel {
		if !graphpath.HasPrefix(info.FilePath, pathPrefix) {
			continue
		}
		if info.Sends == 0 {
			// Pure receive sites — closing is the sender's job and
			// there are none. Skip.
			continue
		}
		anyCloser := false
		for fn := range info.Senders {
			if closesIn[fn] {
				anyCloser = true
				break
			}
		}
		if !anyCloser {
			for fn := range info.Receivers {
				if closesIn[fn] {
					anyCloser = true
					break
				}
			}
		}
		if anyCloser {
			continue
		}
		risk, reason := classifyUnclosed(len(info.Senders), info.Recvs)
		rows = append(rows, unclosedRow{
			Channel:  info.Channel,
			FilePath: info.FilePath,
			Line:     info.Line,
			Sends:    info.Sends,
			Recvs:    info.Recvs,
			Senders:  len(info.Senders),
			Risk:     risk,
			Reason:   reason,
		})
	}

	// Scope filter: keep only channels whose channel node is visible to
	// the current request. Senders/Sends/Recvs are counts (not node IDs)
	// so they need no pruning. total/truncated recompute below. No-op for
	// an unbound request.
	if s.scopeFiltersActive(ctx) {
		kept := make([]unclosedRow, 0, len(rows))
		for _, r := range rows {
			if s.analyzeNodeVisible(ctx, g.GetNode(r.Channel)) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}

	sort.Slice(rows, func(i, j int) bool {
		if rowRiskRank(rows[i].Risk) != rowRiskRank(rows[j].Risk) {
			return rowRiskRank(rows[i].Risk) > rowRiskRank(rows[j].Risk)
		}
		if rows[i].Sends != rows[j].Sends {
			return rows[i].Sends > rows[j].Sends
		}
		return rows[i].Channel < rows[j].Channel
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		// unclosedRow and unclosedChannelItem share the same field
		// shape (only tag annotations differ, which Go's struct
		// conversion ignores). A direct conversion avoids the
		// per-field copy boilerplate.
		items := make([]unclosedChannelItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, unclosedChannelItem(r))
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("unclosed_channels", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "[%s] sends=%d senders=%d recvs=%d  %s\n",
				r.Risk, r.Sends, r.Senders, r.Recvs, r.Channel)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no unclosed channels with senders\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"unclosed_channels": rows,
		"total":             len(rows),
		"truncated":         truncated,
	})
}

// classifyUnclosed returns a (risk, reason) pair for an unclosed
// channel. High-risk: multiple senders + receiver(s) — the canonical
// "fan-in / worker pool, nobody closes after the workers drain"
// shape that hangs `for v := range ch`. Medium: a single sender with
// receivers — the receiver may or may not range; without arg flow
// we can't tell. Low: senders without receivers, almost always a
// fire-and-forget signal.
func classifyUnclosed(senders, recvs int) (string, string) {
	switch {
	case senders >= 2 && recvs >= 1:
		return "high", "multiple senders with consumer(s) and no detected close — receivers will hang on range"
	case senders == 1 && recvs >= 1:
		return "medium", "sender with consumer(s) and no detected close — fine if receiver doesn't range"
	default:
		return "low", "sender(s) with no detected close and no consumer in this scope"
	}
}

func rowRiskRank(r string) int {
	switch r {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}
