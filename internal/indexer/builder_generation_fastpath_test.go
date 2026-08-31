package indexer

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
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

func fastPathPayloadRequest(identity GenerationIdentity) store_sqlite.PayloadGenerationRequest {
	return store_sqlite.PayloadGenerationRequest{
		OwnerKind:            identity.OwnerKind,
		GraphID:              identity.GraphID,
		LayerID:              identity.LayerID,
		CheckoutID:           identity.CheckoutID,
		GenerationKind:       identity.GenerationKind,
		BaseGenerationID:     identity.BaseGenerationID,
		LowerViewFingerprint: identity.LowerViewFingerprint,
		TreeOID:              identity.TreeOID,
		ProvenanceCommitOID:  identity.ProvenanceCommitOID,
		ConfigHash:           identity.ConfigHash,
		ExtractorVersions:    identity.ExtractorVersions,
		ResolverVersion:      identity.ResolverVersion,
		CreatedAt:            identity.CreatedAt,
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
	physicalPasses := 0
	builder.beforePhysicalPass = func(int64) error {
		physicalPasses++
		return errors.New("fresh empty plan must not run a physical pass")
	}
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
		t.Fatal("fresh empty plan walked its target source")
	}
	if physicalPasses != 0 {
		t.Fatalf("fresh empty plan physical passes = %d, want 0", physicalPasses)
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
	physicalPasses := 0
	builder.beforePhysicalPass = func(int64) error {
		physicalPasses++
		return errors.New("fresh deletion-only plan must not run a physical pass")
	}

	generationID, report, err := builder.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("build deletion-only generation: %v", err)
	}
	if target.walked {
		t.Fatal("fresh deletion-only plan walked its target source")
	}
	if physicalPasses != 0 {
		t.Fatalf("fresh deletion-only plan physical passes = %d, want 0", physicalPasses)
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

func TestSparseGenerationBuilderAdoptedEmptyPlanRunsRecoveryPass(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	identity := fastPathGenerationIdentity("adopted")
	generationID, _, err := store.BeginPayloadGeneration(context.Background(), fastPathPayloadRequest(identity))
	if err != nil {
		t.Fatalf("seed building generation: %v", err)
	}
	target := &poisonGenerationSource{}
	req := fastPathBuildRequest(t, store, target, identity.LayerID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	physicalPasses := 0
	builder.beforePhysicalPass = func(gotGenerationID int64) error {
		physicalPasses++
		if gotGenerationID != generationID {
			t.Errorf("physical pass generation = %d, want %d", gotGenerationID, generationID)
		}
		cancel()
		return nil
	}

	gotID, report, err := builder.Build(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("build error = %v, want context canceled", err)
	}
	if !strings.Contains(err.Error(), "indexer: index generation payload:") {
		t.Fatalf("build error = %v, want failure from the real index pass", err)
	}
	if physicalPasses != 1 {
		t.Fatalf("adopted generation physical passes = %d, want 1", physicalPasses)
	}
	if gotID != generationID || report.GenerationID != generationID {
		t.Fatalf("generation = (%d, %d), want adopted %d", gotID, report.GenerationID, generationID)
	}
	row, found, getErr := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found {
		t.Fatalf("get abandoned generation: found=%v err=%v", found, getErr)
	}
	if row.State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("abandoned generation state = %q, want %q", row.State, store_sqlite.ViewGenerationFailed)
	}
}

func TestSparseGenerationBuilderAdoptedNonEmptyPlanRunsRealPass(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	generationID, _, err := fixture.store.BeginPayloadGeneration(
		context.Background(), payloadRequestForBuild(fixture.request),
	)
	if err != nil {
		t.Fatalf("seed building generation: %v", err)
	}
	physicalPasses := 0
	fixture.builder.beforePhysicalPass = func(gotGenerationID int64) error {
		physicalPasses++
		if gotGenerationID != generationID {
			t.Errorf("physical pass generation = %d, want %d", gotGenerationID, generationID)
		}
		return nil
	}

	gotID, report, err := fixture.builder.Build(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("build adopted non-empty generation: %v", err)
	}
	if physicalPasses != 1 {
		t.Fatalf("adopted non-empty generation physical passes = %d, want 1", physicalPasses)
	}
	if gotID != generationID || report.GenerationID != generationID {
		t.Fatalf("generation = (%d, %d), want adopted %d", gotID, report.GenerationID, generationID)
	}
	if len(report.IndexedPaths) == 0 {
		t.Fatal("adopted non-empty generation planned no indexed paths")
	}
	if report.NodeCount == 0 || report.ReplaceMasks == 0 {
		t.Fatalf("real pass payload = nodes %d replace masks %d, want both non-zero",
			report.NodeCount, report.ReplaceMasks)
	}
	row, found, getErr := fixture.store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found {
		t.Fatalf("get published generation: found=%v err=%v", found, getErr)
	}
	if row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation state = %q, want %q", row.State, store_sqlite.ViewGenerationReady)
	}
}

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
