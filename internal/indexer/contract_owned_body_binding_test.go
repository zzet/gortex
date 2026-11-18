package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractOwnedBindingRouteUpdatesEqualColdAndRefreshLines(t *testing.T) {
	src := `package fixture
import "net/http"
type Response struct { Value string }
func result() Response { return Response{} }
func routes(mux *http.ServeMux) { mux.HandleFunc("GET /route",handler) }
func handler(w http.ResponseWriter,r *http.Request) {
 value := result()
 WriteJSON(w,200,value)
}
func WriteJSON(w http.ResponseWriter, status int, value any) {}
`
	files := map[string]string{"routes.go": src}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	first := runCachedContractFixture(t, f, &cache)
	require.Positive(t, first.BodyBindingObserved, "baseline must actually consume the return binding")
	require.NotNil(t, cache)
	require.NotEmpty(t, cache.units["fixture/routes.go"].bodyBindings)
	src = strings.Replace(src, "GET /route", "GET /next", 1)
	files["routes.go"] = src
	f.save("routes.go", src)
	next := runCachedContractFixture(t, f, &cache)
	require.Positive(t, next.BodyBindingSnapshots, "known questions are answered during the accepted parse")
	require.Positive(t, next.BodyBindingHits, "postpass must reuse the current owned value, not parse the file again for it")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	// Source movement cannot reuse an old semantic line even when a handler's
	// normalized contract/body digest would otherwise be equal.
	src = strings.Replace(src, "func handler", "\nfunc handler", 1)
	src = strings.Replace(src, "GET /next", "GET /line", 1) // require a real new boundary job; newline-only correctly schedules none
	files["routes.go"] = src
	f.save("routes.go", src)
	runCachedContractFixture(t, f, &cache)
	for q, a := range cache.units["fixture/routes.go"].bodyBindings {
		if q.name == "value" {
			require.Equal(t, 8, a.binding.Line)
		}
	}
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	// A newly requested variable cannot be certified by an older subset's map.
	src = strings.ReplaceAll(src, "value := result()", "other := result()")
	src = strings.Replace(src, "WriteJSON(w,200,value)", "WriteJSON(w,200,other)", 1)
	files["routes.go"] = src
	f.save("routes.go", src)
	changed := runCachedContractFixture(t, f, &cache)
	require.Positive(t, changed.BodyBindingObserved, "new name takes the original accepted-source parser path")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractOwnedBindingDoesNotCarryChangedSourceHandlerOrUnknownName(t *testing.T) {
	c := &contractConvergence{ctx: t.Context(), live: map[string]bool{"fixture/routes.go": true}, units: map[string]*contractUnit{}, clock: contractChangeClock{last: map[string]ContractReceiptStamp{}}, enrichReads: newContractUnitLedger(), report: &ContractFollowupReport{}}
	node := &graph.Node{ID: "fixture/routes.go::Handler", Name: "Handler", FilePath: "fixture/routes.go", StartLine: 2}
	digest, ok := contractAnswerDigest(node)
	require.True(t, ok)
	stamp := ContractReceiptStamp{Accepted: true, SourceFingerprint: "accepted"}
	unit := &contractUnit{bodyBindingEligible: true, path: node.FilePath, stamp: stamp, ledger: newContractUnitLedger()}
	require.True(t, storeContractBodyBinding(unit, contractBodyBindingQuestion{node.ID, "missing"}, contractBodyBindingAnswer{source: "accepted", handler: digest}))
	c.units[node.FilePath] = unit
	c.clock.last[node.FilePath] = stamp
	_, found := c.ownedBodyBinding(node, "missing")
	require.True(t, found, "explicit zero is a complete requested answer")
	_, found = c.ownedBodyBinding(node, "new")
	require.False(t, found)
	c.clock.last[node.FilePath] = ContractReceiptStamp{Accepted: true, SourceFingerprint: "different"}
	_, found = c.ownedBodyBinding(node, "missing")
	require.False(t, found)
	c.clock.last[node.FilePath] = stamp
	node.StartLine++
	_, found = c.ownedBodyBinding(node, "missing")
	require.False(t, found)
}
