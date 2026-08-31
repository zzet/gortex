package indexer

import (
	"sync"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// contentSnippetCap bounds how much of a CONTENT (data_class="content")
// section body is retained on the graph node after its full text has been
// streamed into the dedicated content index. The full body lives in the
// content FTS; the node keeps only this much for display (the why-layer,
// get_symbol_source) — so a repo of hundreds of thousands of sections
// holds ~240 B × N in the graph instead of ~4 KB × N (~17× less text).
const contentSnippetCap = 240

// Cold content writes are accumulated only to these bounded thresholds. The
// SQLite capability applies one atomic file-replacement transaction per
// group; keeping the indexer group smaller than the store's own bounds avoids
// a second split and caps retained full section text while workers continue.
const (
	parseContentBatchFiles = 32
	parseContentBatchItems = 1024
	parseContentBatchBytes = 4 << 20
)

// isContentNode is the indexer-local alias for graph.IsContentNode — the
// shared predicate for a CONTENT section node (KindDoc, data_class=content).
func isContentNode(n *graph.Node) bool {
	return graph.IsContentNode(n)
}

// contentBody returns the full section text carried on a content node, or
// "" if absent.
func contentBody(n *graph.Node) string {
	if n.Meta == nil {
		return ""
	}
	b, _ := n.Meta["section_text"].(string)
	return b
}

// metaInt reads an integer-valued Meta key, tolerating the int / int64 /
// float64 forms a value can take across a gob round-trip.
func metaInt(meta map[string]any, key string) int {
	switch v := meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// contentOrdinal returns the section's ordinal — "ordinal" for text /
// office chunks, "page" for PDF pages.
func contentOrdinal(n *graph.Node) int {
	if o := metaInt(n.Meta, "ordinal"); o != 0 {
		return o
	}
	return metaInt(n.Meta, "page")
}

// safeTruncate returns s clamped to at most maxBytes, never splitting a
// multi-byte rune.
func safeTruncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// leanContentNode strips a content node's full section body down to a
// capped snippet once the full text has been captured for the content
// index. The key stays "section_text" so display consumers keep working;
// only the length changes. "content_indexed" marks that the full body
// lives in the content index (so a reader knows the snippet is partial).
func leanContentNode(n *graph.Node) {
	body := contentBody(n)
	if len(body) <= contentSnippetCap {
		return
	}
	n.Meta["section_text"] = safeTruncate(body, contentSnippetCap)
	n.Meta["content_indexed"] = true
}

// collectContentItems pulls one ContentFTSItem per content section node in
// the batch, carrying the FULL body for the content index. Returns nil
// when the batch has no content.
func collectContentItems(nodes []*graph.Node) []graph.ContentFTSItem {
	var items []graph.ContentFTSItem
	for _, n := range nodes {
		if !isContentNode(n) {
			continue
		}
		body := contentBody(n)
		if body == "" {
			continue
		}
		items = append(items, graph.ContentFTSItem{
			NodeID:   n.ID,
			FilePath: n.FilePath,
			Ordinal:  contentOrdinal(n),
			Body:     body,
		})
	}
	return items
}

// firstContentFilePath returns the FilePath of the first content node in
// the batch (all of one file's content nodes share it), or "" if none. The
// incremental reindex path uses it to wipe a single file's content rows
// before re-streaming.
func firstContentFilePath(nodes []*graph.Node) string {
	for _, n := range nodes {
		if isContentNode(n) {
			return n.FilePath
		}
	}
	return ""
}

// contentSearcher returns the ContentSearcher this index writes content
// section bodies to: the disk sink captured at the shadow swap (so content
// reaches disk even while idx.graph is the in-memory shadow), else
// idx.graph itself when it is a disk store. Returns nil for an in-memory
// store with no content index — in which case content text is left on the
// nodes and falls back to the symbol search (the small-repo / CLI case).
func (idx *Indexer) contentSearcher() graph.ContentSearcher {
	if idx.contentSink != nil {
		return idx.contentSink
	}
	if cs, ok := idx.graph.(graph.ContentSearcher); ok {
		return cs
	}
	return nil
}

// repoContentPresenceReader is the narrow, repo-scoped projection used to
// decide whether a full parse has any old content to replace. SQLite answers
// it with one indexed EXISTS query over its content ownership sidecar. A
// backend without this capability keeps the conservative historical behavior.
type repoContentPresenceReader interface {
	ContentRepoHasRows(repoPrefix string) (bool, error)
}

type repoContentFileWiper interface {
	WipeContentFileInRepo(repoPrefix, filePath string) error
}

// prepareFullContentFileWiper performs the presence projection once, before
// parse workers start, and returns the per-file callback used by the fallback
// streaming path. On a genuinely empty repo the callback still records every
// streamed file for the authoritative end sweep, but avoids one empty DELETE
// transaction per file. Existing, partially written, and unknown stores retain
// crash-safe per-file replacement. Projection errors fail conservative.
func prepareFullContentFileWiper(
	cs graph.ContentSearcher,
	repoPrefix string,
	recordFile func(string),
) (wipeFile func(string) error, projectionErr error, ok bool) {
	wiper, ok := cs.(repoContentFileWiper)
	if !ok {
		return nil, nil, false
	}

	hasPriorContent := true
	if reader, projected := cs.(repoContentPresenceReader); projected {
		hasPriorContent, projectionErr = reader.ContentRepoHasRows(repoPrefix)
		if projectionErr != nil {
			hasPriorContent = true
		}
	}

	return func(filePath string) error {
		recordFile(filePath)
		if !hasPriorContent {
			return nil
		}
		return wiper.WipeContentFileInRepo(repoPrefix, filePath)
	}, projectionErr, true
}

type stagedContentFile struct {
	replacement graph.ContentFTSFileReplacement
	nodes       []*graph.Node
	edges       []*graph.Edge
	onDurable   func()
}

// parseContentBatch couples the content sidecar commit with the corresponding
// graph AddBatch. Content-bearing files wait in a small bounded group; after
// the sidecar replacement commits, lean node copies and all edges land in one
// graph batch. If the sidecar fails, the original full-body nodes land instead,
// preserving the search fallback. Workers may keep inspecting their extractor
// results because only copies are leaned.
type parseContentBatch struct {
	idx      *Indexer
	replacer graph.ContentFTSBatchReplacer

	mu        sync.Mutex
	pending   []stagedContentFile
	itemCount int
	bodyBytes int
	err       error
	onError   func(error)
}

func newParseContentBatch(idx *Indexer) *parseContentBatch {
	cs := idx.contentSearcher()
	if cs == nil {
		return nil
	}
	replacer, ok := cs.(graph.ContentFTSBatchReplacer)
	if !ok {
		return nil
	}
	return &parseContentBatch{idx: idx, replacer: replacer}
}

func (b *parseContentBatch) setErrorHandler(onError func(error)) {
	if b != nil {
		b.onError = onError
	}
}

func (b *parseContentBatch) add(nodes []*graph.Node, edges []*graph.Edge, onDurable func()) bool {
	items := collectContentItems(nodes)
	if len(items) == 0 {
		return false
	}
	filePath := items[0].FilePath
	if filePath == "" {
		return false
	}
	bytes := 0
	for _, item := range items {
		bytes += len(item.NodeID) + len(item.FilePath) + len(item.Body) + 32
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return true
	}
	if len(b.pending) > 0 && (len(b.pending) >= parseContentBatchFiles ||
		b.itemCount+len(items) > parseContentBatchItems ||
		b.bodyBytes+bytes > parseContentBatchBytes) {
		if err := b.flushLocked(); err != nil {
			return true
		}
	}
	b.pending = append(b.pending, stagedContentFile{
		replacement: graph.ContentFTSFileReplacement{FilePath: filePath, Items: items},
		nodes:       nodes,
		edges:       edges,
		onDurable:   onDurable,
	})
	b.itemCount += len(items)
	b.bodyBytes += bytes
	if len(b.pending) >= parseContentBatchFiles ||
		b.itemCount >= parseContentBatchItems || b.bodyBytes >= parseContentBatchBytes {
		_ = b.flushLocked()
	}
	return true
}

func (b *parseContentBatch) flush() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	return b.flushLocked()
}

func (b *parseContentBatch) flushLocked() error {
	if len(b.pending) == 0 {
		return b.err
	}
	replacements := make([]graph.ContentFTSFileReplacement, len(b.pending))
	nodeCount, edgeCount := 0, 0
	for i, file := range b.pending {
		replacements[i] = file.replacement
		nodeCount += len(file.nodes)
		edgeCount += len(file.edges)
	}
	err := b.replacer.ReplaceContentFiles(b.idx.RepoPrefix(), replacements)

	nodes := make([]*graph.Node, 0, nodeCount)
	edges := make([]*graph.Edge, 0, edgeCount)
	for _, file := range b.pending {
		if err == nil {
			for _, node := range file.nodes {
				if !isContentNode(node) {
					nodes = append(nodes, node)
					continue
				}
				cp := *node
				cp.Meta = make(map[string]any, len(node.Meta))
				for key, value := range node.Meta {
					cp.Meta[key] = value
				}
				leanContentNode(&cp)
				nodes = append(nodes, &cp)
			}
		} else {
			nodes = append(nodes, file.nodes...)
		}
		edges = append(edges, file.edges...)
	}
	if err != nil {
		b.idx.logger.Warn("indexer: batched content replacement failed; retaining full section text on nodes",
			zap.Int("files", len(b.pending)), zap.Error(err))
	}
	graphErr := graph.AddBatchChecked(b.idx.graph, nodes, edges)
	if graphErr == nil {
		for _, file := range b.pending {
			if file.onDurable != nil {
				file.onDurable()
			}
		}
	} else if b.err == nil {
		b.err = graphErr
		if b.onError != nil {
			b.onError(graphErr)
		}
	}
	clear(b.pending)
	b.pending = b.pending[:0]
	b.itemCount = 0
	b.bodyBytes = 0
	return b.err
}

func (b *parseContentBatch) discard() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.pending)
	b.pending = b.pending[:0]
	b.itemCount = 0
	b.bodyBytes = 0
}

