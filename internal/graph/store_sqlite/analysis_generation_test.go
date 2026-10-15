package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func requireAnalysisAccepted(t *testing.T) func(bool, error) {
	t.Helper()
	return func(accepted bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if !accepted {
			t.Fatal("analysis generation write unexpectedly rejected")
		}
	}
}

func buildMinimalAnalysisGeneration(t *testing.T, store *Store, prefix string, conceptCount int, activate bool) int64 {
	t.Helper()
	revision := store.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{
		FormatVersion:  77,
		NodeCount:      1,
		CommunityCount: 1,
		ConceptCount:   conceptCount,
		PageRankMax:    1,
		AuthorityMax:   1,
		HubMax:         1,
		Modularity:     0.5,
	}
	generationID, accepted, err := store.BeginAnalysisGeneration(revision, header)
	requireAnalysisAccepted(t)(accepted, err)
	requireAnalysisAccepted(t)(store.AppendAnalysisCommunities(revision, generationID, []graph.AnalysisCommunitySummary{{ID: prefix + "-community", Label: prefix, Size: 1}}))
	requireAnalysisAccepted(t)(store.AppendAnalysisNodes(revision, generationID, []graph.AnalysisNodeMetric{{NodeID: prefix + "-node", CommunityID: prefix + "-community", PageRank: 1, Authority: 1, Hub: 1}}))
	concepts := make([]graph.AnalysisConcept, conceptCount)
	for i := range concepts {
		concepts[i] = graph.AnalysisConcept{Token: fmt.Sprintf("%s-token-%05d", prefix, i), InVocabulary: i%2 == 0}
	}
	if conceptCount != 0 {
		requireAnalysisAccepted(t)(store.AppendAnalysisConcepts(revision, generationID, concepts, nil))
	}
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("adjacency-" + prefix)}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: []byte("leiden-" + prefix)}))
	for component, rows := range map[graph.AnalysisComponent]int{
		graph.AnalysisComponentNodes:       1,
		graph.AnalysisComponentCommunities: 1,
		graph.AnalysisComponentProcesses:   0,
		graph.AnalysisComponentConcepts:    conceptCount,
		graph.AnalysisComponentAdjacency:   1,
		graph.AnalysisComponentLeiden:      1,
	} {
		requireAnalysisAccepted(t)(store.SealAnalysisComponent(revision, generationID, component, rows))
	}
	if activate {
		requireAnalysisAccepted(t)(store.ActivateAnalysisGeneration(revision, generationID))
	}
	return generationID
}

