package mcp

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// registerCouplingMetricsTool wires get_coupling_metrics — the
// classic Robert C. Martin metrics computed per package or
// community.
//
//	Ca (afferent coupling)  — how many external units depend on us
//	Ce (efferent coupling)  — how many external units we depend on
//	I  (instability)        — Ce / (Ca + Ce). 0 = max stable, 1 = max unstable
//
// The painful packages are the ones with **high Ca + high I** —
// load-bearing and changing all the time. The tool returns rows
// sorted by Ca-and-I so those packages surface first.
func (s *Server) registerCouplingMetricsTool() {
	s.addTool(
		mcp.NewTool("get_coupling_metrics",
			mcp.WithDescription("Afferent / efferent coupling + instability per unit, computed from the dependency edges in the graph. Unit defaults to 'package' (file-directory path slice — the first two segments by default) but can be set to 'community' to roll up via the cached community result. Returns {unit, ca, ce, instability, internal_edges, total_edges, members}. Pairs with get_architecture: that lists the top communities; this grades them on stability. Flag for refactor: high ca + high I means everyone depends on something that's also depending on a lot — a load-bearing tangle."),
			mcp.WithString("unit", mcp.Description("Grouping unit: 'package' (default — first N path segments) or 'community' (cached community result).")),
			mcp.WithNumber("package_depth", mcp.Description("Number of leading path segments to use when unit='package' (default: 2).")),
			mcp.WithNumber("min_members", mcp.Description("Drop units with fewer than this many member symbols (default: 1).")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 50).")),
			mcp.WithString("sort_by", mcp.Description("Sort key: ca_and_instability (default — load-bearing-and-unstable first), ca, ce, instability, members.")),
			mcp.WithString("path_prefix", mcp.Description("Scope to nodes under this file-path prefix.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetCouplingMetrics,
	)
}

type couplingRow struct {
	Unit          string  `json:"unit"`
	Ca            int     `json:"ca"`
	Ce            int     `json:"ce"`
	Instability   float64 `json:"instability"`
	InternalEdges int     `json:"internal_edges"`
	TotalEdges    int     `json:"total_edges"`
	Members       int     `json:"members"`
}

func (s *Server) handleGetCouplingMetrics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	unitKind := strings.TrimSpace(req.GetString("unit", "package"))
	pkgDepth := max(req.GetInt("package_depth", 2), 1)
	minMembers := max(req.GetInt("min_members", 1), 0)
	limit := max(req.GetInt("limit", 50), 1)
	sortBy := strings.TrimSpace(req.GetString("sort_by", "ca_and_instability"))
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))

	// Build node → unit map. Symbol-kind nodes get a unit; structural
	// nodes (files, imports) don't participate as members but their
	// edges still count when they connect two unit-tagged symbols.
	nodeToUnit := map[string]string{}
	members := map[string]int{}
	for _, n := range s.scopedNodesLight(ctx) {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		var unit string
		switch unitKind {
		case "community":
			cr := s.getCommunities()
			if cr == nil {
				continue
			}
			id, ok := cr.NodeToComm[n.ID]
			if !ok {
				continue
			}
			unit = id
		default: // package
			unit = packageOfPath(n.FilePath, pkgDepth)
		}
		if unit == "" {
			continue
		}
		nodeToUnit[n.ID] = unit
		members[unit]++
	}

	type units struct {
		ca       map[string]bool
		ce       map[string]bool
		internal int
		total    int
	}
	stats := map[string]*units{}
	for u := range members {
		stats[u] = &units{ca: map[string]bool{}, ce: map[string]bool{}}
	}

	// Iterate the coupling-edge buckets directly via EdgesByKind
	// instead of AllEdges() + a Go-side filter — the disk backend's
	// EdgesByKind runs one indexed query per kind and ships only
	// the matching rows. Structural edges (defines / member_of /
	// contains-file-of-symbol) which dominate edge counts on large
	// repos drop out before they cross the storage boundary. Order is fixed so the
	// loop body stays trivially identical to the legacy AllEdges
	// branch.
	couplingReader := s.readerFor(ctx)
	for _, k := range []graph.EdgeKind{
		graph.EdgeCalls,
		graph.EdgeImports,
		graph.EdgeImplements,
		graph.EdgeExtends,
		graph.EdgeReferences,
		graph.EdgeInstantiates,
		graph.EdgeCrossRepoCalls,
		graph.EdgeCrossRepoImplements,
		graph.EdgeCrossRepoExtends,
	} {
		for e := range couplingReader.EdgesByKind(k) {
			if e == nil {
				continue
			}
			fromUnit, fromOK := nodeToUnit[e.From]
			toUnit, toOK := nodeToUnit[e.To]
			if !fromOK || !toOK {
				continue
			}
			if fromUnit == toUnit {
				stats[fromUnit].internal++
				stats[fromUnit].total++
				continue
			}
			// Cross-unit: counts as ce for the source unit, ca for the target.
			stats[fromUnit].ce[toUnit] = true
			stats[fromUnit].total++
			stats[toUnit].ca[fromUnit] = true
			stats[toUnit].total++
		}
	}

	rows := make([]couplingRow, 0, len(stats))
	for u, st := range stats {
		if members[u] < minMembers {
			continue
		}
		ca := len(st.ca)
		ce := len(st.ce)
		denom := ca + ce
		instability := 0.0
		if denom > 0 {
			instability = roundScore(float64(ce) / float64(denom))
		}
		rows = append(rows, couplingRow{
			Unit:          u,
			Ca:            ca,
			Ce:            ce,
			Instability:   instability,
			InternalEdges: st.internal,
			TotalEdges:    st.total,
			Members:       members[u],
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		switch sortBy {
		case "ca":
			if rows[i].Ca != rows[j].Ca {
				return rows[i].Ca > rows[j].Ca
			}
		case "ce":
			if rows[i].Ce != rows[j].Ce {
				return rows[i].Ce > rows[j].Ce
			}
		case "instability":
			if rows[i].Instability != rows[j].Instability {
				return rows[i].Instability > rows[j].Instability
			}
		case "members":
			if rows[i].Members != rows[j].Members {
				return rows[i].Members > rows[j].Members
			}
		default: // ca_and_instability
			// Combined: a unit is a problem when many things depend
			// on it AND it's also depending on many things. Score =
			// ca * instability puts load-bearing-and-unstable on top.
			si := float64(rows[i].Ca) * rows[i].Instability
			sj := float64(rows[j].Ca) * rows[j].Instability
			if si != sj {
				return si > sj
			}
		}
		return rows[i].Unit < rows[j].Unit
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"units":     rows,
		"total":     len(rows),
		"truncated": truncated,
		"unit_kind": unitKind,
		"sort_by":   sortBy,
	})
}

// packageOfPath returns the package label for a file path — the
// first `depth` path segments joined with "/". Empty when path has
// fewer segments or is empty.
func packageOfPath(path string, depth int) string {
	if path == "" || depth < 1 {
		return ""
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(clean, "/")
	// Drop leading "" from an absolute path.
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	// Drop the filename when present so a/b/c.go grouped at depth=2
	// becomes "a/b" not "a/b" too short.
	if len(parts) > 0 && strings.Contains(parts[len(parts)-1], ".") {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) < depth {
		return strings.Join(parts, "/")
	}
	return strings.Join(parts[:depth], "/")
}
