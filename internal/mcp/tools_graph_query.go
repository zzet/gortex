package mcp

import (
	"context"
	"fmt"
	"iter"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// graphQueryMaxStages caps how many pipeline stages one query may have.
// The grammar is intentionally tiny; a deep pipeline is almost always a
// malformed query rather than a real need, and the cap bounds the
// per-call work.
const graphQueryMaxStages = 5

// registerGraphQueryTool wires graph_query — an ad-hoc, read-only graph
// query escape hatch. It runs a frozen minimal pipeline DSL so an agent
// can express a one-off shape ("interfaces named ~Handler implemented
// under internal/mcp/") that no purpose-built tool covers, without
// dropping to raw graph traversal code.
func (s *Server) registerGraphQueryTool() {
	s.addTool(
		mcp.NewTool("graph_query",
			mcp.WithDescription("Ad-hoc read-only graph query via a tiny pipeline DSL. Stages are separated by '|':\n"+
				"  nodes FILTER*            — seed the working set with all nodes matching the filters\n"+
				"  traverse EDGEKINDS DIR   — expand the working set one hop along the given edge kinds (DIR: out|in|both, default out)\n"+
				"  filter FILTER+           — narrow the working set in memory\n"+
				"A FILTER is one of: kind=<kind>  name~<regex>  path=<prefix>  lang=<lang>.\n"+
				"Example: nodes kind=interface name~Handler | traverse implements in | filter path=internal/mcp/\n"+
				"The query is read-only by construction (no edit verbs) and bounded by `limit` and a 5-stage cap."),
			mcp.WithString("query", mcp.Required(), mcp.Description("The pipeline DSL query — see the tool description for the grammar.")),
			mcp.WithNumber("limit", mcp.Description("Max nodes in the result (default 100, hard cap 1000).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon.")),
		),
		s.handleGraphQuery,
	)
}

// handleGraphQuery parses and evaluates the pipeline DSL and returns the
// resulting subgraph.
func (s *Server) handleGraphQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := req.GetInt("limit", 100)
	if limit < 1 {
		limit = 100
	}
	// Kept so the response can say the cap, not the caller, chose the
	// effective limit (see the query.SubGraph disclosure fields).
	requestedLimit := limit
	if limit > 1000 {
		limit = 1000
	}

	stages, parseErr := parseGraphQuery(q)
	if parseErr != nil {
		return mcp.NewToolResultError("graph_query: " + parseErr.Error()), nil
	}

	eng := s.engineFor(ctx)
	sg, evalErr := evalGraphQuery(eng, stages, limit)
	if evalErr != nil {
		return mcp.NewToolResultError("graph_query: " + evalErr.Error()), nil
	}
	// Measured before the repo filter below can shrink the set: landing
	// on the cap is the only visible signal that the pipeline stopped on
	// `limit` rather than exhausting the graph (#672). A result of
	// exactly limit nodes is disclosed even when the graph held exactly
	// that many — a spurious "verify this" is the safe direction.
	if boundByLimit(len(sg.Nodes), limit) {
		// sg.Truncated folds the limit cut into the flag the compact
		// renderers already emit: gcx's `truncated` meta and TOON's
		// `truncated` field were rendering `false` next to a limit-clamped
		// count — the exact #672 failure mode in compact formats (#845).
		sg.Truncated = true
		// JSON rides the flat disclosure shape search_text and
		// find_declaration emit, so a client checking
		// `_truncated_by_limit === true` handles every tool the same way.
		sg.TruncatedByLimit = true
		sg.LimitApplied = limit
		exact := false
		sg.CountIsExact = &exact
		sg.TruncationNote = graphQueryTruncationNote
		if requestedLimit > limit {
			sg.LimitRequested = requestedLimit
		}
	}

	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

// graphQueryTruncationNote explains a pipeline whose working set stopped on
// the limit. The cap runs after every stage, so nodes dropped at any point
// cannot re-enter the result in a later stage.
const graphQueryTruncationNote = "the pipeline stopped at `limit` nodes, so the node set is a floor rather than the full result — nodes dropped by the per-stage cap cannot come back in a later stage. Raise `limit` to widen."

// gqStageKind enumerates the three pipeline verbs.
type gqStageKind int

const (
	gqStageNodes gqStageKind = iota
	gqStageTraverse
	gqStageFilter
)

// gqFilter is one parsed FILTER clause. Exactly one of the fields drives
// the predicate, selected by op.
type gqFilter struct {
	op    string // "kind=", "name~", "path=", "lang="
	value string
	// re is the compiled regexp for a "name~" filter; nil otherwise.
	re *regexp.Regexp
}

// gqStage is one parsed pipeline stage.
type gqStage struct {
	kind      gqStageKind
	filters   []gqFilter       // nodes / filter stages
	edgeKinds []graph.EdgeKind // traverse stage
	direction string           // traverse stage: out|in|both
}

// parseGraphQuery tokenizes and parses the pipeline DSL into stages.
// The tokenizer is hand-written: stages split on '|', then each stage
// is whitespace-tokenized. A FILTER is a single token of the form
// kind=X / name~X / path=X / lang=X — values may not contain spaces,
// which keeps the grammar frozen and the parser tiny.
func parseGraphQuery(q string) ([]gqStage, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}
	rawStages := strings.Split(q, "|")
	if len(rawStages) > graphQueryMaxStages {
		return nil, fmt.Errorf("too many stages (%d); the pipeline is capped at %d",
			len(rawStages), graphQueryMaxStages)
	}

	var stages []gqStage
	for i, raw := range rawStages {
		toks := strings.Fields(raw)
		if len(toks) == 0 {
			return nil, fmt.Errorf("stage %d is empty", i+1)
		}
		verb := strings.ToLower(toks[0])
		args := toks[1:]

		switch verb {
		case "nodes":
			if i != 0 {
				return nil, fmt.Errorf("'nodes' must be the first stage")
			}
			filters, ferr := parseGQFilters(args)
			if ferr != nil {
				return nil, ferr
			}
			stages = append(stages, gqStage{kind: gqStageNodes, filters: filters})

		case "traverse":
			if i == 0 {
				return nil, fmt.Errorf("'traverse' cannot be the first stage; start with 'nodes'")
			}
			if len(args) == 0 {
				return nil, fmt.Errorf("'traverse' needs an edge-kind list")
			}
			kinds, kerr := query.ParseEdgeKindsCSV(args[0])
			if kerr != nil {
				return nil, kerr
			}
			if len(kinds) == 0 {
				return nil, fmt.Errorf("'traverse' edge-kind list is empty")
			}
			dir := "out"
			if len(args) >= 2 {
				dir = strings.ToLower(args[1])
				switch dir {
				case "out", "in", "both":
				default:
					return nil, fmt.Errorf("traverse direction must be out, in, or both (got %q)", args[1])
				}
			}
			if len(args) > 2 {
				return nil, fmt.Errorf("'traverse' takes at most an edge-kind list and a direction")
			}
			stages = append(stages, gqStage{kind: gqStageTraverse, edgeKinds: kinds, direction: dir})

		case "filter":
			if i == 0 {
				return nil, fmt.Errorf("'filter' cannot be the first stage; start with 'nodes'")
			}
			if len(args) == 0 {
				return nil, fmt.Errorf("'filter' needs at least one filter clause")
			}
			filters, ferr := parseGQFilters(args)
			if ferr != nil {
				return nil, ferr
			}
			stages = append(stages, gqStage{kind: gqStageFilter, filters: filters})

		default:
			return nil, fmt.Errorf("unknown stage verb %q (expected nodes, traverse, or filter)", toks[0])
		}
	}
	return stages, nil
}

