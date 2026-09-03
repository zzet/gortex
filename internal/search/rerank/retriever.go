package rerank

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Retriever is the pluggable candidate-producer protocol. The existing
// hybrid pipeline (native symbol FTS + vector + adaptive fusion) is one
// implementation; others
// — graph_completion, an in-house embedding model, a research-grade
// LLM-as-retriever — plug in by implementing this interface.
//
// Retrieve returns a slice of *Candidate ready for Pipeline.Rerank.
// The TextRank / VectorRank fields are advisory: producers that don't
// run BM25 or vector search set them to -1, and the corresponding
// signals contribute zero for that candidate. The score breakdown
// fills in during rerank.
type Retriever interface {
	// Name is the stable identifier used in feedback / telemetry /
	// the `retriever` field on Candidate. Required.
	Name() string

	// Retrieve returns candidates for the query. limit caps the
	// result set; producers should obey it but may return fewer.
	// The caller passes its graph reader (so retrievers can do graph
	// walks without owning a reference, and so a request carrying an
	// overlay expands over its own buffers). ctx is honoured for
	// cancellation — long-running retrievers must respect it.
	Retrieve(ctx context.Context, g graph.Reader, query string, limit int) ([]*Candidate, error)
}

// GraphCompletion is a Retriever that uses an upstream Retriever for
// seed candidates and then expands the seed set by 1-hop graph
// traversal — walks every outgoing call / reference edge from each
// seed and adds the target as an additional candidate.
//
// Use case: the agent's query is a name (high vector hit) but the
// intent is "everything tightly coupled to it". GraphCompletion
// gives the rerank pipeline a wider candidate pool without paying
// for a second retrieval pass.
//
// The graph_completion retriever does NOT itself run vector search
// — it accepts a Seeder closure that produces the seed candidates.
// Tests can plug a deterministic seeder; production callers pass a
// closure that invokes the existing hybrid search.
type GraphCompletion struct {
	// Seeder produces the initial candidate set the 1-hop expansion
	// will fan out from. Required.
	Seeder func(ctx context.Context, g graph.Reader, query string, limit int) ([]*Candidate, error)

	// MaxSeedExpansion caps the number of new candidates produced
	// per seed. Defaults to 8 — large enough to surface typical
	// orchestrator fanouts, small enough to keep total candidates
	// bounded.
	MaxSeedExpansion int

	// EdgeKinds filters which edge kinds count as "1-hop". Empty =
	// every edge kind. Typical use: limit to calls + references for
	// "what does this symbol couple to" semantics.
	EdgeKinds []graph.EdgeKind
}

// Name implements Retriever. Stable identifier used by feedback +
// telemetry.
func (gc *GraphCompletion) Name() string { return "graph_completion" }

// Retrieve runs the seeder, then walks 1-hop out from every seed.
// Duplicates (a seed that's also reachable from another seed) are
// merged: the seed copy wins and keeps its rank fields. New nodes
// added by expansion have TextRank=-1 / VectorRank=-1 so the
// downstream rerank knows they came from graph expansion.
func (gc *GraphCompletion) Retrieve(ctx context.Context, g graph.Reader, query string, limit int) ([]*Candidate, error) {
	if gc.Seeder == nil {
		return nil, errNilSeeder
	}
	seeds, err := gc.Seeder(ctx, g, query, limit)
	if err != nil {
		return nil, err
	}

	expansion := gc.MaxSeedExpansion
	if expansion <= 0 {
		expansion = 8
	}

	allowed := make(map[graph.EdgeKind]bool, len(gc.EdgeKinds))
	for _, k := range gc.EdgeKinds {
		allowed[k] = true
	}
	keepAll := len(allowed) == 0

	out := make([]*Candidate, 0, len(seeds)*2)
	seen := make(map[string]*Candidate, len(seeds)*2)
	seedIDs := make([]string, 0, len(seeds))
	for _, c := range seeds {
		if c == nil || c.Node == nil {
			continue
		}
		if _, dup := seen[c.Node.ID]; dup {
			continue
		}
		seen[c.Node.ID] = c
		out = append(out, c)
		seedIDs = append(seedIDs, c.Node.ID)
	}

	// One batched out-edge round-trip across every seed instead of
	// one query per seed. On the disk backend this drops ~30
	// round-trips into 1 for a typical search_symbols completion pass.
	outEdges := g.GetOutEdgesByNodeIDs(seedIDs)

	// Collect every distinct target id, then materialise the target
	// nodes in one batched GetNodesByIDs call — same shape, same win.
	toIDs := make([]string, 0, len(outEdges)*4)
	toSeen := make(map[string]struct{}, len(outEdges)*4)
	for _, seedID := range seedIDs {
		for _, e := range outEdges[seedID] {
			if !keepAll && !allowed[e.Kind] {
				continue
			}
			if _, dup := seen[e.To]; dup {
				continue
			}
			if _, dup := toSeen[e.To]; dup {
				continue
			}
			toSeen[e.To] = struct{}{}
			toIDs = append(toIDs, e.To)
		}
	}
	toNodes := g.GetNodesByIDs(toIDs)

	for _, seedID := range seedIDs {
		added := 0
		for _, e := range outEdges[seedID] {
			if !keepAll && !allowed[e.Kind] {
				continue
			}
			if added >= expansion {
				break
			}
			if _, dup := seen[e.To]; dup {
				continue
			}
			toNode := toNodes[e.To]
			if toNode == nil {
				continue
			}
			expanded := &Candidate{
				Node:       toNode,
				TextRank:   -1,
				VectorRank: -1,
			}
			seen[toNode.ID] = expanded
			out = append(out, expanded)
			added++
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	// Honour the caller's limit at the boundary.
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// errNilSeeder is returned when GraphCompletion.Retrieve is called
// without a seeder closure. Constructors should validate this at
// build time; the runtime error is the safety net.
var errNilSeeder = retrieverError("graph_completion: Seeder closure is required")

// retrieverError is a plain error type kept inside the package so
// retriever errors don't bleed into the broader error taxonomy.
type retrieverError string

func (e retrieverError) Error() string { return string(e) }
