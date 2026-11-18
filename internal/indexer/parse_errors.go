package indexer

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// stampParseErrors checks the parse tree on result for ERROR/MISSING
// nodes and stamps the count plus a boolean flag onto the file node.
//
// Surfacing this on KindFile lets index_health rank files by parse
// failure and lets agents skip badly-broken sources without re-doing
// the parse. Files whose extractor doesn't keep result.Tree (rare;
// mostly "skip-grammar" stubs that emit a file node and call it
// done) get no stamp — absence of the meta key means "unknown", not
// "clean".
func stampParseErrors(result *parser.ExtractionResult) {
	if result == nil {
		return
	}
	if result.ParseChecked && result.ParseErrors >= 0 {
		stampParseErrorCount(result.Nodes, result.ParseErrors)
		return
	}
	if result.Tree == nil {
		return
	}
	if !result.Tree.HasParseErrors() {
		return
	}
	count := result.Tree.CountParseErrors()
	for _, n := range result.Nodes {
		if n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["parse_errors"] = count
		n.Meta["has_parse_errors"] = true
		break
	}
}
