package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// searchSymbolsScopedContext is the request-scoped equivalent of
// Engine.SearchSymbolsScoped. It preserves ranked-candidate order while
// passing the caller context into the engine's contextual search path.
func searchSymbolsScopedContext(ctx context.Context, eng *query.Engine, queryText string, limit int, scope query.QueryOptions) []*graph.Node {
	if ctx == nil {
		ctx = context.Background()
	}
	ranked := eng.SearchSymbolsRankedContext(ctx, queryText, limit, scope, scope.RerankContext)
	if ctx.Err() != nil {
		return nil
	}
	nodes := make([]*graph.Node, 0, len(ranked))
	for _, candidate := range ranked {
		if candidate != nil && candidate.Node != nil {
			nodes = append(nodes, candidate.Node)
		}
	}
	return nodes
}

// fetchAndMergeBM25TimedContext is the request-scoped search-assist fetch.
// The legacy helper remains unchanged for callers that do not carry a request
// context. Canceled calls publish no partial candidate set.
func fetchAndMergeBM25TimedContext(ctx context.Context, eng *query.Engine, original string, expanded []string, fetchLimit int, scope query.QueryOptions, timings *query.SearchTimings) (merged []*graph.Node, primaryCount int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, 0
	}

	scope.SkipInnerRerank = true
	primaryStart := time.Now()
	primary := searchSymbolsScopedContext(ctx, eng, original, fetchLimit, scope)
	if ctx.Err() != nil {
		return nil, 0
	}
	primaryCount = len(primary)
	if timings != nil {
		timings.BM25PrimaryMS += time.Since(primaryStart).Milliseconds()
	}

	cleanedExpansion := make([]string, 0, len(expanded))
	for _, term := range expanded {
		if ctx.Err() != nil {
			return nil, 0
		}
		term = strings.TrimSpace(term)
		if term != "" {
			cleanedExpansion = append(cleanedExpansion, term)
		}
	}
	if len(cleanedExpansion) == 0 {
		return primary, primaryCount
	}

	seen := make(map[string]bool, len(primary)+fetchLimit)
	merged = make([]*graph.Node, 0, len(primary)+fetchLimit)
	for _, node := range primary {
		if node == nil || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		merged = append(merged, node)
	}

	combined := strings.Join(cleanedExpansion, " ")
	expansionScope := scope
	expansionScope.SkipExactNameSplice = true
	expansionStart := time.Now()
	extra := searchSymbolsScopedContext(ctx, eng, combined, fetchLimit, expansionScope)
	if ctx.Err() != nil {
		return nil, 0
	}
	if timings != nil {
		timings.BM25ExpansionMS += time.Since(expansionStart).Milliseconds()
	}
	for _, node := range extra {
		if node == nil || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		merged = append(merged, node)
	}

	// The exact-name rescue has no request-aware reader capability in the
	// captured contract. Bracket it so cancellation never starts it after a
	// canceled BM25 call and never publishes its partial result.
	if ctx.Err() != nil {
		return nil, 0
	}
	if rdr, ok := graphReaderFromEngine(eng); ok {
		nameMap := rdr.FindNodesByNames(cleanedExpansion)
		if ctx.Err() != nil {
			return nil, 0
		}
		for _, term := range cleanedExpansion {
			for _, node := range nameMap[term] {
				if ctx.Err() != nil {
					return nil, 0
				}
				if node == nil || seen[node.ID] {
					continue
				}
				if node.Kind == graph.KindFile || node.Kind == graph.KindImport {
					continue
				}
				if !scope.ScopeAllows(node) {
					continue
				}
				seen[node.ID] = true
				merged = append(merged, node)
			}
		}
	}
	return merged, primaryCount
}
