package indexer

import (
	"context"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Content-admission skip reasons. They ride on a synthetic file node's
// Meta["skip_reason"] so a dropped asset stays listable and index_health
// rolls it up, mirroring the size / timeout / minified skip telemetry.
const (
	skipReasonLargeDocument  = "large_document"    // document over the per-file cap
	skipReasonVectorData     = "vector_data"       // data asset, data indexing off
	skipReasonLargeData      = "large_data_asset"  // data asset over the per-file cap
	skipReasonLargeImage     = "large_image_asset" // image asset over the per-file cap
	skipReasonUntrackedAsset = "untracked_asset"   // asset-class file git does not track
)

// contentAdmissionGate decides, by asset class, whether a non-source artifact
// is admitted into extraction. It is built once before a walk from the
// registry's asset-class map and the resolved config caps, so the hot
// per-file check is a single map lookup. A nil gate is inert — the common
// all-code repo (no asset extractors registered) pays nothing.
type contentAdmissionGate struct {
	classes     map[string]parser.AssetClass
	docLimit    int64
	docCapped   bool
	indexData   bool
	dataLimit   int64
	dataCapped  bool
	imageLimit  int64
	imageCapped bool
}

// newContentAdmissionGate builds the gate from the indexer's registry and
// config. Returns nil when no asset extractors are registered.
func (idx *Indexer) newContentAdmissionGate() *contentAdmissionGate {
	classes := idx.registry.AssetClasses()
	if len(classes) == 0 {
		return nil
	}
	c := idx.config.Content
	docLimit, docCapped := c.EffectiveMaxDocumentBytes()
	dataLimit, dataCapped := c.EffectiveMaxDataBytes()
	imageLimit, imageCapped := c.EffectiveMaxImageBytes()
	return &contentAdmissionGate{
		classes:     classes,
		docLimit:    docLimit,
		docCapped:   docCapped,
		indexData:   c.IndexData,
		dataLimit:   dataLimit,
		dataCapped:  dataCapped,
		imageLimit:  imageLimit,
		imageCapped: imageCapped,
	}
}

// skip reports whether a file of the given walk-time language and size should
// be dropped before it is read and extracted, and the telemetry reason. A
// non-asset language (the gate has no entry for it) is never gated.
func (g *contentAdmissionGate) skip(lang string, size int64) (string, bool) {
	if g == nil {
		return "", false
	}
	switch g.classes[lang] {
	case parser.AssetDocument:
		if g.docCapped && size > g.docLimit {
			return skipReasonLargeDocument, true
		}
	case parser.AssetData:
		if !g.indexData {
			return skipReasonVectorData, true
		}
		if g.dataCapped && size > g.dataLimit {
			return skipReasonLargeData, true
		}
	case parser.AssetImage:
		// An image yields a metadata-only node, but minting it reads and
		// hashes the whole file, so the class needs the same size ceiling
		// documents and data assets already have. It stays admitted by
		// default: the memory blow-up images used to cause was in the
		// literal-search index, and that is fixed in the trigram package
		// by rejecting binary content outright, not by dropping the node.
		if g.imageCapped && size > g.imageLimit {
			return skipReasonLargeImage, true
		}
	}
	return "", false
}

// untrackedAssetGate skips asset-class files (document / data / image) that
// git does not track, when index.skip_untracked_assets is on. It is built
// once per cold walk from the registry's asset-class map and the repo's
// `git ls-files` set. A nil gate is inert — the flag is off, the repo is not
// a git repo, or `git ls-files` failed (admit everything as before).
type untrackedAssetGate struct {
	classes map[string]parser.AssetClass
	tracked map[string]struct{} // absolute paths git tracks
}

// newUntrackedAssetGate builds the gate, returning nil (inert) when the flag
// is off, a content source is installed, no asset extractors are registered,
// or the tracked set can't be resolved.
func (idx *Indexer) newUntrackedAssetGate(ctx context.Context, absRoot string) *untrackedAssetGate {
	if !idx.config.SkipUntrackedAssets {
		return nil
	}
	if idx.contentSource() != nil {
		// The gate's whole question is "does git track this working-tree
		// path", and a snapshot source serves a revision the checkout is not
		// necessarily at — every path in it is by definition committed, and
		// `git ls-files` describes the wrong tree. The gate goes inert rather
		// than answering from a state the pass is not reading, and the index
		// declares the omission in its producer state.
		return nil
	}
	classes := idx.registry.AssetClasses()
	if len(classes) == 0 {
		return nil
	}
	tracked, ok := gitTrackedSet(ctx, absRoot)
	if !ok {
		idx.logger.Info("indexer: skip_untracked_assets is on but the git tracked-set is unavailable; admitting all assets",
			zap.String("root", absRoot))
		return nil
	}
	return &untrackedAssetGate{classes: classes, tracked: tracked}
}

// skip reports whether an asset-class file at absPath should be dropped
// because git does not track it. Non-asset languages (untracked code) and
// tracked assets are never skipped here.
func (g *untrackedAssetGate) skip(lang, absPath string) (string, bool) {
	if g == nil || g.classes[lang] == "" {
		return "", false
	}
	if _, ok := g.tracked[absPath]; ok {
		return "", false
	}
	return skipReasonUntrackedAsset, true
}

// gitTrackedSet returns the set of absolute paths git tracks under root, or
// (nil, false) when root is not a git repo or `git ls-files` fails. The -z
// form is NUL-delimited so paths with spaces / newlines are handled exactly.
func gitTrackedSet(ctx context.Context, root string) (map[string]struct{}, bool) {
	out, err := gitcmd.Output(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, false
	}
	set := make(map[string]struct{})
	for rel := range strings.SplitSeq(out, "\x00") {
		if rel == "" {
			continue
		}
		set[filepath.Join(root, filepath.FromSlash(rel))] = struct{}{}
	}
	return set, true
}

// contentSkipNode builds a synthetic file node for a content / data asset
// dropped by the admission gate, carrying the skip reason and size so the
// file stays visible (queryable, index_health rollup) without being read or
// parsed — the same treatment size-capped files get.
func contentSkipNode(sf skippedFile) *graph.Node {
	return &graph.Node{
		ID:        sf.relPath,
		Kind:      graph.KindFile,
		Name:      filepath.Base(sf.relPath),
		FilePath:  sf.relPath,
		Language:  sf.lang,
		StartLine: 1,
		Meta: map[string]any{
			"skip_reason":            sf.reason,
			"skipped_due_to_content": true,
			"file_size_bytes":        sf.size,
		},
	}
}

// emitContentSkipNodes adds a synthetic file node for every asset dropped by
// the content-admission gate, so the file stays listable with skip telemetry
// instead of vanishing silently.
func (idx *Indexer) emitContentSkipNodes(skipped []skippedFile) {
	if len(skipped) == 0 {
		return
	}
	nodes := make([]*graph.Node, 0, len(skipped))
	for _, sf := range skipped {
		nodes = append(nodes, contentSkipNode(sf))
	}
	idx.applyRepoPrefix(nodes, nil)
	idx.graph.AddBatch(nodes, nil)
}