func TestAnalysisGenerationActivationAndBoundedQueries(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	revision := store.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{
		FormatVersion: 91, NodeCount: 3, CommunityCount: 2, ProcessCount: 1, ConceptCount: 3,
		PageRankMax: 0.9, AuthorityMax: 0.8, HubMax: 0.7, Modularity: 0.4,
		ProcessesTruncated: true, ProcessesTruncationReason: "fixture cap",
	}
	generationID, accepted, err := store.BeginAnalysisGeneration(revision, header)
	requireAnalysisAccepted(t)(accepted, err)
	requireAnalysisAccepted(t)(store.AppendAnalysisCommunities(revision, generationID, []graph.AnalysisCommunitySummary{
		{ID: "c2", Label: "second", Hub: "n3", Size: 1, Cohesion: 0.8, Files: []string{"z.go"}},
		{ID: "c1", Label: "first", Hub: "n2", Size: 2, Cohesion: 0.9, Files: []string{"a.go", "b.go"}},
	}))
	requireAnalysisAccepted(t)(store.AppendAnalysisNodes(revision, generationID, []graph.AnalysisNodeMetric{
		{NodeID: "n3", CommunityID: "c2", PageRank: 0.5, Authority: 0.8, Hub: 0.2},
		{NodeID: "n1", CommunityID: "c1", PageRank: 0.2, Authority: 0.1, Hub: 0.7},
		{NodeID: "n2", CommunityID: "c1", PageRank: 0.9, Authority: 0.4, Hub: 0.3},
	}))
	requireAnalysisAccepted(t)(store.AppendAnalysisProcesses(revision, generationID, []graph.AnalysisProcessSummary{{
		ID: "p1", Name: "request", EntryPoint: "n1", StepCount: 2, Score: 0.75, Truncated: true, Files: []string{"a.go", "z.go"},
	}}, nil))
	requireAnalysisAccepted(t)(store.AppendAnalysisProcesses(revision, generationID, nil, []graph.AnalysisProcessStep{
		{ProcessID: "p1", NodeID: "n1", Ordinal: 0, Depth: 0},
		{ProcessID: "p1", NodeID: "n3", Ordinal: 1, Depth: 1},
	}))
	concepts := []graph.AnalysisConcept{{Token: "alpha", InVocabulary: true}, {Token: "beta", InVocabulary: true}, {Token: "gamma"}}
	requireAnalysisAccepted(t)(store.AppendAnalysisConcepts(revision, generationID, concepts, nil))
	requireAnalysisAccepted(t)(store.AppendAnalysisConcepts(revision, generationID, nil, []graph.AnalysisConceptRelation{
		{Token: "alpha", RelatedToken: "beta", Rank: 0},
		{Token: "beta", RelatedToken: "gamma", Rank: 0},
	}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobAdjacency, Payload: []byte("adj")}))
	requireAnalysisAccepted(t)(store.PutAnalysisBlob(revision, generationID, graph.AnalysisBlob{Component: graph.AnalysisBlobLeiden, Payload: []byte("lei")}))
	if accepted, err := store.ActivateAnalysisGeneration(revision, generationID); err == nil || accepted {
		t.Fatalf("partial activation accepted=%v err=%v", accepted, err)
	}
	for component, rows := range map[graph.AnalysisComponent]int{
		graph.AnalysisComponentNodes: 3, graph.AnalysisComponentCommunities: 2,
		graph.AnalysisComponentProcesses: 1, graph.AnalysisComponentConcepts: 3,
		graph.AnalysisComponentAdjacency: 1, graph.AnalysisComponentLeiden: 1,
	} {
		requireAnalysisAccepted(t)(store.SealAnalysisComponent(revision, generationID, component, rows))
	}
	if accepted, err := store.AppendAnalysisNodes(revision, generationID, nil); err == nil || accepted {
		t.Fatalf("sealed component accepted append: accepted=%v err=%v", accepted, err)
	}
	requireAnalysisAccepted(t)(store.ActivateAnalysisGeneration(revision, generationID))

	gotHeader, found, err := store.LoadActiveAnalysisHeader(91)
	if err != nil || !found {
		t.Fatalf("active header found=%v err=%v", found, err)
	}
	if gotHeader.GenerationID != generationID || gotHeader.GraphRevision != store.AnalysisMutationRevision() || !gotHeader.ProcessesTruncated {
		t.Fatalf("active header = %+v", gotHeader)
	}
	metrics, err := store.AnalysisNodeMetrics(generationID, []string{"n3", "n1"})
	if err != nil || len(metrics) != 2 || metrics[0].NodeID != "n1" || metrics[1].NodeID != "n3" {
		t.Fatalf("point metrics = %+v err=%v", metrics, err)
	}
	page, nextNode, err := store.ListAnalysisNodeMetrics(generationID, 2, "")
	if err != nil || len(page) != 2 || page[0].NodeID != "n1" || page[1].NodeID != "n2" || nextNode != "n2" {
		t.Fatalf("node page = %+v next=%q err=%v", page, nextNode, err)
	}
	page, nextNode, err = store.ListAnalysisNodeMetrics(generationID, 2, nextNode)
	if err != nil || len(page) != 1 || page[0].NodeID != "n3" || nextNode != "" {
		t.Fatalf("node tail = %+v next=%q err=%v", page, nextNode, err)
	}
	top, cursor, err := store.TopAnalysisNodeMetrics(generationID, graph.AnalysisMetricPageRank, 2, nil)
	if err != nil || len(top) != 2 || top[0].NodeID != "n2" || top[1].NodeID != "n3" || cursor == nil {
		t.Fatalf("top = %+v cursor=%+v err=%v", top, cursor, err)
	}
	top, tailCursor, err := store.TopAnalysisNodeMetrics(generationID, graph.AnalysisMetricPageRank, 2, cursor)
	if err != nil || len(top) != 1 || top[0].NodeID != "n1" || tailCursor != nil {
		t.Fatalf("top tail = %+v cursor=%+v err=%v", top, tailCursor, err)
	}
	communities, nextCommunity, err := store.ListAnalysisCommunitySummaries(generationID, 1, "")
	if err != nil || len(communities) != 1 || communities[0].ID != "c1" || len(communities[0].Files) != 2 || nextCommunity != "c1" {
		t.Fatalf("communities = %+v next=%q err=%v", communities, nextCommunity, err)
	}
	members, _, err := store.AnalysisCommunityMembers(generationID, "c1", 10, "")
	if err != nil || len(members) != 2 || members[0].NodeID != "n1" || members[1].NodeID != "n2" {
		t.Fatalf("members = %+v err=%v", members, err)
	}
	processes, _, err := store.ListAnalysisProcessSummaries(generationID, 10, "")
	if err != nil || len(processes) != 1 || len(processes[0].Files) != 2 {
		t.Fatalf("processes = %+v err=%v", processes, err)
	}
	steps, nextOrdinal, err := store.AnalysisProcessSteps(generationID, "p1", 1, -1)
	if err != nil || len(steps) != 1 || steps[0].Ordinal != 0 || nextOrdinal != 0 {
		t.Fatalf("steps = %+v next=%d err=%v", steps, nextOrdinal, err)
	}
	steps, nextOrdinal, err = store.AnalysisProcessSteps(generationID, "p1", 2, nextOrdinal)
	if err != nil || len(steps) != 1 || steps[0].Ordinal != 1 || nextOrdinal != -1 {
		t.Fatalf("step tail = %+v next=%d err=%v", steps, nextOrdinal, err)
	}
	memberships, err := store.AnalysisProcessesForNodes(generationID, []string{"n3", "n1"})
	if err != nil || len(memberships) != 2 || memberships[0].NodeID != "n1" || memberships[1].NodeID != "n3" {
		t.Fatalf("memberships = %+v err=%v", memberships, err)
	}
	forward, err := store.AnalysisConcepts(generationID, []string{"beta", "alpha"}, graph.AnalysisConceptForward)
	if err != nil || len(forward.Concepts) != 2 || len(forward.Relations) != 2 || forward.Relations[0].Token != "alpha" {
		t.Fatalf("forward concepts = %+v err=%v", forward, err)
	}
	reverse, err := store.AnalysisConcepts(generationID, []string{"beta"}, graph.AnalysisConceptReverse)
	if err != nil || len(reverse.Relations) != 1 || reverse.Relations[0].Token != "alpha" {
		t.Fatalf("reverse concepts = %+v err=%v", reverse, err)
	}
	conceptPage, nextConcept, err := store.ListAnalysisConcepts(generationID, 2, "")
	if err != nil || len(conceptPage.Concepts) != 2 || nextConcept != "beta" {
		t.Fatalf("concept page = %+v next=%q err=%v", conceptPage, nextConcept, err)
	}
	blob, found, err := store.LoadAnalysisBlob(generationID, graph.AnalysisBlobAdjacency)
	if err != nil || !found || string(blob) != "adj" {
		t.Fatalf("blob=%q found=%v err=%v", blob, found, err)
	}

	store.AddNode(&graph.Node{ID: "live", Kind: graph.KindFunction, Name: "Live", FilePath: "live.go"})
	if _, found, err := store.LoadActiveAnalysisHeader(91); err != nil || found {
		t.Fatalf("mutated header found=%v err=%v", found, err)
	}
	if _, err := store.AnalysisNodeMetrics(generationID, []string{"n1"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("stale point read err=%v", err)
	}
	var retained int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM analysis_nodes WHERE generation_id = ?`, generationID).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("live-node mutation cascaded snapshot rows: count=%d err=%v", retained, err)
	}
}

func TestAnalysisGenerationCorruptionClearsActivePointer(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generationID := buildMinimalAnalysisGeneration(t, store, "corrupt", 1, true)
	if _, err := store.writerDB.Exec(`DELETE FROM analysis_generation_components WHERE generation_id = ? AND component = ?`, generationID, string(graph.AnalysisComponentNodes)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadActiveAnalysisHeader(77); found || !errors.Is(err, graph.ErrAnalysisGenerationCorrupt) {
		t.Fatalf("corrupt load found=%v err=%v", found, err)
	}
	var active, state int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM analysis_active_generation`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if active != 0 || state != analysisGenerationStale {
		t.Fatalf("corrupt generation active=%d state=%d", active, state)
	}
}

