package resolver

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func frameworkPressureNodes(prefix string, count int) []*graph.Node {
	nodes := make([]*graph.Node, 0, count)
	for i := range count {
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("%s:%d", prefix, i),
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Symbol%d", i),
			FilePath: fmt.Sprintf("pkg/file%d.go", i/20),
			Language: "go",
		})
	}
	return nodes
}

func frameworkPressureLoad(projections map[graph.NodeKind][]*graph.Node, calls map[graph.NodeKind]int) func([]graph.NodeKind) []*graph.Node {
	return func(kinds []graph.NodeKind) []*graph.Node {
		kind := kinds[0]
		calls[kind]++
		nodes := projections[kind]
		loaded := make([]*graph.Node, len(nodes))
		for i, node := range nodes {
			copy := *node
			loaded[i] = &copy
		}
		return loaded
	}
}

func TestFrameworkFullReadCacheKeepsOneShotProjection(t *testing.T) {
	seedKind, targetKind := graph.NodeKind("pressure_seed"), graph.NodeKind("pressure_target")
	projections := map[graph.NodeKind][]*graph.Node{
		seedKind:   frameworkPressureNodes("seed", 64),
		targetKind: frameworkPressureNodes("target", 64),
	}
	calls := make(map[graph.NodeKind]int)
	cache := newFrameworkFullReadCacheWithLimits(64, 1<<20)
	load := frameworkPressureLoad(projections, calls)
	cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
	cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
	cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
	if calls[seedKind] != 1 || calls[targetKind] != 1 {
		t.Fatalf("one-shot pressure changed retained query: calls=%v", calls)
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(seedKind)]; !ok {
		t.Fatal("one-shot target evicted the retained seed")
	}
}

func TestFrameworkFullReadCacheAdmitsRepeatedProjectionUnderPressure(t *testing.T) {
	seedKind, targetKind := graph.NodeKind("pressure_seed"), graph.NodeKind("pressure_target")
	projections := map[graph.NodeKind][]*graph.Node{
		seedKind:   frameworkPressureNodes("seed", 64),
		targetKind: frameworkPressureNodes("target", 64),
	}
	calls := make(map[graph.NodeKind]int)
	cache := newFrameworkFullReadCacheWithLimits(64, 1<<20)
	load := frameworkPressureLoad(projections, calls)
	cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
	for range 3 {
		nodes := cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
		if len(nodes) != 64 || nodes[0].ID != "target:0" {
			t.Fatalf("target projection changed: %#v", nodes)
		}
	}
	if calls[seedKind] != 1 || calls[targetKind] != 2 {
		t.Fatalf("repeated target was not admitted: calls=%v", calls)
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(seedKind)]; ok {
		t.Fatal("unused seed remained")
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(targetKind)]; !ok {
		t.Fatal("repeated target was not retained")
	}
	stats := cache.stats()
	if stats.NodeHits != 1 || stats.NodeMisses != 3 || stats.Bypasses != 1 {
		t.Fatalf("unexpected pressure stats: %+v", stats)
	}
	var telemetry struct {
		NodeQueries []struct {
			Kinds       []graph.NodeKind `json:"kinds"`
			Loads       int              `json:"loads"`
			Hits        int              `json:"hits"`
			Bypasses    int              `json:"bypasses"`
			Admissions  int              `json:"admissions"`
			LoadedRows  int              `json:"loaded_rows"`
			LoadedBytes int              `json:"loaded_bytes"`
		} `json:"node_queries"`
		EvictedQueries     int `json:"evicted_queries"`
		EvictionRebuilds   int `json:"eviction_rebuilds"`
		PeakEstimatedRows  int `json:"peak_estimated_rows"`
		PeakEstimatedBytes int `json:"peak_estimated_bytes"`
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &telemetry); err != nil {
		t.Fatal(err)
	}
	if len(telemetry.NodeQueries) != 2 || telemetry.EvictedQueries != 1 || telemetry.EvictionRebuilds != 1 {
		t.Fatalf("unexpected telemetry: %s", encoded)
	}
	var targetLoads, targetHits, targetBypasses, targetAdmissions int
	for _, query := range telemetry.NodeQueries {
		if len(query.Kinds) == 1 && query.Kinds[0] == targetKind {
			targetLoads, targetHits = query.Loads, query.Hits
			targetBypasses, targetAdmissions = query.Bypasses, query.Admissions
			if query.LoadedRows != 128 || query.LoadedBytes == 0 {
				t.Fatalf("unexpected target volume: %+v", query)
			}
		}
	}
	if targetLoads != 2 || targetHits != 1 || targetBypasses != 1 || targetAdmissions != 1 {
		t.Fatalf("unexpected target telemetry: %s", encoded)
	}
	if telemetry.PeakEstimatedRows <= stats.RetainedRows || telemetry.PeakEstimatedBytes <= stats.RetainedBytes {
		t.Fatalf("peak omitted old cache plus candidate: %s", encoded)
	}
}

