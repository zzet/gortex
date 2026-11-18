package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The graph is intentionally absent. Collection must remain a pure accepted
// parse operation even with unresolved constants and nonempty real routes.
func boundaryTestReceipt(t *testing.T, source string) contractBoundaryReceipt {
	t.Helper()
	idx := newTestIndexer(graph.New())
	idx.graph = nil
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	parsed, err := languages.NewGoExtractor().Extract("fixture/routes.go", []byte(source))
	require.NoError(t, err)
	if parsed.Tree != nil {
		defer parsed.Tree.Release()
	}
	receipt, err := idx.collectContractBoundaryReceipt(context.Background(), "fixture/routes.go", "go", []byte(source), parsed)
	require.NoError(t, err)
	return receipt
}

func TestContractBoundaryReceiptOrdinaryEditsOweNothing(t *testing.T) {
	before := boundaryTestReceipt(t, "package fixture\nfunc Value() int { return 1 }\n")
	after := boundaryTestReceipt(t, "package fixture\n// An ordinary comment.\nfunc Value() int { return 2 }\nfunc Additional() { Value() }\n")
	diff := diffContractBoundaryReceipts(&before, &after)
	require.Empty(t, diff.Scope.Causes)
	require.Empty(t, after.LookupKeys, "new unrelated functions can produce dependency keys but have no boundary membership")
	require.Empty(t, diff.Scope.Groups)
	require.NotEqual(t, before.Source, after.Source, "source provenance remains exact independently of contract inputs")
	require.Empty(t, diffContractBoundaryReceipts(nil, &after).Scope.Causes, "new contract-free function must not create repo-wide debt")
}

const receiptHandlerSource = `package fixture
import "net/http"
func register(r Router) { r.GET("/users", users) }
func users(w http.ResponseWriter,r *http.Request) {
 calculation := 1
 _ = calculation
 WriteJSON(w,200,map[string]any{"name":"Ada"})
}
`

func TestContractBoundaryReceiptHandlerFactsIgnoreUnrelatedCalculation(t *testing.T) {
	before := boundaryTestReceipt(t, receiptHandlerSource)
	require.NotEmpty(t, before.Groups, "use an actual detected route")
	require.NotEmpty(t, before.HandlerInputs)
	after := boundaryTestReceipt(t, strings.Replace(receiptHandlerSource, "calculation := 1", "calculation := 200 // changed calculation", 1))
	require.Equal(t, before.HandlerInputs, after.HandlerInputs)
	require.Empty(t, diffContractBoundaryReceipts(&before, &after).Scope.Causes)
	changed := boundaryTestReceipt(t, strings.Replace(receiptHandlerSource, `"name":"Ada"`, `"display_name":"Ada"`, 1))
	diff := diffContractBoundaryReceipts(&before, &changed)
	require.Contains(t, diff.Scope.Causes, "handler_inputs_changed")
	require.NotEmpty(t, diff.Scope.Groups)
	require.False(t, diff.Scope.Unknown)
}

func TestContractBoundaryReceiptUnresolvedLookupAndRemoval(t *testing.T) {
	before := boundaryTestReceipt(t, constantRouteConsumer)
	require.Empty(t, before.Groups, "local extraction cannot invent a cross-file value")
	require.Contains(t, before.LookupKeys, contractBoundaryLookupKey("fixture", "constant_name", "Route"))
	changed := boundaryTestReceipt(t, strings.Replace(constantRouteConsumer, "r.GET(Route, handler)", "r.GET(NewRoute, handler)", 1))
	diff := diffContractBoundaryReceipts(&before, &changed)
	require.Contains(t, diff.Scope.Causes, "boundary_lookup_changed")
	require.Contains(t, diff.Scope.LookupKeys, contractBoundaryLookupKey("fixture", "constant_name", "Route"))
	require.Contains(t, diff.Scope.LookupKeys, contractBoundaryLookupKey("fixture", "constant_name", "NewRoute"))
	removed := diffContractBoundaryReceipts(&before, nil)
	require.True(t, removed.Scope.Deleted)
	require.Contains(t, removed.Scope.LookupKeys, contractBoundaryLookupKey("fixture", "constant_name", "Route"))
}

