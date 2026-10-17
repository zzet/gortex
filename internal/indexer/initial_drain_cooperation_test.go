package indexer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type initialDrainRecordingStore struct {
	*graph.Graph
	batches []int
	ids     []string
	before  func()
}

func (s *initialDrainRecordingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.before != nil {
		s.before()
	}
	s.batches = append(s.batches, len(nodes)+len(edges))
	for _, node := range nodes {
		s.ids = append(s.ids, node.ID)
	}
	for _, edge := range edges {
		s.ids = append(s.ids, edge.To)
	}
	s.Graph.AddBatch(nodes, edges)
}

func TestInitialDrainContinuesWhenInteractiveDemandClears(t *testing.T) {
	var demand atomic.Bool
	demand.Store(true)
	observed, cleared := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var checks int
	ctx := withInitialDrainCooperation(t.Context(), func() bool {
		once.Do(func() { close(observed) })
		checks++
		if checks == 1 {
			return true
		}
		<-cleared
		return demand.Load()
	})
	go func() {
		<-observed
		demand.Store(false)
		close(cleared)
	}()
	target := &initialDrainRecordingStore{Graph: graph.New(), before: func() {
		if demand.Load() {
			t.Error("drain wrote before the interactive writer cleared its demand")
		}
	}}
	if err := drainAddBatch(ctx, target, []*graph.Node{{ID: "payload"}}, nil); err != nil {
		t.Fatal(err)
	}
	<-cleared
	if !slices.Equal(target.batches, []int{1}) || target.GetNode("payload") == nil {
		t.Fatalf("cleared demand lost the payload: batches=%v", target.batches)
	}
}

func TestInitialDrainMakesBoundedProgressWithContinuousDemandAndPreservesPayload(t *testing.T) {
	nodes := make([]*graph.Node, 2049)
	edges := make([]*graph.Edge, 1025)
	var wantIDs []string
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("repo/file.go::N%04d", i), Kind: graph.KindFunction,
			FilePath: "repo/file.go", Meta: map[string]interface{}{"signature": fmt.Sprintf("N%d()", i)}}
		wantIDs = append(wantIDs, nodes[i].ID)
	}
	for i := range edges {
		edges[i] = &graph.Edge{From: nodes[0].ID, To: nodes[i+1].ID, Kind: graph.EdgeCalls, FilePath: "repo/file.go"}
		wantIDs = append(wantIDs, edges[i].To)
	}
	var checks int
	ctx := withInitialDrainCooperation(t.Context(), func() bool { checks++; return true })
	target := &initialDrainRecordingStore{Graph: graph.New()}
	// Demand never clears. The helper must return because each stand-down is
	// bounded, not because a test clears demand or cancels the build.
	if err := drainAddBatch(ctx, target, nodes, edges); err != nil {
		t.Fatal(err)
	}
	if checks < len(target.batches) || !slices.Equal(target.batches, []int{1024, 1024, 1, 1024, 1}) ||
		!slices.Equal(target.ids, wantIDs) {
		t.Fatalf("continuous demand changed slices or order: checks=%d batches=%v", checks, target.batches)
	}
	baseline := graph.New()
	baseline.AddBatch(nodes, edges)
	for _, node := range nodes {
		if !reflect.DeepEqual(target.GetNode(node.ID), baseline.GetNode(node.ID)) {
			t.Fatalf("slice changed node %s", node.ID)
		}
	}
	gotEdges, wantEdges := target.GetOutEdges(nodes[0].ID), baseline.GetOutEdges(nodes[0].ID)
	byTarget := func(a, b *graph.Edge) int { return strings.Compare(a.To, b.To) }
	slices.SortFunc(gotEdges, byTarget)
	slices.SortFunc(wantEdges, byTarget)
	if !reflect.DeepEqual(gotEdges, wantEdges) {
		t.Fatal("slice changed edge payload")
	}
	ordinary := &initialDrainRecordingStore{Graph: graph.New()}
	if err := drainAddBatch(t.Context(), ordinary, nodes, edges); err != nil ||
		!slices.Equal(ordinary.batches, []int{len(nodes) + len(edges)}) {
		t.Fatalf("ordinary drain changed: batches=%v err=%v", ordinary.batches, err)
	}
}

func TestInitialDrainCancellationStopsBeforeAnotherWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = withInitialDrainCooperation(ctx, func() bool { cancel(); return true })
	target := &initialDrainRecordingStore{Graph: graph.New()}
	err := drainAddBatch(ctx, target, []*graph.Node{{ID: "unwritten"}}, nil)
	if !errors.Is(err, context.Canceled) || len(target.batches) != 0 {
		t.Fatalf("canceled demand wait wrote payload: batches=%v err=%v", target.batches, err)
	}
}

func TestInitialClaimedDrainCancellationDoesNotPublish(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	previous := drainSubBatchHook
	defer func() { drainSubBatchHook = previous }()
	var slices int
	// This small committed fixture drains one node slice and one edge slice.
	// Cancel on its last edge slice to exercise the post-drain success guard.
	drainSubBatchHook = func() {
		slices++
		if slices == 2 {
			cancel()
		}
	}
	_, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	if slices != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("initial drain did not stop: slices=%d err=%v", slices, err)
	}
	_, present, stateErr := builder.Store.AtGeneration(claim.GenerationID).GetRepoIndexState(request.RepoPrefix)
	if stateErr != nil || present {
		t.Fatalf("canceled final slice recorded successful index provenance: present=%t err=%v", present, stateErr)
	}
	row, found, err := builder.Store.Catalog().GetViewGeneration(t.Context(), claim.GenerationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("canceled initial reservation was published or lost: row=%+v found=%t err=%v", row, found, err)
	}
	if _, err := builder.Store.Catalog().AdoptDedicatedBaseGeneration(t.Context(),
		store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err == nil {
		t.Fatal("canceled initial generation was adoptable")
	}
}