// parseGQFilters parses a run of FILTER tokens.
func parseGQFilters(toks []string) ([]gqFilter, error) {
	var out []gqFilter
	for _, tok := range toks {
		var op string
		switch {
		case strings.HasPrefix(tok, "kind="):
			op = "kind="
		case strings.HasPrefix(tok, "name~"):
			op = "name~"
		case strings.HasPrefix(tok, "path="):
			op = "path="
		case strings.HasPrefix(tok, "lang="):
			op = "lang="
		default:
			return nil, fmt.Errorf("malformed filter %q (expected kind=, name~, path=, or lang=)", tok)
		}
		value := tok[len(op):]
		if value == "" {
			return nil, fmt.Errorf("filter %q has an empty value", tok)
		}
		f := gqFilter{op: op, value: value}
		if op == "name~" {
			re, err := regexp.Compile(value)
			if err != nil {
				return nil, fmt.Errorf("invalid name~ regex %q: %v", value, err)
			}
			f.re = re
		}
		out = append(out, f)
	}
	return out, nil
}

// matches reports whether n satisfies the filter.
func (f gqFilter) matches(n *graph.Node) bool {
	switch f.op {
	case "kind=":
		return string(n.Kind) == f.value
	case "name~":
		return f.re.MatchString(n.Name)
	case "path=":
		return strings.HasPrefix(n.FilePath, f.value)
	case "lang=":
		return strings.EqualFold(n.Language, f.value)
	}
	return false
}

// matchesAll reports whether n satisfies every filter in fs.
func matchesAll(n *graph.Node, fs []gqFilter) bool {
	for _, f := range fs {
		if !f.matches(n) {
			return false
		}
	}
	return true
}

