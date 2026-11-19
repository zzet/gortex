package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestSemanticBindingTypesAreRepoIsolatedBatchedAndReplaceable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	aOne := graph.SemanticBindingSite{RepoPrefix: "a", FilePath: "a/main.go", Line: 8, Name: "value"}
	aTwo := graph.SemanticBindingSite{RepoPrefix: "a", FilePath: "a/other.go", Line: 4, Name: "other"}
	bSame := graph.SemanticBindingSite{RepoPrefix: "b", FilePath: "b/main.go", Line: 8, Name: "value"}
	empty := graph.SemanticBindingSite{FilePath: "main.go", Line: 8, Name: "value"}

	require.NoError(t, store.ReplaceSemanticBindingTypes("a", []graph.SemanticBindingType{
		{Site: aOne, TypeName: "TypeA"},
		{Site: aTwo, TypeName: "TypeA2"},
	}))
	require.NoError(t, store.ReplaceSemanticBindingTypes("b", []graph.SemanticBindingType{{Site: bSame, TypeName: "TypeB"}}))
	require.NoError(t, store.ReplaceSemanticBindingTypes("", []graph.SemanticBindingType{{Site: empty, TypeName: "TypeEmpty"}}))

	got, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{aOne, aOne, aTwo, bSame, empty})
	require.NoError(t, err)
	assert.Equal(t, map[graph.SemanticBindingSite]string{
		aOne:  "TypeA",
		aTwo:  "TypeA2",
		bSame: "TypeB",
		empty: "TypeEmpty",
	}, got)
	files := make([]string, semanticBindingChunk+1)
	for i := range files {
		files[i] = aOne.FilePath
	}
	files = append(files, aTwo.FilePath, bSame.FilePath)
	projection, err := store.SemanticBindingTypesForFiles("a", files)
	require.NoError(t, err)
	assert.Equal(t, map[graph.SemanticBindingSite]string{aOne: "TypeA", aTwo: "TypeA2"}, projection)
	otherGeneration, err := store.AtGeneration(99).SemanticBindingTypesForFiles("a", files)
	require.NoError(t, err)
	assert.Empty(t, otherGeneration, "file projection must not borrow another generation's bindings")

	require.NoError(t, store.ReplaceSemanticBindingTypesForFiles("a", []string{aOne.FilePath}, []graph.SemanticBindingType{{Site: aOne, TypeName: "TypeANew"}}))
	got, err = store.SemanticBindingTypes([]graph.SemanticBindingSite{aOne, aTwo, bSame})
	require.NoError(t, err)
	assert.Equal(t, "TypeANew", got[aOne])
	assert.Equal(t, "TypeA2", got[aTwo], "file replacement must preserve sibling files")
	assert.Equal(t, "TypeB", got[bSame], "file replacement must preserve sibling repos")

	require.NoError(t, store.DeleteSemanticBindingTypesByFiles("a", []string{aTwo.FilePath}))
	got, err = store.SemanticBindingTypes([]graph.SemanticBindingSite{aOne, aTwo})
	require.NoError(t, err)
	assert.Equal(t, "TypeANew", got[aOne])
	assert.NotContains(t, got, aTwo)

	require.NoError(t, store.ReplaceSemanticBindingTypes("a", nil))
	got, err = store.SemanticBindingTypes([]graph.SemanticBindingSite{aOne, bSame, empty})
	require.NoError(t, err)
	assert.NotContains(t, got, aOne, "empty repo replacement must clear prior rows")
	assert.Equal(t, "TypeB", got[bSame])
	assert.Equal(t, "TypeEmpty", got[empty], "empty-prefix rows must remain isolated")
}

func TestSemanticBindingTypesSurviveWarmReopenAndEvictWithRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := Open(path)
	require.NoError(t, err)

	site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/main.go", Line: 5, Name: "value"}
	require.NoError(t, store.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{{Site: site, TypeName: "Widget"}}))
	store.AddNode(&graph.Node{ID: "repo/main.go::Value", Kind: graph.KindVariable, Name: "value", FilePath: site.FilePath, RepoPrefix: "repo"})
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	got, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.Equal(t, "Widget", got[site], "warm reopen must not require a retained compiler program")

	nodes, _ := store.EvictRepo("repo")
	require.Equal(t, 1, nodes)
	got, err = store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.NotContains(t, got, site, "repo eviction must remove side-table rows")
}

func TestSemanticBindingTypesPurgeAndOrphanLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	site := graph.SemanticBindingSite{RepoPrefix: "orphan", FilePath: "orphan/main.go", Line: 5, Name: "value"}
	require.NoError(t, store.ReplaceSemanticBindingTypes("orphan", []graph.SemanticBindingType{{Site: site, TypeName: "Widget"}}))

	assert.Equal(t, []string{"orphan"}, store.OrphanRepoPrefixes(nil), "binding-only residue must be discoverable")
	assert.Empty(t, store.OrphanRepoPrefixes([]string{"ORPHAN"}), "known-prefix matching remains case-insensitive")

	require.NoError(t, store.PurgeRepo("orphan"))
	got, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.NotContains(t, got, site, "repo purge must remove compiler binding rows")
	assert.Empty(t, store.OrphanRepoPrefixes(nil), "purged binding rows must not remain orphan residue")
}

func TestSemanticBindingTypesSoloToMultiRekeyDropsUnscopedPaths(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	oldSite := graph.SemanticBindingSite{FilePath: "main.go", Line: 5, Name: "value"}
	newSite := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/main.go", Line: 5, Name: "value"}
	require.NoError(t, store.ReplaceSemanticBindingTypes("", []graph.SemanticBindingType{{Site: oldSite, TypeName: "StaleWidget"}}))
	require.NoError(t, store.ReplaceSemanticBindingTypes("repo", []graph.SemanticBindingType{{Site: newSite, TypeName: "Widget"}}))

	require.NoError(t, store.RekeyRepoPrefix("", "repo"))
	got, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{oldSite, newSite})
	require.NoError(t, err)
	assert.NotContains(t, got, oldSite, "unscoped paths become invalid when solo indexing gains a repo prefix")
	assert.Equal(t, "Widget", got[newSite], "already re-indexed scoped rows must survive the rekey cleanup")
	assert.Equal(t, []string{"repo"}, store.OrphanRepoPrefixes(nil), "the scoped binding-only prefix remains visible to orphan accounting")
}
