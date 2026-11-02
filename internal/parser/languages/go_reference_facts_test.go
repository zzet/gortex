package languages

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"strings"
	"testing"
)

// captureValueRefCandidates records a value-reference candidate read for every
// distinctive identifier in the parse tree that names a file-scope constant or
// variable this extractor just emitted, attributed to its enclosing function.
// The resolver's ResolveValueRefs then binds (or shadow-prunes) each candidate
// into a tiered EdgeReads, so a config constant's blast radius reaches its
// readers — closing a change-impact gap that plain call/arg extraction misses.
//
// It is grammar-agnostic: it keys on `identifier` leaf nodes (the value-position
// token in nearly every tree-sitter grammar) and pre-filters to names the file
// actually declares, so it never emits a candidate that cannot bind. Call it
// once at the end of an extractor's Extract with the parse-tree root.
func legacyGoValueRefCapture(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	consts := map[string]bool{}
	for _, n := range result.Nodes {
		if n == nil || n.FilePath != filePath {
			continue
		}
		if n.Kind != graph.KindConstant && n.Kind != graph.KindVariable {
			continue
		}
		if isDistinctiveConstName(n.Name) {
			consts[n.Name] = true
		}
	}
	if len(consts) == 0 {
		return
	}
	funcRanges := buildFuncRanges(result)
	if len(funcRanges) == 0 {
		return
	}
	seen := map[string]bool{}
	walkNodes(root, func(n *sitter.Node) {
		if !isValueRefIdentNode(n.Type()) {
			return
		}
		name := n.Content(src)
		if !consts[name] {
			return
		}
		line := int(n.StartPoint().Row) + 1
		fromID := findEnclosingFunc(funcRanges, line)
		if fromID == "" || fromID == filePath {
			return // not inside a function (e.g. the declaration itself)
		}
		key := fromID + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: fromID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads,
			FilePath: filePath, Line: line, Origin: graph.OriginSpeculative,
			Meta: map[string]any{"via": "value_ref_candidate", "name": name},
		})
	})
}