func TestAnalysisGenerationForeignKeysEnabledOnPoolAndNoLiveNodeFK(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	poolSize := store.db.Stats().MaxOpenConnections
	if poolSize < 1 {
		t.Fatalf("invalid configured SQLite pool size: %d", poolSize)
	}
	connections := make([]*sql.Conn, 0, poolSize)
	defer func() {
		// On an acquisition/query failure, release every lease before the
		// deferred Store.Close runs its final checkpoint.
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < poolSize; i++ {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire pooled connection %d/%d: %v", i+1, poolSize, err)
		}
		connections = append(connections, conn)
	}
	for i, conn := range connections {
		var enabled int
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Fatalf("connection %d foreign_keys=%d", i, enabled)
		}
	}
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatalf("release pooled connection: %v", err)
		}
	}
	connections = nil
	if _, err := store.writerDB.ExecContext(ctx, `INSERT INTO analysis_nodes(generation_id,node_id,pagerank,authority,hub) VALUES(999,'orphan',0,0,0)`); err == nil {
		t.Fatal("orphan analysis node bypassed generation FK")
	}
	store.AddNode(&graph.Node{ID: "same-id", Kind: graph.KindFunction, Name: "Same", FilePath: "same.go"})
	generationID := buildMinimalAnalysisGeneration(t, store, "same-id", 0, true)
	store.EvictFile("same.go")
	var retained int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analysis_nodes WHERE generation_id = ?`, generationID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("live node eviction cascaded generation-local node: count=%d err=%v", retained, err)
	}
}

func TestCheckpointWALPoolWaitHonorsContext(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAcquire()
	poolSize := store.writerDB.Stats().MaxOpenConnections
	if poolSize < 1 {
		t.Fatalf("invalid configured SQLite pool size: %d", poolSize)
	}
	connections := make([]*sql.Conn, 0, poolSize)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < poolSize; i++ {
		conn, err := store.writerDB.Conn(acquireCtx)
		if err != nil {
			t.Fatalf("acquire pooled connection %d/%d: %v", i+1, poolSize, err)
		}
		connections = append(connections, conn)
	}

	checkpointCtx, cancelCheckpoint := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	err = store.checkpointWAL(checkpointCtx)
	cancelCheckpoint()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkpoint with exhausted pool error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("checkpoint ignored pool-wait deadline: elapsed=%s", elapsed)
	}

	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatalf("release pooled connection: %v", err)
		}
	}
	connections = nil
}

func TestAnalysisGenerationV4MigrationPreservesGraphAndDropsProvisionalCache(t *testing.T) {
	path := filepathForAnalysisTest(t)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An OWNED node. A file-backed node with an empty repo_prefix is
	// single-repo-mode residue, which the v7 migration in this same chain
	// purges by design.
	store.AddNode(&graph.Node{ID: "repo/kept.go::Kept", Kind: graph.KindFunction, Name: "Kept", FilePath: "repo/kept.go", RepoPrefix: "repo"})
	if _, err := store.writerDB.Exec(`CREATE TABLE analysis_cache(component TEXT PRIMARY KEY, format_version INTEGER NOT NULL, payload BLOB NOT NULL) WITHOUT ROWID; INSERT INTO analysis_cache(component,format_version,payload) VALUES('pagerank',1,'unreleased-v3')`); err != nil {
		t.Fatal(err)
	}
	drop := `
		DROP TABLE analysis_active_generation;
		DROP TABLE analysis_generation_components;
		DROP TABLE analysis_process_steps;
		DROP TABLE analysis_process_files;
		DROP TABLE analysis_processes;
		DROP TABLE analysis_concept_relations;
		DROP TABLE analysis_concepts;
		DROP TABLE analysis_community_files;
		DROP TABLE analysis_nodes;
		DROP TABLE analysis_communities;
		DROP TABLE analysis_blobs;
		DROP TABLE analysis_generations;`
	if _, err := store.writerDB.Exec(drop); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writerDB.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.GetNode("repo/kept.go::Kept") == nil {
		t.Fatal("v4 in-place migration lost graph rows")
	}
	var version, tables, provisional int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='analysis_cache'`).Scan(&provisional); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('analysis_generations','analysis_nodes','analysis_blobs')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion || provisional != 0 || tables != 3 {
		t.Fatalf("migration version=%d provisional=%d tables=%d", version, provisional, tables)
	}
}

