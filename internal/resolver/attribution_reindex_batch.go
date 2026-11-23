package resolver

import "github.com/zzet/gortex/internal/graph"

// attributionReindexBatchSize bounds the number of full edge payloads retained
// while attribution passes rewrite graph targets. It is deliberately below the
// SQLite store's transaction chunk so one resolver batch remains one write.
const attributionReindexBatchSize = 2048

// attributionReindexCollector preserves rewrite order while keeping both the
// caller-local buffer and the incremental cross-file queue bounded.
type attributionReindexCollector struct {
	resolver *Resolver
	batch    []graph.EdgeReindex
}

func newAttributionReindexCollector(r *Resolver) *attributionReindexCollector {
	return &attributionReindexCollector{resolver: r}
}

func (c *attributionReindexCollector) add(edge *graph.Edge, oldTo string) {
	if edge == nil || oldTo == "" {
		return
	}
	c.batch = append(c.batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
	if len(c.batch) == attributionReindexBatchSize {
		c.flush()
	}
}

func (c *attributionReindexCollector) flush() {
	if len(c.batch) == 0 {
		return
	}
	c.resolver.persistAttributionReindexes(c.batch)
	// Drop the backing array as well as its length: EdgeReindex contains a full
	// *Edge, so retaining capacity would keep already-written payloads alive.
	c.batch = nil
}

// reindexAttributionEdgesBatched scans only the requested edge kinds and
// rewrites them in bounded batches. Backends with a native batch scanner close
// each read cursor before the callback, allowing ReindexEdges to safely re-enter
// a single-connection store.
func (r *Resolver) reindexAttributionEdgesBatched(
	kinds []graph.EdgeKind,
	rewrite func(*graph.Edge) string,
) {
	// Both whole-graph callers reject resolved targets before consulting their
	// attribution indexes. Production stores can therefore discover only compact
	// unresolved identities, then reuse the exact bounded refetch path. Adapter
	// stores retain the established full-edge scanner fallback.
	scanner, scanOK := r.graph.(graph.UnresolvedEdgeIdentityBatchScanner)
	finder, findOK := r.graph.(graph.EdgeIdentityBatchFinder)
	targeter, targetOK := r.graph.(graph.UnresolvedEdgeTargetBatchReindexer)
	if scanOK && findOK && targetOK {
		scanner.ScanUnresolvedEdgeIdentitiesBatched(
			kinds,
			attributionReindexBatchSize,
			func(identities []graph.EdgeIdentity) bool {
				r.reindexAttributionTargetsBatched(targeter, finder, identities, rewrite)
				return true
			},
		)
		return
	}
	if scanOK && findOK {
		scanner.ScanUnresolvedEdgeIdentitiesBatched(
			kinds,
			attributionReindexBatchSize,
			func(identities []graph.EdgeIdentity) bool {
				r.reindexAttributionIdentitiesBatched(finder, identities, rewrite)
				return true
			},
		)
		return
	}

	collector := newAttributionReindexCollector(r)
	graph.ScanEdgesByKindsBatched(r.graph, kinds, attributionReindexBatchSize, func(edges []*graph.Edge) bool {
		for _, edge := range edges {
			collector.add(edge, rewrite(edge))
		}
		return true
	})
	collector.flush()
}

type attributionTargetCandidate struct {
	old         graph.EdgeIdentity
	destination graph.EdgeIdentity
	direct      bool
}

// reindexAttributionTargetsBatched evaluates the two whole-graph attribution
// callbacks against an identity-only edge projection. Both callbacks depend
// only on From, To, Kind, FilePath, and Line. Unique resolved destinations can
// therefore update only SQLite's to_id column without materializing Meta or any
// other payload. Unsafe shapes and same-page convergence retain the exact
// full-edge refetch path, preserving the established rewrite contract.
func (r *Resolver) reindexAttributionTargetsBatched(
	targeter graph.UnresolvedEdgeTargetBatchReindexer,
	finder graph.EdgeIdentityBatchFinder,
	identities []graph.EdgeIdentity,
	rewrite func(*graph.Edge) string,
) {
	if targeter == nil || finder == nil || rewrite == nil || len(identities) == 0 {
		return
	}

	candidates := make([]attributionTargetCandidate, 0, len(identities))
	destinationCounts := make(map[graph.EdgeIdentity]int, len(identities))
	for _, identity := range identities {
		edge := graph.Edge{
			From: identity.From, To: identity.To, Kind: identity.Kind,
			FilePath: identity.FilePath, Line: identity.Line,
		}
		oldTo := rewrite(&edge)
		if oldTo == "" {
			continue
		}
		destination := graph.EdgeIdentityFor(&edge)
		direct := oldTo == identity.To &&
			edge.From == identity.From &&
			edge.Kind == identity.Kind &&
			edge.FilePath == identity.FilePath &&
			edge.Line == identity.Line &&
			!graph.IsUnresolvedTarget(edge.To)
		candidates = append(candidates, attributionTargetCandidate{
			old: identity, destination: destination, direct: direct,
		})
		destinationCounts[destination]++
	}

	direct := make([]graph.UnresolvedEdgeTargetReindex, 0, len(candidates))
	fallback := make([]graph.EdgeIdentity, 0)
	for _, candidate := range candidates {
		if candidate.direct && destinationCounts[candidate.destination] == 1 {
			direct = append(direct, graph.UnresolvedEdgeTargetReindex{
				Old: candidate.old, NewTo: candidate.destination.To,
			})
			continue
		}
		fallback = append(fallback, candidate.old)
	}
	if len(direct) > 0 {
		r.noteImportTargetReindexes(direct)
		r.applyUnresolvedTargetReindexes(targeter, direct)
	}
	if len(fallback) > 0 {
		r.reindexAttributionIdentitiesBatched(finder, fallback, rewrite)
	}
}

// reindexAttributionIdentitiesBatched exact-refetches census candidates in
// bounded pages. Re-fetching immediately before each pass preserves sequential
// attribution semantics: an edge retargeted or removed after the census no
// longer exists under its old logical identity and is therefore not
// resurrected. The finder never receives more than the existing attribution
// payload bound, and each page is persisted before its full rows are released.
func (r *Resolver) reindexAttributionIdentitiesBatched(
	finder graph.EdgeIdentityBatchFinder,
	identities []graph.EdgeIdentity,
	rewrite func(*graph.Edge) string,
) {
	if finder == nil || rewrite == nil {
		return
	}
	for start := 0; start < len(identities); start += attributionReindexBatchSize {
		end := start + attributionReindexBatchSize
		if end > len(identities) {
			end = len(identities)
		}
		page := identities[start:end]
		current := finder.FindEdgesByIdentities(page)
		collector := newAttributionReindexCollector(r)
		for _, identity := range page {
			if edge := current[identity]; edge != nil {
				collector.add(edge, rewrite(edge))
			}
		}
		collector.flush()
	}
}
