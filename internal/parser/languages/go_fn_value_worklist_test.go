package languages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func goFnValueEdgeRows(t *testing.T, edges []*graph.Edge) []byte {
	t.Helper()
	type row struct {
		*graph.Edge
		Confidence float64
		Meta       map[string]any
	}
	rows := make([]row, 0, len(edges))
	for _, edge := range edges {
		rows = append(rows, row{edge, edge.Confidence, edge.Meta})
	}
	data, err := json.Marshal(rows)
	require.NoError(t, err)
	return data
}

// The old all-node callback is frozen below. Compare complete ordered edges,
// including private metadata/confidence, rather than just callback counts.
func TestGoFnValueWorklist_FullOrderedAllNodeParity(t *testing.T) {
	const source = `package handlers
import "some/pkg"
type Item struct{}
type Box[T any] struct{}
type Ctrl struct{}
type Request struct { g.Meta ` + "`path:\"/work\"`" + ` }
func Register(router Router, c *Ctrl) {
 router.GET("/", Future, Future, pkg.Foreign, c.Handle)
 var callbacks=[]func(){Future, func(){ _=Future; _=pkg.Foreign }}
 _=callbacks; Future(); _=Box[Item]{}
 _=Generic[Item]; _=c.Handle
}
// Future keeps owned docs.
func Future() {}
func Generic[T any]() {}
func(c *Ctrl) Handle(req *Request) (*Response,error) { _=Future; return nil,nil }
type Response struct{}
func LaterBind(){ router.Bind(new(Ctrl)) }
`
	for _, tc := range []struct{ name, source string }{
		{"ordinary", source}, {"crlf", strings.ReplaceAll(source, "\n", "\r\n")},
		{"no_final_newline", strings.TrimSuffix(source, "\n")}, {"malformed_tail", source + "func Broken("},
		{"only_selector", "package p\nfunc F(){ _=pkg.External }"},
		{"no_callbacks", "package p\nfunc F(){ F(); println(1) }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.source)
			result, err := NewGoExtractor().Extract("callbacks.go", src)
			require.NoError(t, err)
			defer result.ReleaseTree()
			wantAll := goScratchSemantics(t, result)
			wantEdges := goFnValueEdgeRows(t, result.Edges)
			var actual []*graph.Edge
			first := -1
			last := -1
			firstLate := len(result.Edges)
			for i, edge := range result.Edges {
				if edge.Meta["via"] == fnValueCandidateVia {
					if first < 0 {
						first = i
					}
					last = i
					actual = append(actual, edge)
				}
				if edge.Meta["ref_context"] == "generic_arg" || edge.Meta["via"] == goframeRouteViaTag {
					if i < firstLate {
						firstLate = i
					}
				}
			}
			if len(actual) > 0 {
				require.Equal(t, len(actual), last-first+1, "callback edges must remain contiguous")
				require.Less(t, last, firstLate, "FnValue precedes typeargs and GoFrame")
			}
			if tc.name == "ordinary" || tc.name == "only_selector" {
				require.NotEmpty(t, actual)
				found := false
				for _, edge := range actual {
					if edge.Meta["fn_value_ungated"] == true {
						found = true
					}
				}
				require.True(t, found, "qualified selector must produce a nonvacuous oracle case")
			}
			oracle := &parser.ExtractionResult{Nodes: result.Nodes}
			root := result.Tree.Tree().RootNode()
			root.WithScratch(func() { legacyGoFnValueCapture(oracle, root, "callbacks.go", src) })
			require.Equal(t, string(goFnValueEdgeRows(t, oracle.Edges)), string(goFnValueEdgeRows(t, actual)), "complete ordered callback edges must match original all-node path")
			rebuilt := make([]*graph.Edge, 0, len(result.Edges))
			inserted := false
			for _, edge := range result.Edges {
				if edge.Meta["via"] == fnValueCandidateVia {
					if !inserted {
						rebuilt = append(rebuilt, oracle.Edges...)
						inserted = true
					}
					continue
				}
				rebuilt = append(rebuilt, edge)
			}
			require.Equal(t, string(wantEdges), string(goFnValueEdgeRows(t, rebuilt)), "full output order and every surrounding edge must remain unchanged")
			require.Equal(t, string(wantAll), string(goScratchSemantics(t, result)), "old oracle may not mutate owned graph/metadata")
			root.WithScratch(func() { walkNodes(root, func(n *sitter.Node) { require.NotEmpty(t, n.Type()) }) })
			require.Equal(t, string(wantAll), string(goScratchSemantics(t, result)), "values must survive scratch reuse")
		})
	}
}

