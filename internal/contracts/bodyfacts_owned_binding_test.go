package contracts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestOwnedGoBindingsSurviveTreeAndPreserveSitesAndAbsence(t *testing.T) {
	src := []byte("package fixture\nfunc Handler() {\n value := service.Fetch()\n _ = value\n}\n")
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	handler := &graph.Node{ID: "r/handler.go::Handler", Name: "Handler", Kind: graph.KindFunction, FilePath: "r/handler.go", StartLine: 2, EndLine: 5}
	expected := MakeBodyFacts(tree, handler).VarBinding("value")
	rows, known, err := SnapshotGoVarBindings(t.Context(), tree, handler, []string{"value", "missing", " &value "}, 16384)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, expected, rows["value"])
	require.Equal(t, 3, rows["value"].Line)
	absent, explicit := rows["missing"]
	require.True(t, explicit)
	require.True(t, absent.IsZero())
	require.Equal(t, expected, rows[" &value "])
	_, newName := rows["new-name"]
	require.False(t, newName)
	tree.Release()
	for i := range src {
		src[i] = 'x'
	}
	require.Equal(t, "service.Fetch()", rows["value"].RawExpr, "retained bindings must not share accepted source backing")
	require.Equal(t, expected, rows["value"])
}

type customOwnedBindingFacts struct{ nopBodyFacts }

func (customOwnedBindingFacts) VarBinding(string) Binding { return Binding{TypeID: "custom"} }

func TestOwnedGoBindingsUnknownBudgetAndCancellationFallBack(t *testing.T) {
	_, native := OwnedGoVarBinding(customOwnedBindingFacts{}, "value")
	require.False(t, native)
	src := []byte("package fixture\nfunc Handler() { value := makeValue(); _ = value }\n")
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	defer tree.Release()
	handler := &graph.Node{ID: "Handler", Name: "Handler", Kind: graph.KindFunction, StartLine: 2, EndLine: 2}
	rows, native, err := SnapshotGoVarBindings(t.Context(), tree, handler, []string{"value"}, 1)
	require.NoError(t, err)
	require.False(t, native)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, native, err = SnapshotGoVarBindings(ctx, tree, handler, []string{"value"}, 16384)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, native)
	require.Nil(t, rows)
}

func TestOwnedGoBindingsFactoryReplacementRefusesNativeReuse(t *testing.T) {
	original := FactoryForLang("go")
	t.Cleanup(func() { RegisterBodyFactsFactory("go", original) })
	require.True(t, NativeGoBodyFactsFactory())
	RegisterBodyFactsFactory("go", func(*parser.ParseTree, *graph.Node) BodyFacts { return customOwnedBindingFacts{} })
	require.False(t, NativeGoBodyFactsFactory())
	tree := ParseTreeForLang("go", []byte("package fixture\nfunc Handler(){value:=makeValue();_=value}"))
	require.NotNil(t, tree)
	defer tree.Release()
	values, known, err := SnapshotGoVarBindings(t.Context(), tree, &graph.Node{ID: "Handler", Name: "Handler", StartLine: 2, EndLine: 2}, []string{"value"}, 16384)
	require.NoError(t, err)
	require.False(t, known)
	require.Nil(t, values)
}
