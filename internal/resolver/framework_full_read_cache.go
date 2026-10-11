package resolver

import (
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// The cold framework loop may reuse decoded node projections, but it must
	// not become another unbounded whole-graph cache. This budget is temporary
	// and includes unique nodes, query pointer slices, and member projections.
	frameworkFullReadCacheRowCap            = 262144
	frameworkFullReadCacheByteCap           = 128 << 20
	frameworkFullReadCacheQueryTelemetryCap = 256
)

type FrameworkFullReadCacheQueryStats struct {
	Epoch       int              `json:"epoch"`
	Kinds       []graph.NodeKind `json:"kinds"`
	Requests    int              `json:"requests"`
	Loads       int              `json:"loads"`
	Hits        int              `json:"hits"`
	Bypasses    int              `json:"bypasses"`
	Admissions  int              `json:"admissions"`
	LoadedRows  int              `json:"loaded_rows"`
	LoadedBytes int              `json:"loaded_bytes"`
	LoadNanos   int64            `json:"load_nanos"`
}

type FrameworkFullReadCacheStats struct {
	NodeHits                  int                                `json:"node_hits"`
	NodeMisses                int                                `json:"node_misses"`
	MemberHits                int                                `json:"member_hits"`
	MemberMisses              int                                `json:"member_misses"`
	Bypasses                  int                                `json:"bypasses"`
	RetainedRows              int                                `json:"retained_rows"`
	RetainedBytes             int                                `json:"retained_bytes"`
	RowCap                    int                                `json:"row_cap"`
	ByteCap                   int                                `json:"byte_cap"`
	NodeQueries               []FrameworkFullReadCacheQueryStats `json:"node_queries,omitempty"`
	DroppedNodeQueryTelemetry int                                `json:"dropped_node_query_telemetry,omitempty"`
	EvictedQueries            int                                `json:"evicted_queries,omitempty"`
	EvictionRebuilds          int                                `json:"eviction_rebuilds,omitempty"`
	EvictionRowsScanned       int                                `json:"eviction_rows_scanned,omitempty"`
	EvictionNanos             int64                              `json:"eviction_nanos,omitempty"`
	PeakEstimatedRows         int                                `json:"peak_estimated_rows"`
	PeakEstimatedBytes        int                                `json:"peak_estimated_bytes"`
}

type frameworkNodeProjection struct {
	nodes           []*graph.Node
	unique          map[string]*graph.Node
	rows            int
	bytes           int
	additionalRows  int
	additionalBytes int
}

type frameworkNodeRetentionState struct {
	queries    map[string][]*graph.Node
	order      []string
	hits       map[string]int
	nodesByID  map[string]*graph.Node
	nodeRows   int
	nodeBytes  int
	queryBytes int
}

// frameworkFullReadCache owns immutable node-side projections shared by the
// serial cold/full framework synthesizers. Framework passes mutate edges, not
// declarations; the batching facade invalidates this cache if a future pass
// does mutate nodes or member_of relationships.
type frameworkFullReadCache struct {
	rowCap  int
	byteCap int

	nodeQueries    map[string][]*graph.Node
	nodeQueryOrder []string
	nodeQueryHits  map[string]int
	nodesByID      map[string]*graph.Node
	nodeRows       int
	nodeBytes      int
	queryBytes     int

	memberMethods      map[string][]graph.MemberMethodInfo
	memberMethodsReady bool
	memberRows         int
	memberBytes        int

	nodeHits     int
	nodeMisses   int
	memberHits   int
	memberMisses int
	bypasses     int

	epoch                     int
	nodeQueryTelemetry        []FrameworkFullReadCacheQueryStats
	nodeQueryTelemetryByKey   map[string]int
	droppedNodeQueryTelemetry int
	evictedQueries            int
	evictionRebuilds          int
	evictionRowsScanned       int
	evictionNanos             int64
	peakEstimatedRows         int
	peakEstimatedBytes        int
}

func newFrameworkFullReadCache() *frameworkFullReadCache {
	return newFrameworkFullReadCacheWithLimits(frameworkFullReadCacheRowCap, frameworkFullReadCacheByteCap)
}