func legacyGoFnValueCapture(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	lang, fileID := "", ""
	funcs := map[string]bool{}
	for _, n := range result.Nodes {
		if n == nil || n.FilePath != filePath {
			continue
		}
		if n.Kind == graph.KindFile {
			if lang == "" {
				lang = n.Language
			}
			if fileID == "" {
				fileID = n.ID
			}
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			funcs[n.Name] = true
		}
	}
	funcRanges := buildFuncRanges(result)
	if len(funcRanges) == 0 && fileID == "" {
		return
	}
	spec := fnRefSpecFor(lang)
	var cands []FnValueCandidate
	seen := map[string]bool{}
	walkNodes(root, func(n *sitter.Node) {
		// Special whole-node forms (method references, this.m / self.m,
		// Ruby symbol callables, selectors) are normalised to a (member,
		// receiver-hint) pair before the plain-identifier path.
		if refName, recvHint, ok := normalizeFnRefSpecial(n, src); ok {
			typeQualified := recvHint != "" && recvHint != "<self>"
			// Flood guard: a `<self>` / selector member must name a same-file
			// function/method; a type-qualified ref (`Foo::bar`) may bind
			// cross-file via the gate.
			if !typeQualified && !funcs[refName] {
				return
			}
			if fnRefStartsCall(spec, n, src) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			fromID := findEnclosingFunc(funcRanges, line)
			if fromID == "" {
				// A `Foo::bar` in a field or static initializer sits outside
				// every method body, yet is one of the commonest places a
				// method reference appears (constant handler tables,
				// `Comparator` fields). Anchor it to the file node, as the
				// Java type-use pass already does for class-level type refs,
				// instead of dropping the reference entirely. Only the
				// unambiguous `::`-style / selector forms take this fallback —
				// a bare identifier outside any function is far more likely a
				// local or a builtin than a callable reference.
				fromID = fileID
			}
			if fromID == "" {
				return
			}
			key := fromID + "\x00special\x00" + refName + "\x00" + recvHint
			if seen[key] {
				return
			}
			seen[key] = true
			cands = append(cands, FnValueCandidate{
				FromID: fromID, Name: refName, FilePath: filePath, Line: line,
				Form: "special", RecvHint: recvHint, Lang: lang, Ungated: typeQualified,
			})
			return
		}
		if !spec.matchesIDNode(n.Type()) {
			return
		}
		// Skip an identifier that is a component of a special form already
		// captured as a whole (the `valueOf` in `Demo::valueOf`, the `handle`
		// in `self.handle`), so it is not double-counted as a bare name.
		if p := n.Parent(); p != nil {
			if _, _, ok := normalizeFnRefSpecial(p, src); ok {
				return
			}
		}
		name := fnRefNodeName(n, src)
		if name == "" {
			return
		}
		ungated := false
		if !funcs[name] {
			// A name the file does not declare can only bind cross-module, and
			// only when it is an explicit qualified path (e.g. Rust `m::f`) —
			// a bare identifier that is not a same-file function is a local /
			// param / builtin and is dropped to avoid flooding the gate.
			if !spec.ungated || !isQualifiedFnRefNode(n.Type()) {
				return
			}
			ungated = true
		}
		if fnRefStartsCall(spec, n, src) {
			return // callee of a call (incl. tagged template), not a value use
		}
		if fnRefIsCallableDeclName(n) {
			return // the declaration of the function, not a use of it
		}
		line := int(n.StartPoint().Row) + 1
		fromID := findEnclosingFunc(funcRanges, line)
		if fromID == "" {
			return
		}
		key := fromID + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		cands = append(cands, FnValueCandidate{
			FromID: fromID, Name: name, FilePath: filePath, Line: line,
			Form: spec.fnRefForm(n), Lang: lang, Ungated: ungated,
		})
	})
	EmitFnValueCandidates(result, cands)
}