func TestFrameworkFullReadCacheProtectsReusedProjectionAndOversizedTarget(t *testing.T) {
	seedKind, targetKind := graph.NodeKind("pressure_seed"), graph.NodeKind("pressure_target")
	for _, test := range []struct {
		name        string
		targetRows  int
		protectSeed bool
	}{
		{"reused seed", 64, true},
		{"oversized target", 65, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			projections := map[graph.NodeKind][]*graph.Node{
				seedKind:   frameworkPressureNodes("seed", 64),
				targetKind: frameworkPressureNodes("target", test.targetRows),
			}
			calls := make(map[graph.NodeKind]int)
			cache := newFrameworkFullReadCacheWithLimits(64, 1<<20)
			load := frameworkPressureLoad(projections, calls)
			cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
			if test.protectSeed {
				cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
			}
			for range 3 {
				cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
			}
			if calls[targetKind] != 3 {
				t.Fatalf("ineligible target loads = %d, want 3", calls[targetKind])
			}
			if _, ok := cache.nodeQueries[frameworkPressureKey(seedKind)]; !ok {
				t.Fatal("ineligible target evicted seed")
			}
			if _, ok := cache.nodeQueries[frameworkPressureKey(targetKind)]; ok {
				t.Fatal("ineligible target was retained")
			}
		})
	}
}

func TestFrameworkFullReadCacheSkipsRebuildWithoutUnusedProjection(t *testing.T) {
	const protectedRows = 4096
	protectedKind, targetKind := graph.NodeKind("protected_large"), graph.NodeKind("protected_target")
	projections := map[graph.NodeKind][]*graph.Node{
		protectedKind: frameworkPressureNodes("protected", protectedRows),
		targetKind:    frameworkPressureNodes("target", protectedRows/2),
	}
	calls := make(map[graph.NodeKind]int)
	cache := newFrameworkFullReadCacheWithLimits(protectedRows, 1<<30)
	load := frameworkPressureLoad(projections, calls)
	cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
	cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
	for range 3 {
		cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
	}

	stats := cache.stats()
	if calls[targetKind] != 3 {
		t.Fatalf("unretainable target loads = %d, want 3", calls[targetKind])
	}
	if stats.EvictionRebuilds != 0 || stats.EvictionRowsScanned != 0 || stats.EvictedQueries != 0 {
		t.Fatalf("no-unused pressure rebuilt protected state: %+v", stats)
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(protectedKind)]; !ok {
		t.Fatal("protected query was removed")
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(targetKind)]; ok {
		t.Fatal("unretainable target was cached")
	}
}

func TestFrameworkFullReadCacheFailedPreflightDoesNotEvict(t *testing.T) {
	const protectedRows = 60
	protectedKind := graph.NodeKind("preflight_protected")
	unusedKind := graph.NodeKind("preflight_unused")
	targetKind := graph.NodeKind("preflight_target")
	projections := map[graph.NodeKind][]*graph.Node{
		protectedKind: frameworkPressureNodes("protected", protectedRows),
		unusedKind:    frameworkPressureNodes("unused", 30),
		targetKind:    frameworkPressureNodes("target", 50),
	}
	calls := make(map[graph.NodeKind]int)
	cache := newFrameworkFullReadCacheWithLimits(100, 1<<20)
	load := frameworkPressureLoad(projections, calls)
	cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
	cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
	cache.nodesByKinds([]graph.NodeKind{unusedKind}, load)
	cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
	cache.nodesByKinds([]graph.NodeKind{targetKind}, load)

	stats := cache.stats()
	if stats.EvictionRebuilds != 1 || stats.EvictionRowsScanned != protectedRows || stats.EvictionNanos <= 0 {
		t.Fatalf("failed constructed preflight was not measured: %+v", stats)
	}
	if stats.EvictedQueries != 0 {
		t.Fatalf("failed preflight evicted %d queries", stats.EvictedQueries)
	}
	if cache.nodeRows != 90 || len(cache.nodeQueries) != 2 {
		t.Fatalf("failed preflight changed retained cache: rows=%d queries=%d", cache.nodeRows, len(cache.nodeQueries))
	}
	for _, kind := range []graph.NodeKind{protectedKind, unusedKind} {
		if _, ok := cache.nodeQueries[frameworkPressureKey(kind)]; !ok {
			t.Fatalf("failed preflight removed %q", kind)
		}
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(targetKind)]; ok {
		t.Fatal("failed preflight retained target")
	}
}

