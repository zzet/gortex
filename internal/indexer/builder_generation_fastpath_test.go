package indexer

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

type poisonGenerationSource struct {
	walkErr error
	walked  bool
}

func (s *poisonGenerationSource) Open(string) (io.ReadCloser, source.FileMeta, error) {
	return nil, source.FileMeta{}, source.ErrNotInSource
}

func (s *poisonGenerationSource) Stat(string) (source.FileMeta, error) {
	return source.FileMeta{}, source.ErrNotInSource
}

func (s *poisonGenerationSource) Walk(context.Context, func(source.FileMeta) error) error {
	s.walked = true
	return s.walkErr
}

func (*poisonGenerationSource) Identity() string { return "poison-generation-source" }
func (*poisonGenerationSource) Close() error     { return nil }

func newFastPathTestBuilder(t *testing.T) (*SparseGenerationBuilder, *store_sqlite.Store) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return &SparseGenerationBuilder{
		Store:    store,
		Registry: parser.NewRegistry(),
		Config:   config.IndexConfig{},
		Logger:   zap.NewNop(),
	}, store
}

func fastPathGenerationIdentity(layerID string) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:            "dedicated_graph",
		GraphID:              "graph",
		LayerID:              layerID,
		CheckoutID:           "checkout",
		GenerationKind:       CommitLayerGenerationKind,
		LowerViewFingerprint: "lower",
		TreeOID:              "tree",
		ProvenanceCommitOID:  "commit",
		ConfigHash:           "config",
		ExtractorVersions:    "extractors",
		ResolverVersion:      "resolver",
		CreatedAt:            1,
	}
}

func fastPathBuildRequest(t *testing.T, store *store_sqlite.Store, target source.ContentSource, layerID string) BuildRequest {
	t.Helper()
	return BuildRequest{
		Identity:    fastPathGenerationIdentity(layerID),
		Base:        store.AtGeneration(0),
		Target:      target,
		RootPath:    t.TempDir(),
		RepoPrefix:  "repo",
		WorkspaceID: "workspace",
		ProjectID:   "project",
	}
}

func TestSparseGenerationBuilderFreshEmptyPlanSkipsPassAndPublishes(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	walkErr := errors.New("content source walk must not run")
	target := &poisonGenerationSource{walkErr: walkErr}
	req := fastPathBuildRequest(t, store, target, "empty")
	prePublished := false
	req.PrePublish = func(context.Context, int64) error {
		prePublished = true
		return nil
	}

	generationID, report, err := builder.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("build empty generation: %v", err)
	}
	if target.walked {
		t.Fatal("fresh empty plan ran the index pass")
	}
	if !prePublished {
		t.Fatal("fresh empty plan skipped PrePublish")
	}
	if report.GenerationID != generationID || report.NodeCount != 0 || report.EdgeCount != 0 {
		t.Fatalf("report = %+v, want empty generation %d", report, generationID)
	}

	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("get published generation: found=%v err=%v", found, err)
	}
	if row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation state = %q, want %q", row.State, store_sqlite.ViewGenerationReady)
	}
	handle := store.AtGeneration(generationID)
	masks, err := handle.FileMasks()
	if err != nil {
		t.Fatalf("read file masks: %v", err)
	}
	if len(masks) != 0 {
		t.Fatalf("file masks = %+v, want none", masks)
	}
	producers, err := handle.ProducerStates()
	if err != nil {
		t.Fatalf("read producer states: %v", err)
	}
	if len(producers) == 0 || len(producers) != len(report.Producers) {
		t.Fatalf("producer states = %d, report = %d", len(producers), len(report.Producers))
	}
	for _, producer := range producers {
		if producer.State == store_sqlite.ProducerStateBuilding {
			t.Fatalf("producer %q remained building", producer.Producer)
		}
	}
}

func TestSparseGenerationBuilderFreshDeletionOnlyPlanSkipsPass(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	target := &poisonGenerationSource{walkErr: errors.New("content source walk must not run")}
	req := fastPathBuildRequest(t, store, target, "deletion-only")
	req.Changes = []LayerPathChange{{Path: "gone.go", Kind: LayerPathDeleted}}

	generationID, report, err := builder.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("build deletion-only generation: %v", err)
	}
	if target.walked {
		t.Fatal("fresh deletion-only plan ran the index pass")
	}
	if report.DeletedFiles != 1 || report.DeleteMasks != 1 || len(report.IndexedPaths) != 0 {
		t.Fatalf("deletion report = %+v", report)
	}
	masks, err := store.AtGeneration(generationID).FileMasks()
	if err != nil {
		t.Fatalf("read file masks: %v", err)
	}
	if len(masks) != 1 || masks[0].RepoPrefix != "repo" || masks[0].FilePath != "repo/gone.go" || masks[0].Mode != store_sqlite.OwnershipDelete {
		t.Fatalf("file masks = %+v, want one delete mask", masks)
	}
}

