package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
)

// errSymbolRemoved reports that a symbol the graph still lists is no longer
// declared in the file's current bytes: an edit removed it before the index
// caught up.
var errSymbolRemoved = errors.New("symbol was removed from the file since it was indexed")

// errSymbolAmbiguous reports that a symbol's ID was derived from its position
// and declarations sharing that ID were added or removed since the file was
// indexed, so the current bytes cannot say which of them it is.
var errSymbolAmbiguous = errors.New("symbol cannot be told apart from same-named declarations added or removed since the file was indexed")

// nodeLinesIn returns node's 1-based line range within content, the bytes
// the caller read for node's file. The stored range is used only while the
// index records exactly these bytes and still holds that range; otherwise the
// bytes are parsed once and the symbol is located again, so a range read from
// one version of a file is never applied to another.
func (s *Server) nodeLinesIn(ctx context.Context, node *graph.Node, content []byte) (int, int, error) {
	owner, rel := s.indexerForRel(node.FilePath)
	if owner == nil || rel == "" {
		return 0, 0, fmt.Errorf("no indexer owns %s", node.FilePath)
	}
	if !requestViewFromContext(ctx).routed() && owner.IndexedContentMatches(rel, content) {
		// A reindex writes a file's nodes before its content identity, so
		// the node read now belongs to these bytes. node may have been read
		// before that reindex committed.
		if live := owner.Graph().GetNode(node.ID); live != nil &&
			live.StartLine == node.StartLine && live.EndLine == node.EndLine {
			return node.StartLine, node.EndLine, nil
		}
	}
	if requestViewFromContext(ctx).routed() {
		if allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); ok && allowance.tool == "get_symbol_source" {
			abs, err := s.resolveNodePath(ctx, node)
			if err != nil {
				return 0, 0, err
			}
			if nodes, matched := s.readFileCompressionSymbols(ctx, abs, node.FilePath, content); matched {
				for _, live := range nodes {
					if live.ID == node.ID && live.RepoPrefix == node.RepoPrefix && live.Kind == node.Kind && live.StartLine == node.StartLine && live.EndLine == node.EndLine {
						return node.StartLine, node.EndLine, nil
					}
				}
			}
		}
	}

	s.noteAcknowledgementParser(ctx, "source_extraction_for_symbol_location")
	parseLap := s.beginAcknowledgementLap(ctx, editAckParseGate)
	extracted, err := owner.ExtractSource(ctx, rel, content)
	parseLap.finish()
	if err != nil {
		return 0, 0, fmt.Errorf("locate %s in the current file: %w", node.ID, err)
	}
	defer extracted.ReleaseTree()
	return s.declarationLinesIn(ctx, node, extracted.Nodes)
}

func (s *Server) declarationLinesIn(ctx context.Context, node *graph.Node, nodes []*graph.Node) (int, int, error) {
	_, local, ok := strings.Cut(node.ID, "::")
	if !ok {
		return 0, 0, fmt.Errorf("symbol %s has no file-local id", node.ID)
	}
	// A declaration is located by its order among the declarations that
	// share its ID base, as the reader that produced node listed them. An
	// ID with no position in it is unique, so that is an exact match; a
	// positional one pairs only while the count holds.
	base := declarationBase(local)
	current := declarationsSharing(nodes, base)
	if len(current) == 0 {
		return 0, 0, errSymbolRemoved
	}
	stored := declarationsSharing(s.readerFor(ctx).GetFileNodes(node.FilePath), base)
	at := slices.IndexFunc(stored, func(n *graph.Node) bool { return n.ID == node.ID })
	if at < 0 || len(stored) != len(current) {
		return 0, 0, fmt.Errorf("%w: %s", errSymbolAmbiguous, node.ID)
	}
	return current[at].StartLine, current[at].EndLine, nil
}

// declarationBase strips the position an extractor appends to a file-local
// ID to keep same-named declarations apart: _L<line> on a redeclaration
// (the first keeps the bare ID), @<line> on an object member or anonymous
// class, and @+<offset> on a local binding.
func declarationBase(local string) string {
	trimmed := strings.TrimRight(local, "0123456789")
	if len(trimmed) == len(local) {
		return local
	}
	for _, marker := range []string{"_L", "@+", "@"} {
		if base, ok := strings.CutSuffix(trimmed, marker); ok {
			return base
		}
	}
	return local
}

