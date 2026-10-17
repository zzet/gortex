package indexer

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func newRouterContractFixture(t *testing.T, files map[string]string) *contractJobFixture {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	reg.Register(languages.NewJavaScriptExtractor())
	reg.Register(languages.NewTypeScriptExtractor())
	return newContractJobFixtureWithRegistry(t, files, reg)
}

func assertRouterColdParity(t *testing.T, f *contractJobFixture, files map[string]string) {
	t.Helper()
	cold := newRouterContractFixture(t, files)
	report, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, contractMeta(cold.contracts()), contractMeta(f.contracts()))
}

func TestContractDeltaRouterMountLifecycleEqualsCold(t *testing.T) {
	files := map[string]string{
		"routes.py": "from fastapi import APIRouter\nrouter = APIRouter()\n@router.get('/item')\ndef item():\n    return {}\n",
		"main.py":   "from fastapi import FastAPI\nfrom routes import router\napp = FastAPI()\n",
	}
	f := newRouterContractFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Empty(t, cache.units["fixture/main.py"].raw)
	require.NotNil(t, cache.units["fixture/main.py"].router)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/item")
	held := cache.units["fixture/routes.py"].router.Clone()
	heldFacts := cache.units["fixture/routes.py"].router
	for i, mount := range []string{"app.include_router(router, prefix='/api')\n", "app.include_router(router, prefix='/v2')\n", ""} {
		files["main.py"] = "from fastapi import FastAPI\nfrom routes import router\napp = FastAPI()\n" + mount
		f.save("main.py", files["main.py"])
		runCachedContractFixture(t, f, &cache)
		require.Equal(t, held, *cache.units["fixture/routes.py"].router)
		require.Same(t, heldFacts, cache.units["fixture/routes.py"].router, "unchanged route facts are reused")
		ids := contractIDs(f.contracts())
		expected := []string{"http::GET::/api/item", "http::GET::/v2/item", "http::GET::/item"}[i]
		require.Contains(t, ids, expected)
		for _, old := range []string{"http::GET::/api/item", "http::GET::/v2/item", "http::GET::/item"} {
			if old != expected {
				require.NotContains(t, ids, old)
			}
		}
		assertRouterColdParity(t, f, files)
	}
}

func TestContractDeltaRouterEligibilityFlipKeepsMountFacts(t *testing.T) {
	files := map[string]string{
		"mount.js":  "const express = require('express');\nconst app = express();\napp.use('/api', router);\n",
		"routes.js": "const express = require('express');\nconst router = express.Router();\n",
	}
	f := newRouterContractFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Empty(t, f.contracts())
	mount := cache.units["fixture/mount.js"].router.Clone()
	files["routes.js"] += "router.get('/item', (req, res) => res.json({}));\n"
	f.save("routes.js", files["routes.js"])
	runCachedContractFixture(t, f, &cache)
	require.Equal(t, mount, *cache.units["fixture/mount.js"].router)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/api/item")
	require.NotContains(t, contractIDs(f.contracts()), "http::GET::/item")
	assertRouterColdParity(t, f, files)
}

func TestContractDeltaRouterMountFileRemovalAndRecreationEqualsCold(t *testing.T) {
	const mountFile = "fixture/main.py"
	route := "from fastapi import APIRouter\nrouter = APIRouter()\n@router.get('/item')\ndef item():\n    return {}\n"
	mount := func(prefix string) string {
		return "from fastapi import FastAPI\nfrom routes import router\napp = FastAPI()\napp.include_router(router, prefix='" + prefix + "')\n"
	}
	files := map[string]string{"routes.py": route, "main.py": mount("/api")}
	f := newRouterContractFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	old := cache.units[mountFile]
	require.NotNil(t, old)
	require.Empty(t, old.raw, "the mount-file census cannot depend on owning route contracts")
	require.NotNil(t, old.router)
	require.Equal(t, "/api", old.router.MountPrefix["router"])
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/api/item")

	// The file itself departs the accepted census, not just its include text.
	f.remove("main.py")
	delete(files, "main.py")
	report := runCachedContractFixture(t, f, &cache)
	require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
	require.NotContains(t, cache.units, mountFile)
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/item")
	require.NotContains(t, contractIDs(f.contracts()), "http::GET::/api/item")
	assertRouterColdParity(t, f, files)

	// Reusing the same path must extract a new unit with its new mount facts.
	files["main.py"] = mount("/recreated")
	f.save("main.py", files["main.py"])
	report = runCachedContractFixture(t, f, &cache)
	require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
	current := cache.units[mountFile]
	require.NotNil(t, current)
	require.NotNil(t, current.router)
	require.NotSame(t, old.router, current.router)
	require.Equal(t, "/recreated", current.router.MountPrefix["router"])
	require.Contains(t, contractIDs(f.contracts()), "http::GET::/recreated/item")
	require.NotContains(t, contractIDs(f.contracts()), "http::GET::/api/item")
	require.NotContains(t, contractIDs(f.contracts()), "http::GET::/item")
	assertRouterColdParity(t, f, files)
	require.Equal(t, "/api", old.router.MountPrefix["router"], "departed cached facts remain immutable")
}