// evalGraphQuery threads a working set of nodes through the stages and
// builds the final SubGraph. The working set is capped at `limit` after
// each stage so an unbounded `nodes` seed or a fan-out `traverse` can't
// blow up memory. Edges produced by traverse stages are collected so
// the result shows the relationships the query walked.
func evalGraphQuery(eng *query.Engine, stages []gqStage, limit int) (*query.SubGraph, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("empty pipeline")
	}

	var working []*graph.Node
	seen := make(map[string]bool)
	var collectedEdges []*graph.Edge

	add := func(n *graph.Node) {
		if n == nil || seen[n.ID] {
			return
		}
		seen[n.ID] = true
		working = append(working, n)
	}

	for _, st := range stages {
		switch st.kind {
		case gqStageNodes:
			// When the pipeline opens with a `kind=` predicate (the
			// common case — e.g. `nodes kind=function ...`), iterate
			// the backend's per-kind bucket instead of AllNodes(). On
			// a disk backend NodesByKind hits a server-side filter and only
			// the matching rows cross the storage boundary; AllNodes() materialised the
			// whole node table per request. Other filters
			// (`name~`/`path=`/`lang=`) still post-filter in Go.
			//
			// Overlay views (NodesByKindReader-unaware) fall through
			// to the AllNodes() walk — they're already in-memory, so
			// the bucket optimisation has no win there.
			seedKinds := seedKindsFromFilters(st.filters)
			byKind, _ := eng.Reader().(nodesByKindReader)
			if byKind != nil && len(seedKinds) > 0 {
				done := false
				for _, k := range seedKinds {
					if done {
						break
					}
					for n := range byKind.NodesByKind(k) {
						if n == nil {
							continue
						}
						if !matchesAll(n, st.filters) {
							continue
						}
						add(n)
						if len(working) >= limit {
							done = true
							break
						}
					}
				}
			} else {
				for _, n := range eng.AllNodes() {
					if matchesAll(n, st.filters) {
						add(n)
						if len(working) >= limit {
							break
						}
					}
				}
			}

		case gqStageFilter:
			kept := working[:0]
			newSeen := make(map[string]bool, len(working))
			for _, n := range working {
				if matchesAll(n, st.filters) {
					kept = append(kept, n)
					newSeen[n.ID] = true
				}
			}
			working = kept
			seen = newSeen

		case gqStageTraverse:
			kindSet := make(map[graph.EdgeKind]bool, len(st.edgeKinds))
			for _, k := range st.edgeKinds {
				kindSet[k] = true
			}
			both := st.direction == "both"
			forward := st.direction != "in"

			var next []*graph.Node
			nextSeen := make(map[string]bool)
			addNext := func(n *graph.Node) {
				if n == nil || nextSeen[n.ID] {
					return
				}
				nextSeen[n.ID] = true
				next = append(next, n)
			}

			for _, src := range working {
				var edges []*graph.Edge
				if both {
					edges = append(eng.GetOutEdges(src.ID), eng.GetInEdges(src.ID)...)
				} else if forward {
					edges = eng.GetOutEdges(src.ID)
				} else {
					edges = eng.GetInEdges(src.ID)
				}
				for _, e := range edges {
					if !kindSet[e.Kind] {
						continue
					}
					var targetID string
					if both {
						if e.From == src.ID {
							targetID = e.To
						} else {
							targetID = e.From
						}
					} else if forward {
						if e.From != src.ID {
							continue
						}
						targetID = e.To
					} else {
						if e.To != src.ID {
							continue
						}
						targetID = e.From
					}
					if graph.IsUnresolvedTarget(targetID) ||
						strings.HasPrefix(targetID, "external::") {
						continue
					}
					tn := eng.GetSymbol(targetID)
					if tn == nil {
						continue
					}
					collectedEdges = append(collectedEdges, e)
					addNext(tn)
				}
				if len(next) >= limit {
					break
				}
			}
			// traverse replaces the working set with the expanded one.
			working = next
			seen = nextSeen
		}

		if len(working) > limit {
			working = working[:limit]
			// Rebuild seen so a later traverse only fans out from the
			// retained nodes.
			seen = make(map[string]bool, len(working))
			for _, n := range working {
				seen[n.ID] = true
			}
		}
	}

	// Keep only edges whose endpoints are both in the final node set,
	// so the subgraph is internally consistent.
	inSet := make(map[string]bool, len(working))
	for _, n := range working {
		inSet[n.ID] = true
	}
	var edges []*graph.Edge
	edgeSeen := make(map[string]bool)
	for _, e := range collectedEdges {
		if !inSet[e.From] || !inSet[e.To] {
			continue
		}
		key := string(e.Kind) + "\x00" + e.From + "\x00" + e.To
		if edgeSeen[key] {
			continue
		}
		edgeSeen[key] = true
		edges = append(edges, e)
	}

	return &query.SubGraph{
		Nodes:      working,
		Edges:      edges,
		TotalNodes: len(working),
		TotalEdges: len(edges),
	}, nil
}

// nodesByKindReader is the optional read-side capability the eng.Reader
// underlying type may implement. *graph.Graph satisfies it directly
// (Store has NodesByKind); OverlaidView does not, which is fine —
// overlays already work in-memory and don't benefit from the bucket
// fast path.
type nodesByKindReader interface {
	NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node]
}

// seedKindsFromFilters extracts every `kind=` predicate from a stage's
// filter list so the seed loop can iterate the corresponding NodesByKind
// buckets instead of AllNodes(). Returns nil when no `kind=` filter is
// present — the caller falls back to the AllNodes() walk in that case.
// Duplicates are deduped so a sloppy author writing `kind=function
// kind=function` doesn't double-iterate.
func seedKindsFromFilters(filters []gqFilter) []graph.NodeKind {
	var out []graph.NodeKind
	seen := make(map[graph.NodeKind]struct{}, len(filters))
	for _, f := range filters {
		if f.op != "kind=" {
			continue
		}
		k := graph.NodeKind(f.value)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
