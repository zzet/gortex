package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	outlineCacheEntries       = 32
	outlineCachePayloadBytes  = 64 << 10
	outlineCacheIdentityBytes = 4 << 10
)

type outlineResponseKey struct {
	reader            *store_sqlite.Store
	revision          uint64
	analysisReader    *store_sqlite.Store
	analysisRevision  uint64
	epoch, scopeEpoch uint64
	generation        int64
	ready             bool
	scope, mask       string
}

const outlineSummaryEntries = 16

type outlineResponseCache struct {
	proof       outlineResponseKey
	proofSet    bool
	summaryKeys []outlineResponseKey
	summaries   map[outlineResponseKey]outlinePreparedSummary
	mu          sync.Mutex
	scopeEpoch  uint64
	keys        []outlineResponseKey
	values      map[outlineResponseKey]map[string]any
}

// Only known concrete stores and the exact contract-core mask are cacheable.
// A composed/overlay reader may change without the base revision advancing.
// No capability probe tunnels through those readers to the physical store.
func (s *Server) outlineCacheKey(reader graph.Reader, scope graph.NodeScope, bound bool) (outlineResponseKey, bool) {
	if !bound {
		return outlineResponseKey{}, false
	}
	key := outlineResponseKey{}
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		core := wrapped.contractCore()
		reader = core.Reader
		if core.contractIDs == nil {
			key.mask = "kinds"
		} else {
			ids := make([]string, 0, len(core.contractIDs))
			for id, excluded := range core.contractIDs {
				if excluded {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			data, _ := json.Marshal(ids)
			key.mask = "ids:" + string(data)
		}
	}
	store, ok := reader.(*store_sqlite.Store)
	if !ok {
		return outlineResponseKey{}, false
	}
	analysisStore, ok := s.graph.(*store_sqlite.Store)
	if !ok {
		return outlineResponseKey{}, false
	}
	// Both clocks are complete mutation clocks, not counts. The exact handle
	// fixes payload generation; rebinding another view changes the key too.
	key.reader, key.revision = store, store.MutationRevision()
	key.analysisReader, key.analysisRevision = analysisStore, analysisStore.MutationRevision()
	writer, _ := s.analysisGenerationBackends()
	if writer == nil || !s.analysisMu.TryRLock() {
		return outlineResponseKey{}, false
	}
	key.epoch = s.analysisEpoch
	key.generation = s.analysisGeneration.GenerationID
	key.ready = s.analysisGenerationReady
	current := s.analysisEpoch > 0 && s.communitiesToken.analysisRevision == writer.AnalysisMutationRevision() && s.communitiesToken.edgeIdentity == s.graph.EdgeIdentityRevisions()
	hotReady := s.hotspotsReady
	s.analysisMu.RUnlock()
	// Do not turn a transient failed/lazy materialization into a permanent
	// empty answer. Before the first analysis, absence retains legacy behavior.
	if current && !hotReady {
		return outlineResponseKey{}, false
	}
	key.scope = outlineScopeIdentity(scope)
	if len(key.scope) > outlineCacheIdentityBytes || len(key.mask) > outlineCacheIdentityBytes {
		return outlineResponseKey{}, false
	}
	s.outlineResponses.mu.Lock()
	key.scopeEpoch = s.outlineResponses.scopeEpoch
	s.outlineResponses.mu.Unlock()
	return key, true
}

func outlineScopeIdentity(scope graph.NodeScope) string {
	repos := make([]string, 0, len(scope.RepoAllow))
	for repo, allowed := range scope.RepoAllow {
		if allowed {
			repos = append(repos, repo)
		}
	}
	slices.Sort(repos)
	data, _ := json.Marshal(struct {
		Workspace, Project string
		Narrow             bool
		Repos              []string
	}{scope.WorkspaceID, scope.ProjectID, len(scope.RepoAllow) > 0, repos})
	return string(data)
}

func (c *outlineResponseCache) clear() {
	c.mu.Lock()
	c.scopeEpoch++
	c.keys = nil
	c.values = nil
	c.summaryKeys = nil
	c.summaries = nil
	c.proofSet = false
	c.mu.Unlock()
}
func (c *outlineResponseCache) get(key outlineResponseKey) (map[string]any, bool) {
	c.mu.Lock()
	payload, ok := c.values[key]
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	return cloneOutlinePayload(payload), true
}
func (c *outlineResponseCache) put(key outlineResponseKey, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > outlineCachePayloadBytes {
		return
	}
	owned := cloneOutlinePayload(payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.prepareProofLocked(key) {
		return
	}
	if _, ok := c.values[key]; ok {
		return
	}
	if len(c.keys) == outlineCacheEntries {
		delete(c.values, c.keys[0])
		copy(c.keys, c.keys[1:])
		c.keys = c.keys[:len(c.keys)-1]
	}
	if c.values == nil {
		c.values = make(map[outlineResponseKey]map[string]any)
	}
	c.keys = append(c.keys, key)
	c.values[key] = owned
}

// prepareProofLocked drops old derived state and rejects a delayed producer
// from an older mutation/analysis epoch instead of letting it evict fresh data.
func (c *outlineResponseCache) prepareProofLocked(key outlineResponseKey) bool {
	if key.scopeEpoch != c.scopeEpoch {
		return false
	}
	if c.proofSet {
		old := c.proof
		if old.reader == key.reader && old.analysisReader == key.analysisReader && (key.revision < old.revision || key.analysisRevision < old.analysisRevision || key.epoch < old.epoch) {
			return false
		}
		if old.reader != key.reader || old.revision != key.revision || old.analysisReader != key.analysisReader || old.analysisRevision != key.analysisRevision || old.epoch != key.epoch || old.generation != key.generation || old.ready != key.ready {
			c.keys = nil
			c.values = nil
			c.summaryKeys = nil
			c.summaries = nil
		}
	}
	c.proof = key
	c.proofSet = true
	return true
}

type outlinePreparedSummary struct {
	imports     []map[string]any // Complete displayed top ten, not an incomplete graph aggregate API.
	counts      graph.OutlineCounts
	communities map[string]any // nil means unavailable: retain the exact live fallback.
}

func (c *outlineResponseCache) getSummary(key outlineResponseKey) (outlinePreparedSummary, bool) {
	c.mu.Lock()
	summary, ok := c.summaries[key]
	c.mu.Unlock()
	if !ok {
		return outlinePreparedSummary{}, false
	}
	out := outlinePreparedSummary{counts: cloneOutlineCounts(summary.counts)}
	out.imports = cloneOutlineValue(summary.imports).([]map[string]any)
	if summary.communities != nil {
		out.communities = cloneOutlinePayload(summary.communities)
	}
	return out, true
}
func (c *outlineResponseCache) putSummary(key outlineResponseKey, counts graph.OutlineCounts, communities map[string]any) (int, bool) {
	imports := make(map[string]int, len(counts.FileImports))
	for _, row := range counts.FileImports {
		imports[row.FilePath] = row.Count
	}
	displayed := rankImportedFiles(imports, outlineMostImportedLimit)
	// Only outlines consume this private preparation. Retain the complete final
	// top-ten section; do not retain all candidate paths or change graph APIs.
	counts.FileImports = nil
	bytes := outlineSummaryOwnedBytes(counts)
	data, err := json.Marshal(displayed)
	if err != nil {
		return 0, false
	}
	bytes += len(data) + 4096

	if communities != nil {
		data, err := json.Marshal(communities)
		if err != nil {
			return 0, false
		}
		bytes += len(data) + 2048 // fixed section: at most five small maps and scalar leaves.
	}
	if bytes > outlineCachePayloadBytes {
		return 0, false
	}
	owned := outlinePreparedSummary{counts: cloneOutlineCounts(counts), imports: cloneOutlineValue(displayed).([]map[string]any)}
	if communities != nil {
		owned.communities = cloneOutlinePayload(communities)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.prepareProofLocked(key) {
		return 0, false
	}
	if _, exists := c.summaries[key]; exists {
		return bytes, true
	}
	if len(c.summaryKeys) == outlineSummaryEntries {
		return 0, false
	}
	if c.summaries == nil {
		c.summaries = make(map[outlineResponseKey]outlinePreparedSummary)
	}
	c.summaryKeys = append(c.summaryKeys, key)
	c.summaries[key] = owned
	return bytes, true
}

// A conservative owned-allocation envelope, including exact-length row slice,
// cloned string allocation rounding, and map storage (128 bytes per entry is
// above the Go map slot/bucket share). Reject before cloning, never truncate.
func outlineSummaryOwnedBytes(counts graph.OutlineCounts) int {
	bytes := int(unsafe.Sizeof(counts)) + 64
	add := func(n int) bool {
		if n > outlineCachePayloadBytes-bytes {
			bytes = outlineCachePayloadBytes + 1
			return false
		}
		bytes += n
		return true
	}
	for language := range counts.ByLanguage {
		if !add(128 + len(language) + 16) {
			return bytes
		}
	}
	for _, row := range counts.FileImports {
		if !add(int(unsafe.Sizeof(row)) + len(row.FilePath) + 16) {
			return bytes
		}
	}
	return bytes
}

func cloneOutlineCounts(counts graph.OutlineCounts) graph.OutlineCounts {
	out := counts
	out.ByLanguage = make(map[string]int, len(counts.ByLanguage))
	for language, count := range counts.ByLanguage {
		out.ByLanguage[strings.Clone(language)] = count
	}
	if counts.FileImports != nil {
		out.FileImports = make([]graph.FileImportCountRow, len(counts.FileImports))
		for i, row := range counts.FileImports {
			out.FileImports[i] = graph.FileImportCountRow{FilePath: strings.Clone(row.FilePath), Count: row.Count}
		}
	}
	return out
}

// The payload has one small, fixed map/list vocabulary. Copy it before
// formatting/degradation and own every retained string: backend substrings or
// request decorations must not retain large buffers or mutate another reply.
func cloneOutlinePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = cloneOutlineValue(value)
	}
	return out
}
func cloneOutlineValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneOutlinePayload(v)
	case []map[string]any:
		if v == nil {
			return v
		}
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = cloneOutlinePayload(item)
		}
		return out
	case []outlineLanguage:
		if v == nil {
			return v
		}
		out := slices.Clone(v)
		for i := range out {
			out[i].Name = strings.Clone(out[i].Name)
		}
		return out
	case string:
		return strings.Clone(v)
	default:
		return v // The remaining payload leaves are immutable scalar numbers.
	}
}