func TestContractDeltaRouterSameNameMountsStayInTheirRepository(t *testing.T) {
	route := "from fastapi import APIRouter\nrouter = APIRouter()\n@router.get('/item')\ndef item():\n    return {}\n"
	mount := func(prefix string) string {
		return "from fastapi import FastAPI\nfrom routes import router\napp = FastAPI()\napp.include_router(router, prefix='" + prefix + "')\n"
	}
	filesA := map[string]string{"routes.py": route, "main.py": mount("/alpha")}
	filesB := map[string]string{"routes.py": route, "main.py": mount("/beta")}
	f := newRouterContractFixture(t, filesA)
	otherRoot := t.TempDir()
	for name, source := range filesB {
		writeFile(t, filepath.Join(otherRoot, name), source)
	}
	result, err := f.mi.TrackRepo(config.RepoEntry{Name: "neighbor", Path: otherRoot})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), f.options, "neighbor"))
	repos := []string{"fixture", "neighbor"}
	var cache *contractDeltaCache
	run := func() {
		f.layers++
		inputs, err := NewContractPrimaryInputCapture(f.options.Materializer)(t.Context(), repos, "fixture")
		require.NoError(t, err)
		snapshot, targets, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
		if snapshot.Release != nil {
			defer snapshot.Release() // capture closure is idempotent; also covers fatal pre-worker assertions
		}
		require.NoError(t, err)
		require.Len(t, targets, 2, "the actual capture must admit both repositories")
		for i := range targets {
			_, targets[i].Payload, err = f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "router-isolation", LayerID: fmt.Sprintf("%d-%s", f.layers, targets[i].Key.RepoPrefix), GenerationKind: "contract_analysis", CreatedAt: int64(f.layers)})
			require.NoError(t, err)
		}
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		batch, err := RunContractFollowupBatch(t.Context(), ContractFollowupBatchRequest{Snapshot: snapshot, Targets: targets, RebuildReason: ContractFollowupUnknownInterval, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, delta: session})
		require.NoError(t, err)
		cache = session.cache
		require.NotNil(t, cache)
		if f.layers > 1 {
			require.Equal(t, viewmetrics.ContractModeDelta, batch.Cohort.Mode)
		}
		for _, target := range batch.Targets {
			require.True(t, target.Report.Published)
			require.NoError(t, target.Err)
		}
	}
	assertRepo := func(repo string, sources map[string]string, want string) {
		inputs, err := NewContractPrimaryInputCapture(f.options.Materializer)(t.Context(), repos, repo)
		require.NoError(t, err)
		analysis, err := f.options.Materializer.OpenContractAnalysisForInputs(t.Context(), nil, inputs)
		require.NoError(t, err)
		defer analysis.Close()
		registry, err := contracts.LoadRegistryFromGraphChecked(t.Context(), analysis.RegistryReader, contracts.RegistryLoadOptions{RepoPrefix: repo})
		require.NoError(t, err)
		ids := registry.AllIDs()
		for _, record := range registry.All() {
			require.Equal(t, repo, record.RepoPrefix, "the selected registry must retain exact owner scope")
		}
		require.Contains(t, ids, "http::GET::"+want+"/item")
		cold := newRouterContractFixture(t, sources)
		report, err := cold.job(nil, nil)
		require.NoError(t, err)
		require.True(t, report.Published)
		require.ElementsMatch(t, contractIDs(cold.contracts()), ids, "selected registry must equal independent cold without foreign mounts")
		require.NoError(t, analysis.Validate(t.Context()))
	}
	run()
	assertRepo("fixture", filesA, "/alpha")
	assertRepo("neighbor", filesB, "/beta")
	require.NotNil(t, cache.units["neighbor/main.py"])
	held := cache.units["neighbor/main.py"].router
	require.NotNil(t, held)
	filesA["main.py"] = mount("/changed-alpha")
	f.save("main.py", filesA["main.py"])
	run()
	assertRepo("fixture", filesA, "/changed-alpha")
	assertRepo("neighbor", filesB, "/beta")
	require.Same(t, held, cache.units["neighbor/main.py"].router, "another repository's edit must not replace these cached facts")
}