func newFrameworkFullReadCacheWithLimits(rowCap, byteCap int) *frameworkFullReadCache {
	return &frameworkFullReadCache{
		rowCap:                  rowCap,
		byteCap:                 byteCap,
		nodeQueries:             make(map[string][]*graph.Node),
		nodeQueryHits:           make(map[string]int),
		nodesByID:               make(map[string]*graph.Node),
		nodeQueryTelemetryByKey: make(map[string]int),
	}
}

func (c *frameworkFullReadCache) nodesByKinds(kinds []graph.NodeKind, load func([]graph.NodeKind) []*graph.Node) []*graph.Node {
	if c == nil {
		return load(kinds)
	}
	normalized, key := frameworkNodeKindsCacheKey(kinds)
	if len(normalized) == 0 {
		return nil
	}
	telemetryIndex, telemetryOK := c.nodeQueryTelemetryIndex(normalized, key)
	if telemetryOK {
		c.nodeQueryTelemetry[telemetryIndex].Requests++
	}
	if nodes, ok := c.nodeQueries[key]; ok {
		c.nodeHits++
		c.nodeQueryHits[key]++
		if telemetryOK {
			c.nodeQueryTelemetry[telemetryIndex].Hits++
		}
		return nodes
	}

	c.nodeMisses++
	started := time.Now()
	loaded := load(normalized)
	loadNanos := time.Since(started).Nanoseconds()
	projection := prepareFrameworkNodeProjection(loaded, c.nodesByID)
	c.recordProjectionLoad(telemetryIndex, telemetryOK, len(projection.nodes), projection.bytes, loadNanos)
	if c.retainNodeProjection(key, projection, projection.additionalRows, projection.additionalBytes) {
		if telemetryOK {
			c.nodeQueryTelemetry[telemetryIndex].Admissions++
		}
		return c.nodeQueries[key]
	}
	if telemetryOK && c.nodeQueryTelemetry[telemetryIndex].Loads > 1 {
		if c.admitAfterUnusedEviction(key, projection) {
			c.nodeQueryTelemetry[telemetryIndex].Admissions++
			return c.nodeQueries[key]
		}
	}
	c.bypasses++
	if telemetryOK {
		c.nodeQueryTelemetry[telemetryIndex].Bypasses++
	}
	return projection.nodes
}

func prepareFrameworkNodeProjection(nodes []*graph.Node, retained map[string]*graph.Node) frameworkNodeProjection {
	projection := frameworkNodeProjection{
		nodes:           nodes,
		unique:          make(map[string]*graph.Node),
		bytes:           len(nodes) * 8,
		additionalBytes: len(nodes) * 8,
	}
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		if _, exists := projection.unique[node.ID]; exists {
			continue
		}
		projection.unique[node.ID] = node
		projection.rows++
		nodeBytes := frameworkNodeBytes(node)
		projection.bytes += nodeBytes
		if _, exists := retained[node.ID]; !exists {
			projection.additionalRows++
			projection.additionalBytes += nodeBytes
		}
	}
	return projection
}

func (c *frameworkFullReadCache) nodeQueryTelemetryIndex(kinds []graph.NodeKind, key string) (int, bool) {
	telemetryKey := strconv.Itoa(c.epoch) + ":" + key
	if index, ok := c.nodeQueryTelemetryByKey[telemetryKey]; ok {
		return index, true
	}
	if len(c.nodeQueryTelemetry) >= frameworkFullReadCacheQueryTelemetryCap {
		c.droppedNodeQueryTelemetry++
		return 0, false
	}
	index := len(c.nodeQueryTelemetry)
	c.nodeQueryTelemetryByKey[telemetryKey] = index
	c.nodeQueryTelemetry = append(c.nodeQueryTelemetry, FrameworkFullReadCacheQueryStats{
		Epoch: c.epoch,
		Kinds: append([]graph.NodeKind(nil), kinds...),
	})
	return index, true
}

func (c *frameworkFullReadCache) recordProjectionLoad(index int, tracked bool, rows, bytes int, nanos int64) {
	if tracked {
		query := &c.nodeQueryTelemetry[index]
		query.Loads++
		query.LoadedRows += rows
		query.LoadedBytes += bytes
		query.LoadNanos += nanos
	}
	c.recordEstimatedPeak(rows, bytes)
}