func TestFrameworkFullReadCacheRecomputesOverlapAfterEviction(t *testing.T) {
	protectedKind := graph.NodeKind("overlap_protected")
	unusedKind := graph.NodeKind("overlap_unused")
	candidateKind := graph.NodeKind("overlap_candidate")
	extraKind := graph.NodeKind("overlap_extra")
	protected := frameworkPressureNodes("overlap-protected", 4)
	unused := frameworkPressureNodes("overlap-unused", 4)
	candidate := append(append([]*graph.Node{}, unused...), frameworkPressureNodes("overlap-new", 3)...)
	projections := map[graph.NodeKind][]*graph.Node{
		protectedKind: protected,
		unusedKind:    unused,
		candidateKind: candidate,
	}
	projectionBytes := func(queries ...[]*graph.Node) int {
		bytes := 0
		unique := make(map[string]*graph.Node)
		for _, query := range queries {
			bytes += len(query) * 8
			for _, node := range query {
				if node != nil && node.ID != "" {
					unique[node.ID] = node
				}
			}
		}
		for _, node := range unique {
			bytes += frameworkNodeBytes(node)
		}
		return bytes
	}
	protectedKey := frameworkPressureKey(protectedKind)
	unusedKey := frameworkPressureKey(unusedKind)
	candidateKey := frameworkPressureKey(candidateKind)
	expectedInitialBytes := projectionBytes(protected, unused)
	expectedFinalBytes := projectionBytes(protected, candidate)

	t.Run("failed preflight preserves retained state", func(t *testing.T) {
		calls := make(map[graph.NodeKind]int)
		cache := newFrameworkFullReadCacheWithLimits(10, 1<<20)
		load := frameworkPressureLoad(projections, calls)
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{unusedKind}, load)
		initialOrder := append([]string(nil), cache.nodeQueryOrder...)
		initialNodes := make(map[string]*graph.Node, len(cache.nodesByID))
		for id, node := range cache.nodesByID {
			initialNodes[id] = node
		}

		cache.nodesByKinds([]graph.NodeKind{candidateKind}, load)
		cache.nodesByKinds([]graph.NodeKind{candidateKind}, load)

		stats := cache.stats()
		if calls[candidateKind] != 2 || stats.EvictionRebuilds != 1 || stats.EvictedQueries != 0 {
			t.Fatalf("unexpected rejected candidate accounting: calls=%v stats=%+v", calls, stats)
		}
		if cache.nodeRows != 8 || len(cache.nodesByID) != 8 || len(cache.nodeQueries) != 2 {
			t.Fatalf("failed preflight changed retained rows: rows=%d nodes=%d queries=%d", cache.nodeRows, len(cache.nodesByID), len(cache.nodeQueries))
		}
		if cache.nodeBytes+cache.queryBytes != expectedInitialBytes {
			t.Fatalf("failed preflight changed retained bytes: got %d want %d", cache.nodeBytes+cache.queryBytes, expectedInitialBytes)
		}
		if len(cache.nodeQueryOrder) != 2 || cache.nodeQueryOrder[0] != initialOrder[0] || cache.nodeQueryOrder[1] != initialOrder[1] {
			t.Fatalf("failed preflight changed query order: got %v want %v", cache.nodeQueryOrder, initialOrder)
		}
		for id, want := range initialNodes {
			if got := cache.nodesByID[id]; got != want {
				t.Fatalf("failed preflight replaced canonical node %q", id)
			}
		}
		if _, ok := cache.nodeQueries[protectedKey]; !ok {
			t.Fatal("failed preflight removed protected query")
		}
		if _, ok := cache.nodeQueries[unusedKey]; !ok {
			t.Fatal("failed preflight removed unused query")
		}
		if _, ok := cache.nodeQueries[candidateKey]; ok {
			t.Fatal("failed preflight retained candidate")
		}
	})

	t.Run("successful eviction recounts overlap", func(t *testing.T) {
		calls := make(map[graph.NodeKind]int)
		cache := newFrameworkFullReadCacheWithLimits(11, expectedFinalBytes)
		load := frameworkPressureLoad(projections, calls)
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{unusedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{candidateKind}, load)
		got := cache.nodesByKinds([]graph.NodeKind{candidateKind}, load)

		stats := cache.stats()
		if calls[candidateKind] != 2 || stats.EvictionRebuilds != 1 || stats.EvictedQueries != 1 {
			t.Fatalf("unexpected admitted candidate accounting: calls=%v stats=%+v", calls, stats)
		}
		if cache.nodeRows != 11 || len(cache.nodesByID) != 11 || len(cache.nodeQueries) != 2 {
			t.Fatalf("recomputed retained rows: rows=%d nodes=%d queries=%d", cache.nodeRows, len(cache.nodesByID), len(cache.nodeQueries))
		}
		if cache.nodeBytes+cache.queryBytes != expectedFinalBytes {
			t.Fatalf("recomputed retained bytes = %d, want %d", cache.nodeBytes+cache.queryBytes, expectedFinalBytes)
		}
		if len(cache.nodeQueryOrder) != 2 || cache.nodeQueryOrder[0] != protectedKey || cache.nodeQueryOrder[1] != candidateKey {
			t.Fatalf("unexpected rebuilt query order: %v", cache.nodeQueryOrder)
		}
		if _, ok := cache.nodeQueries[unusedKey]; ok {
			t.Fatal("unused query survived successful eviction")
		}
		for i, node := range got {
			if node.ID != candidate[i].ID || node != cache.nodesByID[node.ID] {
				t.Fatalf("candidate order/canonical node mismatch at %d: %#v", i, node)
			}
		}

		cache.nodesByKinds([]graph.NodeKind{candidateKind}, load)
		if calls[candidateKind] != 2 {
			t.Fatalf("admitted candidate reloaded: calls=%v", calls)
		}
		extra := []*graph.Node{{ID: "x"}}
		projections[extraKind] = extra
		overlapBytes := 0
		for _, node := range unused {
			overlapBytes += frameworkNodeBytes(node)
		}
		extraBytes := 8 + frameworkNodeBytes(extra[0])
		if extraBytes >= overlapBytes {
			t.Fatalf("byte-limit fixture cannot expose overlap undercount: extra=%d overlap=%d", extraBytes, overlapBytes)
		}
		cache.rowCap = 100
		cache.byteCap = expectedFinalBytes
		beforeOrder := append([]string(nil), cache.nodeQueryOrder...)
		beforeStats := cache.stats()
		cache.nodesByKinds([]graph.NodeKind{extraKind}, load)
		cache.nodesByKinds([]graph.NodeKind{extraKind}, load)
		afterStats := cache.stats()
		if calls[extraKind] != 2 {
			t.Fatalf("byte-limited query loads = %d, want 2", calls[extraKind])
		}
		if _, ok := cache.nodeQueries[frameworkPressureKey(extraKind)]; ok {
			t.Fatal("byte-limited query exploited stale overlap accounting")
		}
		if afterStats.Bypasses != beforeStats.Bypasses+2 || afterStats.EvictionRebuilds != beforeStats.EvictionRebuilds {
			t.Fatalf("unexpected byte-limited bypass stats: before=%+v after=%+v", beforeStats, afterStats)
		}
		if cache.nodeRows != 11 || cache.nodeBytes+cache.queryBytes != expectedFinalBytes {
			t.Fatalf("byte-limited bypass changed retained accounting: rows=%d bytes=%d", cache.nodeRows, cache.nodeBytes+cache.queryBytes)
		}
		if len(cache.nodeQueryOrder) != len(beforeOrder) || cache.nodeQueryOrder[0] != beforeOrder[0] || cache.nodeQueryOrder[1] != beforeOrder[1] {
			t.Fatalf("byte-limited bypass changed query order: got %v want %v", cache.nodeQueryOrder, beforeOrder)
		}
	})
}

