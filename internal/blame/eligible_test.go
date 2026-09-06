package blame

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestEligibleMatchesWhatEnrichGraphAdmits pins the predicate a coverage
// reporter has to agree with. Eligible is exported so callers can count the
// population this pass actually considers; if it drifted wider than
// EnrichGraph's own admission, every such caller would report a permanent
// shortfall that no enrichment could close, and if it drifted narrower they
// would report full coverage over symbols the pass never stamped.
func TestEligibleMatchesWhatEnrichGraphAdmits(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *graph.Node
		want bool
	}{
		{"function", &graph.Node{Kind: graph.KindFunction, FilePath: "a.go", StartLine: 1}, true},
		{"method", &graph.Node{Kind: graph.KindMethod, FilePath: "a.go", StartLine: 1}, true},
		{"type", &graph.Node{Kind: graph.KindType, FilePath: "a.go", StartLine: 1}, true},
		{"interface", &graph.Node{Kind: graph.KindInterface, FilePath: "a.go", StartLine: 1}, true},
		{"field", &graph.Node{Kind: graph.KindField, FilePath: "a.go", StartLine: 1}, true},
		{"variable", &graph.Node{Kind: graph.KindVariable, FilePath: "a.go", StartLine: 1}, true},
		{"constant", &graph.Node{Kind: graph.KindConstant, FilePath: "a.go", StartLine: 1}, true},
		{"enum member", &graph.Node{Kind: graph.KindEnumMember, FilePath: "a.go", StartLine: 1}, true},

		// Kinds this pass has never stamped. A file or package node is not an
		// unstamped symbol; it is not a symbol.
		{"file", &graph.Node{Kind: graph.KindFile, FilePath: "a.go", StartLine: 1}, false},
		{"import", &graph.Node{Kind: graph.KindImport, FilePath: "a.go", StartLine: 1}, false},
		{"contract", &graph.Node{Kind: graph.KindContract, FilePath: "a.go", StartLine: 1}, false},

		// Position is required: EnrichGraph groups by path and picks the
		// latest author across [StartLine, EndLine], so neither can be absent.
		{"no path", &graph.Node{Kind: graph.KindFunction, StartLine: 1}, false},
		{"no line", &graph.Node{Kind: graph.KindFunction, FilePath: "a.go"}, false},
		{"nil node", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eligible(tc.node); got != tc.want {
				t.Fatalf("Eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEligibleAgreesWithTheKindSwitch is the drift guard. Eligible must not
// admit a kind shouldEnrichBlame refuses, in either direction — the two are
// one decision, and the exported half is the one other packages count on.
func TestEligibleAgreesWithTheKindSwitch(t *testing.T) {
	kinds := []graph.NodeKind{
		graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface,
		graph.KindField, graph.KindVariable, graph.KindConstant, graph.KindEnumMember,
		graph.KindFile, graph.KindImport, graph.KindContract, graph.KindDoc,
		graph.KindEvent, graph.KindFlag, graph.KindColumn,
	}
	for _, kind := range kinds {
		positioned := &graph.Node{Kind: kind, FilePath: "a.go", StartLine: 1}
		if got, want := Eligible(positioned), shouldEnrichBlame(kind); got != want {
			t.Fatalf("kind %q: Eligible = %v, shouldEnrichBlame = %v", kind, got, want)
		}
	}
}
