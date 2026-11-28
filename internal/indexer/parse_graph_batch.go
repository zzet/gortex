package indexer

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

const (
	parseGraphBatchFiles = 128
	parseGraphBatchNodes = 12_000
	parseGraphBatchEdges = 48_000
	parseGraphBatchBytes = 32 << 20
)

type parseGraphBatchLimits struct {
	files int
	nodes int
	edges int
	bytes int64
}

var defaultParseGraphBatchLimits = parseGraphBatchLimits{
	files: parseGraphBatchFiles,
	nodes: parseGraphBatchNodes,
	edges: parseGraphBatchEdges,
	bytes: parseGraphBatchBytes,
}

type stagedParseGraph struct {
	nodes          []*graph.Node
	edges          []*graph.Edge
	priorAliasFile string
	onDurable      func()
}

// parseGraphBatch amortises direct-to-SQLite parse writes without building a
// repository-sized in-memory graph. Parsed files are retained only until the
// first file/node/edge/byte threshold is reached, then committed by one
// AddBatch. Callbacks run strictly after AddBatch returns, preserving the
// invariant that a persisted mtime never advertises graph rows that are not yet
// durable. The original slice order and duplicates are retained so backend
// upsert/dedup semantics are unchanged.
type parseGraphBatch struct {
	store  graph.Store
	limits parseGraphBatchLimits

	mu        sync.Mutex
	pending   []stagedParseGraph
	nodeCount int
	edgeCount int
	bytes     int64
}

// newParseGraphBatch enables the accumulator only for the direct durable path.
// FileMtimeWriter is the same capability IndexCtx uses to distinguish SQLite
// from an in-memory whole-repo or streaming-chunk shadow. Unsupported adapters
// retain the existing immediate AddBatch fallback.
func newParseGraphBatch(store graph.Store) *parseGraphBatch {
	if store == nil {
		return nil
	}
	if _, ok := store.(graph.FileMtimeWriter); !ok {
		return nil
	}
	return newParseGraphBatchWithLimits(store, defaultParseGraphBatchLimits)
}

func newParseGraphBatchWithLimits(store graph.Store, limits parseGraphBatchLimits) *parseGraphBatch {
	if store == nil || limits.files <= 0 || limits.nodes <= 0 || limits.edges <= 0 || limits.bytes <= 0 {
		return nil
	}
	return &parseGraphBatch{store: store, limits: limits}
}

func (b *parseGraphBatch) add(nodes []*graph.Node, edges []*graph.Edge, onDurable func()) bool {
	return b.addWithPriorAlias(nodes, edges, "", onDurable)
}

func (b *parseGraphBatch) addWithPriorAlias(nodes []*graph.Node, edges []*graph.Edge, priorAliasFile string, onDurable func()) bool {
	if b == nil {
		return false
	}
	entryBytes := estimateParseGraphBytes(nodes, edges)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) > 0 && b.wouldExceed(len(nodes), len(edges), entryBytes) {
		b.flushLocked()
	}
	b.pending = append(b.pending, stagedParseGraph{
		nodes: nodes, edges: edges, priorAliasFile: priorAliasFile, onDurable: onDurable,
	})
	b.nodeCount += len(nodes)
	b.edgeCount += len(edges)
	b.bytes += entryBytes
	if b.atLimit() {
		b.flushLocked()
	}
	return true
}

func (b *parseGraphBatch) flush() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

func (b *parseGraphBatch) wouldExceed(nodes, edges int, bytes int64) bool {
	return len(b.pending)+1 > b.limits.files ||
		b.nodeCount+nodes > b.limits.nodes ||
		b.edgeCount+edges > b.limits.edges ||
		b.bytes+bytes > b.limits.bytes
}

func (b *parseGraphBatch) atLimit() bool {
	return len(b.pending) >= b.limits.files ||
		b.nodeCount >= b.limits.nodes ||
		b.edgeCount >= b.limits.edges ||
		b.bytes >= b.limits.bytes
}

func (b *parseGraphBatch) flushLocked() {
	if len(b.pending) == 0 {
		return
	}
	var priorFiles []string
	nodes := make([]*graph.Node, 0, b.nodeCount)
	edges := make([]*graph.Edge, 0, b.edgeCount)
	for _, entry := range b.pending {
		if entry.priorAliasFile != "" {
			priorFiles = append(priorFiles, entry.priorAliasFile)
		}
		nodes = append(nodes, entry.nodes...)
		edges = append(edges, entry.edges...)
	}
	if len(priorFiles) > 0 {
		removePriorAliasEdgesBatch(b.store, priorFiles)
	}
	b.store.AddBatch(nodes, edges)
	for _, entry := range b.pending {
		if entry.onDurable != nil {
			entry.onDurable()
		}
	}
	clear(b.pending)
	b.pending = b.pending[:0]
	b.nodeCount = 0
	b.edgeCount = 0
	b.bytes = 0
}

func estimateParseGraphBytes(nodes []*graph.Node, edges []*graph.Edge) int64 {
	var total int64
	for _, node := range nodes {
		if node == nil {
			continue
		}
		total += 256 + int64(len(node.ID)+len(node.Name)+len(node.QualName)+
			len(node.FilePath)+len(node.Language)+len(node.RepoPrefix)+
			len(node.WorkspaceID)+len(node.ProjectID)+len(node.Origin))
		for key, value := range node.Meta {
			total += int64(len(key)) + estimateParseMetaBytes(value)
		}
	}
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		total += 192 + int64(len(edge.From)+len(edge.To)+len(edge.FilePath)+
			len(edge.Origin)+len(edge.ConfidenceLabel)+len(edge.Context)+
			len(edge.ReturnUsage)+len(edge.Via)+len(edge.Alias))
		for key, value := range edge.Meta {
			total += int64(len(key)) + estimateParseMetaBytes(value)
		}
	}
	return total
}

func estimateParseMetaBytes(value any) int64 {
	switch typed := value.(type) {
	case string:
		return int64(len(typed))
	case []byte:
		return int64(len(typed))
	case []string:
		var total int64
		for _, item := range typed {
			total += int64(len(item))
		}
		return total
	case map[string]string:
		var total int64
		for key, item := range typed {
			total += int64(len(key) + len(item))
		}
		return total
	default:
		return 16
	}
}