type frameworkPressureMemberStore struct {
	graph.Store
	methods map[string][]graph.MemberMethodInfo
}

func (s frameworkPressureMemberStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	return s.methods
}

func TestFrameworkFullReadCacheMemberLoadUpdatesPeakEstimate(t *testing.T) {
	seedKind := graph.NodeKind("member_peak_seed")
	projections := map[graph.NodeKind][]*graph.Node{
		seedKind: frameworkPressureNodes("member-seed", 2),
	}
	cache := newFrameworkFullReadCacheWithLimits(100, 1<<20)
	cache.nodesByKinds([]graph.NodeKind{seedKind}, frameworkPressureLoad(projections, make(map[graph.NodeKind]int)))
	before := cache.stats()
	methods := map[string][]graph.MemberMethodInfo{
		"TypeA": make([]graph.MemberMethodInfo, 3),
		"TypeB": make([]graph.MemberMethodInfo, 2),
	}
	memberRows, memberBytes := frameworkMemberMethodsSize(methods)
	got, ok := cache.memberMethodsByType(frameworkPressureMemberStore{methods: methods})
	if !ok || len(got) != len(methods) {
		t.Fatalf("member method load failed: ok=%v methods=%v", ok, got)
	}

	stats := cache.stats()
	if stats.MemberMisses != 1 || stats.MemberHits != 0 {
		t.Fatalf("unexpected member cache stats: %+v", stats)
	}
	if stats.PeakEstimatedRows != before.RetainedRows+memberRows {
		t.Fatalf("member rows absent from peak: got %d want %d", stats.PeakEstimatedRows, before.RetainedRows+memberRows)
	}
	if stats.PeakEstimatedBytes != before.RetainedBytes+memberBytes {
		t.Fatalf("member bytes absent from peak: got %d want %d", stats.PeakEstimatedBytes, before.RetainedBytes+memberBytes)
	}
}