// replaceContentSections is the atomic one-file path used by partial indexing
// and the compatibility cold fallback. An explicit filePath makes an empty
// section set authoritative, removing stale content when a document becomes
// empty or changes classification.
func (idx *Indexer) replaceContentSections(filePath string, nodes []*graph.Node, repoScoped bool) bool {
	cs := idx.contentSearcher()
	if cs == nil || filePath == "" {
		return false
	}
	items := collectContentItems(nodes)
	var err error
	if replacer, ok := cs.(graph.ContentFTSBatchReplacer); ok {
		err = replacer.ReplaceContentFiles(idx.RepoPrefix(), []graph.ContentFTSFileReplacement{{
			FilePath: filePath,
			Items:    items,
		}})
	} else {
		if repoScoped {
			if wiper, ok := cs.(interface {
				WipeContentFileInRepo(repoPrefix, filePath string) error
			}); ok {
				err = wiper.WipeContentFileInRepo(idx.RepoPrefix(), filePath)
			} else {
				err = cs.WipeContentFile(filePath)
			}
		} else {
			err = cs.WipeContentFile(filePath)
		}
		if err == nil && len(items) > 0 {
			err = cs.AppendContent(idx.RepoPrefix(), items)
		}
	}
	if err != nil {
		idx.logger.Warn("indexer: content file replacement failed; leaving section text on nodes",
			zap.String("file", filePath), zap.Error(err))
		return false
	}
	for _, node := range nodes {
		if isContentNode(node) {
			leanContentNode(node)
		}
	}
	return true
}

// streamContentSections is the per-file content path: it streams a parsed
// file's content section bodies into the dedicated content index, then
// leans the nodes to a snippet — so the bulk text never enters the graph,
// the symbol search, or the materialising code passes. Called in the parse
// worker before AddBatch, so the nodes added to the graph are already
// lean. A nil content searcher (in-memory store) leaves the nodes full.
func (idx *Indexer) streamContentSections(nodes []*graph.Node) {
	cs := idx.contentSearcher()
	if cs == nil {
		return
	}
	items := collectContentItems(nodes)
	if len(items) == 0 {
		return
	}
	if err := cs.AppendContent(idx.RepoPrefix(), items); err != nil {
		// Keep the full body on the nodes if the append failed, so content
		// stays searchable via the symbol-search fallback rather than lost.
		idx.logger.Warn("indexer: content index append failed; leaving section text on nodes",
			zap.Error(err))
		return
	}
	for _, n := range nodes {
		if isContentNode(n) {
			leanContentNode(n)
		}
	}
}