// The original value-reference walk above and original function-reference walk
// in go_fn_value_worklist_test.go independently freeze both ordered consumers.
func TestGoReferenceFacts_OriginalOrderedConsumers(t *testing.T) {
	const source = `package p
import "pkg"
type Box[T any] struct{}
type Ctrl struct{}
const CONFIG_LIMIT = 42
var Shared_Value = CONFIG_LIMIT
func Register(c *Ctrl) {
 _=CONFIG_LIMIT; _=CONFIG_LIMIT; _=Shared_Value
 _=Future; _=Future; _=pkg.External; _=c.Handle
 _=Box[int]{}; _=Generic[int]
 f:=func(){ _=CONFIG_LIMIT; _=Future; _=pkg.External }; f()
 Future(); Generic[int]()
}
func Future() {}
func Generic[T any]() {}
func(c *Ctrl) Handle() { _=Shared_Value; _=Future }
`
	for _, tc := range []struct{ name, src string }{
		{"ordinary", source}, {"crlf", strings.ReplaceAll(source, "\n", "\r\n")},
		{"no_final_newline", strings.TrimSuffix(source, "\n")},
		{"malformed", source + "func Broken( { _=CONFIG_LIMIT; _=pkg.External; _=Future }"},
		{"forward", "package p\nfunc F(){ _=LATE_CONST; _=Later; _=pkg.Foreign }\nconst LATE_CONST=1\nfunc Later(){}"},
		{"unicode", "package p\nconst CONFIG_値=1\nfunc F(){ _=CONFIG_値; _=Later }\nfunc Later(){}"},
		{"collisions", "package p\nconst SAME_NAME=1\nfunc F(SAME_NAME int){ _=SAME_NAME; _=pkg.F; _=pkg.External }"},
		{"no_values", "package p\nfunc F(){ _=pkg.Foreign }"},
		{"no_functions", "package p\nconst CONFIG=1\nvar TABLE=CONFIG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			result, err := NewGoExtractor().Extract("facts.go", src)
			require.NoError(t, err)
			defer result.ReleaseTree()
			before := goScratchSemantics(t, result)
			var actual []*graph.Edge
			first, last := -1, -1
			for i, e := range result.Edges {
				if e.Meta["via"] == "value_ref_candidate" || e.Meta["via"] == fnValueCandidateVia {
					if first < 0 {
						first = i
					}
					last = i
					actual = append(actual, e)
				}
			}
			oracle := &parser.ExtractionResult{Nodes: result.Nodes}
			root := result.Tree.Tree().RootNode()
			root.WithScratch(func() {
				legacyGoValueRefCapture(oracle, root, "facts.go", src)
				legacyGoFnValueCapture(oracle, root, "facts.go", src)
			})
			require.Equal(t, string(goFnValueEdgeRows(t, oracle.Edges)), string(goFnValueEdgeRows(t, actual)), "full ordered values then callbacks, all metadata/confidence")
			if len(actual) > 0 {
				require.Equal(t, len(actual), last-first+1, "both reference phases remain adjacent")
			}
			if tc.name == "ordinary" || tc.name == "forward" {
				require.NotEmpty(t, actual)
				foundValue, foundFn := false, false
				for _, e := range actual {
					foundValue = foundValue || e.Meta["via"] == "value_ref_candidate"
					foundFn = foundFn || e.Meta["via"] == fnValueCandidateVia
				}
				require.True(t, foundValue && foundFn)
			}
			// Compare all-child traversal parent identity including malformed/nested nodes.
			root.WithScratch(func() {
				var visit func(*sitter.Node, *sitter.Node)
				visit = func(n, parent *sitter.Node) {
					if n == nil {
						return
					}
					got := n.Parent()
					if parent == nil {
						require.Nil(t, got)
					} else {
						require.NotNil(t, got)
						require.Equal(t, parent.Id(), got.Id())
					}
					for i, count := 0, int(n.ChildCount()); i < count; i++ {
						visit(n.Child(i), n)
					}
				}
				visit(root, nil)
			})

			// Compare the candidate's filtered queue to an independent original full
			// walk, including parent identities, name bytes and duplicate preorder.
			root.WithScratch(func() {
				functions, values := map[string]bool{}, map[string]bool{}
				for _, node := range result.Nodes {
					if node == nil || node.FilePath != "facts.go" {
						continue
					}
					if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
						functions[node.Name] = true
					}
					if (node.Kind == graph.KindConstant || node.Kind == graph.KindVariable) && isDistinctiveConstName(node.Name) {
						values[node.Name] = true
					}
				}
				type factRow struct {
					node, parent           uintptr
					kind, parentKind, name string
				}
				var want, got []factRow
				walkNodes(root, func(n *sitter.Node) {
					kind := n.Type()
					if kind != "identifier" && kind != "selector_expression" {
						return
					}
					name := fnRefNodeName(n, src)
					if kind == "identifier" && !functions[name] && !values[name] {
						return
					}
					row := factRow{node: n.Id(), kind: kind, name: name}
					if parent := n.Parent(); parent != nil {
						row.parent = parent.Id()
						row.parentKind = parent.Type()
					}
					want = append(want, row)
				})
				work := collectGoTailWork(root, src, result, "facts.go", buildFuncRanges(result))
				for _, fact := range work.references {
					row := factRow{node: fact.node.Id(), kind: fact.kind, parentKind: fact.parentKind, name: fact.name}
					if fact.parent != nil {
						row.parent = fact.parent.Id()
					}
					got = append(got, row)
				}
				require.Equal(t, want, got, "full unpruned all-child reference preorder, duplicates and parent context")
			})
			require.Equal(t, string(before), string(goScratchSemantics(t, result)), "owned values survive both consumers and scratch reuse")
		})
	}
}
