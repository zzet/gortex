package parser

import (
	"io"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Extractor extracts graph nodes and edges from a single source file.
type Extractor interface {
	Language() string
	Extensions() []string
	Extract(filePath string, src []byte) (*ExtractionResult, error)
}

// ExtractionOptions is immutable request-scoped parser configuration. Its
// fields remain private so an extractor cannot mutate repository-owned state.
// Use NewExtractionOptions to construct a normalized value and accessors to
// obtain defensive copies for serialization.
type ExtractionOptions struct {
	temporalEnvHelpers []string
}

// NewExtractionOptions normalizes Temporal helper names by trimming whitespace,
// dropping empty entries, exact case-sensitive deduplication, and sorting. The
// stable order makes crash-worker serialization deterministic without changing
// Go identifier semantics.
func NewExtractionOptions(temporalEnvHelpers []string) ExtractionOptions {
	if len(temporalEnvHelpers) == 0 {
		return ExtractionOptions{}
	}
	seen := make(map[string]struct{}, len(temporalEnvHelpers))
	normalized := make([]string, 0, len(temporalEnvHelpers))
	for _, name := range temporalEnvHelpers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		return ExtractionOptions{}
	}
	sort.Strings(normalized)
	return ExtractionOptions{temporalEnvHelpers: normalized}
}

// TemporalEnvHelpers returns a defensive copy of the configured names.
func (o ExtractionOptions) TemporalEnvHelpers() []string {
	return append([]string(nil), o.temporalEnvHelpers...)
}

// OptionsExtractor is an optional capability. Extractor remains unchanged so
// existing languages and external plugins keep their source compatibility.
type OptionsExtractor interface {
	ExtractWithOptions(filePath string, src []byte, opts ExtractionOptions) (*ExtractionResult, error)
}

// Extract dispatches one request through the optional options-aware capability,
// falling back to the base Extractor contract for ordinary languages/plugins.
func Extract(e Extractor, filePath string, src []byte, opts ExtractionOptions) (*ExtractionResult, error) {
	if oe, ok := e.(OptionsExtractor); ok {
		return oe.ExtractWithOptions(filePath, src, opts)
	}
	return e.Extract(filePath, src)
}

// PreParser is an optional Extractor capability: a source-rewriting hook run
// before tree-sitter parsing. It lets a language neutralise constructs that
// confuse the grammar (e.g. C-family conditional-compilation directives that
// detach an enclosing declaration) without discarding any code.
//
// Implementations MUST preserve byte offsets and line counts exactly — the
// returned slice has the same length as the input and every newline stays in
// place — so all extracted node ranges, line numbers, and downstream
// resolution remain byte-accurate. Returning nil means "no rewrite".
//
// The hook is a first-class, language-agnostic interface rather than a
// per-language private step: any extractor opts in by implementing it, and the
// same offset-preserving rewrite machinery is then reusable across languages.
type PreParser interface {
	PreParse(src []byte) []byte
}

// ApplyPreParse runs e's PreParse hook when e implements PreParser and the hook
// returns a non-nil rewrite; otherwise it returns src unchanged. The identity
// default means extractors opt in by implementing PreParser, with no behaviour
// change for those that don't.
func ApplyPreParse(e Extractor, src []byte) []byte {
	if pp, ok := e.(PreParser); ok {
		if rewritten := pp.PreParse(src); rewritten != nil {
			return rewritten
		}
	}
	return src
}

// StreamingExtractor is an optional Extractor capability: an extractor that
// reads the file itself — one unit (page / slide / sheet) at a time — through
// an io.ReaderAt, emitting nodes and edges as it goes, instead of receiving the
// whole file as a byte slice. Content extractors (PDF, office documents)
// implement it because their inputs are large and their per-unit processing
// never needs the whole file resident. The indexer prefers this path when it is
// available (on the in-process route), so peak memory is O(one unit), not
// O(file).
//
// emit is called once per produced node with that node's outgoing edges (pass
// nil for none). The extractor MUST recover its own per-document panics or
// return an error; the indexer isolates failures either way.
type StreamingExtractor interface {
	Extractor
	ExtractStream(filePath string, r io.ReaderAt, size int64, emit func(*graph.Node, []*graph.Edge)) error
}

// AssetClass labels a non-code extractor by the kind of large
// binary/document/data artifact it ingests, so the indexer can apply
// corpus-admission caps before a heavy file is ever read and extracted.
// Code extractors return the empty class and are never gated.
type AssetClass string

const (
	// AssetDocument is a human document ingested as searchable content
	// (one KindDoc node per page / slide / sheet): PDF, pptx, xlsx,
	// plain text. Large ones dominate parse memory, so they are subject
	// to a per-file size cap.
	AssetDocument AssetClass = "document"
	// AssetData is a pure binary / columnar / vector artifact recorded
	// as a metadata-only node and never parsed: parquet, npy, lance, …
	// Not code-intelligence content; admitted only when opted in.
	AssetData AssetClass = "data"
	// AssetImage is an image asset (metadata-only node). Cheap to
	// admit; kept un-gated by default.
	AssetImage AssetClass = "image"
)

// AssetExtractor is an optional Extractor capability: a non-code extractor
// that ingests large binary/document/data artifacts rather than source.
// The indexer reads AssetClass at walk time (via the registry, keyed by the
// detected language) to decide admission — a per-file size cap for documents
// and an opt-in gate for data assets — so a content-heavy corpus can't pull
// gigabytes of non-source artifacts into the parse pipeline. Extractors that
// don't implement it are treated as code (always admitted).
type AssetExtractor interface {
	Extractor
	AssetClass() AssetClass
}

// AssetClassOf returns e's AssetClass when e implements AssetExtractor, or the
// empty class (code) otherwise. The single place callers map an extractor to
// its admission class.
func AssetClassOf(e Extractor) AssetClass {
	if a, ok := e.(AssetExtractor); ok {
		return a.AssetClass()
	}
	return ""
}

// ExtractionResult holds the nodes and edges extracted from a single
// file, plus an optional handle to the parse tree the extractor used.
//
// When Tree is non-nil the indexer is responsible for releasing it
// after every per-file consumer (contract extractors, body-fact
// resolvers) has run. Languages whose extractor doesn't have a
// downstream consumer for the tree leave Tree as nil and close their
// own trees internally — the contract pipeline degrades to its regex
// fallback for those languages.
type ExtractionResult struct {
	// ParseChecked is a successful full native parse's explicit syntax
	// verdict. A zero error count without this bit is unknown, never clean.
	// Producers set both scalars before releasing their native tree.
	ParseChecked bool
	ParseErrors  int
	Nodes        []*graph.Node
	Edges        []*graph.Edge
	Tree         *ParseTree
	// ConstValues carries the literal value of each KindConstant node
	// whose RHS is a string / numeric literal, for the indexer to persist
	// in the queryable constant_values sidecar (kept out of the gob Meta
	// blob). Keyed by the const node id. Empty for languages / files with
	// no literal constants.
	ConstValues []ConstValue
}

// ConstValue is one constant's persisted literal value: the const node id,
// its file (for file-scoped eviction), and the literal text.
type ConstValue struct {
	NodeID   string
	FilePath string
	Value    string
}
