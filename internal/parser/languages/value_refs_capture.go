package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
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
func captureValueRefCandidates(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	captureValueRefCandidatesWithGoWork(result, root, filePath, src, nil)
}

func captureValueRefCandidatesWithGoWork(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte, goWork *goTailWork) {
	if root == nil || result == nil {
		return
	}
	var consts map[string]bool
	var funcRanges []funcRange
	if goWork != nil {
		consts = goWork.referenceScope.values
		funcRanges = goWork.referenceScope.ranges
	} else {
		consts = map[string]bool{}
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
		funcRanges = buildFuncRanges(result)
	}
	if len(consts) == 0 {
		return
	}
	if len(funcRanges) == 0 {
		return
	}
	seen := map[string]bool{}
	visit := func(n *sitter.Node, name string) {
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
	}
	if goWork == nil {
		walkNodes(root, func(n *sitter.Node) {
			if isValueRefIdentNode(n.Type()) {
				visit(n, n.Content(src))
			}
		})
	} else {
		for _, fact := range goWork.references {
			if fact.kind == "identifier" {
				visit(fact.node, fact.name)
			}
		}
	}
}

// isValueRefIdentNode reports whether a node type is a value-position
// identifier leaf the constant-read scan should consider. Beyond the common
// `identifier`, Kotlin and Swift use `simple_identifier` and PHP a bare `name`,
// so a constant read in those grammars is captured too.
func isValueRefIdentNode(t string) bool {
	switch t {
	case "identifier", "simple_identifier", "name", "constant":
		return true
	}
	return false
}

// isDistinctiveConstName mirrors the resolver's distinctive-name gate: at least
// 3 characters with an uppercase letter or underscore — the config-constant
// shape, unlikely to collide with an ordinary lowerCamelCase local.
func isDistinctiveConstName(name string) bool {
	if len(name) < 3 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}
