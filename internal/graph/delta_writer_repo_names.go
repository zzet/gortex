package graph

import "strings"

// The resolver's warm lookup asks for its pending references' names by
// repository and language (FindNodesByResolverNameScopes). A DeltaWriter
// without that capability was answered through the adapter fallback: one
// unscoped name read over every tracked repository, filtered in Go, so a
// common name ("New", "String", "Close") brought in every repository's
// same-named rows on every save. The delta composes the scoped read instead:
// the store at the bottom of the stack keeps the repository and language
// predicates, and each layer contributes its own matching nodes, each row
// kept only when no layer above its level speaks for its identity (the same
// composition rule as composedFindNodesByNames).

var _ ResolverNameScopeFinder = (*DeltaWriter)(nil)
var _ RepoNamesNodeFinder = (*DeltaWriter)(nil)

// FindNodesByResolverNameScopes implements ResolverNameScopeFinder through
// the stack.
func (dw *DeltaWriter) FindNodesByResolverNameScopes(scopes []ResolverNameScope) ([]map[string][]*Node, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	s := dw.stack()
	base, ok := s.base.(RepoLanguageNameFinder)
	if !ok {
		return resolverNameScopesFromNames(dw, scopes)
	}
	out := make([]map[string][]*Node, len(scopes))
	for i, scope := range scopes {
		names := uniqueNonEmptyNames(scope.Names)
		if len(names) == 0 {
			continue
		}
		var hits map[string][]*Node
		if scope.AllRepos {
			if names, ok := s.base.(batchedNamesReader); ok && dw.splitsCache(s) {
				hits := dw.cachedAllRepoNames(s, names, uniqueNonEmptyNames(scope.Names), scope.Languages)
				for name := range hits {
					resolverNameScopeSort(hits[name], scope)
				}
				out[i] = hits
				continue
			}
			global, err := resolverNameScopesFromNames(dw, []ResolverNameScope{scope})
			if err != nil {
				return nil, err
			}
			out[i] = global[0]
			continue
		}
		if dw.splitsCache(s) {
			hits = dw.cachedRepoNames(s, base, names, scope.RepoPrefix, scope.Languages)
		} else {
			hits = dw.composeRepoNames(s, base.FindNodesByNamesInRepoLanguages(names, scope.RepoPrefix, scope.Languages),
				names, scope.RepoPrefix, scope.Languages)
		}
		for name := range hits {
			resolverNameScopeSort(hits[name], scope)
		}
		out[i] = hits
	}
	return out, nil
}

// FindNodesByNamesInRepo implements RepoNamesNodeFinder through the stack.
func (dw *DeltaWriter) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*Node {
	names = uniqueNonEmptyNames(names)
	if len(names) == 0 {
		return nil
	}
	s := dw.stack()
	if hits, ok := dw.stackCachedRepoNames(s, names, repoPrefix, nil, false); ok {
		return hits
	}
	if base, ok := s.base.(RepoNamesNodeFinder); ok {
		return dw.composeRepoNames(s, base.FindNodesByNamesInRepo(names, repoPrefix), names, repoPrefix, nil)
	}
	out := make(map[string][]*Node, len(names))
	for name, nodes := range dw.FindNodesByNames(names) {
		for _, n := range nodes {
			if n != nil && n.RepoPrefix == repoPrefix {
				out[name] = append(out[name], n)
			}
		}
	}
	return out
}

// composeRepoNames composes the bottom store's scoped rows (fromBase) with
// every layer's nodes of names in repoPrefix and, when languages is nonempty,
// one of languages. The returned nodes are the caller's copies.
func (dw *DeltaWriter) composeRepoNames(s deltaStack, fromBase map[string][]*Node, names []string, repoPrefix string, languages []string) map[string][]*Node {
	out := dw.composeRepoNamesRaw(s, fromBase, names, repoPrefix, languages)
	for name, nodes := range out {
		out[name] = cloneDeltaNodes(nodes)
	}
	return out
}

