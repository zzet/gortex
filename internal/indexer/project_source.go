package indexer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// projectionLanguages are the languages whose single-file projection equals a
// full index of the same bytes: node IDs, kinds and line ranges. The golden
// parity test pins the list; a language that drifts leaves it.
var projectionLanguages = map[string]bool{
	"go":         true,
	"typescript": true,
	"python":     true,
	"c":          true,
	"java":       true,
	"rust":       true,
}

// projectionBuildDeadline bounds one build, including the wait for a parse
// slot. A projection that cannot be built in time is not worth an edit's ack.
const projectionBuildDeadline = 200 * time.Millisecond

// projectionParseAdmission is reserved for projection builds, apart from the
// shared budget bulk indexing fills, so an edit's projection never queues
// behind a cold index. It admits two maximum-size sources at once.
var projectionParseAdmission = newParseAdmissionBudget(2 * maxAdHocExtractSourceBytes)

// Reasons a source is not projected. Callers fall back to the store.
var (
	ErrProjectionLanguage  = errors.New("projection: language not projected")
	ErrProjectionTransform = errors.New("projection: source transform does not preserve coordinates")
	ErrProjectionTooLarge  = errors.New("projection: source over the size limit")
	ErrProjectionNoSlot    = errors.New("projection: no parse slot before the deadline")
	ErrProjectionDeadline  = errors.New("projection: build past the deadline")
	ErrProjectionRefused   = errors.New("projection: extraction refused")
)

// Projection is the graph content one file contributes to the index, built
// from caller-owned bytes without reading the store: the file's nodes and the
// edges they record, stamped as the indexer would stamp them. Edges keep their
// unresolved targets; resolution is cross-file work the projection skips.
type Projection struct {
	GraphPath string
	Language  string
	// ContentHash is computed over the transformed bytes with the same
	// function as FileMetaRow.ContentHash, so the two are equal once the
	// index holds these bytes.
	ContentHash  string
	Nodes        []*graph.Node
	Edges        []*graph.Edge
	ParseErrors  int
	ParseChecked bool // zero errors are trustworthy only with the explicit full-parse proof
	BuildTime    time.Duration
}

// ProjectSource builds the projection of src, the bytes a tool just wrote to
// absPath. It runs the indexer's own transforms, extraction and coverage
// passes, so the nodes equal what a full index stores for the file. Sources
// the indexer would treat differently from a plain parse, and builds that
// cannot finish within projectionBuildDeadline, return one of the
// ErrProjection errors.
func (idx *Indexer) ProjectSource(ctx context.Context, absPath string, src []byte) (*Projection, error) {
	started := time.Now()
	if idx == nil || idx.registry == nil {
		return nil, fmt.Errorf("indexer source projection is unavailable")
	}
	limit := int64(maxAdHocExtractSourceBytes)
	if idx.config.MaxFileSize > 0 && idx.config.MaxFileSize < limit {
		limit = idx.config.MaxFileSize
	}
	if int64(len(src)) > limit {
		return nil, ErrProjectionTooLarge
	}
	language, ok := idx.effectiveLanguage(absPath, src)
	if !ok || !projectionLanguages[language] {
		return nil, ErrProjectionLanguage
	}
	extractor, ok := idx.registry.GetByLanguage(language)
	if !ok {
		return nil, ErrProjectionLanguage
	}
	if _, err := idx.transforms.prepareCoordinateStable(absPath, src); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProjectionTransform, err)
	}

	ctx, cancel := context.WithTimeout(ctx, projectionBuildDeadline)
	defer cancel()
	relPath := idx.relKey(absPath)
	// No command transform matches this path, so run only applies the
	// built-in passes the index applies; hashing its output reproduces
	// the stored content hash.
	transformed := idx.transforms.run(relPath, src)
	rawLease, err := acquireParseAdmission(ctx, int64(len(transformed)), 0, nil, projectionParseAdmission)
	if err != nil {
		return nil, ErrProjectionNoSlot
	}
	// A parse cannot be cancelled, so a build still running at the deadline
	// is left to finish in the background. It keeps its lease until then,
	// which bounds how many abandoned builds can pile up.
	type extraction struct {
		result *parser.ExtractionResult
		err    error
	}
	done := make(chan extraction, 1)
	go func() {
		defer rawLease.Release()
		result, err := idx.extractSourceWith(
			ctx, rawLease, nil,
			absPath, relPath, language, extractor, transformed,
		)
		done <- extraction{result, err}
	}()
	var built extraction
	select {
	case built = <-done:
	case <-ctx.Done():
		go func() {
			if late := <-done; late.result != nil {
				late.result.ReleaseTree()
			}
		}()
		return nil, ErrProjectionDeadline
	}
	switch {
	case built.err != nil && ctx.Err() != nil:
		return nil, ErrProjectionDeadline
	case built.err != nil:
		return nil, fmt.Errorf("%w: %v", ErrProjectionRefused, built.err)
	}
	result := built.result
	defer result.ReleaseTree()

	if !extractionDispositionFor(result).omitSecondarySourceScans() {
		idx.applyCoverageDomains(relPath, language, transformed, result)
	}
	idx.applyRepoPrefix(result.Nodes, result.Edges)
	parseErrors := 0
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindFile {
			parseErrors, _ = n.Meta["parse_errors"].(int)
			break
		}
	}
	if result.ParseChecked && result.ParseErrors >= 0 {
		parseErrors = result.ParseErrors
	}
	if time.Since(started) > projectionBuildDeadline {
		return nil, ErrProjectionDeadline
	}
	return &Projection{
		GraphPath:    idx.prefixPath(relPath),
		Language:     language,
		ContentHash:  contentHashForSource(transformed),
		Nodes:        result.Nodes,
		Edges:        result.Edges,
		ParseErrors:  parseErrors,
		ParseChecked: result.ParseChecked && result.ParseErrors >= 0,
		BuildTime:    time.Since(started),
	}, nil
}

// DiffProjectedNodes lists how a projection's nodes differ from the nodes the
// store holds for the same file, compared by ID, kind and line range. It is
// empty when they agree.
func DiffProjectedNodes(projected, stored []*graph.Node) []string {
	type span struct {
		kind       graph.NodeKind
		start, end int
	}
	want := make(map[string]span, len(stored))
	for _, n := range stored {
		if n != nil {
			want[n.ID] = span{n.Kind, n.StartLine, n.EndLine}
		}
	}
	var diffs []string
	seen := make(map[string]bool, len(projected))
	for _, n := range projected {
		// The store keeps one node per ID, and so does a layer.
		if n == nil || seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		got := span{n.Kind, n.StartLine, n.EndLine}
		have, ok := want[n.ID]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("only projected: %s %s %d-%d", n.ID, n.Kind, n.StartLine, n.EndLine))
		case have != got:
			diffs = append(diffs, fmt.Sprintf("differs: %s projected %s %d-%d, stored %s %d-%d",
				n.ID, got.kind, got.start, got.end, have.kind, have.start, have.end))
		}
		delete(want, n.ID)
	}
	for id, s := range want {
		diffs = append(diffs, fmt.Sprintf("only stored: %s %s %d-%d", id, s.kind, s.start, s.end))
	}
	sort.Strings(diffs)
	return diffs
}
