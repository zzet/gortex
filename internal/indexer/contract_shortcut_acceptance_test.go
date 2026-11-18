package indexer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func shortcutAcceptanceRecords(t *testing.T, g graph.Store) []contracts.Contract {
	t.Helper()
	return contracts.LoadRegistryFromGraph(g, "fixture").ByRepo("fixture")
}

func TestContractShortcutRouteLineShiftPersistsOwnerLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "routes.go")
	writeFile(t, path, unchangedTypedRouteSource)
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	before := shortcutAcceptanceRecords(t, g)
	require.Len(t, before, 1)
	writeFile(t, path, strings.Replace(unchangedTypedRouteSource, "func register", "\n\nfunc register", 1))
	require.NoError(t, idx.IndexFile(path))
	after := shortcutAcceptanceRecords(t, g)
	require.Len(t, after, 1)
	require.Equal(t, before[0].Line+2, after[0].Line, "persisted owner position must move; semantic-only equality is insufficient")
	clean := graph.New()
	full := newTestIndexer(clean)
	full.SetRepoPrefix("fixture")
	t.Cleanup(full.Close)
	_, err = full.Index(root)
	require.NoError(t, err)
	require.True(t, contractSetsEqual(shortcutAcceptanceRecords(t, clean), after))
}

func TestContractShortcutMixedBatchPreservesSameIDOwners(t *testing.T) {
	root := t.TempDir()
	sources := map[string]string{"a_routes.go": unchangedTypedRouteSource,
		"b_routes.go": strings.NewReplacer("func register", "func registerB", "func login", "func loginB", ", login)", ", loginB)", "func Value", "func ValueB").Replace(unchangedTypedRouteSource),
		"c_routes.go": strings.NewReplacer("func register", "func registerC", "func login", "func loginC", ", login)", ", loginC)", "func Value", "func ValueC").Replace(unchangedTypedRouteSource)}
	for path, src := range sources {
		writeFile(t, filepath.Join(root, path), src)
	}
	g := graph.New()
	initial := newTestIndexer(g)
	initial.SetRepoPrefix("fixture")
	t.Cleanup(initial.Close)
	_, err := initial.Index(root)
	require.NoError(t, err)
	before := shortcutAcceptanceRecords(t, g)
	require.Len(t, before, 3)
	require.Equal(t, before[0].ID, before[1].ID)
	require.Equal(t, before[1].ID, before[2].ID)
	probe := &contractHydrationProbe{Graph: g}
	idx := newTestIndexer(probe)
	idx.SetRepoPrefix("fixture")
	idx.storeRootPath(root)
	idx.SetFileMtimes(initial.FileMtimes())
	t.Cleanup(idx.Close)
	writeFile(t, filepath.Join(root, "a_routes.go"), strings.Replace(sources["a_routes.go"], "return 1", "return 2", 1))
	writeFile(t, filepath.Join(root, "c_routes.go"), strings.Replace(sources["c_routes.go"], "/login", "/changed", 1))
	_, err = idx.IncrementalReindexPaths(root, []string{"a_routes.go", "c_routes.go"})
	require.NoError(t, err)
	clean := graph.New()
	full := newTestIndexer(clean)
	full.SetRepoPrefix("fixture")
	t.Cleanup(full.Close)
	_, err = full.Index(root)
	require.NoError(t, err)
	actual, expected := shortcutAcceptanceRecords(t, g), shortcutAcceptanceRecords(t, clean)
	require.Len(t, actual, 3, "changing one owner must not discard same-ID unchanged siblings")
	require.True(t, contractSetsEqual(expected, actual), "compare all enriched owner metadata and positions")
	owners := map[string]int{}
	for _, record := range actual {
		owners[record.FilePath]++
	}
	require.Equal(t, map[string]int{"fixture/a_routes.go": 1, "fixture/b_routes.go": 1, "fixture/c_routes.go": 1}, owners)
}

func TestContractShortcutMountEditAndRemovalRefreshSiblingPaths(t *testing.T) {
	root := t.TempDir()
	users := "from fastapi import APIRouter\nrouter = APIRouter(prefix=\"/users\")\n@router.get(\"/{id}\")\ndef get_user(id: int):\n    return id\n"
	mounted := "from fastapi import FastAPI\nfrom .users import router\napp = FastAPI()\napp.include_router(router, prefix=\"/api\")\n"
	writeFile(t, filepath.Join(root, "users.py"), users)
	writeFile(t, filepath.Join(root, "main.py"), mounted)
	makeIndexer := func(g graph.Store) *Indexer {
		reg := parser.NewRegistry()
		reg.Register(languages.NewPythonExtractor())
		cfg := config.Default().Index
		cfg.Workers = 2
		idx := New(g, reg, cfg, zap.NewNop())
		idx.SetRepoPrefix("fixture")
		t.Cleanup(idx.Close)
		return idx
	}
	g := graph.New()
	idx := makeIndexer(g)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Contains(t, constantProviderPaths(g, "fixture"), "/api/users/{p1}", "initial real mount must affect sibling route")
	for _, change := range []struct{ name, source, want string }{
		{"edit", strings.Replace(mounted, "/api", "/v2", 1), "/v2/users/{p1}"},
		{"removal", "from fastapi import FastAPI\napp = FastAPI()\n", "/users/{p1}"},
	} {
		t.Run(change.name, func(t *testing.T) {
			writeFile(t, filepath.Join(root, "main.py"), change.source)
			_, err := idx.IncrementalReindexPaths(root, []string{"main.py"})
			require.NoError(t, err)
			require.Contains(t, constantProviderPaths(g, "fixture"), change.want)
			require.NotContains(t, constantProviderPaths(g, "fixture"), "/api/users/{p1}")
			clean := graph.New()
			full := makeIndexer(clean)
			_, err = full.Index(root)
			require.NoError(t, err)
			require.True(t, contractSetsEqual(shortcutAcceptanceRecords(t, clean), shortcutAcceptanceRecords(t, g)), "mount change must refresh actual sibling owner records")
		})
	}
}
