package resolver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

type resolverNameLookupScope struct {
	repo        string
	languageKey string
}

type resolverNameLookupGroup struct {
	scope     resolverNameLookupScope
	languages []string
	names     map[string]struct{}
}

// resolverCompatibleLanguages returns the stable cache key and exact promoted
// language values that may bind a source language. The empty candidate language
// is always included because parsers and synthetic definition producers may
// emit language-neutral nodes. Unknown/empty and unclassified source languages
// return nil, the conservative "all languages in this repository" fallback.
// Only language families and standalone languages with proven isolation are
// narrowed: host/template parsers such as Vue, Svelte, Astro, and HTML emit
// their own language while legitimately referring to JavaScript/TypeScript.
func resolverCompatibleLanguages(language string) (string, []string) {
	language = strings.ToLower(strings.TrimSpace(language))
	if language == "" {
		return "unknown", nil
	}
	switch languageFamily(language) {
	case "jvm":
		return "family:jvm", []string{"", "java", "kotlin", "scala"}
	case "apple":
		return "family:apple", []string{"", "swift", "objc", "objective-c", "objectivec"}
	case "web":
		return "family:web", []string{"", "typescript", "ts", "tsx", "javascript", "js", "jsx"}
	case "c":
		return "family:c", []string{"", "c", "cpp", "c++", "cxx"}
	case "dotnet":
		return "family:dotnet", []string{"", "csharp", "c#", "fsharp", "f#", "razor"}
	}
	switch language {
	case "go", "python", "rust":
		return "language:" + language, []string{"", language}
	default:
		// Share one repo-scoped group across every unclassified host language.
		// The grouped query remains set-oriented and bounded by repo, not edges.
		return "unclassified", nil
	}
}

func resolverNameScope(repo, language string) (resolverNameLookupScope, []string) {
	key, languages := resolverCompatibleLanguages(language)
	return resolverNameLookupScope{repo: repo, languageKey: key}, languages
}

func (r *Resolver) resolverNameScopeForEdge(e *graph.Edge, repo string) (resolverNameLookupScope, []string) {
	var language string
	if e != nil {
		source := r.nodeByID[e.From]
		// Direct/single-file resolution may run without a page cache. One source
		// lookup there preserves language filtering; the ResolveAll hot path has
		// a non-nil authoritative ID cache and never takes this branch.
		if source == nil && r.nodeByID == nil {
			source = r.graph.GetNode(e.From)
		}
		if source != nil {
			if repo == "" {
				repo = source.RepoPrefix
			}
			language = source.Language
		}
	}
	return resolverNameScope(repo, language)
}

func addResolverLookupName(groups map[resolverNameLookupScope]*resolverNameLookupGroup, scope resolverNameLookupScope, languages []string, name string) {
	if name == "" {
		return
	}
	group := groups[scope]
	if group == nil {
		group = &resolverNameLookupGroup{
			scope:     scope,
			languages: append([]string(nil), languages...),
			names:     make(map[string]struct{}),
		}
		groups[scope] = group
	}
	group.names[name] = struct{}{}
}