func TestAnalysisGenerationReopenMarksBuildingStale(t *testing.T) {
	path := filepathForAnalysisTest(t)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	generationID, accepted, err := store.BeginAnalysisGeneration(store.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 1})
	requireAnalysisAccepted(t)(accepted, err)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state int
	if err := store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != analysisGenerationStale {
		t.Fatalf("reopened building generation state=%d", state)
	}
}

func TestAnalysisGenerationPruneKeepsActiveAndFallback(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := buildMinimalAnalysisGeneration(t, store, "first", 3, true)
	second := buildMinimalAnalysisGeneration(t, store, "second", 2, true)
	third := buildMinimalAnalysisGeneration(t, store, "third", 1, true)
	removed, err := store.PruneAnalysisGenerations(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The first generation has 3 concepts, 1 node, 1 community, 2 blobs,
	// 6 component seals, and its generation row.
	if removed != 14 {
		t.Fatalf("removed=%d want=14", removed)
	}
	for generationID, want := range map[int64]int{first: 0, second: 1, third: 1} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("generation %d count=%d want=%d", generationID, count, want)
		}
	}
	header, found, err := store.LoadActiveAnalysisHeader(77)
	if err != nil || !found || header.GenerationID != third {
		t.Fatalf("active after gc = %+v found=%v err=%v", header, found, err)
	}
}

