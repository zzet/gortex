package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func publishOutlineCommunities(t *testing.T, s *Store, summaries []graph.AnalysisCommunitySummary, members []graph.AnalysisNodeMetric) int64 {
	t.Helper()
	revision := s.AnalysisMutationRevision()
	id, accepted, err := s.BeginAnalysisGeneration(revision, graph.AnalysisGenerationHeader{
		FormatVersion: 77, NodeCount: len(members), CommunityCount: len(summaries),
	})
	requireAnalysisAccepted(t)(accepted, err)
	requireAnalysisAccepted(t)(s.AppendAnalysisCommunities(revision, id, summaries))
	requireAnalysisAccepted(t)(s.AppendAnalysisNodes(revision, id, members))
	for _, component := range []graph.AnalysisBlobComponent{graph.AnalysisBlobAdjacency, graph.AnalysisBlobLeiden} {
		requireAnalysisAccepted(t)(s.PutAnalysisBlob(revision, id, graph.AnalysisBlob{Component: component, Payload: []byte("fixture")}))
	}
	for component, count := range map[graph.AnalysisComponent]int{
		graph.AnalysisComponentNodes: len(members), graph.AnalysisComponentCommunities: len(summaries),
		graph.AnalysisComponentProcesses: 0, graph.AnalysisComponentConcepts: 0,
		graph.AnalysisComponentAdjacency: 1, graph.AnalysisComponentLeiden: 1,
	} {
		requireAnalysisAccepted(t)(s.SealAnalysisComponent(revision, id, component, count))
	}
	requireAnalysisAccepted(t)(s.ActivateAnalysisGeneration(revision, id))
	return id
}

func TestOutlineGenerationCommunitiesScopeParity(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	nodes := []*graph.Node{
		{ID: "a", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "b", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "q"},
		{ID: "legacy", RepoPrefix: "legacy"},
		{ID: "global", FilePath: "external::symbol"},
		{ID: "call", FilePath: "external-call::symbol"},
		{ID: "unowned", WorkspaceID: "ws", ProjectID: "p", FilePath: "source.go"},
		{ID: "case", FilePath: "External::source"},
		{ID: "empty"},
	}
	s.AddBatch(nodes, nil)
	byID := make(map[string]*graph.Node)
	var summaries []graph.AnalysisCommunitySummary
	var members []graph.AnalysisNodeMetric
	for i, node := range append(nodes, &graph.Node{ID: "missing"}) {
		id := fmt.Sprintf("c%02d", i)
		summaries = append(summaries, graph.AnalysisCommunitySummary{ID: id, Label: id, Size: 1, Files: []string{"omitted"}})
		members = append(members, graph.AnalysisNodeMetric{NodeID: node.ID, CommunityID: id})
		if node.ID != "missing" {
			byID[node.ID] = node
		}
	}
	// The last community has a missing first member and a visible late one.
	summaries = append(summaries, graph.AnalysisCommunitySummary{ID: "late", Label: "late", Size: 2})
	members = append(members, graph.AnalysisNodeMetric{NodeID: "missing-late", CommunityID: "late"}, graph.AnalysisNodeMetric{NodeID: "late-a", CommunityID: "late"})
	s.AddNode(&graph.Node{ID: "late-a", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"})
	byID["late-a"] = s.GetNode("late-a")
	id := publishOutlineCommunities(t, s, summaries, members)
	for name, scope := range map[string]graph.NodeScope{
		"unbound": {}, "repo": {RepoAllow: map[string]bool{"a": true}},
		"workspace": {WorkspaceID: "ws"}, "project": {WorkspaceID: "ws", ProjectID: "p"},
		"project-only": {ProjectID: "ignored"}, "fallback": {WorkspaceID: "legacy", ProjectID: "legacy"},
		"false-only": {RepoAllow: map[string]bool{"a": false}},
		"mixed":      {RepoAllow: map[string]bool{"a": true, "b": false}},
		"globals":    {WorkspaceID: "unknown", RepoAllow: map[string]bool{"unknown": true}},
	} {
		t.Run(name, func(t *testing.T) {
			// Independent in-memory reference: missing IDs never pass membership,
			// even though NodeScope.Allows(nil) intentionally permits traversal.
			var want []graph.AnalysisCommunitySummary
			for _, summary := range summaries {
				for _, member := range members {
					node := byID[member.NodeID]
					if member.CommunityID == summary.ID && node != nil && scope.Allows(node) {
						summary.Files = nil
						want = append(want, summary)
						break
					}
				}
			}
			var got []graph.AnalysisCommunitySummary
			cursor := ""
			for {
				rows, next, err := s.ScopedAnalysisCommunitySummaries(id, scope, 2, cursor)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, rows...)
				if next == "" {
					break
				}
				if next <= cursor {
					t.Fatalf("cursor did not advance: %q -> %q", cursor, next)
				}
				cursor = next
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
	query := outlineCommunitiesQuery(id, 0, graph.NodeScope{RepoAllow: map[string]bool{"a": true}}, 2, "")
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"c", "a", "n"} {
		if strings.Contains(plan.String(), "SCAN "+table) {
			t.Fatalf("unbounded scan: %s", plan.String())
		}
	}
	if !strings.Contains(plan.String(), "analysis_nodes_by_community") {
		t.Fatalf("membership index absent: %s", plan.String())
	}
}

func TestOutlineGenerationCommunitiesReceiptAndCancellation(t *testing.T) {
	s, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddNode(&graph.Node{ID: "n"})
	id := publishOutlineCommunities(t, s, []graph.AnalysisCommunitySummary{{ID: "c", Size: 1}}, []graph.AnalysisNodeMetric{{NodeID: "n", CommunityID: "c"}})
	for _, invalid := range []int{-1, 1001} {
		if _, _, err := s.ScopedAnalysisCommunitySummaries(id, graph.NodeScope{}, invalid, ""); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, _, err := s.AtGeneration(8).ScopedAnalysisCommunitySummaries(id, graph.NodeScope{}, 1, ""); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("foreign receipt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.WithReadContext(ctx).ScopedAnalysisCommunitySummaries(id, graph.NodeScope{}, 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	s.AddNode(&graph.Node{ID: "mutation"})
	if _, _, err := s.ScopedAnalysisCommunitySummaries(id, graph.NodeScope{}, 1, ""); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("stale receipt: %v", err)
	}
}