func (c *frameworkFullReadCache) recordEstimatedPeak(rows, bytes int) {
	// This is an accounting estimate, not a heap measurement. It captures the
	// temporary coexistence of retained cache data and a newly decoded result.
	estimatedRows := c.nodeRows + c.memberRows + rows
	estimatedBytes := c.nodeBytes + c.queryBytes + c.memberBytes + bytes
	if estimatedRows > c.peakEstimatedRows {
		c.peakEstimatedRows = estimatedRows
	}
	if estimatedBytes > c.peakEstimatedBytes {
		c.peakEstimatedBytes = estimatedBytes
	}
}

func (c *frameworkFullReadCache) retainNodeProjection(
	key string,
	projection frameworkNodeProjection,
	newRows, newBytes int,
) bool {
	if !c.canRetain(newRows, newBytes) {
		return false
	}
	for id, node := range projection.unique {
		if _, exists := c.nodesByID[id]; exists {
			continue
		}
		c.nodesByID[id] = node
		c.nodeRows++
		c.nodeBytes += frameworkNodeBytes(node)
	}
	cached := c.canonicalNodeQuery(projection.nodes)
	c.nodeQueries[key] = cached
	c.nodeQueryOrder = append(c.nodeQueryOrder, key)
	c.nodeQueryHits[key] = 0
	c.queryBytes += len(cached) * 8
	return true
}

func (c *frameworkFullReadCache) canonicalNodeQuery(nodes []*graph.Node) []*graph.Node {
	cached := make([]*graph.Node, len(nodes))
	for i, node := range nodes {
		if node != nil && node.ID != "" {
			if canonical := c.nodesByID[node.ID]; canonical != nil {
				cached[i] = canonical
				continue
			}
		}
		cached[i] = node
	}
	return cached
}

func (c *frameworkFullReadCache) admitAfterUnusedEviction(key string, projection frameworkNodeProjection) bool {
	if c.rowCap <= 0 || c.byteCap <= 0 || c.memberRows+projection.rows > c.rowCap ||
		c.memberBytes+len(projection.nodes)*8 > c.byteCap {
		return false
	}
	hasUnused := false
	for retainedKey := range c.nodeQueries {
		if c.nodeQueryHits[retainedKey] == 0 {
			hasUnused = true
			break
		}
	}
	if !hasUnused {
		return false
	}

	started := time.Now()
	state, evicted, rowsScanned := c.protectedNodeRetentionState()
	c.evictionRebuilds++
	c.evictionRowsScanned += rowsScanned
	c.evictionNanos += time.Since(started).Nanoseconds()
	if evicted == 0 {
		return false
	}
	newRows := 0
	newBytes := len(projection.nodes) * 8
	for id, node := range projection.unique {
		if _, exists := state.nodesByID[id]; exists {
			continue
		}
		newRows++
		newBytes += frameworkNodeBytes(node)
	}
	if c.memberRows+state.nodeRows+newRows > c.rowCap ||
		c.memberBytes+state.nodeBytes+state.queryBytes+newBytes > c.byteCap {
		return false
	}

	c.nodeQueries = state.queries
	c.nodeQueryOrder = state.order
	c.nodeQueryHits = state.hits
	c.nodesByID = state.nodesByID
	c.nodeRows = state.nodeRows
	c.nodeBytes = state.nodeBytes
	c.queryBytes = state.queryBytes
	c.evictedQueries += evicted
	return c.retainNodeProjection(key, projection, newRows, newBytes)
}

func (c *frameworkFullReadCache) protectedNodeRetentionState() (frameworkNodeRetentionState, int, int) {
	state := frameworkNodeRetentionState{
		queries:   make(map[string][]*graph.Node),
		hits:      make(map[string]int),
		nodesByID: make(map[string]*graph.Node),
	}
	evicted := 0
	rowsScanned := 0
	for _, key := range c.nodeQueryOrder {
		nodes, ok := c.nodeQueries[key]
		if !ok {
			continue
		}
		if c.nodeQueryHits[key] == 0 {
			evicted++
			continue
		}
		state.order = append(state.order, key)
		state.hits[key] = c.nodeQueryHits[key]
		cached := make([]*graph.Node, len(nodes))
		for i, node := range nodes {
			rowsScanned++
			if node != nil && node.ID != "" {
				canonical := state.nodesByID[node.ID]
				if canonical == nil {
					canonical = node
					state.nodesByID[node.ID] = canonical
					state.nodeRows++
					state.nodeBytes += frameworkNodeBytes(canonical)
				}
				cached[i] = canonical
				continue
			}
			cached[i] = node
		}
		state.queries[key] = cached
		state.queryBytes += len(cached) * 8
	}
	return state, evicted, rowsScanned
}