func TestContractRouterFactsCloneOwnsNestedStorage(t *testing.T) {
	for _, row := range []struct {
		name  string
		facts contracts.RouterFacts
	}{
		{name: "zero"},
		{name: "allocated_empty", facts: contracts.RouterFacts{
			SelfPrefix: map[string]string{}, MountPrefix: map[string]string{}, MountParent: map[string]string{},
			ModulePrefix: map[string]string{}, ControllerModule: map[string]string{}, Lines: []contracts.RouterLineFacts{},
		}},
		{name: "raw_empty_mount", facts: contracts.RouterFileFacts("fixture/mount.js", []byte("app.use('/api', router);\n"))},
		{name: "receiver_map_representations", facts: contracts.RouterFacts{Lines: []contracts.RouterLineFacts{
			{Line: 1}, {Line: 2, Receivers: map[string]string{}},
		}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			cloned := row.facts.Clone()
			require.Equal(t, row.facts, cloned, "cloning must preserve nil and allocated-empty representations")
			maps := [][2]map[string]string{
				{row.facts.SelfPrefix, cloned.SelfPrefix}, {row.facts.MountPrefix, cloned.MountPrefix},
				{row.facts.MountParent, cloned.MountParent}, {row.facts.ModulePrefix, cloned.ModulePrefix},
				{row.facts.ControllerModule, cloned.ControllerModule},
			}
			for i := range row.facts.Lines {
				maps = append(maps, [2]map[string]string{row.facts.Lines[i].Receivers, cloned.Lines[i].Receivers})
			}
			for _, pair := range maps {
				if pair[1] != nil {
					pair[1]["clone-only"] = "/changed"
					require.NotContains(t, pair[0], "clone-only", "non-nil maps must remain independently owned")
				}
			}
			if len(cloned.Lines) != 0 {
				cloned.Lines[0].Line = -1
				require.NotEqual(t, -1, row.facts.Lines[0].Line, "line slices must remain independently owned")
			}
		})
	}
	files := map[string]string{"routes.py": "from fastapi import APIRouter\nrouter = APIRouter(prefix='/api')\n@router.get('/item')\ndef item():\n    return {}\n"}
	f := newRouterContractFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	copy := cloneContractUnits(cache.units)
	original := cache.units["fixture/routes.py"].router
	cloned := copy["fixture/routes.py"].router
	cloned.SelfPrefix["router"] = "/changed"
	for _, line := range cloned.Lines {
		line.Receivers["fastapi/flask"] = "changed"
	}
	require.Equal(t, "/api", original.SelfPrefix["router"])
	for _, line := range original.Lines {
		require.NotEqual(t, "changed", line.Receivers["fastapi/flask"])
	}
}

func TestContractRouterPostpassReplaysSemanticMountResult(t *testing.T) {
	for _, row := range []struct {
		name, edit string
		reruns     int
	}{
		{name: "comment", edit: "# unrelated comment\n", reruns: 0},
		{name: "negative_becomes_mount", edit: "app.include_router(router, prefix='/api')\n", reruns: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			files := map[string]string{
				"routes.py": "from fastapi import APIRouter\nrouter = APIRouter()\n@router.get('/item')\ndef item():\n    return {}\n",
				"main.py":   "from fastapi import FastAPI\nfrom routes import router\napp = FastAPI()\n",
			}
			f := newRouterContractFixture(t, files)
			edited := false
			report, err := f.job(&contractConvergenceHooks{afterPP: func() {
				if edited {
					return
				}
				edited = true
				files["main.py"] += row.edit
				f.saveKeepingKey("main.py", files["main.py"])
			}}, nil)
			require.NoError(t, err)
			require.True(t, report.Published)
			require.True(t, edited)
			require.Equal(t, row.reruns, report.EnrichReruns)
			assertRouterColdParity(t, f, files)
		})
	}
}

func TestContractDeltaRouterFactsDoNotReadUnrelatedGoComments(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Nil(t, cache.units["fixture/helper.go"].router)
	files["consts.go"] = strings.ReplaceAll(jobConsts, "/before", "/next")
	f.save("consts.go", files["consts.go"])
	saved := false
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, epoch: f.epoch.Load, wait: f.waitPublished, hooks: &contractConvergenceHooks{afterPP: func() {
		if saved {
			return
		}
		saved = true
		files["helper.go"] += "// unrelated comment\n"
		f.saveKeepingKey("helper.go", files["helper.go"])
	}}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, saved)
	require.Equal(t, 1, report.Iterations)
	require.Zero(t, report.EnrichReruns)
	require.Nil(t, cache.units["fixture/helper.go"].router)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractDeltaRouterLanguageCensusIncludesMountExtensions(t *testing.T) {
	for _, ext := range []string{".mjs", ".mts"} {
		t.Run(ext, func(t *testing.T) {
			files := map[string]string{
				"mount" + ext: "const express = require('express');\nconst app = express();\napp.use('/api', router);\n",
				"routes.js":   "const express = require('express');\nconst router = express.Router();\n",
			}
			f := newRouterContractFixture(t, files)
			var cache *contractDeltaCache
			runCachedContractFixture(t, f, &cache)
			mount := cache.units["fixture/mount"+ext]
			require.NotNil(t, mount)
			require.Empty(t, mount.raw)
			require.NotNil(t, mount.router)
			require.Equal(t, "/api", mount.router.MountPrefix["router"])
			files["routes.js"] += "router.get('/item', (req, res) => res.json({}));\n"
			f.save("routes.js", files["routes.js"])
			report := runCachedContractFixture(t, f, &cache)
			require.Equal(t, 1, report.Files)
			require.Same(t, mount.router, cache.units["fixture/mount"+ext].router, "unchanged rawempty mount facts are reused")
			require.Contains(t, contractIDs(f.contracts()), "http::GET::/api/item")
			require.NotContains(t, contractIDs(f.contracts()), "http::GET::/item")
			assertRouterColdParity(t, f, files)
		})
	}
}
