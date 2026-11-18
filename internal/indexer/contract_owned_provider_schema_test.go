package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestContractOwnedProviderPlainRouteUpdatesEqualCold(t *testing.T) {
	routes := `package fixture
import "net/http"
func routes(mux *http.ServeMux) { mux.HandleFunc("GET /route",handler) }
`
	handler := `package fixture
import "net/http"
type Response struct { Value string }
func handler(w http.ResponseWriter,r *http.Request) {
 value := Response{}
 WriteJSON(w,200,value)
}
func WriteJSON(w http.ResponseWriter, status int, value any) {}
`
	files := map[string]string{"routes.go": routes, "handler.go": handler}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.NotEmpty(t, cache.units["fixture/handler.go"].providerSchemas, "baseline must actually execute cross-file provider kernel")
	routes = strings.Replace(routes, "GET /route", "GET /next", 1)
	files["routes.go"] = routes
	f.save("routes.go", routes)
	next := runCachedContractFixture(t, f, &cache)
	require.Positive(t, next.ProviderSchemaHits, "changed plain path must apply owned original-kernel output")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	// A changed handler is re-answered on its new mandatory accepted tree.
	handler = strings.Replace(handler, "Value string", "Value string; Other int", 1)
	files["handler.go"] = handler
	f.save("handler.go", handler)
	routes = strings.Replace(routes, "GET /next", "GET /refresh", 1) // force a real route boundary job after accepted handler refresh
	files["routes.go"] = routes
	f.save("routes.go", routes)
	refreshed := runCachedContractFixture(t, f, &cache)
	require.Positive(t, refreshed.ProviderSchemaSnapshots)
	require.Positive(t, refreshed.ProviderSchemaHits)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	// Placeholder routes intentionally remain on the original provider kernel.
	routes = strings.Replace(routes, "GET /refresh", "GET /users/{id}", 1)
	files["routes.go"] = routes
	f.save("routes.go", routes)
	runCachedContractFixture(t, f, &cache)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractOwnedProviderRequiresExactPreimageOrderedNodesAndSource(t *testing.T) {
	input := contracts.Contract{Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "fixture/handler.go", SymbolID: "fixture/handler.go::handler", Line: 3, Meta: map[string]any{"path": "/before", "response_expr": "value"}}
	nodes := []*graph.Node{{ID: input.SymbolID, FilePath: input.FilePath, Kind: graph.KindFunction, StartLine: 3, EndLine: 8}, {ID: "fixture/handler.go::Response", Name: "Response", Kind: graph.KindType, FilePath: input.FilePath}}
	c := &contractConvergence{ctx: t.Context(), live: map[string]bool{input.FilePath: true}, units: map[string]*contractUnit{}, clock: contractChangeClock{last: map[string]ContractReceiptStamp{}}, enrichReads: newContractUnitLedger(), report: &ContractFollowupReport{}}
	stamp := ContractReceiptStamp{Accepted: true, SourceFingerprint: "accepted"}
	unit := &contractUnit{bodyBindingEligible: true, path: input.FilePath, stamp: stamp, ledger: newContractUnitLedger()}
	c.units[input.FilePath], c.clock.last[input.FilePath] = unit, stamp
	c.enrichReads.source(input.FilePath, "accepted")
	c.rememberProviderSchema(input, nodes, map[string]any{"path": "/before", "schema_source": "partial", "response_expr": "value"})
	require.Len(t, unit.providerSchemas, 1)
	input.Meta["path"] = "/after"
	out, ok := c.ownedProviderSchema(input, nodes)
	require.True(t, ok)
	require.Equal(t, "/after", out["path"])
	out["response_expr"] = "mutated"
	out, ok = c.ownedProviderSchema(input, nodes)
	require.True(t, ok)
	require.Equal(t, "value", out["response_expr"])
	_, ok = c.ownedProviderSchema(input, []*graph.Node{nodes[1], nodes[0]})
	require.False(t, ok, "candidate order is consumed by original type resolution")
	input.Meta["response_expr"] = "other"
	_, ok = c.ownedProviderSchema(input, nodes)
	require.False(t, ok, "all input Meta precedence participates")
	input.Meta["response_expr"] = "value"
	input.Meta["path"] = "/users/{id}"
	_, ok = c.ownedProviderSchema(input, nodes)
	require.False(t, ok)
	input.Meta["path"] = "/after"
	c.clock.last[input.FilePath] = ContractReceiptStamp{Accepted: true, SourceFingerprint: "new"}
	_, ok = c.ownedProviderSchema(input, nodes)
	require.False(t, ok)
}

func TestContractOwnedProviderFactoryChangeFallsBackAndEqualsCold(t *testing.T) {
	src := `package fixture
import "net/http"
type Response struct { Value string }
func routes(mux *http.ServeMux) {mux.HandleFunc("GET /route",handler)}
func handler(w http.ResponseWriter,r *http.Request) {
 value:=Response{}
 WriteJSON(w,200,value)
}
func WriteJSON(w http.ResponseWriter,status int,value any){}
`
	files := map[string]string{"routes.go": src}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	original := contracts.FactoryForLang("go")
	t.Cleanup(func() { contracts.RegisterBodyFactsFactory("go", original) })
	calls := 0
	contracts.RegisterBodyFactsFactory("go", func(*parser.ParseTree, *graph.Node) contracts.BodyFacts { calls++; return nopFacts })
	src = strings.Replace(src, "GET /route", "GET /changed", 1)
	files["routes.go"] = src
	f.save("routes.go", src)
	report := runCachedContractFixture(t, f, &cache)
	require.Zero(t, report.BodyBindingHits)
	require.Zero(t, report.ProviderSchemaHits)
	require.Positive(t, calls, "the original extension factory remains the authority")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}