func TestFrameworkFullReadCacheRebuildsSharedNodeAccounting(t *testing.T) {
	kindA, kindB, targetKind := graph.NodeKind("shared_a"), graph.NodeKind("shared_b"), graph.NodeKind("shared_target")
	shared := frameworkPressureNodes("shared", 50)
	a := append(append([]*graph.Node{}, shared...), frameworkPressureNodes("a", 10)...)
	bProjection := append(append([]*graph.Node{}, shared...), frameworkPressureNodes("b", 40)...)
	target := append(append([]*graph.Node{}, shared[:20]...), frameworkPressureNodes("target", 30)...)
	projections := map[graph.NodeKind][]*graph.Node{kindA: a, kindB: bProjection, targetKind: target}
	calls := make(map[graph.NodeKind]int)
	cache := newFrameworkFullReadCacheWithLimits(100, 1<<20)
	load := frameworkPressureLoad(projections, calls)
	cache.nodesByKinds([]graph.NodeKind{kindA}, load)
	cache.nodesByKinds([]graph.NodeKind{kindB}, load)
	cache.nodesByKinds([]graph.NodeKind{kindA}, load)
	for range 3 {
		cache.nodesByKinds([]graph.NodeKind{targetKind}, load)
	}
	if calls[kindA] != 1 || calls[kindB] != 1 || calls[targetKind] != 2 {
		t.Fatalf("unexpected shared loads: %v", calls)
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(kindA)]; !ok {
		t.Fatal("reused shared projection was evicted")
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(kindB)]; ok {
		t.Fatal("unused shared projection survived")
	}
	if _, ok := cache.nodeQueries[frameworkPressureKey(targetKind)]; !ok {
		t.Fatal("repeated shared target was not retained")
	}
	if cache.nodeRows != 90 || len(cache.nodesByID) != 90 {
		t.Fatalf("rebuilt rows = %d/%d, want 90", cache.nodeRows, len(cache.nodesByID))
	}
	expectedBytes := len(a)*8 + len(target)*8
	for _, node := range cache.nodesByID {
		expectedBytes += frameworkNodeBytes(node)
	}
	if cache.nodeBytes+cache.queryBytes != expectedBytes {
		t.Fatalf("rebuilt bytes = %d, want %d", cache.nodeBytes+cache.queryBytes, expectedBytes)
	}
	stats := cache.stats()
	if stats.RetainedRows != 90 || stats.RetainedBytes != expectedBytes {
		t.Fatalf("accounting drifted: %+v", stats)
	}
}

