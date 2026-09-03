package mcp

import (
	"context"
	"go.uber.org/zap"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/cochange"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

// registerCoChangeTool wires find_co_changing_symbols — the MCP
// surface over the git-history co-change graph.
func (s *Server) registerCoChangeTool() {
	s.addTool(
		mcp.NewTool("find_co_changing_symbols",
			mcp.WithDescription("Files (and their symbols) that change together with a target across git history — \"logical coupling\" the import graph cannot see: a handler and its test, a struct and the serializer that mirrors it, a schema and its migration are coupled even when neither imports the other. Mines `git log` for files co-occurring in a commit, weighted by a cosine association score over per-file commit counts. Pass either symbol_id or file_path. Returns co-changing files ranked by score with {file, score, count, symbols}. The same signal also materialises EdgeCoChange graph edges and feeds search ranking as the co_change rerank signal."),
			mcp.WithString("symbol_id", mcp.Description("Symbol node ID — resolved to its defining file. One of symbol_id / file_path is required.")),
			mcp.WithString("file_path", mcp.Description("File path to analyse directly. One of symbol_id / file_path is required.")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of co-changing files returned (default: 20).")),
			mcp.WithNumber("min_score", mcp.Description("Drop co-change relationships scoring below this threshold, 0..1 (default: 0).")),
			mcp.WithBoolean("refresh", mcp.Description("Start the legacy lazy git-history mine when the cache is cold (default true). Compact public analysis fixes this false; daemon prewarming remains independent.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleFindCoChangingSymbols,
	)
}

type coChangeRow struct {
	File    string   `json:"file"`
	Score   float64  `json:"score"`
	Count   int      `json:"count"`
	Symbols []string `json:"symbols,omitempty"`
}

func (s *Server) handleFindCoChangingSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolID := strings.TrimSpace(req.GetString("symbol_id", ""))
	filePath := strings.TrimSpace(req.GetString("file_path", ""))
	limit := max(req.GetInt("limit", 20), 1)
	minScore := 0.0
	if v, ok := req.GetArguments()["min_score"].(float64); ok {
		minScore = v
	}

	var targetFile string
	switch {
	case symbolID != "":
		n := s.readerFor(ctx).GetNode(symbolID)
		if n == nil {
			return mcp.NewToolResultError("symbol not found: " + symbolID), nil
		}
		targetFile = n.FilePath
	case filePath != "":
		targetFile = filePath
	default:
		return mcp.NewToolResultError("one of symbol_id or file_path is required"), nil
	}
	if targetFile == "" {
		return mcp.NewToolResultError("target symbol has no file path"), nil
	}

	if requestBoolDefault(req, "refresh", true) {
		s.ensureCoChange()
	}
	scores := s.coChangeScores(targetFile)
	counts := s.coChangeCounts(targetFile)

	// Two-phase build: first collect (file, score, count) tuples that
	// survive the minScore gate, then sort + truncate to the requested
	// limit, then batch-resolve the per-file symbol names. The Symbols
	// lookup is the only graph-touching work in this handler — pulling
	// it through one capability call instead of N GetFileNodes round-
	// trips is the entire disk-backend win.
	type pending struct {
		file  string
		score float64
		count int
	}
	pendings := make([]pending, 0, len(scores))
	for file, score := range scores {
		if score < minScore {
			continue
		}
		pendings = append(pendings, pending{file: file, score: score, count: counts[file]})
	}
	sort.Slice(pendings, func(i, j int) bool {
		if pendings[i].score != pendings[j].score {
			return pendings[i].score > pendings[j].score
		}
		return pendings[i].file < pendings[j].file
	})
	truncated := false
	if len(pendings) > limit {
		pendings = pendings[:limit]
		truncated = true
	}
	keepFiles := make([]string, 0, len(pendings))
	for _, p := range pendings {
		keepFiles = append(keepFiles, p.file)
	}
	symbolsByFile := s.symbolNamesByFiles(ctx, keepFiles)
	rows := make([]coChangeRow, 0, len(pendings))
	for _, p := range pendings {
		rows = append(rows, coChangeRow{
			File:    p.file,
			Score:   roundScore(p.score),
			Count:   p.count,
			Symbols: symbolsByFile[p.file],
		})
	}

	result := map[string]any{
		"target_file": targetFile,
		"co_changing": rows,
		"total":       len(rows),
		"truncated":   truncated,
	}
	if symbolID != "" {
		result["symbol_id"] = symbolID
	}
	// When the cache is empty AND the background mine has not finished
	// yet, surface an in-progress marker so the caller can distinguish
	// "this file has no co-change data" from "the daemon hasn't built
	// the data yet". The mine is fired at daemon-ready by RunAnalysis;
	// a fresh daemon on a disk backend takes tens of seconds before the cache is
	// populated.
	if len(rows) == 0 && !s.coChangeReady() {
		result["mining_in_progress"] = true
		result["note"] = "co-change graph is still being mined; retry shortly"
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// ensureCoChange triggers the co-change mine if it has not run yet
// and returns IMMEDIATELY — the mine itself runs asynchronously.
//
// Why async? On a disk backend with no pre-existing
// EdgeCoChange edges, mineCoChange spends 60+ seconds in
// cochange.AddEdges: an AllNodes full-table scan plus thousands of
// per-pair AddEdge round-trips. Wrapping that in sync.Once.Do
// turned every queued tool call into a blocked-for-60s caller. The
// async shape keeps the request path off the slow path.
//
// PrewarmCoChange (called from RunAnalysis at daemon-ready) is the only normal
// entrypoint. Read-only query handlers never call ensureCoChange: doing so
// would make a query causally responsible for durable EdgeCoChange writes.
//
// Returning immediately means the first user call may see an empty
// cache when the prewarm goroutine has not yet completed. That is
// the deliberate trade-off — the alternative is a 60s blocked tool
// call. The handler surfaces an `in_progress` flag when the cache is
// empty so callers know to retry rather than treating the file as
// genuinely uncoupled.
func (s *Server) ensureCoChange() {
	s.cochangeOnce.Do(func() {
		go s.mineCoChange()
	})
}

// PrewarmCoChange triggers the co-change mine in the background so a
// later find_co_changing_symbols / search rerank call sees a
// populated cache without blocking. Safe to call multiple times — the
// underlying sync.Once still gates the work to one execution.
//
// Returns immediately whether mining is in progress, completed, or
// freshly started.
func (s *Server) PrewarmCoChange() {
	go s.cochangeOnce.Do(s.mineCoChange)
}

// coChangeReady reports whether the mine has completed and the cache
// is populated. Used by the handler to set an `in_progress` flag
// when the cache is empty but mining is still running.
func (s *Server) coChangeReady() bool {
	s.cochangeMu.RLock()
	defer s.cochangeMu.RUnlock()
	return s.cochangeByFile != nil
}

// mineCoChange populates the co-change caches. It prefers EdgeCoChange
// edges already present in the graph (an enriched snapshot); only when
// none exist does it mine `git log`.
//
// The mine populates the in-memory caches AND persists the mined
// pairs as EdgeCoChange edges (cochange.AddEdges) so a subsequent daemon
// start takes the coChangeFromEdges fast path instead of re-mining
// `git log` (the 5-15s restart cost).
//
// The earlier version deliberately skipped the persist to avoid the
// analyze[clusters] partition cache (keyed on NodeCount/EdgeCount/
// EdgeIdentityRevisions) being invalidated by edge-count drift. That
// concern was about CONTINUOUS drift; here the persist is bounded —
// mineCoChange runs once per process (sync.Once) and the fast path skips
// the mine once edges exist — so the edge count (and the clusters token)
// moves at most ONCE per graph, triggering a single recompute rather
// than per-restart thrash. Co-change edges are partition-irrelevant
// (edgeWeight 0; both endpoints are KindFile nodes, filtered out of
// community detection), so that one recompute yields the same partition.
//
// Reads are unaffected: find_co_changing_symbols and the search rerank's
// CoChangeOf hook both read the in-memory cache. The CLI cochange.EnrichGraph
// path already persisted via AddEdges; this aligns the lazy daemon path
// with it. Refreshing stale co-change after a HEAD move is still a manual
// `gortex enrich cochange` (or a cold reindex) — the lazy path does not
// auto-re-mine once edges exist.
func (s *Server) mineCoChange() {
	runtimeactivity.Begin("cochange")
	defer func() {
		runtimeactivity.End("cochange")
		scheduleOSMemoryReleaseAfterBurst(s.logger, "cochange")
	}()

	scores := map[string]map[string]float64{}
	counts := map[string]map[string]int{}

	if s.coChangeFromEdges(scores, counts) {
		s.storeCoChange(scores, counts)
		// The co-change graph COULD be refreshed by re-mining git log,
		// but was NOT: persisted EdgeCoChange edges already exist, so the
		// lazy path serves them as-is. If history advanced since the last
		// mine these counts are stale until an explicit refresh. Surface
		// that rather than silently serving possibly-stale data.
		if s.logger != nil {
			s.logger.Info("co-change served from persisted edges; not re-mined (could be updated, but was not) — run `gortex enrich cochange` to refresh after history changes",
				zap.Int("file_relations", len(scores)))
		}
		return
	}

	for prefix, root := range s.collectRepoRoots("") {
		res := cochange.Mine(context.Background(), root, cochange.Options{})
		if len(res.Pairs) == 0 {
			continue
		}
		for _, p := range res.Pairs {
			fa, fb := p.FileA, p.FileB
			if prefix != "" {
				fa = prefix + "/" + fa
				fb = prefix + "/" + fb
			}
			addCoChangeLink(scores, counts, fa, fb, p.Score, p.Count)
			addCoChangeLink(scores, counts, fb, fa, p.Score, p.Count)
		}
		// Persist the mined pairs as EdgeCoChange edges so a later daemon
		// start takes the coChangeFromEdges fast path instead of re-mining
		// git log (the 5-15s restart cost). Bounded: mineCoChange runs once
		// per process (sync.Once) and the fast path above skips the mine
		// once edges exist, so this persist (and its one clusters-cache
		// token bump) happens at most once per graph, not per restart.
		cochange.AddEdges(s.graph, res.Pairs, prefix)
	}
	s.storeCoChange(scores, counts)
}

// coChangeFromEdges rebuilds the path-keyed caches from EdgeCoChange
// edges already in the graph. Returns true when at least one edge was
// found — the signal that an enriched snapshot is loaded and no fresh
// git mine is needed.
//
// Reads the base store on purpose: the caches it fills are process-wide
// and outlive the request that triggered the mine, so they must never be
// built from one caller's buffers.
//
// EdgesByKind streams only the CoChange edges; the endpoint nodes are
// fetched in one batched GetNodesByIDs call instead of two GetNode
// round-trips per edge. On disk backends that drops the
// whole-graph AllEdges materialisation plus the per-edge
// GetNode trips that loaded the file paths.
func (s *Server) coChangeFromEdges(scores map[string]map[string]float64, counts map[string]map[string]int) bool {
	// First pass: collect CoChange edges + the set of node IDs they
	// reference. Both can stream from EdgesByKind in one
	// round-trip on disk backends.
	type ccEdge struct {
		from, to string
		score    float64
		count    int
	}
	var edges []ccEdge
	idSet := make(map[string]struct{})
	for e := range s.graph.EdgesByKind(graph.EdgeCoChange) {
		if e == nil {
			continue
		}
		score := e.Confidence
		if e.Meta != nil {
			if v, ok := e.Meta["score"].(float64); ok {
				score = v
			}
		}
		count := 0
		if e.Meta != nil {
			switch v := e.Meta["count"].(type) {
			case int:
				count = v
			case int64:
				count = int(v)
			case float64:
				count = int(v)
			}
		}
		edges = append(edges, ccEdge{from: e.From, to: e.To, score: score, count: count})
		idSet[e.From] = struct{}{}
		idSet[e.To] = struct{}{}
	}
	if len(edges) == 0 {
		return false
	}

	// Batched endpoint resolution — one batched id-IN query vs.
	// 2 * len(edges) per-row GetNode trips. On a workspace with
	// thousands of co-change edges this is the bulk of the latency.
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	nodes := s.graph.GetNodesByIDs(ids)

	for _, e := range edges {
		from, ok := nodes[e.from]
		if !ok || from == nil {
			continue
		}
		to, ok := nodes[e.to]
		if !ok || to == nil {
			continue
		}
		addCoChangeLink(scores, counts, from.FilePath, to.FilePath, e.score, e.count)
	}
	return true
}

// addCoChangeLink records one directed co-change relationship.
func addCoChangeLink(scores map[string]map[string]float64, counts map[string]map[string]int, from, to string, score float64, count int) {
	if scores[from] == nil {
		scores[from] = map[string]float64{}
	}
	if counts[from] == nil {
		counts[from] = map[string]int{}
	}
	scores[from][to] = score
	counts[from][to] = count
}

// storeCoChange publishes the freshly built caches under the lock.
func (s *Server) storeCoChange(scores map[string]map[string]float64, counts map[string]map[string]int) {
	s.cochangeMu.Lock()
	s.cochangeByFile = scores
	s.cochangeCount = counts
	s.cochangeMu.Unlock()
}

// coChangeScores returns the co-changing file -> score map for a file,
// or nil when the file has no co-change data.
func (s *Server) coChangeScores(filePath string) map[string]float64 {
	s.cochangeMu.RLock()
	defer s.cochangeMu.RUnlock()
	return s.cochangeByFile[filePath]
}

// coChangeCounts returns the co-changing file -> commit-overlap count
// map for a file.
func (s *Server) coChangeCounts(filePath string) map[string]int {
	s.cochangeMu.RLock()
	defer s.cochangeMu.RUnlock()
	return s.cochangeCount[filePath]
}

// hasCoChangeData reports whether the co-change caches hold anything —
// used by buildRerankContext to decide whether to wire the signal.
func (s *Server) hasCoChangeData() bool {
	s.cochangeMu.RLock()
	defer s.cochangeMu.RUnlock()
	return len(s.cochangeByFile) > 0
}