// warmRepoLanguageNameCache groups a pending page by exact source repository
// and compatible language family, then submits every group in one correlated
// backend call. Explicit extern targets use one global scope per language family
// rather than being copied across every repository. Results are installed only
// after the complete query succeeds, so failures never become negative cache
// entries.
func (r *Resolver) warmRepoLanguageNameCache(pending []*graph.Edge) (groupCount int, queriedNames int, err error) {
	groups := make(map[resolverNameLookupScope]*resolverNameLookupGroup)
	externGroups := make(map[string]*resolverNameLookupGroup)

	for _, edge := range pending {
		if edge == nil {
			continue
		}
		source := r.nodeByID[edge.From]
		repo := ""
		language := ""
		if source != nil {
			repo = source.RepoPrefix
			language = source.Language
		} else {
			repo = graph.RepoPrefixOfID(edge.From)
		}
		target := graph.UnresolvedName(edge.To)
		name := identifierFromTarget(target)
		if strings.HasPrefix(target, "extern::") {
			if name != "" {
				globalScope, globalLanguages := resolverNameScope("", language)
				global := externGroups[globalScope.languageKey]
				if global == nil {
					global = &resolverNameLookupGroup{
						scope:     globalScope,
						languages: append([]string(nil), globalLanguages...),
						names:     make(map[string]struct{}),
					}
					externGroups[globalScope.languageKey] = global
				}
				global.names[name] = struct{}{}
			}
			continue
		}

		scope, languages := resolverNameScope(repo, language)
		addResolverLookupName(groups, scope, languages, name)
		if receiverType := edgeReceiverType(edge); receiverType != "" {
			addResolverLookupName(groups, scope, languages, receiverType)
		}
	}

	ordered := make([]resolverNameLookupScope, 0, len(groups))
	for scope := range groups {
		ordered = append(ordered, scope)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].repo != ordered[j].repo {
			return ordered[i].repo < ordered[j].repo
		}
		return ordered[i].languageKey < ordered[j].languageKey
	})
	externKeys := make([]string, 0, len(externGroups))
	for languageKey := range externGroups {
		externKeys = append(externKeys, languageKey)
	}
	sort.Strings(externKeys)

	groupCount = len(ordered) + len(externKeys)
	// Repository-scoped names may be answered from the pass-scoped hot cache
	// (including cached negatives — repository definition nodes cannot be
	// created while the pass holds the resolve mutex); only cache misses are
	// queried. Extern/AllRepos groups are never cached: extern candidate
	// nodes can be materialised mid-pass.
	cachedByScope := make(map[resolverNameLookupScope]map[string][]*graph.Node)
	requests := make([]graph.ResolverNameScope, 0, groupCount)
	for _, scope := range ordered {
		group := groups[scope]
		names := make([]string, 0, len(group.names))
		for name := range group.names {
			if r.hotCache != nil && scope.repo != "" {
				if hits, ok := r.hotCache.getNames(hotNameKey(scope.repo, scope.languageKey, name)); ok {
					cached := cachedByScope[scope]
					if cached == nil {
						cached = make(map[string][]*graph.Node)
						cachedByScope[scope] = cached
					}
					cached[name] = hits
					continue
				}
			}
			names = append(names, name)
		}
		sort.Strings(names)
		queriedNames += len(names)
		requests = append(requests, graph.ResolverNameScope{
			RepoPrefix: scope.repo,
			Languages:  append([]string(nil), group.languages...),
			Names:      names,
		})
	}
	for _, languageKey := range externKeys {
		group := externGroups[languageKey]
		names := make([]string, 0, len(group.names))
		for name := range group.names {
			names = append(names, name)
		}
		sort.Strings(names)
		queriedNames += len(names)
		requests = append(requests, graph.ResolverNameScope{
			AllRepos:  true,
			Languages: append([]string(nil), group.languages...),
			Names:     names,
		})
	}

	results, lookupErr := graph.FindNodesByResolverNameScopes(r.graph, requests)
	if lookupErr != nil {
		return groupCount, queriedNames, lookupErr
	}
	if len(results) != len(requests) {
		return groupCount, queriedNames, fmt.Errorf("resolver name scope lookup returned %d results for %d scopes", len(results), len(requests))
	}

	nodesByRepoLanguageName := make(map[resolverNameLookupScope]map[string][]*graph.Node, len(groups))
	nodesByRepoName := make(map[string]map[string][]*graph.Node)
	nodesByExternLanguageName := make(map[string]map[string][]*graph.Node, len(externGroups))
	for i, scope := range ordered {
		hits := results[i]
		if hits == nil {
			hits = make(map[string][]*graph.Node, len(requests[i].Names))
		}
		for _, name := range requests[i].Names {
			if _, ok := hits[name]; !ok {
				hits[name] = nil
			}
			if r.hotCache != nil && scope.repo != "" {
				r.hotCache.putNames(hotNameKey(scope.repo, scope.languageKey, name), hits[name])
			}
		}
		for name, cachedHits := range cachedByScope[scope] {
			hits[name] = cachedHits
		}
		nodesByRepoLanguageName[scope] = hits

		repoHits := nodesByRepoName[scope.repo]
		if repoHits == nil {
			repoHits = make(map[string][]*graph.Node)
			nodesByRepoName[scope.repo] = repoHits
		}
		for name, nodes := range hits {
			repoHits[name] = appendUniqueResolverNodes(repoHits[name], nodes)
		}
	}
	for i, languageKey := range externKeys {
		resultIndex := len(ordered) + i
		hits := results[resultIndex]
		if hits == nil {
			hits = make(map[string][]*graph.Node, len(requests[resultIndex].Names))
		}
		for _, name := range requests[resultIndex].Names {
			if _, ok := hits[name]; !ok {
				hits[name] = nil
			}
		}
		nodesByExternLanguageName[languageKey] = hits
	}

	// Install the page cache atomically. A SQLite/query/scan error therefore
	// cannot turn an incomplete result into an authoritative negative cache.
	r.nodesByRepoLanguageName = nodesByRepoLanguageName
	r.nodesByRepoName = nodesByRepoName
	r.nodesByExternLanguageName = nodesByExternLanguageName
	return groupCount, queriedNames, nil
}

func appendUniqueResolverNodes(dst, src []*graph.Node) []*graph.Node {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	for _, node := range dst {
		if node != nil {
			seen[node.ID] = struct{}{}
		}
	}
	for _, node := range src {
		if node == nil {
			continue
		}
		if _, duplicate := seen[node.ID]; duplicate {
			continue
		}
		seen[node.ID] = struct{}{}
		dst = append(dst, node)
	}
	return dst
}

