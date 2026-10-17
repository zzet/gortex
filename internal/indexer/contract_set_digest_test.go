package indexer

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestContractAnswerDigestPreservesTheContractSet(t *testing.T) {
	records := []contracts.Contract{
		{ID: "http::GET::/first", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "fixture/routes.py", SymbolID: "fixture/routes.py::first", Line: 3, Meta: map[string]any{"framework": "fastapi/flask", "path": "/first", "method": "GET"}},
		{ID: "http::GET::/second", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "fixture/routes.py", SymbolID: "fixture/routes.py::second", Line: 5, Meta: map[string]any{"framework": "fastapi/flask", "path": "/second", "method": "GET"}},
	}
	before, ok := contractAnswerDigest(records)
	require.True(t, ok)
	reversed := []contracts.Contract{records[1], records[0]}
	after, ok := contractAnswerDigest(reversed)
	require.True(t, ok)
	require.Equal(t, before, after, "map enumeration order must not invalidate a router result")
	for _, mutate := range []func([]contracts.Contract){
		func(r []contracts.Contract) { r[0].Line++ },
		func(r []contracts.Contract) { r[0].FilePath = "other/routes.py" },
		func(r []contracts.Contract) { r[0].SymbolID = "fixture/routes.py::other" },
		func(r []contracts.Contract) { r[0].Role = contracts.RoleConsumer },
		func(r []contracts.Contract) { r[0].Meta["path"] = "/changed" },
	} {
		changed := cloneContractRecords(records)
		mutate(changed)
		digest, ok := contractAnswerDigest(changed)
		require.True(t, ok)
		require.NotEqual(t, before, digest)
	}
	duplicate := append(cloneContractRecords(records), records[0])
	digest, ok := contractAnswerDigest(duplicate)
	require.True(t, ok)
	require.NotEqual(t, before, digest, "canonical ordering must retain multiplicity")
	// Exercise the actual registry-backed proof repeatedly, with distinct route IDs.
	c := &contractConvergence{files: map[string]ContractFollowupFile{}, units: map[string]*contractUnit{}, live: map[string]bool{}}
	registry := contracts.NewRegistry()
	registry.AddAll(cloneContractRecords(records), "fixture")
	contracts.JoinRouterPrefixFacts(registry, nil)
	proofDigest, ok := contractAnswerDigest(registry.All())
	require.True(t, ok)
	proof := []contractRouterProof{{repo: "fixture", records: cloneContractRecords(registry.All()), digest: proofDigest}}
	for range 100 {
		require.True(t, c.routerProofsHold(proof))
	}
}

func TestContractRouterProofAllowsTornEnrichmentToRecheck(t *testing.T) {
	routes := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", h.serveItems) }\n"
	serve := "package fixture\nimport \"net/http\"\ntype H struct{}\nfunc (H) serveItems(w http.ResponseWriter, r *http.Request) {}\n"
	python := "from fastapi import APIRouter\nrouter = APIRouter()\n"
	for n := range 12 {
		python += fmt.Sprintf("@router.get('/route%d')\ndef route%d():\n    return {}\n", n, n)
	}
	files := map[string]string{"routes.go": routes, "serve.go": serve, "routes.py": python}
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	registry.Register(languages.NewPythonExtractor())
	f := newContractJobFixtureWithRegistry(t, files, registry)
	var release func()
	armed, staged := false, false
	waits := 0
	hooks := &contractConvergenceHooks{
		beforeR1: func([]string) { armed = true },
		openedEvidence: func(*store_sqlite.Store) {
			if armed && !staged {
				staged = true
				f.stageInFlight("fixture/serve.go")
			}
		},
	}
	f.onWait = func() {
		waits++
		switch waits {
		case 1:
			release = f.beginHeld("fixture/serve.go")
		case 2:
			if release != nil {
				release()
				release = nil
			}
		}
	}
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	f.onWait = nil
	if release != nil {
		release()
	}
	require.NoError(t, err)
	require.True(t, staged)
	require.GreaterOrEqual(t, waits, 2)
	require.True(t, report.Published)
	require.Equal(t, 1, report.EnrichRuns)
	require.Zero(t, report.EnrichReruns)
	require.Positive(t, report.EnrichRechecks, "the torn read must be re-proven after its real write window closes")
	cold := newContractJobFixtureWithRegistry(t, files, registry)
	coldReport, err := cold.job(nil, nil)
	require.NoError(t, err)
	require.True(t, coldReport.Published)
	require.Equal(t, contractMeta(cold.contracts()), contractMeta(f.contracts()))
}