func TestContractBoundaryReceiptSharedInputsReturnKeysNotBroadWork(t *testing.T) {
	before := boundaryTestReceipt(t, "package fixture\nconst Route = \"/before\"\ntype Request struct { Name string }\n")
	after := boundaryTestReceipt(t, "package fixture\nconst Route = \"/after\"\ntype Request struct { Name int }\n")
	diff := diffContractBoundaryReceipts(&before, &after)
	require.NotEmpty(t, diff.ChangedProducedKeys, "reverse lookup must include value/type changes even with no local route")
	require.Empty(t, diff.Scope.Causes, "producing a key alone is not evidence that every repo contract depends on it")
	require.False(t, diff.Scope.Unknown)
	deleted := diffContractBoundaryReceipts(&before, nil)
	require.Equal(t, len(before.ProducedInputs), len(deleted.ChangedProducedKeys))
}

func TestContractBoundaryReceiptOldAndNewGroupsAndPolicy(t *testing.T) {
	before := boundaryTestReceipt(t, receiptHandlerSource)
	after := boundaryTestReceipt(t, strings.Replace(receiptHandlerSource, "/users", "/renamed", 1))
	diff := diffContractBoundaryReceipts(&before, &after)
	require.Contains(t, diff.Scope.Causes, "local_boundary_changed")
	require.Len(t, diff.Scope.Groups, 2, "removal and addition both remain pending")
	deleted := diffContractBoundaryReceipts(&before, nil)
	require.True(t, deleted.Scope.Deleted)
	require.Equal(t, before.Groups, deleted.Scope.Groups)
	after.Policy = "another-policy"
	require.True(t, diffContractBoundaryReceipts(&before, &after).Scope.Unknown)
	before.Version = "unsupported"
	require.Contains(t, diffContractBoundaryReceipts(&before, &after).Scope.Causes, "legacy_inputs_unknown")
}

func TestContractBoundaryReceiptCancelAndIncompleteAreErrors(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := idx.collectContractBoundaryReceipt(ctx, "fixture/routes.go", "go", nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = idx.collectContractBoundaryReceipt(context.Background(), "fixture/routes.go", "go", nil, nil)
	require.Error(t, err)
}

func TestContractBoundaryReceiptProducedKeysSelectNegativeLookupConsumer(t *testing.T) {
	consumer := boundaryTestReceipt(t, constantRouteConsumer)
	unrelated := boundaryTestReceipt(t, strings.Replace(constantRouteConsumer, "r.GET(Route", "r.GET(Unrelated", 1))
	absent := boundaryTestReceipt(t, "package fixture\n// no provider\n")
	before := boundaryTestReceipt(t, "package fixture\nconst Route = \"/before\"\n")
	after := boundaryTestReceipt(t, "package fixture\nconst Route = \"/after\"\n")
	renamed := boundaryTestReceipt(t, "package fixture\nconst NewRoute = \"/after\"\n")
	for _, test := range []struct {
		name         string
		old, current *contractBoundaryReceipt
	}{
		{"addition", &absent, &before}, {"value_change", &before, &after}, {"deletion", &before, nil}, {"rename", &before, &renamed},
	} {
		t.Run(test.name, func(t *testing.T) {
			diff := diffContractBoundaryReceipts(test.old, test.current)
			require.True(t, contractBoundaryAffectedByProducedKeys(consumer, diff.ChangedProducedKeys), "negative lookup must join a newly created/removed provider")
			require.False(t, contractBoundaryAffectedByProducedKeys(unrelated, diff.ChangedProducedKeys), "unrelated name is not a dependent")
		})
	}
}

func TestContractBoundaryReceiptCrossFileHandlerInputs(t *testing.T) {
	routes := boundaryTestReceipt(t, "package fixture\nfunc register(r Router) { r.GET(\"/users\",users) }\n")
	handlers := strings.Replace(receiptHandlerSource, "func register(r Router) { r.GET(\"/users\", users) }\n", "", 1)
	before := boundaryTestReceipt(t, handlers)
	after := boundaryTestReceipt(t, strings.Replace(handlers, `"name":"Ada"`, `"display_name":"Ada"`, 1))
	require.Empty(t, before.Groups, "handler file owns no route itself")
	diff := diffContractBoundaryReceipts(&before, &after)
	require.True(t, contractBoundaryAffectedByProducedKeys(routes, diff.ChangedProducedKeys))
	calculation := boundaryTestReceipt(t, strings.Replace(handlers, "calculation := 1", "calculation := 123 // local calculation", 1))
	require.False(t, contractBoundaryAffectedByProducedKeys(routes, diffContractBoundaryReceipts(&before, &calculation).ChangedProducedKeys))
	other := boundaryTestReceipt(t, "package fixture\nfunc register(r Router) { r.GET(\"/other\",other) }\n")
	require.False(t, contractBoundaryAffectedByProducedKeys(other, diff.ChangedProducedKeys))
}

func TestContractBoundaryReceiptSharedWireTypeInputs(t *testing.T) {
	consumer := contractBoundaryReceipt{LookupKeys: []string{contractBoundaryLookupKey("fixture", "type_name", "Request")}}
	for _, test := range []struct{ name, before, after string }{
		{"json_tag", "type Request struct { Name string `json:\"old\"` }", "type Request struct { Name string `json:\"new,omitempty\"` }"},
		{"field_type", "type Request struct { Name string }", "type Request struct { Name int }"},
		{"embedding", "type Request struct { Before }", "type Request struct { After }"},
		{"alias", "type Request = Before", "type Request = After"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := boundaryTestReceipt(t, "package fixture\n"+test.before+"\n")
			after := boundaryTestReceipt(t, "package fixture\n"+test.after+"\n")
			require.True(t, contractBoundaryAffectedByProducedKeys(consumer, diffContractBoundaryReceipts(&before, &after).ChangedProducedKeys))
		})
	}
}

