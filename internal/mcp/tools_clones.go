package mcp

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// registerCloneTools wires the find_clones MCP tool — the query
// surface over the EdgeSimilarTo graph layer materialised by the
// MinHash + LSH clone-detection pass (see internal/clones and the
// indexer's detectClonesAndEmitEdges).
func (s *Server) registerCloneTools() {
	s.addTool(
		mcp.NewTool("find_clones",
			mcp.WithDescription("Surfaces near-duplicate ('clone') function/method clusters from the EdgeSimilarTo graph layer. Each cluster is a connected component of bodies whose MinHash + LSH estimated Jaccard similarity crossed the index-time threshold — catches copy-paste and renamed-variable (Type-1/Type-2) clones. Every member is flagged is_dead (zero incoming calls/refs), so dead_only=true yields the Gortex-unique \"dead duplicates of live code\" diagnostic: dead functions that are near-copies of code still in use."),
			mcp.WithNumber("min_similarity", mcp.Description("Only report clone pairs at or above this estimated Jaccard similarity (0..1). Default 0 — every recorded EdgeSimilarTo edge.")),
			mcp.WithBoolean("dead_only", mcp.Description("Only return clusters that contain at least one dead-code symbol — the \"dead duplicates of live code\" view. Default false.")),
			mcp.WithString("path_prefix", mcp.Description("Restrict to symbols whose file path starts with this prefix.")),
			mcp.WithString("repo", mcp.Description("Narrow to a single repository prefix (tracked name or path), clamped to the session workspace.")),
			mcp.WithString("project", mcp.Description("Narrow to the repositories in a project, clamped to the session workspace.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow the search, clamped to the session workspace.")),
			mcp.WithNumber("limit", mcp.Description("Maximum clusters to return (default: 50). Clusters are ranked largest-first.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleFindClones,
	)
}

// cloneMember is one symbol inside a clone cluster.
type cloneMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	IsDead    bool   `json:"is_dead"`
}

// cloneCluster is a connected component of the clone graph.
type cloneCluster struct {
	Members       []cloneMember `json:"members"`
	Size          int           `json:"size"`
	AvgSimilarity float64       `json:"avg_similarity"`
	DeadCount     int           `json:"dead_count"`
	HasDeadCode   bool          `json:"has_dead_code"`
}

func (s *Server) handleFindClones(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	minSim := req.GetFloat("min_similarity", 0)
	deadOnly := req.GetBool("dead_only", false)
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := req.GetInt("limit", 50)
	if limit <= 0 {
		limit = 50
	}

	// Resolve the uniform repo/project/workspace/scope narrowing the way
	// the analyze dispatcher does, then let analyzeNodeVisible enforce
	// both the session-workspace ceiling and the resolved allow-set. The
	// `repo` arg used to be matched here as a bare RepoPrefix equality,
	// which honoured an exact tracked name but silently ignored the path
	// form and never clamped an unnarrowed call to the session's own
	// workspace — so a bound session saw sibling workspaces' clones.
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)

	inScope := func(n *graph.Node) bool {
		if n == nil {
			return false
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			return false
		}
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			return false
		}
		return s.analyzeNodeVisible(ctx, n)
	}

	// Walk EdgeSimilarTo edges. The graph holds them symmetrically
	// (fA→fB and fB→fA); canonicalise to A<B and dedupe so each clone
	// pair is counted once.
	//
	// EdgesByKind streams only the SimilarTo edges -- on disk backends
	// that is one kind-filtered edge query instead of the full AllEdges
	// scan we used to pay for. ~500k edge rows materialised over the
	// storage boundary dropped to the SimilarTo-bearing
	// subset (~hundreds-to-thousands on a normal workspace).
	// One reader for the whole call — the edge stream and every endpoint
	// lookup must agree, and under an overlay they resolve to the
	// caller's buffers.
	reader := s.readerFor(ctx)
	seen := make(map[[2]string]struct{})
	var pairs []clones.Pair
	for e := range reader.EdgesByKind(graph.EdgeSimilarTo) {
		if e == nil {
			continue
		}
		a, b := e.From, e.To
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		key := [2]string{a, b}
		if _, ok := seen[key]; ok {
			continue
		}
		sim := similarityOf(e)
		if sim < minSim {
			continue
		}
		from := reader.GetNode(a)
		to := reader.GetNode(b)
		if !inScope(from) || !inScope(to) {
			continue
		}
		seen[key] = struct{}{}
		pairs = append(pairs, clones.Pair{A: a, B: b, Similarity: sim})
	}

	clusters := clones.ClusterPairs(pairs)

	// Dead-code set — the "dead duplicates of live code" differentiator
	// depends on knowing which clone members have zero incoming
	// calls/references. Computed once and shared across every cluster.
	deadSet := make(map[string]bool)
	for _, d := range analysis.FindDeadCode(reader, s.getProcesses(), nil) {
		deadSet[d.ID] = true
	}

	out := make([]cloneCluster, 0, len(clusters))
	for _, c := range clusters {
		cc := cloneCluster{
			Size:          c.Size,
			AvgSimilarity: roundSim(c.AvgSimilarity),
		}
		for _, id := range c.Members {
			n := reader.GetNode(id)
			if n == nil {
				continue
			}
			dead := deadSet[id]
			if dead {
				cc.DeadCount++
			}
			cc.Members = append(cc.Members, cloneMember{
				ID:        n.ID,
				Name:      n.Name,
				Kind:      string(n.Kind),
				FilePath:  n.FilePath,
				StartLine: n.StartLine,
				IsDead:    dead,
			})
		}
		if len(cc.Members) < 2 {
			continue // a cluster needs at least two live endpoints to be meaningful
		}
		cc.HasDeadCode = cc.DeadCount > 0
		if deadOnly && !cc.HasDeadCode {
			continue
		}
		out = append(out, cc)
	}

	// ClusterPairs already sorts largest-first; re-sort after filtering
	// so dead-bearing clusters bubble up when dead_only is off.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DeadCount != out[j].DeadCount {
			return out[i].DeadCount > out[j].DeadCount
		}
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Members[0].ID < out[j].Members[0].ID
	})

	totalClusters := len(out)
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeFindClones(out, totalClusters, len(pairs), truncated))
	}

	result := map[string]any{
		"clusters": out,
		"total":    totalClusters,
		"pairs":    len(pairs),
	}
	if truncated {
		result["truncated"] = true
		result["limit"] = limit
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// similarityOf reads an EdgeSimilarTo edge's estimated Jaccard score.
// The clone pass stores it on Meta["similarity"]; Confidence carries
// the same value as a fallback for edges restored from a snapshot
// written before the Meta key was set.
func similarityOf(e *graph.Edge) float64 {
	if e.Meta != nil {
		if v, ok := e.Meta["similarity"].(float64); ok {
			return v
		}
	}
	return e.Confidence
}

// roundSim rounds a similarity score to three decimal places so the
// response doesn't carry float noise.
func roundSim(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}

// encodeFindClones emits a GCX1 envelope with two sections:
// `find_clones.summary` (one row with the totals) and
// `find_clones.clusters` (one row per cluster, member IDs / names /
// dead-flags flattened into parallel comma-joined fields).
func encodeFindClones(clusters []cloneCluster, total, pairs int, truncated bool) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := newGCX(&buf, "find_clones.summary",
		[]string{"clusters", "pairs", "returned", "truncated"})
	if err := sumEnc.WriteRow(total, pairs, len(clusters), truncated); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	clEnc := newGCX(&buf, "find_clones.clusters",
		[]string{"size", "avg_similarity", "dead_count", "has_dead_code", "ids", "names", "paths", "dead_flags"},
		"count", fmt.Sprintf("%d", len(clusters)),
	)
	for _, c := range clusters {
		ids := make([]string, 0, len(c.Members))
		names := make([]string, 0, len(c.Members))
		paths := make([]string, 0, len(c.Members))
		deadFlags := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			ids = append(ids, m.ID)
			names = append(names, m.Name)
			paths = append(paths, fmt.Sprintf("%s:%d", m.FilePath, m.StartLine))
			if m.IsDead {
				deadFlags = append(deadFlags, "1")
			} else {
				deadFlags = append(deadFlags, "0")
			}
		}
		if err := clEnc.WriteRow(
			c.Size,
			c.AvgSimilarity,
			c.DeadCount,
			c.HasDeadCode,
			strings.Join(ids, ","),
			strings.Join(names, ","),
			strings.Join(paths, ","),
			strings.Join(deadFlags, ","),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), clEnc.Close()
}