// declarationsSharing returns the nodes whose file-local ID has base, in
// source order, keeping one node per ID.
func declarationsSharing(nodes []*graph.Node, base string) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if _, local, ok := strings.Cut(n.ID, "::"); ok && declarationBase(local) == base {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b *graph.Node) int {
		return cmp.Or(cmp.Compare(a.StartLine, b.StartLine), strings.Compare(a.ID, b.ID))
	})
	return slices.CompactFunc(out, func(a, b *graph.Node) bool { return a.ID == b.ID })
}

// nodeSource is a node's current line range and the content it indexes into.
type nodeSource struct {
	content   string // UTF-16 sources decoded, as the index reads them
	startLine int
	endLine   int
}

// readNodeSource reads the file at absPath that declares node and resolves
// node's range against exactly the bytes it read. An editor overlay is served
// with the node's own range, since the overlay view parsed that content.
func (s *Server) readNodeSource(ctx context.Context, node *graph.Node, absPath string) (nodeSource, error) {
	caller := ctx
	if deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	if content, ok := s.overlayContentFor(ctx, absPath); ok {
		return nodeSource{content: content, startLine: node.StartLine, endLine: node.EndLine}, nil
	}
	owner, _ := s.indexerForRel(node.FilePath)
	if owner == nil {
		return nodeSource{}, fmt.Errorf("no indexer owns %s", node.FilePath)
	}
	if reason := requestDeferredProjectionFiles(ctx)[node.FilePath]; reason == "storm" || reason == "external_write" {
		return nodeSource{}, errProjectedSourceDeferred
	}
	root, rel, key, err := selectedProjectionReadPath(ctx, owner.RootPath(), owner.RepoPrefix(), absPath)
	if err != nil {
		return nodeSource{}, err
	}
	files, err := source.NewFilesystemSource(root)
	if err != nil {
		return nodeSource{}, err
	}
	defer files.Close()
	raw, _, err := func() ([]byte, source.FileMeta, error) {
		release := s.beginEditedFileRead(ctx, absPath)
		defer release()
		return files.ReadRegularFile(ctx, filepath.ToSlash(rel), 1<<20)
	}()
	if err != nil {
		if caller.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nodeSource{}, errProjectedSourceDeferred
		}
		return nodeSource{}, err
	}
	var entry *projectionEntry
	if facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts); ok {
		facts.mu.RLock()
		entry = facts.entries[key]
		facts.mu.RUnlock()
	}
	if entry != nil {
		if !liveProjectionForAcknowledgement(entry) || entry.rawHash != blake3.Sum256(raw) {
			return nodeSource{}, invalidateProjectedSource(entry)
		}
		current := entry.layer.NodeByID(node.ID)
		if current == nil {
			return nodeSource{}, errSymbolRemoved
		}
		return nodeSource{content: string(indexer.DecodeUTF16Source(raw)), startLine: current.StartLine, endLine: current.EndLine}, nil
	}
	start, end, err := s.nodeLinesIn(ctx, node, raw)
	if err != nil {
		if caller.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nodeSource{}, errProjectedSourceDeferred
		}
		return nodeSource{}, err
	}
	return nodeSource{content: string(indexer.DecodeUTF16Source(raw)), startLine: start, endLine: end}, nil
}

// symbolLinesUnderLock is the write-side resolver. The source snapshot and
// declaration identity are checked only while this request owns the path.
func (s *Server) symbolLinesUnderLock(ctx context.Context, node *graph.Node, absPath string, content []byte) (int, int, error) {
	if !mutationPathAdmitted(ctx, pathkey.MutationKey(absPath)) {
		return 0, 0, fmt.Errorf("%w: symbol path is not held", errMutationNotApplied)
	}
	key := pathkey.MutationKey(absPath)
	entry := pendingFileProjections.snap.Load().entries[key]
	if entry != nil && !entry.tombstone && entry.graphPath == node.FilePath && entry.rawHash == blake3.Sum256(content) {
		start, end, err := s.declarationLinesIn(ctx, node, entry.layer.FileNodes(node.FilePath))
		if pendingFileProjections.snap.Load().entries[key] == entry {
			return start, end, err
		}
	}
	return s.nodeLinesIn(ctx, node, content)
}
