package contracts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

func TestGoBodyFactsForFileMatchesIndividualFacts(t *testing.T) {
	src := []byte("package fixture\nfunc first() { WriteJSON(w,200,map[string]any{\"first\":1}) };func second() { WriteJSON(w,201,map[string]any{\"second\":2}) }\nfunc third() { _ = 3 }\n")
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	require.NoError(t, err)
	parsed := parser.NewParseTree(tree, src, "go")
	defer parsed.Release()
	nodes := []*graph.Node{{ID: "first", Name: "first", Kind: graph.KindFunction, StartLine: 2}, {ID: "second", Name: "second", Kind: graph.KindFunction, StartLine: 2}, {ID: "third", Name: "third", Kind: graph.KindFunction, StartLine: 3}}
	facts, err := GoBodyFactsForFile(context.Background(), parsed, nodes)
	require.NoError(t, err)
	require.Len(t, facts["first"].ResponseCalls(), 1)
	require.Len(t, facts["second"].ResponseCalls(), 1)
	require.Contains(t, facts["first"].ResponseCalls()[0].ValueExpr, "first")
	require.Contains(t, facts["second"].ResponseCalls()[0].ValueExpr, "second", "same-line functions must not share another function's facts")
	require.Empty(t, facts["third"].ResponseCalls())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := GoBodyFactsForFile(ctx, parsed, nodes)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	rows, err = GoBodyFactsForFile(context.Background(), parsed, []*graph.Node{{ID: "missing", Kind: graph.KindFunction, StartLine: 99}})
	require.Error(t, err)
	require.Nil(t, rows, "an incomplete accepted lookup cannot certify empty facts")
}

func TestGoBodyFactsForFileSameLineMethodReceivers(t *testing.T) {
	src := []byte("package fixture\ntype A struct{};type B struct{}\nfunc (A) Serve() { WriteJSON(w,200,map[string]any{\"a\":1}) };func (B) Serve() { WriteJSON(w,201,map[string]any{\"b\":2}) }\n")
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	require.NoError(t, err)
	parsed := parser.NewParseTree(tree, src, "go")
	defer parsed.Release()
	nodes := []*graph.Node{{ID: "A.Serve", Name: "Serve", Kind: graph.KindMethod, StartLine: 3, Meta: map[string]any{"receiver": "A"}}, {ID: "B.Serve", Name: "Serve", Kind: graph.KindMethod, StartLine: 3, Meta: map[string]any{"receiver": "B"}}}
	facts, err := GoBodyFactsForFile(context.Background(), parsed, nodes)
	require.NoError(t, err)
	require.Contains(t, facts["A.Serve"].ResponseCalls()[0].ValueExpr, `"a"`)
	require.Contains(t, facts["B.Serve"].ResponseCalls()[0].ValueExpr, `"b"`)
}