func TestContractBoundaryReceiptConsumerSharedDTO(t *testing.T) {
	consumer := boundaryTestReceipt(t, `package fixture
func call(ctx Context) { _, _ = pb.NewUsersClient(conn).GetUser(ctx, &pb.GetUserRequest{Id:"x"}) }
`)
	require.NotEmpty(t, consumer.Groups, "actual gRPC consumer must emit a contract")
	require.Contains(t, consumer.LookupKeys, contractBoundaryLookupKey("fixture", "type_name", "GetUserRequest"))
	before := boundaryTestReceipt(t, "package fixture\ntype GetUserRequest struct { Id string `json:\"old\"` }\n")
	after := boundaryTestReceipt(t, "package fixture\ntype GetUserRequest struct { Id string `json:\"new\"` }\n")
	require.True(t, contractBoundaryAffectedByProducedKeys(consumer, diffContractBoundaryReceipts(&before, &after).ChangedProducedKeys))
}

func TestContractBoundaryReceiptHelperReturnType(t *testing.T) {
	before := boundaryTestReceipt(t, "package fixture\nfunc load() Before { return Before{} }\n")
	after := boundaryTestReceipt(t, "package fixture\nfunc load() After { return After{} }\n")
	consumer := boundaryTestReceipt(t, strings.Replace(receiptHandlerSource, `WriteJSON(w,200,map[string]any{"name":"Ada"})`, `value := load(); WriteJSON(w,200,value)`, 1))
	require.Contains(t, consumer.LookupKeys, contractBoundaryLookupKey("fixture", "symbol_name", "load"))
	require.True(t, contractBoundaryAffectedByProducedKeys(consumer, diffContractBoundaryReceipts(&before, &after).ChangedProducedKeys), "a helper with zero response calls still produces typed signature inputs")
	unrelated := boundaryTestReceipt(t, "package fixture\nfunc load() Before { _ = 2; return Before{} }\n")
	require.False(t, contractBoundaryAffectedByProducedKeys(consumer, diffContractBoundaryReceipts(&before, &unrelated).ChangedProducedKeys))
}