func (c *frameworkFullReadCache) memberMethodsByType(store graph.Store) (map[string][]graph.MemberMethodInfo, bool) {
	reader, ok := store.(graph.MemberMethodsByType)
	if !ok {
		return nil, false
	}
	if c == nil {
		return reader.MemberMethodsByType(), true
	}
	if c.memberMethodsReady {
		c.memberHits++
		return c.memberMethods, true
	}
	c.memberMisses++
	methods := reader.MemberMethodsByType()
	rows, bytes := frameworkMemberMethodsSize(methods)
	c.recordEstimatedPeak(rows, bytes)
	if !c.canRetain(rows, bytes) {
		c.bypasses++
		return methods, true
	}
	c.memberMethods = methods
	c.memberMethodsReady = true
	c.memberRows = rows
	c.memberBytes = bytes
	return methods, true
}

func (c *frameworkFullReadCache) canRetain(rows, bytes int) bool {
	if c == nil || c.rowCap <= 0 || c.byteCap <= 0 {
		return false
	}
	return c.nodeRows+c.memberRows+rows <= c.rowCap &&
		c.nodeBytes+c.queryBytes+c.memberBytes+bytes <= c.byteCap
}

func (c *frameworkFullReadCache) invalidateNodes() {
	if c == nil {
		return
	}
	c.nodeQueries = make(map[string][]*graph.Node)
	c.nodeQueryOrder = nil
	c.nodeQueryHits = make(map[string]int)
	c.nodesByID = make(map[string]*graph.Node)
	c.nodeRows = 0
	c.nodeBytes = 0
	c.queryBytes = 0
	c.epoch++
	c.invalidateMemberMethods()
}

func (c *frameworkFullReadCache) invalidateMemberMethods() {
	if c == nil {
		return
	}
	c.memberMethods = nil
	c.memberMethodsReady = false
	c.memberRows = 0
	c.memberBytes = 0
}

func (c *frameworkFullReadCache) stats() FrameworkFullReadCacheStats {
	if c == nil {
		return FrameworkFullReadCacheStats{}
	}
	queries := make([]FrameworkFullReadCacheQueryStats, len(c.nodeQueryTelemetry))
	copy(queries, c.nodeQueryTelemetry)
	for i := range queries {
		queries[i].Kinds = append([]graph.NodeKind(nil), queries[i].Kinds...)
	}
	return FrameworkFullReadCacheStats{
		NodeHits:                  c.nodeHits,
		NodeMisses:                c.nodeMisses,
		MemberHits:                c.memberHits,
		MemberMisses:              c.memberMisses,
		Bypasses:                  c.bypasses,
		RetainedRows:              c.nodeRows + c.memberRows,
		RetainedBytes:             c.nodeBytes + c.queryBytes + c.memberBytes,
		RowCap:                    c.rowCap,
		ByteCap:                   c.byteCap,
		NodeQueries:               queries,
		DroppedNodeQueryTelemetry: c.droppedNodeQueryTelemetry,
		EvictedQueries:            c.evictedQueries,
		EvictionRebuilds:          c.evictionRebuilds,
		EvictionRowsScanned:       c.evictionRowsScanned,
		EvictionNanos:             c.evictionNanos,
		PeakEstimatedRows:         c.peakEstimatedRows,
		PeakEstimatedBytes:        c.peakEstimatedBytes,
	}
}

func frameworkNodeKindsCacheKey(kinds []graph.NodeKind) ([]graph.NodeKind, string) {
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	normalized := make([]graph.NodeKind, 0, len(kinds))
	var key strings.Builder
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		normalized = append(normalized, kind)
		value := string(kind)
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
		key.WriteByte(';')
	}
	return normalized, key.String()
}

func frameworkMemberMethodsSize(methods map[string][]graph.MemberMethodInfo) (rows, bytes int) {
	for typeID, infos := range methods {
		bytes += len(typeID) + 16
		for _, info := range infos {
			rows++
			bytes += 80 + len(info.MethodID) + len(info.Name) +
				len(info.FilePath) + len(info.RepoPrefix)
		}
	}
	return rows, bytes
}