func TestAnalysisGenerationGCReleasesWriterLockBetweenChunks(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := buildMinimalAnalysisGeneration(t, store, "gc-large", analysisGenerationChunkLimit, true)
	buildMinimalAnalysisGeneration(t, store, "gc-fallback", 0, true)
	buildMinimalAnalysisGeneration(t, store, "gc-active", 0, true)

	gcDone := make(chan error, 1)
	go func() {
		_, err := store.PruneAnalysisGenerations(context.Background(), 1, 1)
		gcDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	observedPartial := false
	for time.Now().Before(deadline) {
		var remaining int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM analysis_concepts WHERE generation_id = ?`, first).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining > 0 && remaining < analysisGenerationChunkLimit {
			observedPartial = true
			break
		}
		select {
		case err := <-gcDone:
			t.Fatalf("gc completed before an interleavable chunk boundary: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if !observedPartial {
		t.Fatal("did not observe bounded child deletion")
	}
	writerDone := make(chan struct{})
	go func() {
		store.AddNode(&graph.Node{ID: "gc-interleaved-writer", Kind: graph.KindFunction, Name: "Writer", FilePath: "writer.go"})
		close(writerDone)
	}()
	select {
	case <-writerDone:
		// The graph writer acquired writeMu between GC chunks.
	case err := <-gcDone:
		select {
		case <-writerDone:
		default:
			t.Fatalf("gc monopolized writer lock until completion: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("graph writer starved behind chunked analysis GC")
	}
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	if store.GetNode("gc-interleaved-writer") == nil {
		t.Fatal("interleaved graph writer was lost")
	}
}

func TestAnalysisGenerationInvalidationLatchRunsOnce(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	buildMinimalAnalysisGeneration(t, store, "latch", 0, true)
	if _, err := store.writerDB.Exec(`
		CREATE TABLE analysis_invalidation_audit(n INTEGER);
		CREATE TRIGGER analysis_active_deleted AFTER DELETE ON analysis_active_generation
		BEGIN INSERT INTO analysis_invalidation_audit(n) VALUES(1); END;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		store.AddNode(&graph.Node{ID: fmt.Sprintf("live-%03d", i), Kind: graph.KindFunction, Name: "Live", FilePath: "live.go"})
	}
	store.AddEdge(&graph.Edge{From: "live-000", To: "live-001", Kind: graph.EdgeCalls})
	var deletes int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM analysis_invalidation_audit`).Scan(&deletes); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || store.analysisGenerationPresent {
		t.Fatalf("invalidation deletes=%d cachePresent=%v", deletes, store.analysisGenerationPresent)
	}
	revision := store.AnalysisMutationRevision()
	generationID, accepted, err := store.BeginAnalysisGeneration(revision, graph.AnalysisGenerationHeader{FormatVersion: 2})
	requireAnalysisAccepted(t)(accepted, err)
	store.AddNode(&graph.Node{ID: "after-building", Kind: graph.KindFunction, Name: "After", FilePath: "after.go"})
	var state int
	if err := store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != analysisGenerationStale || store.analysisGenerationPresent {
		t.Fatalf("building invalidation state=%d cachePresent=%v", state, store.analysisGenerationPresent)
	}
}

func TestAnalysisGenerationActivationMutationRaceNeverPublishesStale(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 25; i++ {
		generationID := buildMinimalAnalysisGeneration(t, store, fmt.Sprintf("race-%02d", i), 0, false)
		revision := store.AnalysisMutationRevision()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = store.ActivateAnalysisGeneration(revision, generationID)
		}()
		go func(iteration int) {
			defer wg.Done()
			<-start
			store.AddNode(&graph.Node{ID: fmt.Sprintf("mutation-%02d", iteration), Kind: graph.KindFunction, Name: "Mutation", FilePath: "mutation.go"})
		}(i)
		close(start)
		wg.Wait()
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil || found {
			t.Fatalf("iteration %d stale generation remained active: found=%v err=%v", i, found, err)
		}
	}
}

func TestAnalysisGenerationChunkLimitRejectsBeforeWriterLock(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generationID, accepted, err := store.BeginAnalysisGeneration(store.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 1})
	requireAnalysisAccepted(t)(accepted, err)
	nodes := make([]graph.AnalysisNodeMetric, analysisGenerationChunkLimit+1)
	for i := range nodes {
		nodes[i].NodeID = fmt.Sprintf("node-%05d", i)
	}
	store.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := store.AppendAnalysisNodes(store.AnalysisMutationRevision(), generationID, nodes)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("oversized chunk err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized chunk waited on writer lock instead of failing before it")
	}
	store.writeMu.Unlock()
}

func TestAnalysisGenerationQueryPlansUseBoundedIndexes(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	plans := map[string]struct {
		query string
		args  []any
	}{
		"analysis_nodes_by_pagerank":            {`SELECT id FROM analysis_nodes WHERE generation_id = ? ORDER BY pagerank DESC, id ASC LIMIT ?`, []any{1, 10}},
		"analysis_nodes_by_community":           {`SELECT id FROM analysis_nodes WHERE generation_id = ? AND community_id = ? AND node_id > ? ORDER BY node_id LIMIT ?`, []any{1, "c", "", 10}},
		"analysis_concept_relations_by_related": {`SELECT token FROM analysis_concept_relations WHERE generation_id = ? AND related_token = ? ORDER BY rank, token`, []any{1, "x"}},

		// The node_rowid index carries the WITHOUT ROWID primary key, so it
		// covers this lookup on both columns.
		"analysis_process_step_node_fk (node_rowid=? AND generation_id=?)": {`SELECT process_id FROM analysis_process_steps WHERE generation_id = ? AND node_rowid = ? ORDER BY process_id`, []any{1, 1}},
		// The foreign-key lookup SQLite runs for each deleted analysis_nodes row.
		"analysis_process_step_node_fk (node_rowid=?)": {`SELECT 1 FROM analysis_process_steps WHERE node_rowid = ?`, []any{1}},
	}
	for index, fixture := range plans {
		rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+fixture.query, fixture.args...)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		plan := strings.Join(details, " | ")
		if !strings.Contains(plan, index) {
			t.Fatalf("plan for %s did not use index: %s", index, plan)
		}
	}
}

func TestOpenDropsRedundantAnalysisProcessStepIndex(t *testing.T) {
	path := filepathForAnalysisTest(t)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A store opened by an earlier build still has the old index.
	if _, err := store.writerDB.Exec(`CREATE INDEX analysis_process_steps_by_node ON analysis_process_steps(generation_id, node_rowid, process_id)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, want := range map[string]int{"analysis_process_steps_by_node": 0, "analysis_process_step_node_fk": 1} {
		var got int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("index %s: count %d, want %d", name, got, want)
		}
	}
}

func filepathForAnalysisTest(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/graph.sqlite"
}