func TestFrameworkFullReadCacheTelemetrySeparatesInvalidationAndStaysBounded(t *testing.T) {
	cache := newFrameworkFullReadCacheWithLimits(1, 1)
	load := func([]graph.NodeKind) []*graph.Node { return nil }
	firstKind := graph.NodeKind("epoch_first")
	cache.nodesByKinds([]graph.NodeKind{firstKind}, load)
	cache.invalidateNodes()
	cache.nodesByKinds([]graph.NodeKind{firstKind}, load)
	for i := range 300 {
		cache.nodesByKinds([]graph.NodeKind{graph.NodeKind(fmt.Sprintf("kind_%d", i))}, load)
	}
	encoded, err := json.Marshal(cache.stats())
	if err != nil {
		t.Fatal(err)
	}
	var telemetry struct {
		NodeQueries []struct {
			Epoch int `json:"epoch"`
			Loads int `json:"loads"`
		} `json:"node_queries"`
		Dropped int `json:"dropped_node_query_telemetry"`
	}
	if err := json.Unmarshal(encoded, &telemetry); err != nil {
		t.Fatal(err)
	}
	if len(telemetry.NodeQueries) != 256 || telemetry.Dropped == 0 {
		t.Fatalf("telemetry was not bounded: %s", encoded)
	}
	if len(telemetry.NodeQueries) < 2 || telemetry.NodeQueries[0].Epoch == telemetry.NodeQueries[1].Epoch {
		t.Fatalf("invalidation did not separate epochs: %s", encoded)
	}
	if cache.nodeRows != 0 || cache.nodeBytes != 0 || cache.queryBytes != 0 {
		t.Fatalf("zero-row accounting drifted: %d/%d/%d", cache.nodeRows, cache.nodeBytes, cache.queryBytes)
	}
}

func frameworkPressureKey(kind graph.NodeKind) string {
	_, key := frameworkNodeKindsCacheKey([]graph.NodeKind{kind})
	return key
}

type frameworkOriginalBypassReference struct {
	rowCap  int
	byteCap int

	nodeQueries map[string][]*graph.Node
	nodesByID   map[string]*graph.Node
	nodeRows    int
	nodeBytes   int
	queryBytes  int
	memberRows  int
	memberBytes int
}

func newFrameworkOriginalBypassReference(rowCap, byteCap int) *frameworkOriginalBypassReference {
	return &frameworkOriginalBypassReference{
		rowCap:      rowCap,
		byteCap:     byteCap,
		nodeQueries: make(map[string][]*graph.Node),
		nodesByID:   make(map[string]*graph.Node),
	}
}

// nodesByKinds reproduces the bounded retain-or-bypass policy that preceded
// pressure-aware admission. It intentionally excludes current telemetry and
// eviction work so the benchmark remains a faithful historical reference.
func (c *frameworkOriginalBypassReference) nodesByKinds(kinds []graph.NodeKind, load func([]graph.NodeKind) []*graph.Node) []*graph.Node {
	normalized, key := frameworkNodeKindsCacheKey(kinds)
	if len(normalized) == 0 {
		return nil
	}
	if nodes, ok := c.nodeQueries[key]; ok {
		return nodes
	}
	loaded := load(normalized)
	newNodes := make(map[string]*graph.Node)
	newBytes := len(loaded) * 8
	for _, node := range loaded {
		if node == nil || node.ID == "" {
			continue
		}
		if _, ok := c.nodesByID[node.ID]; ok {
			continue
		}
		if _, ok := newNodes[node.ID]; ok {
			continue
		}
		newNodes[node.ID] = node
		newBytes += frameworkNodeBytes(node)
	}
	if c.nodeRows+c.memberRows+len(newNodes) > c.rowCap ||
		c.nodeBytes+c.queryBytes+c.memberBytes+newBytes > c.byteCap {
		return loaded
	}
	for id, node := range newNodes {
		c.nodesByID[id] = node
		c.nodeBytes += frameworkNodeBytes(node)
	}
	c.nodeRows += len(newNodes)
	cached := make([]*graph.Node, len(loaded))
	for i, node := range loaded {
		if node != nil && node.ID != "" {
			if canonical, ok := c.nodesByID[node.ID]; ok {
				cached[i] = canonical
				continue
			}
		}
		cached[i] = node
	}
	c.nodeQueries[key] = cached
	c.queryBytes += len(cached) * 8
	return cached
}

