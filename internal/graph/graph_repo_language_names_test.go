package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGraphRepoLanguageNamesMatchesScan(t *testing.T) {
	g := New()
	finder, ok := any(g).(RepoLanguageNameFinder)
	require.True(t, ok, "*Graph must implement RepoLanguageNameFinder")

	g.AddBatch([]*Node{
		{ID: "a-go-1", Name: "Shared", RepoPrefix: "a", Language: "go"},
		{ID: "a-go-2", Name: "Shared", RepoPrefix: "a", Language: "go"},
		{ID: "a-python", Name: "Shared", RepoPrefix: "a", Language: "python"},
		{ID: "b-go", Name: "Shared", RepoPrefix: "b", Language: "go"},
		{ID: "b-python", Name: "Shared", RepoPrefix: "b", Language: "python"},
		{ID: "a-other", Name: "Other", RepoPrefix: "a", Language: "go"},
		{ID: "a-neutral", Name: "Shared", RepoPrefix: "a"},
		{ID: "unprefixed-go", Name: "Shared", Language: "go"},
		{ID: "unprefixed-python", Name: "Shared", Language: "python"},
		{ID: "unnamed", RepoPrefix: "a", Language: "go"},
	}, nil)

	// Embedding only Store hides the optional finder and exercises the scan fallback.
	scanStore := struct{ Store }{g}
	for _, tc := range []struct {
		name       string
		names      []string
		repoPrefix string
		languages  []string
	}{
		{name: "present and absent", names: []string{"Shared", "Other", "Missing"}, repoPrefix: "a", languages: []string{"go"}},
		{name: "second repository", names: []string{"Shared"}, repoPrefix: "b", languages: []string{"go"}},
		{name: "second language", names: []string{"Shared"}, repoPrefix: "a", languages: []string{"python"}},
		{name: "multiple languages", names: []string{"Shared"}, repoPrefix: "a", languages: []string{"go", "python"}},
		{name: "all languages", names: []string{"Shared"}, repoPrefix: "a"},
		{name: "empty language identifier", names: []string{"Shared"}, repoPrefix: "a", languages: []string{""}},
		{name: "empty repository", names: []string{"Shared"}, languages: []string{"go"}},
		{name: "empty repository all languages", names: []string{"Shared"}},
		{name: "missing repository", names: []string{"Shared"}, repoPrefix: "missing"},
		{name: "absent", names: []string{"Missing"}, repoPrefix: "a", languages: []string{"go"}},
		{name: "duplicates and empty name", names: []string{"Shared", "Shared", ""}, repoPrefix: "a", languages: []string{"go", "go"}},
		{name: "empty names", repoPrefix: "a", languages: []string{"go"}},
		{name: "only empty name", names: []string{""}, repoPrefix: "a", languages: []string{"go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := FindNodesByNamesInRepoLanguages(scanStore, tc.names, tc.repoPrefix, tc.languages)
			got := finder.FindNodesByNamesInRepoLanguages(tc.names, tc.repoPrefix, tc.languages)
			require.Len(t, got, len(want))
			for name, nodes := range want {
				assert.ElementsMatch(t, nodes, got[name], "nodes named %q", name)
			}
		})
	}
}
