package graph

var _ RepoLanguageNameFinder = (*Graph)(nil)

// FindNodesByNamesInRepoLanguages reads name buckets and filters them by exact
// repository and language. An empty languages slice accepts all languages.
func (g *Graph) FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*Node {
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
	for _, shard := range g.shards {
		shard.mu.RLock()
		for name := range wantedNames {
			for _, node := range shard.byName[name] {
				if node == nil || node.RepoPrefix != repoPrefix {
					continue
				}
				if len(wantedLanguages) > 0 {
					if _, ok := wantedLanguages[node.Language]; !ok {
						continue
					}
				}
				out[node.Name] = append(out[node.Name], node)
			}
		}
		shard.mu.RUnlock()
	}
	return out
}
