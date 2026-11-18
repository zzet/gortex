package indexer

import (
	"errors"
	"github.com/zzet/gortex/internal/parser"
)

var ErrSourceDeclarationGraphRequired = errors.New("source declaration needs indexed transform")

// PrepareSourceDeclaration preserves index-time language and transform policy
// without consulting or mutating the relationship graph.
func (idx *Indexer) PrepareSourceDeclaration(path, graphPath string, src []byte) (parser.Extractor, []byte, bool, error) {
	if idx.transforms != nil {
		for _, transform := range idx.transforms.transforms {
			switch transform.(type) {
			case bomStripTransform, utf16DecodeTransform:
				// Read-only declarations use the same built-in decoding as indexing.
				continue
			}
			if transform.matches(graphPath) || transform.matches(path) {
				return nil, nil, false, ErrSourceDeclarationGraphRequired
			}
		}
	}
	lang, ok := idx.effectiveLanguage(path, src)
	if !ok {
		return nil, nil, false, nil
	}
	extractor, ok := idx.registry.GetByLanguage(lang)
	if !ok {
		return nil, nil, false, nil
	}
	return extractor, idx.transforms.run(graphPath, src), true, nil
}

// NormalizeSourceDeclarations derives retrieval names from this file's syntax
// facts using the same policy as normal indexing.
func NormalizeSourceDeclarations(result *parser.ExtractionResult, src []byte) {
	normalizeExtractionMetadata(result, src)
}
