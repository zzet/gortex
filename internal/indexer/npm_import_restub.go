package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Park only npm-linked incoming imports and their calls before replacing or
// removing a provider. Other references keep their existing eviction rules.
func restubTrackedNpmRepoIncoming(g graph.Store, repo string) {
	if repo == "" {
		return
	}
	var ids []string
	flush := func() {
		identities := graph.InEdgeIdentitiesByNodeIDs(g, ids)
		var sites []graph.EdgeSite
		for _, edges := range identities {
			for _, edge := range edges {
				if edge.Kind == graph.EdgeImports && graph.RepoPrefixOfID(edge.From) != repo {
					sites = append(sites, graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: graph.EdgeImports})
				}
			}
		}
		if len(sites) == 0 {
			ids = ids[:0]
			return
		}
		imports := g.GetEdgeCandidates(nil, sites)
		var targets []string
		for target, edges := range identities {
			marked := false
			for _, edge := range edges {
				if edge.Kind != graph.EdgeImports || graph.RepoPrefixOfID(edge.From) == repo {
					continue
				}
				for _, candidate := range imports.Site(edge.From, edge.Line, graph.EdgeImports) {
					if candidate.To == target {
						if spec, _ := candidate.Meta[resolver.NpmImportSpecifierMeta].(string); spec != "" {
							marked = true
						}
					}
				}
			}
			if marked {
				targets = append(targets, target)
			}
		}
		if len(targets) > 0 {
			incoming := g.GetInEdgesByNodeIDs(targets)
			evicted := make(map[string]struct{})
			for _, edges := range incoming {
				for _, edge := range edges {
					if edge != nil && graph.RepoPrefixOfID(edge.From) == repo {
						evicted[edge.From] = struct{}{}
					}
				}
			}
			restubTrackedNpmImportsFromView(g, evicted, incoming)
		}
		ids = ids[:0]
	}
	for node := range graph.RepoNodeIdentitiesSeq(g, []string{repo},
		graph.KindFile, graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant) {
		ids = append(ids, node.ID)
		if len(ids) == 512 {
			flush()
		}
	}
	if len(ids) > 0 {
		flush()
	}
}

func restubTrackedNpmImportsFromView(g graph.Store, evicted map[string]struct{}, incoming map[string][]*graph.Edge) []string {
	var consumerFiles []string
	consumers := make(map[string]map[string]string)
	var batch []graph.EdgeReindex
	for _, edges := range incoming {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeImports {
				continue
			}
			spec, _ := edge.Meta[resolver.NpmImportSpecifierMeta].(string)
			if _, sourceEvicted := evicted[edge.From]; spec == "" || sourceEvicted {
				continue
			}
			consumerFiles = appendUniqueSorted(consumerFiles, edge.From)
			oldTo := edge.To
			if _, name, symbol := strings.Cut(oldTo, "::"); symbol {
				if alias, _ := edge.Meta["import_alias"].(string); alias != "" {
					name = alias
				}
				if consumers[oldTo] == nil {
					consumers[oldTo] = make(map[string]string)
				}
				consumers[oldTo][edge.From] = name
			}
			edge.To = "external::" + spec
			edge.CrossRepo = false
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
		}
	}
	for target, files := range consumers {
		for _, edge := range incoming[target] {
			if edge == nil || edge.Kind != graph.EdgeCalls || !edge.CrossRepo {
				continue
			}
			if _, sourceEvicted := evicted[edge.From]; sourceEvicted {
				continue
			}
			name, imported := files[edge.FilePath]
			if !imported {
				continue
			}
			graph.StashRestubProvenance(edge)
			edge.To = graph.UnresolvedMarker + name
			edge.CrossRepo, edge.ConfidenceLabel = false, ""
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: target})
		}
	}

	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return consumerFiles
}

// npmConsumerFilesForRepo finds parked imports claimed by this provider and
// this repo's imports claimed by any tracked provider. Used after a full index
// or track; incremental saves carry exact consumer files from the restub step.
func (mi *MultiIndexer) npmConsumerFilesForRepo(repo string) []string {
	lookup := mi.npmPackageLookup()
	if lookup == nil {
		return nil
	}
	files := make(map[string]struct{})
	claims := make(map[string]map[string]resolver.NpmPackageTarget)
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
		if !strings.HasPrefix(edge.To, "external::") {
			continue
		}
		spec, _, _ := strings.Cut(strings.TrimPrefix(edge.To, "external::"), "::")
		providers, known := claims[spec]
		if !known {
			providers = lookup(spec)
			claims[spec] = providers
		}
		for dir := range providers {
			if dir == repo || strings.HasPrefix(dir, repo+"/") || graph.RepoPrefixOfID(edge.From) == repo {
				files[edge.From] = struct{}{}
				break
			}
		}
	}
	return sortedStringKeys(files)
}
