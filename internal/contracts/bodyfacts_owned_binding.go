package contracts

import (
	"context"
	"reflect"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// OwnedGoVarBinding returns only the scalar answer consumed by contract return
// type resolution. It deliberately does not pretend to implement BodyFacts:
// response/composite facts contain AST handles and cannot outlive their tree.
// Unknown/custom factories retain their original execution path.
func OwnedGoVarBinding(facts BodyFacts, name string) (Binding, bool) {
	native, ok := facts.(*goBodyFacts)
	if !ok || native == nil {
		return Binding{}, false
	}
	return cloneOwnedBinding(native.VarBinding(name)), true
}

func cloneOwnedBinding(value Binding) Binding {
	value.Kind = BindingKind(strings.Clone(string(value.Kind)))
	value.TypeID, value.CallExpr, value.RawExpr = strings.Clone(value.TypeID), strings.Clone(value.CallExpr), strings.Clone(value.RawExpr)
	value.Origin = Origin(strings.Clone(string(value.Origin)))
	return value
}

// SnapshotGoVarBindings materializes only explicitly requested answers while
// the accepted tree is alive, including zero answers. A later new name is a
// cache miss, never an inferred absence. Line is preserved for semantic sites.
// The supplied budget bounds owned strings/entries before cloning them.
func SnapshotGoVarBindings(ctx context.Context, tree *parser.ParseTree, handler *graph.Node, names []string, budgetBytes int64) (map[string]Binding, bool, error) {
	if ctx == nil {
		return nil, false, graph.ErrContractProjectionIncomplete
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !NativeGoBodyFactsFactory() || tree == nil || tree.Lang() != "go" || handler == nil || len(names) > graph.ContractProjectionRowLimit || budgetBytes < 0 {
		return nil, false, nil
	}
	native, ok := MakeBodyFacts(tree, handler).(*goBodyFacts)
	if !ok || native == nil {
		return nil, false, nil
	}
	bytes := int64(256) + int64(len(names))*1024
	for _, name := range names {
		b := native.VarBinding(name)
		bytes += 2 * int64(len(name)+len(b.Kind)+len(b.TypeID)+len(b.CallExpr)+len(b.RawExpr)+len(b.Origin))
		if bytes > budgetBytes {
			return nil, false, nil
		}
	}
	out := make(map[string]Binding, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		out[strings.Clone(name)] = cloneOwnedBinding(native.VarBinding(name))
	}
	return out, true, ctx.Err()
}

// NativeGoBodyFactsFactory excludes extension factories, including a changed
// registration after a prior owned answer was recorded. newGoBodyFacts is a
// fixed non-closure function, so code identity names its complete factory.
func NativeGoBodyFactsFactory() bool {
	return reflect.ValueOf(FactoryForLang("go")).Pointer() == reflect.ValueOf(newGoBodyFacts).Pointer()
}