func frameworkPressureBenchmarkSet(prefix string, queryCount, rowCount int) ([]graph.NodeKind, map[graph.NodeKind][]*graph.Node) {
	kinds := make([]graph.NodeKind, 0, queryCount)
	projections := make(map[graph.NodeKind][]*graph.Node, queryCount)
	for i := range queryCount {
		kind := graph.NodeKind(fmt.Sprintf("%s_%d", prefix, i))
		kinds = append(kinds, kind)
		projections[kind] = frameworkPressureNodes(fmt.Sprintf("%s-%d", prefix, i), rowCount)
	}
	return kinds, projections
}

var frameworkPressureBenchmarkRows int

func BenchmarkFrameworkFullReadCacheRepeatedPressure(b *testing.B) {
	seedKind, targetKind := graph.NodeKind("pressure_seed"), graph.NodeKind("pressure_target")
	projections := map[graph.NodeKind][]*graph.Node{
		seedKind:   frameworkPressureNodes("seed", 64),
		targetKind: frameworkPressureNodes("target", 64),
	}
	b.ReportAllocs()
	for b.Loop() {
		cache := newFrameworkFullReadCacheWithLimits(64, 1<<20)
		load := frameworkPressureLoad(projections, make(map[graph.NodeKind]int))
		cache.nodesByKinds([]graph.NodeKind{seedKind}, load)
		for range 8 {
			frameworkPressureBenchmarkRows = len(cache.nodesByKinds([]graph.NodeKind{targetKind}, load))
		}
	}
}

func BenchmarkFrameworkFullReadCacheMissHeavy(b *testing.B) {
	const (
		queryCount = 8
		rowCount   = 2048
	)
	b.StopTimer()
	kinds, projections := frameworkPressureBenchmarkSet("miss-heavy", queryCount, rowCount)

	b.Run("current_cache", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			load := frameworkPressureLoad(projections, make(map[graph.NodeKind]int))
			cache := newFrameworkFullReadCacheWithLimits(rowCount, 1<<30)
			for _, kind := range kinds {
				frameworkPressureBenchmarkRows = len(cache.nodesByKinds([]graph.NodeKind{kind}, load))
			}
		}
	})
	b.Run("historical_original_bypass_reference", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			load := frameworkPressureLoad(projections, make(map[graph.NodeKind]int))
			cache := newFrameworkOriginalBypassReference(rowCount, 1<<30)
			for _, kind := range kinds {
				frameworkPressureBenchmarkRows = len(cache.nodesByKinds([]graph.NodeKind{kind}, load))
			}
		}
	})
}

func BenchmarkFrameworkFullReadCacheNearCapPressure(b *testing.B) {
	const (
		protectedRows = 60000
		unusedRows    = 50000
		candidateRows = 210000
	)
	b.StopTimer()
	protectedKind := graph.NodeKind("near-cap-protected")
	unusedKind := graph.NodeKind("near-cap-unused")
	candidateKind := graph.NodeKind("near-cap-candidate")
	projections := map[graph.NodeKind][]*graph.Node{
		protectedKind: frameworkPressureNodes("near-cap-protected", protectedRows),
		unusedKind:    frameworkPressureNodes("near-cap-unused", unusedRows),
		candidateKind: frameworkPressureNodes("near-cap-candidate", candidateRows),
	}
	var totalRetainedEstimate, totalPeakEstimate int64
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for b.Loop() {
		cache := newFrameworkFullReadCache()
		load := frameworkPressureLoad(projections, make(map[graph.NodeKind]int))
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{protectedKind}, load)
		cache.nodesByKinds([]graph.NodeKind{unusedKind}, load)
		for range 3 {
			frameworkPressureBenchmarkRows = len(cache.nodesByKinds([]graph.NodeKind{candidateKind}, load))
		}
		stats := cache.stats()
		totalRetainedEstimate += int64(stats.RetainedBytes)
		totalPeakEstimate += int64(stats.PeakEstimatedBytes)
		if stats.RetainedRows < 100000 || stats.EvictionRebuilds == 0 || stats.EvictedQueries != 0 {
			b.Fatalf("near-cap workload drifted: %+v", stats)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(totalRetainedEstimate)/float64(b.N), "estimated-retained-bytes/op")
	b.ReportMetric(float64(totalPeakEstimate)/float64(b.N), "estimated-peak-bytes/op")
}
