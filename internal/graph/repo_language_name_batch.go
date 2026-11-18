package graph

import "sort"

// ResolverNameScope is one correlated candidate request for the resolver's
// page-local warm cache. RepoPrefix is exact unless AllRepos is true. Languages
// is an allow-list; an empty slice is the conservative all-language fallback
// used only when the source language is unknown. Names are exact identifiers.
type ResolverNameScope struct {
	RepoPrefix string
	AllRepos   bool
	Languages  []string
	Names      []string
}

// ResolverNameScopeFinder is the optional SQLite-first, set-oriented resolver
// lookup. One call covers every repository/language/name scope in a pending
// page. Results correspond positionally to scopes and are keyed by requested
// name. An error means the result is incomplete and MUST NOT be treated as an
// authoritative negative cache.
type ResolverNameScopeFinder interface {
	FindNodesByResolverNameScopes(scopes []ResolverNameScope) ([]map[string][]*Node, error)
}

// FindNodesByResolverNameScopes uses the correlated backend capability when it
// is available. Adapter stores retain correctness with one global batched-name
// read followed by in-process scope filtering: no per-scope or per-name store
// calls, and no AllNodes materialisation.
func FindNodesByResolverNameScopes(store Store, scopes []ResolverNameScope) ([]map[string][]*Node, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	if finder, ok := store.(ResolverNameScopeFinder); ok {
		return finder.FindNodesByResolverNameScopes(scopes)
	}
	return resolverNameScopesFromNames(store, scopes)
}

// resolverNameScopesFromNames answers the scopes from one unscoped batched
// name lookup, filtering each scope in Go.
func resolverNameScopesFromNames(store batchedNamesReader, scopes []ResolverNameScope) ([]map[string][]*Node, error) {
	nameSet := make(map[string]struct{})
	for _, scope := range scopes {
		for _, name := range scope.Names {
			if name != "" {
				nameSet[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	globalHits := store.FindNodesByNames(names)

	out := make([]map[string][]*Node, len(scopes))
	for i, scope := range scopes {
		wantedLanguages := make(map[string]struct{}, len(scope.Languages))
		for _, language := range scope.Languages {
			wantedLanguages[language] = struct{}{}
		}
		wantedNames := make(map[string]struct{}, len(scope.Names))
		for _, name := range scope.Names {
			if name != "" {
				wantedNames[name] = struct{}{}
			}
		}
		if len(wantedNames) == 0 {
			continue
		}
		hits := make(map[string][]*Node, len(wantedNames))
		for name := range wantedNames {
			for _, node := range globalHits[name] {
				if node == nil {
					continue
				}
				if !scope.AllRepos && node.RepoPrefix != scope.RepoPrefix {
					continue
				}
				if len(wantedLanguages) > 0 {
					if _, ok := wantedLanguages[node.Language]; !ok {
						continue
					}
				}
				hits[name] = append(hits[name], node)
			}
			resolverNameScopeSort(hits[name], scope)
		}
		out[i] = hits
	}
	return out, nil
}

// resolverNameScopeSort defines candidate iteration order independently of the
// backend query plan. That matters because several conservative resolver
// fallbacks deliberately choose the first equally-ranked candidate.
func resolverNameScopeSort(nodes []*Node, scope ResolverNameScope) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if scope.AllRepos && a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if len(scope.Languages) > 0 && a.Language != b.Language {
			return a.Language < b.Language
		}
		return a.ID < b.ID
	})
}

// RepoLanguageNameFinder is the optional set-oriented lookup used by the
// resolver's disk-first warm cache. Implementations must keep repository,
// language, and name predicates inside the backend and return only matching
// rows. An empty languages slice means "all languages in this repository";
// callers use that only when the source language is unknown.
type RepoLanguageNameFinder interface {
	FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*Node
}

// FindNodesByNamesInRepoLanguages uses the backend capability when available.
// The conservative fallback still performs one repository projection per
// resolver group: it never loops over names and never materialises AllNodes.
func FindNodesByNamesInRepoLanguages(store Store, names []string, repoPrefix string, languages []string) map[string][]*Node {
	if finder, ok := store.(RepoLanguageNameFinder); ok {
		return finder.FindNodesByNamesInRepoLanguages(names, repoPrefix, languages)
	}

	wantedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name != "" {
			wantedNames[name] = struct{}{}
		}
	}
	if len(wantedNames) == 0 {
		return nil
	}
	wantedLanguages := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		wantedLanguages[language] = struct{}{}
	}

	out := make(map[string][]*Node, len(wantedNames))
	for _, node := range store.GetRepoNodes(repoPrefix) {
		if node == nil {
			continue
		}
		if _, ok := wantedNames[node.Name]; !ok {
			continue
		}
		if len(wantedLanguages) > 0 {
			if _, ok := wantedLanguages[node.Language]; !ok {
				continue
			}
		}
		out[node.Name] = append(out[node.Name], node)
	}
	return out
}
