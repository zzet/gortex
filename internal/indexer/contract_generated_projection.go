package indexer

import (
	"reflect"
	"strings"

	"github.com/zzet/gortex/internal/parser"
)

// A generated C parser has no local records when no configured contract
// extractor applies. Its public declarations must still participate in foreign
// handler/type lookup dependencies. This proof covers the accepted public-entry
// projection, not the omitted internal C declarations or runtime behavior.
func (idx *Indexer) contractGeneratedProjectionPolicy(path, language string, src []byte, result *parser.ExtractionResult, applicableExtractors int) (int, bool) {
	if language != "c" || idx.config.IndexGeneratedParsers || applicableExtractors != 0 || result == nil || result.Tree != nil || len(result.ConstValues) != 0 {
		return 0, false
	}
	rel := path
	if idx.repoPrefix != "" {
		rel = strings.TrimPrefix(path, idx.repoPrefix+"/")
	}
	expected, recognized := generatedTreeSitterParserProjection(rel, language, src)
	if !recognized {
		return 0, false
	}
	// Foreground accepted rows can already carry the repository namespace;
	// background reconstruction still returns raw extraction rows. Compare
	// against the exact same current policy in whichever form was captured.
	if idx.repoPrefix != "" && len(result.Nodes) > 0 && result.Nodes[0].RepoPrefix == idx.repoPrefix {
		idx.applyRepoPrefix(expected.Nodes, expected.Edges)
	}
	if !reflect.DeepEqual(result.Nodes, expected.Nodes) || !reflect.DeepEqual(result.Edges, expected.Edges) {
		return 0, false
	}
	return generatedParserProjectionPolicyVersion, true
}
