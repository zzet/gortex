package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// subgraphNodeCap bounds the neighbour ring returned by /v1/subgraph so a
// single hydration call can never pull an unbounded slice of the graph.
const subgraphNodeCap = 200

// subgraphMaxDepth caps the requested ring depth server-side.
const subgraphMaxDepth = 2

// SubGraphResponse is the /v1/subgraph payload: FULL nodes (Meta /
// QualName / EndLine intact, unlike the brief /v1/graph projection) for a
// node and its neighbour ring, used by cross-daemon proxy-edge proxy-node
// hydration.
type SubGraphResponse struct {
	Root  *graph.Node   `json:"root"`
	Nodes []*graph.Node `json:"nodes"`
	Edges []*graph.Edge `json:"edges"`
	Stats SubGraphMeta  `json:"stats"`
	// View is the base-scoped rider; see HealthResponse.View. A hydrating
	// remote reads it to learn that the ring it just fetched describes the
	// base corpus, not the checkout its caller is in.
	View *BaseScopedRider `json:"view,omitempty"`
}

// SubGraphMeta carries the freshness + truncation metadata the hydrator
// stamps onto proxy nodes.
type SubGraphMeta struct {
	SchemaVersion int       `json:"schema_version"`
	FetchedAt     time.Time `json:"fetched_at"`
	Truncated     bool      `json:"truncated"`
}

// handleSubGraph serves GET /v1/subgraph?id=<id>&depth=<n>. Returns the
// requested node and its in/out neighbour ring out to depth (default 1,
// capped at subgraphMaxDepth), with FULL node bodies so a remote daemon
// can hydrate a proxy node's neighbours. Read-only; obeys the same auth
// rule as the rest of /v1 (enforced by the WithAuth wrapper).
func (h *Handler) handleSubGraph(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		WriteJSONError(w, http.StatusBadRequest, "id query parameter is required")
		return
	}
	depth := 1
	if d := strings.TrimSpace(r.URL.Query().Get("depth")); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 {
			depth = v
		}
	}
	if depth > subgraphMaxDepth {
		depth = subgraphMaxDepth
	}

	// The BFS below walks the store one hop at a time. Pin the base corpus
	// for the whole walk so the ring cannot be assembled from two
	// generations, and so a retirement sweep waits behind this reader
	// rather than through it.
	read := h.beginBaseRead(r, "",
		graphview.CapSyntaxGraph, graphview.CapResolutionLocal, graphview.CapIncomingEdges)
	defer read.release()

	root := h.graph.GetNode(id)
	if root == nil {
		WriteJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", id))
		return
	}

	visited := map[string]bool{id: true}
	seenEdge := map[string]bool{}
	var nodes []*graph.Node
	var edges []*graph.Edge
	truncated := false

	addEdge := func(e *graph.Edge) {
		key := e.From + "\x00" + e.To + "\x00" + string(e.Kind)
		if seenEdge[key] {
			return
		}
		seenEdge[key] = true
		edges = append(edges, e)
	}
	addNode := func(nid string) bool {
		if visited[nid] {
			return true
		}
		if len(nodes) >= subgraphNodeCap {
			truncated = true
			return false
		}
		visited[nid] = true
		if n := h.graph.GetNode(nid); n != nil {
			nodes = append(nodes, n)
		}
		return true
	}

	frontier := []string{id}
	for d := 0; d < depth && !truncated; d++ {
		var next []string
		for _, nid := range frontier {
			for _, e := range h.graph.GetOutEdges(nid) {
				addEdge(e)
				if !visited[e.To] {
					if !addNode(e.To) {
						break
					}
					next = append(next, e.To)
				}
			}
			if truncated {
				break
			}
			for _, e := range h.graph.GetInEdges(nid) {
				addEdge(e)
				if !visited[e.From] {
					if !addNode(e.From) {
						break
					}
					next = append(next, e.From)
				}
			}
			if truncated {
				break
			}
		}
		frontier = next
	}

	WriteJSON(w, http.StatusOK, SubGraphResponse{
		Root:  root,
		Nodes: nodes,
		Edges: edges,
		Stats: SubGraphMeta{
			SchemaVersion: SchemaVersion,
			FetchedAt:     time.Now(),
			Truncated:     truncated,
		},
		View: read.close(),
	})
}