// composeRepoNamesRaw is composeRepoNames without the copies: the rows are
// the readers' own.
func (dw *DeltaWriter) composeRepoNamesRaw(s deltaStack, fromBase map[string][]*Node, names []string, repoPrefix string, languages []string) map[string][]*Node {
	inScope := repoNameScope(repoPrefix, languages)
	out := make(map[string][]*Node, len(names))
	for name, nodes := range fromBase {
		for _, n := range nodes {
			if inScope(n) && !s.hiddenAbove(n.ID, 0) {
				out[name] = append(out[name], n)
			}
		}
	}
	for i, l := range s.layers {
		if l == OverlayLayerReader(dw.layer) {
			for _, name := range names {
				for _, n := range dw.work.FindNodesByName(name) {
					if inScope(n) && !s.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
			continue
		}
		if scoped, ok := l.(OverlayLayerScopedNameReader); ok {
			// The layer's query keeps the repository and languages: only
			// in-scope rows are read.
			for name, nodes := range scoped.LayerNodesByNamesInRepoLanguages(names, repoPrefix, languages) {
				dw.noteLayerRowsFor("repo_names", len(nodes))
				for _, n := range nodes {
					if inScope(n) && !s.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
			continue
		}
		if p, ok := l.(OverlayLayerProjectionReader); ok {
			for name, nodes := range p.LayerNodesByNames(names) {
				dw.noteLayerRowsFor("repo_names", len(nodes))
				for _, n := range nodes {
					if inScope(n) && !s.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
			continue
		}
		index := dw.nameIndexFor(l)
		for _, name := range names {
			for _, n := range index[name] {
				if inScope(n) && !s.hiddenAbove(n.ID, i+1) {
					out[name] = append(out[name], n)
				}
			}
		}
	}
	for name, nodes := range out {
		if len(nodes) == 0 {
			delete(out, name)
		}
	}
	return out
}

// OverlayLayerScopedNameReader is a layer that reads nodes by name scoped to
// one repository and a language set in its own query.
type OverlayLayerScopedNameReader interface {
	LayerNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*Node
}

func uniqueNonEmptyNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// cachedRepoNames is composeRepoNames with the part of the stack the cache is
// kept for kept per stack: the names' rows its immutable layers compose over
// the bottom store are read once per stack, and the layers above it (the
// dirty chain's and the delta's own) are applied per read.
func (dw *DeltaWriter) cachedRepoNames(s deltaStack, base RepoLanguageNameFinder, names []string, repoPrefix string, languages []string) map[string][]*Node {
	k, _ := dw.cacheSplit(s)
	below := deltaStack{base: s.base, layers: s.layers[:k]}
	scopeKey := repoPrefix + "\x00" + strings.Join(append([]string(nil), languages...), ",")
	kept := dw.baseCache.stackNames(scopeKey, names, func(missing []string) map[string][]*Node {
		return dw.composeRepoNamesRaw(below, base.FindNodesByNamesInRepoLanguages(missing, repoPrefix, languages), missing, repoPrefix, languages)
	})
	inScope := repoNameScope(repoPrefix, languages)
	out := make(map[string][]*Node, len(names))
	for _, name := range names {
		for _, n := range kept[name] {
			if !s.hiddenAbove(n.ID, k) {
				out[name] = append(out[name], n)
			}
		}
	}
	dw.upperLayerNames(s, k, names, inScope, func(l OverlayLayerReader) map[string][]*Node {
		if scoped, ok := l.(OverlayLayerScopedNameReader); ok {
			rows := scoped.LayerNodesByNamesInRepoLanguages(names, repoPrefix, languages)
			for _, nodes := range rows {
				dw.noteLayerRowsFor("repo_names", len(nodes))
			}
			return rows
		}
		return nil
	}, out)
	for name, nodes := range out {
		if len(nodes) == 0 {
			delete(out, name)
			continue
		}
		out[name] = cloneDeltaNodes(nodes)
	}
	return out
}

// splitsCache reports whether a projection cache is installed over a stack
// the delta's reads can split at (cacheSplit).
func (dw *DeltaWriter) splitsCache(s deltaStack) bool {
	_, ok := dw.cacheSplit(s)
	return ok
}

// upperLayerNames appends, per name, the in-scope nodes every layer from k up
// carries under it, each kept only when no layer above that layer speaks for
// its identity: the delta's own from its working graph, an immutable layer
// from its scoped read when scoped answers for it (non-nil), else from its
// generation-scoped name projection or its name index.
func (dw *DeltaWriter) upperLayerNames(s deltaStack, k int, names []string, inScope func(*Node) bool, scoped func(OverlayLayerReader) map[string][]*Node, out map[string][]*Node) {
	for i := k; i < len(s.layers); i++ {
		l := s.layers[i]
		keep := func(name string, n *Node) {
			if inScope(n) && !s.hiddenAbove(n.ID, i+1) {
				out[name] = append(out[name], n)
			}
		}
		if l == OverlayLayerReader(dw.layer) {
			for _, name := range names {
				for _, n := range dw.work.FindNodesByName(name) {
					keep(name, n)
				}
			}
			continue
		}
		if rows, ok := dw.chainRowsFor(l); ok {
			for _, name := range names {
				for _, n := range rows.byName[name] {
					keep(name, n)
				}
			}
			continue
		}
		if scoped != nil {
			if rows := scoped(l); rows != nil {
				for _, name := range names {
					for _, n := range rows[name] {
						keep(name, n)
					}
				}
				continue
			}
		}
		if p, ok := l.(OverlayLayerProjectionReader); ok {
			rows := p.LayerNodesByNames(names)
			for _, nodes := range rows {
				dw.noteLayerRowsFor("repo_names", len(nodes))
			}
			for _, name := range names {
				for _, n := range rows[name] {
					keep(name, n)
				}
			}
			continue
		}
		index := dw.nameIndexFor(l)
		for _, name := range names {
			for _, n := range index[name] {
				keep(name, n)
			}
		}
	}
}

// stackCachedRepoNames answers a repository's (and, when languages is
// nonempty, its languages') name read from the stack's name cache when one is
// installed over a delta: ok is false otherwise. Each bucket keeps the
// composition's order (the stack's rows, then the delta's), or is ordered by
// identity when byID is set.
func (dw *DeltaWriter) stackCachedRepoNames(s deltaStack, names []string, repoPrefix string, languages []string, byID bool) (map[string][]*Node, bool) {
	base, ok := s.base.(RepoLanguageNameFinder)
	if !ok || !dw.splitsCache(s) {
		return nil, false
	}
	hits := dw.cachedRepoNames(s, base, names, repoPrefix, languages)
	if byID {
		for name := range hits {
			sortNodesByID(hits[name])
		}
	}
	return hits, true
}

// repoNameScope is composeRepoNames' row predicate.
func repoNameScope(repoPrefix string, languages []string) func(*Node) bool {
	wantLanguage := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		wantLanguage[language] = struct{}{}
	}
	return func(n *Node) bool {
		if n == nil || n.RepoPrefix != repoPrefix {
			return false
		}
		if len(wantLanguage) > 0 {
			if _, ok := wantLanguage[n.Language]; !ok {
				return false
			}
		}
		return true
	}
}

// cachedAllRepoNames is the every-repository scope of the resolver's name
// read (the extern references' candidates) with the part of the stack the
// cache is kept for kept per stack, as cachedRepoNames keeps a repository's:
// the bottom store's batched name read and that part's rows are read once per
// stack and name, and the layers above it are applied per read.
func (dw *DeltaWriter) cachedAllRepoNames(s deltaStack, base batchedNamesReader, names []string, languages []string) map[string][]*Node {
	k, _ := dw.cacheSplit(s)
	below := deltaStack{base: s.base, layers: s.layers[:k]}
	inScope := languageNameScope(languages)
	scopeKey := "*\x00" + strings.Join(append([]string(nil), languages...), ",")
	kept := dw.baseCache.stackNames(scopeKey, names, func(missing []string) map[string][]*Node {
		out := make(map[string][]*Node, len(missing))
		for name, nodes := range base.FindNodesByNames(missing) {
			for _, n := range nodes {
				if inScope(n) && !below.hiddenAbove(n.ID, 0) {
					out[name] = append(out[name], n)
				}
			}
		}
		for i, l := range below.layers {
			if p, ok := l.(OverlayLayerProjectionReader); ok {
				for name, nodes := range p.LayerNodesByNames(missing) {
					dw.noteLayerRowsFor("repo_names", len(nodes))
					for _, n := range nodes {
						if inScope(n) && !below.hiddenAbove(n.ID, i+1) {
							out[name] = append(out[name], n)
						}
					}
				}
				continue
			}
			index := dw.nameIndexFor(l)
			for _, name := range missing {
				for _, n := range index[name] {
					if inScope(n) && !below.hiddenAbove(n.ID, i+1) {
						out[name] = append(out[name], n)
					}
				}
			}
		}
		return out
	})
	out := make(map[string][]*Node, len(names))
	for _, name := range names {
		for _, n := range kept[name] {
			if !s.hiddenAbove(n.ID, k) {
				out[name] = append(out[name], n)
			}
		}
	}
	dw.upperLayerNames(s, k, names, inScope, nil, out)
	for name, nodes := range out {
		if len(nodes) == 0 {
			delete(out, name)
			continue
		}
		out[name] = cloneDeltaNodes(nodes)
	}
	return out
}

// languageNameScope keeps a node of one of languages (every node when
// languages is empty).
func languageNameScope(languages []string) func(*Node) bool {
	want := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		want[language] = struct{}{}
	}
	return func(n *Node) bool {
		if n == nil {
			return false
		}
		if len(want) > 0 {
			if _, ok := want[n.Language]; !ok {
				return false
			}
		}
		return true
	}
}