// cachedFindNodesByNameInRepoForEdge selects the exact repository/language
// group warmed for this pending edge. Both hits and misses are authoritative,
// so workers never fall through into per-edge SQLite lookups.
func (r *Resolver) cachedFindNodesByNameInRepoForEdge(name, repo string, edge *graph.Edge) []*graph.Node {
	if name == "" {
		return nil
	}
	scope, languages := r.resolverNameScopeForEdge(edge, repo)
	if r.nodesByRepoLanguageName != nil {
		if byName, ok := r.nodesByRepoLanguageName[scope]; ok {
			if hits, warmed := byName[name]; warmed {
				return genericInstantiationOnly(edge, withoutAttributedTerminals(hits))
			}
		}
	}
	return genericInstantiationOnly(edge, withoutAttributedTerminals(r.namesInScopeMiss(scope, languages, name)))
}

// namesInScopeMiss answers a name the warm-up did not read, through the
// store's scoped name finder when it has one (a per-file delta keeps those
// answers per stack), and keeps the answer for the rest of the pass.
//
// It runs inside ResolveAllContext's parallel workers. The warmed page cache
// (nodesByRepoLanguageName) is installed whole before the workers start and
// only read by them, so the answers kept here go to a map of their own under
// namesMissMu; writing them into the page cache raced every worker's read of
// it (fatal "concurrent map read and map write").
func (r *Resolver) namesInScopeMiss(scope resolverNameLookupScope, languages []string, name string) []*graph.Node {
	finder, ok := r.graph.(graph.ResolverNameScopeFinder)
	if !ok {
		return graph.FindNodesByNamesInRepoLanguages(r.graph, []string{name}, scope.repo, languages)[name]
	}
	r.namesMissMu.RLock()
	hits, kept := r.namesMiss[scope][name]
	r.namesMissMu.RUnlock()
	if kept {
		return hits
	}
	results, err := finder.FindNodesByResolverNameScopes([]graph.ResolverNameScope{{
		RepoPrefix: scope.repo, Languages: append([]string(nil), languages...), Names: []string{name},
	}})
	if err != nil || len(results) != 1 {
		return graph.FindNodesByNamesInRepoLanguages(r.graph, []string{name}, scope.repo, languages)[name]
	}
	hits = results[0][name]
	if r.nodesByRepoLanguageName == nil {
		// No page is warmed (a single-file resolve): nothing to keep it for,
		// and nothing would clear it before the graph changes.
		return hits
	}
	r.namesMissMu.Lock()
	if r.namesMiss == nil {
		r.namesMiss = make(map[resolverNameLookupScope]map[string][]*graph.Node)
	}
	byName := r.namesMiss[scope]
	if byName == nil {
		byName = make(map[string][]*graph.Node)
		r.namesMiss[scope] = byName
	}
	byName[name] = hits
	r.namesMissMu.Unlock()
	return hits
}

// genericInstantiationOnly narrows a call edge's candidates when the
// extractor marked it as an explicit generic instantiation.
//
// Some grammars spell an instantiation exactly like something unrelated:
// Go parses `Zero[int]()` the same as indexing a func-valued map or
// struct field, and `One[int](1)` the same as a conversion to a generic
// type. Emitting all three is what recovers the real generic calls, but
// it also hands the ordinary name tiers candidates a genuine index or
// conversion should never reach — and those tiers bound them, pointing
// `t.handlers[k]()` at an unrelated same-named method.
//
// A true instantiation can only target a declaration that DECLARES type
// parameters. Requiring that is exact rather than heuristic, and it is
// what separates the call from both impostors: a func-valued field and a
// shadowing local name nothing generic. Narrowing only — an unmarked
// edge, or one whose candidates carry no evidence either way, is
// returned untouched. An empty result leaves the edge unresolved, which
// is the correct answer for an index or a conversion.
//
// The returned slice is never the cached input when it filters, so the
// shared name cache is not mutated.
func genericInstantiationOnly(edge *graph.Edge, hits []*graph.Node) []*graph.Node {
	if edge == nil || edge.Meta == nil || len(hits) == 0 {
		return hits
	}
	if marked, _ := edge.Meta[graph.MetaGenericInstantiation].(bool); !marked {
		return hits
	}
	out := make([]*graph.Node, 0, len(hits))
	for _, n := range hits {
		if n == nil || n.Meta == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if tp, ok := n.Meta["type_params"]; ok && tp != nil {
			out = append(out, n)
		}
	}
	return out
}

// cachedFindExternNodesByName keeps explicit extern candidates isolated by the
// source language family. An error is surfaced instead of being converted into
// a negative result, so a transient SQLite failure cannot classify the edge.
func (r *Resolver) cachedFindExternNodesByName(name string, edge *graph.Edge) ([]*graph.Node, error) {
	if name == "" {
		return nil, nil
	}
	scope, languages := r.resolverNameScopeForEdge(edge, "")
	if r.nodesByExternLanguageName != nil {
		if byName, ok := r.nodesByExternLanguageName[scope.languageKey]; ok {
			if hits, warmed := byName[name]; warmed {
				return hits, nil
			}
		}
	}
	requests := []graph.ResolverNameScope{
		{
			AllRepos:  true,
			Languages: languages,
			Names:     []string{name},
		},
	}
	results, err := graph.FindNodesByResolverNameScopes(r.graph, requests)
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("resolver extern lookup returned %d results for one scope", len(results))
	}
	return results[0][name], nil
}