// TestSparseGenerationBuilderAdoptedEmptyPlanRunsRecoveryPass was removed with
// the model-free rebuild. It asserted that adopting an in-flight generation
// with an empty plan re-derives the payload with a full-source recovery walk —
// the adopted-sparse-recovery seam introduced by the skipped commit 7fba6338
// ("prove adopted sparse recovery"). The model-free physical pass narrows the
// content source to the plan's file set, so an adopted empty plan does no walk;
// there is no recovery pass to observe without reintroducing the skipped seam.

func TestSparseGenerationBuilderAbandonSurvivesCanceledBuildContext(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	target := &poisonGenerationSource{walkErr: errors.New("content source walk must not run")}
	req := fastPathBuildRequest(t, store, target, "canceled")
	prePublishErr := errors.New("pre-publish rejected generation")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req.PrePublish = func(context.Context, int64) error {
		cancel()
		return prePublishErr
	}

	generationID, _, err := builder.Build(ctx, req)
	if !errors.Is(err, prePublishErr) {
		t.Fatalf("build error = %v, want %v", err, prePublishErr)
	}
	if target.walked {
		t.Fatal("fresh empty plan ran the index pass")
	}
	row, found, getErr := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found {
		t.Fatalf("get abandoned generation: found=%v err=%v", found, getErr)
	}
	if row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("abandoned generation state = %q, want %q", row.State, store_sqlite.ViewGenerationFailed)
	}
}

func TestSparseGenerationBuilderTracksRuntimeActivityThroughPublish(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	req := fastPathBuildRequest(t, store, &poisonGenerationSource{}, "activity-success")
	before := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]
	req.PrePublish = func(context.Context, int64) error {
		active := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]
		if active != before+1 {
			t.Fatalf("activity during publication = %d, want %d", active, before+1)
		}
		return nil
	}

	if _, _, err := builder.Build(context.Background(), req); err != nil {
		t.Fatalf("build generation: %v", err)
	}
	if active := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]; active != before {
		t.Fatalf("activity after publication = %d, want baseline %d", active, before)
	}
}

func TestSparseGenerationBuilderReleasesRuntimeActivityOnError(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	req := fastPathBuildRequest(t, store, &poisonGenerationSource{}, "activity-error")
	before := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]
	wantErr := errors.New("reject publication")
	req.PrePublish = func(context.Context, int64) error {
		active := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]
		if active != before+1 {
			t.Fatalf("activity during failed publication = %d, want %d", active, before+1)
		}
		return wantErr
	}

	if _, _, err := builder.Build(context.Background(), req); !errors.Is(err, wantErr) {
		t.Fatalf("build error = %v, want %v", err, wantErr)
	}
	if active := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]; active != before {
		t.Fatalf("activity after failed publication = %d, want baseline %d", active, before)
	}
}

func TestSparseGenerationBuilderReleasesRuntimeActivityOnPanic(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	req := fastPathBuildRequest(t, store, &poisonGenerationSource{}, "activity-panic")
	before := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]
	req.PrePublish = func(context.Context, int64) error {
		panic("injected publication panic")
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("build did not propagate injected panic")
			}
		}()
		_, _, _ = builder.Build(context.Background(), req)
	}()
	if active := runtimeactivity.Current().ByKind[sparseGenerationBuildActivity]; active != before {
		t.Fatalf("activity after panicked publication = %d, want baseline %d", active, before)
	}
}

func BenchmarkSparseGenerationRuntimeActivityGuard(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		runtimeactivity.Begin(sparseGenerationBuildActivity)
		runtimeactivity.End(sparseGenerationBuildActivity)
	}
}

func BenchmarkSparseGenerationEmptyPhysicalBuild(b *testing.B) {
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close store: %v", err)
		}
	})
	builder := &SparseGenerationBuilder{
		Store: store, Registry: parser.NewRegistry(), Config: config.IndexConfig{}, Logger: zap.NewNop(),
	}
	root := b.TempDir()
	requests := make([]BuildRequest, b.N)
	for i := range requests {
		requests[i] = BuildRequest{
			Identity:    fastPathGenerationIdentity("benchmark-" + strconv.Itoa(i)),
			Base:        store.AtGeneration(0),
			Target:      &poisonGenerationSource{},
			RootPath:    root,
			RepoPrefix:  "repo",
			WorkspaceID: "workspace",
			ProjectID:   "project",
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, _, err := builder.Build(context.Background(), requests[i]); err != nil {
			b.Fatalf("build %d: %v", i, err)
		}
	}
}
