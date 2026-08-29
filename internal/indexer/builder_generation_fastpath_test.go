package indexer

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
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

func TestSparseGenerationBuilderAdoptedEmptyPlanRunsRecoveryPass(t *testing.T) {
	builder, store := newFastPathTestBuilder(t)
	identity := fastPathGenerationIdentity("adopted")
	generationID, _, err := store.BeginPayloadGeneration(context.Background(), fastPathPayloadRequest(identity))
	if err != nil {
		t.Fatalf("seed building generation: %v", err)
	}
	walkErr := errors.New("adopted generation recovery walk")
	target := &poisonGenerationSource{}
	req := fastPathBuildRequest(t, store, target, identity.LayerID)
	physicalPasses := 0
	builder.beforePhysicalPass = func(gotGenerationID int64) error {
		physicalPasses++
		if gotGenerationID != generationID {
			t.Errorf("physical pass generation = %d, want %d", gotGenerationID, generationID)
		}
		return walkErr
	}

	gotID, report, err := builder.Build(context.Background(), req)
	if !errors.Is(err, walkErr) {
		t.Fatalf("build error = %v, want %v", err, walkErr)
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