func TestContractBoundaryReceiptBodylessSignature(t *testing.T) {
	before := boundaryTestReceipt(t, "package fixture\n//go:noescape\nfunc external(p *Before)\n")
	after := boundaryTestReceipt(t, "package fixture\n//go:noescape\nfunc external(p *After)\n")
	consumer := contractBoundaryReceipt{LookupKeys: []string{contractBoundaryLookupKey("fixture", "symbol_name", "external")}}
	require.True(t, contractBoundaryAffectedByProducedKeys(consumer, diffContractBoundaryReceipts(&before, &after).ChangedProducedKeys))
}

// The synthetic hook is attributed to its real implementation, including the
// complete source span. It must participate in accepted boundary provenance.
func TestContractBoundaryReceiptDrupalSyntheticHookSpan(t *testing.T) {
	const source = `<?php
function mymodule_help($route_name, $route_match) {
    return "Module help";
}
`
	idx := newTestIndexer(graph.New())
	idx.graph = nil
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	parsed, err := languages.NewPHPExtractor().Extract("fixture/mymodule.module", []byte(source))
	require.NoError(t, err)
	defer parsed.ReleaseTree()
	var hook *graph.Node
	for _, node := range parsed.Nodes {
		if node != nil && node.ID == "drupal::hook::hook_help" {
			hook = node
			break
		}
	}
	require.NotNil(t, hook)
	require.Equal(t, graph.KindInterface, hook.Kind)
	require.Equal(t, 2, hook.StartLine)
	require.Equal(t, 4, hook.EndLine)
	require.Equal(t, true, hook.Meta["drupal_hook_def"])
	receipt, err := idx.collectContractBoundaryReceipt(context.Background(), "fixture/mymodule.module", "php", []byte(source), parsed)
	require.NoError(t, err)
	key := contractBoundaryLookupKey("fixture", "type_name", "hook_help")
	require.NotEmpty(t, receipt.ProducedInputs[key], "synthetic hook retains accepted type-name provenance")
	stored, err := contractCoreStoredReceipt("fixture", "", contractCoreInputChange{
		FilePath: "fixture/mymodule.module", Current: &receipt, SourceFingerprint: receipt.Source,
	})
	require.NoError(t, err)
	require.False(t, stored.Scope.Unknown)
	require.Contains(t, stored.ProducedKeys, key)
}

func TestContractBoundaryReceiptSpringParameterCandidates(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.graph = nil
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	collect := func(path, source string) contractBoundaryReceipt {
		parsed, err := languages.NewJavaExtractor().Extract(path, []byte(source))
		require.NoError(t, err)
		if parsed.Tree != nil {
			defer parsed.Tree.Release()
		}
		receipt, err := idx.collectContractBoundaryReceipt(t.Context(), path, "java", []byte(source), parsed)
		require.NoError(t, err)
		return receipt
	}
	row := contractColdMatrixFixtures()[3]
	bean := collect("fixture/Clocks.java", row.files["Clocks.java"])
	legacy := bean
	legacy.Version = "contract-boundary-v1"
	require.True(t, diffContractBoundaryReceipts(&legacy, &bean).Scope.Unknown, "old baselines lack the new negative candidate lookup")
	consumer := collect("fixture/OrderService.java", row.files["OrderService.java"])
	key := contractBoundaryLookupKey("fixture", "spring_parameter_type", "Clock")
	require.Contains(t, bean.LookupKeys, key)
	require.Contains(t, consumer.ProducedInputs, key)
	for _, delta := range []contractBoundaryDelta{diffContractBoundaryReceipts(nil, &consumer), diffContractBoundaryReceipts(&consumer, nil)} {
		require.True(t, contractBoundaryAffectedByProducedKeys(bean, delta.ChangedProducedKeys), "consumer membership must invalidate its bean provider")
	}
	renamed := collect("fixture/OrderService.java", strings.ReplaceAll(row.files["OrderService.java"], "Clock clock", "Clock renamedClock"))
	require.Equal(t, consumer.ProducedInputs[key], renamed.ProducedInputs[key], "an unchanged matching type retains the same candidate identity")
	unrelated := collect("fixture/Unrelated.java", "class Unrelated { public void ordinary(String value) {} }")
	require.False(t, contractBoundaryAffectedByProducedKeys(bean, diffContractBoundaryReceipts(nil, &unrelated).ChangedProducedKeys))
}
