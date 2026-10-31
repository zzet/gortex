package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

type failingStartupTestProjection struct {
	*store_sqlite.Store
	file     string
	err      error
	attempts int
}

func (s *failingStartupTestProjection) AddBatchChecked(nodes []*graph.Node, edges []*graph.Edge) error {
	for _, node := range nodes {
		if node.Kind == graph.KindFile && node.FilePath == s.file && node.Meta[indexer.PendingTestProjectionMeta] == nil {
			s.attempts++
			return s.err
		}
	}
	return s.Store.AddBatchChecked(nodes, edges)
}

func TestDaemonWarmRestartRepairsCanceledTestProjectionBeforeReady(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "tracked")
	if err := os.WriteFile(filepath.Join(root, "main_test.go"), []byte("package a\nfunc TestCaller() { A() }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	startupPublicationCommit(t, root)
	ctx, finish := context.WithTimeout(context.Background(), 2*time.Minute)
	defer finish()
	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.shared.Close() })
	if _, err = state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI); err != nil {
		t.Fatal(err)
	}
	if err = state.lifecycle.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mw, _ := warmupDaemonStateContext(ctx, state, zap.NewNop(), func() {}, nil)
	if mw != nil {
		if err = mw.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := state.graph.(*store_sqlite.Store); !ok {
		t.Fatal("fixture did not use the durable SQLite store")
	}
	callers := state.graph.FindNodesByName("TestCaller")
	if len(callers) != 1 {
		t.Fatalf("test caller count = %d", len(callers))
	}
	caller := callers[0]
	var call *graph.Edge
	for _, edge := range state.graph.GetOutEdgesByNodeIDs([]string{caller.ID})[caller.ID] {
		if edge.Kind == graph.EdgeCalls && !graph.IsUnresolvedTarget(edge.To) {
			call = edge
			break
		}
	}
	if call == nil || !state.graph.RemoveEdge(call.From, call.To, graph.EdgeTests) {
		t.Fatal("fixture had no resolved call and test projection")
	}
	oldTarget := call.To
	call.To = graph.UnresolvedMarker + "A"
	state.graph.ReindexEdges([]graph.EdgeReindex{{Edge: call, OldFrom: call.From, OldTo: oldTarget}})
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	if err = state.multiIndexer.RunPreEnrichResolve(canceled, nil, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("real resolve cancellation = %v", err)
	}
	if file := state.graph.GetNode(caller.FilePath); file == nil || file.Meta[indexer.PendingTestProjectionMeta] != true {
		t.Fatal("canceled real resolver did not persist the actual frontier")
	}
	bound := false
	for _, edge := range state.graph.GetOutEdgesByNodeIDs([]string{caller.ID})[caller.ID] {
		bound = bound || edge.Kind == graph.EdgeCalls && edge.To == oldTarget
	}
	if !bound {
		t.Fatal("fixture did not bind the call before cancellation")
	}
	if err = state.shared.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise the ordinary warmup boundary with a genuine failed checked
	// publication. It must retain debt, unwind batch mode, and prevent the
	// outer startup owner from marking enrichment complete while ctx is live.
	disk, err := store_sqlite.Open(daemonBackendPath)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("injected startup test projection publication failure")
	fault := &failingStartupTestProjection{Store: disk, file: caller.FilePath, err: writeErr}
	// Match NewSharedServer: cfgFile is the workspace settings file, not
	// the global tracked-repository configuration.
	cm, err := config.NewConfigManager("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.Global().Repos) != 1 || cm.Global().Repos[0].Path != root {
		t.Fatalf("injected startup did not load the actual tracked repository: %+v", cm.Global().Repos)
	}
	conf, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	reg := state.indexer.Registry()
	idx := indexer.New(fault, reg, conf.Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(fault, reg, idx.Search(), cm, zap.NewNop())
	failedState := &daemonState{graph: fault, indexer: idx, multiIndexer: mi, configManager: cm}
	failedReady, failedWatcher := false, false
	watcher, failed := warmupDaemonStateContext(ctx, failedState, zap.NewNop(), func() { failedReady = true }, func(context.Context, *indexer.MultiWatcher) error {
		failedWatcher = true
		return nil
	})
	if fault.attempts == 0 || len(mi.AllMetadata()) != 1 || failed.reposChanged != 0 || failed.filesReindexed != 0 {
		t.Fatalf("injected startup did not reach unchanged repair: attempts=%d repos=%d changed=%d files=%d", fault.attempts, len(mi.AllMetadata()), failed.reposChanged, failed.filesReindexed)
	}
	if watcher != nil || failedReady || failedWatcher || !errors.Is(failed.err, writeErr) || failed.canFinalize(ctx) || ctx.Err() != nil {
		t.Fatalf("failed live-context startup was promoted: ready=%v watcher=%v error=%v", failedReady, failedWatcher, failed.err)
	}
	if file := disk.GetNode(caller.FilePath); file == nil || file.Meta[indexer.PendingTestProjectionMeta] != true {
		t.Fatal("failed startup publication discharged durable repair debt")
	}
	if err = mi.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = disk.Close(); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	restarted, err := buildDaemonState(logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.shared.Close() })
	if err = restarted.lifecycle.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	ready := false
	checkRepair := func() {
		ready = true
		file := restarted.graph.GetNode(caller.FilePath)
		if file == nil || file.Meta[indexer.PendingTestProjectionMeta] != nil {
			t.Error("ordinary startup advertised readiness with repair debt")
		}
		found := false
		for _, edge := range restarted.graph.GetOutEdgesByNodeIDs([]string{caller.ID})[caller.ID] {
			found = found || edge.Kind == graph.EdgeTests && edge.To == oldTarget
		}
		if !found {
			t.Error("ordinary startup did not repair the already-bound call before ready")
		}
	}
	mw, _ = warmupDaemonStateContext(ctx, restarted, logger, checkRepair, nil)
	if mw != nil {
		defer func() { _ = mw.Stop() }()
	}
	if !ready {
		t.Fatal("ordinary restarted warmup never became ready")
	}
	unchanged := false
	for _, event := range observed.All() {
		if event.Message == "daemon: warmup change detection" {
			unchanged = event.ContextMap()["changed_repos"] == int64(0)
		}
	}
	if !unchanged {
		t.Fatal("restart fixture did not exercise the unchanged derived-pass fast path")
	}
}
