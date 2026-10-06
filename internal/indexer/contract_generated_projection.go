package indexer

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func (idx *Indexer) contractGeneratedProjectionEligible(language string) bool {
	if language != "c" || idx.config.IndexGeneratedParsers {
		return false
	}
	_, byLanguage := idx.buildPerFileContractExtractors()
	return len(byLanguage[language]) == 0
}

func (idx *Indexer) contractFollowupPolicyMode(language, policy string) (bool, error) {
	ordinary, err := contractFollowupPolicy(idx, language)
	if err != nil || policy == ordinary {
		return false, err
	}
	if idx.contractGeneratedProjectionEligible(language) {
		projected, err := contractBoundaryPolicy(idx, language, generatedParserProjectionPolicyVersion)
		if err != nil {
			return false, err
		}
		if policy == projected {
			return true, nil
		}
	}
	return false, graph.ErrContractProjectionStale
}

func contractGeneratedFileHint(node *graph.Node, path string) bool {
	if node == nil || node.ID != path || node.FilePath != path || node.Kind != graph.KindFile {
		return false
	}
	projected, _ := node.Meta[generatedParserProjectionMetaKey].(bool)
	return projected && metaInt(node.Meta, generatedParserProjectionVersionMetaKey) == generatedParserProjectionPolicyVersion
}

// Selected core rows keep resolver/enrichment metadata and decoded numeric
// representations. Verify every projection-owned fact without discarding that
// selected evidence or comparing resolver-added fields with raw extraction.
func (idx *Indexer) contractGeneratedCoreProof(file ContractFollowupFile, src []byte, nodes []*graph.Node, edges []*graph.Edge, core graph.Reader) bool {
	if !idx.contractGeneratedProjectionEligible(file.Language) {
		return false
	}
	rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
	expected, recognized := generatedTreeSitterParserProjection(rel, file.Language, src)
	if !recognized {
		return false
	}
	idx.applyRepoPrefix(expected.Nodes, expected.Edges)
	if len(nodes) != len(expected.Nodes) {
		return false
	}
	byID := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		if node == nil || byID[node.ID] != nil {
			return false
		}
		byID[node.ID] = node
	}
	for _, want := range expected.Nodes {
		got := byID[want.ID]
		if got == nil {
			return false
		}
		// Metadata extensions are retained; every typed declaration field and
		// every parser-owned metadata value must still match accepted bytes.
		gotFields, wantFields := *got, *want
		gotFields.Meta, wantFields.Meta = nil, nil
		if !reflect.DeepEqual(gotFields, wantFields) || !contractProjectionMetadataContains(got.Meta, want.Meta) {
			return false
		}
	}
	structural := 0
	for _, edge := range edges {
		if edge != nil && edge.From == file.Path && (edge.Kind == graph.EdgeDefines || edge.Kind == graph.EdgeImports) {
			structural++
		}
	}
	if structural != len(expected.Edges) {
		return false
	}
	for _, want := range expected.Edges {
		matched := false
		for _, got := range edges {
			if got == nil || got.From != want.From || got.Kind != want.Kind || got.FilePath != want.FilePath || got.Line != want.Line || !contractProjectionMetadataContains(got.Meta, want.Meta) {
				continue
			}
			targetMatches := got.To == want.To
			if !targetMatches && want.Kind == graph.EdgeImports {
				// The existing resolver can externalize or resolve this exact
				// quoted include to its selected header file.
				targetMatches = got.To == "external::tree_sitter/parser.h"
				if !targetMatches && core != nil {
					header := core.GetNode(got.To)
					targetMatches = header != nil && header.Kind == graph.KindFile && header.ID == header.FilePath && strings.HasSuffix(header.FilePath, "/tree_sitter/parser.h")
				}
			}
			if targetMatches {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func contractProjectionMetadataContains(actual, expected map[string]any) bool {
	for key, value := range expected {
		got, exists := actual[key]
		if !exists {
			return false
		}
		wantJSON, wantErr := json.Marshal(value)
		gotJSON, gotErr := json.Marshal(got)
		if wantErr != nil || gotErr != nil || !bytes.Equal(wantJSON, gotJSON) {
			return false
		}
	}
	return true
}

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
