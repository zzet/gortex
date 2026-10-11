package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSparseGenerationBuildOwnsGenerationBulkWindow(t *testing.T) {
	tests := []struct {
		name       string
		prePublish error
	}{
		{name: "success"},
		{name: "pre-publish failure", prePublish: errors.New("stop before publication")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSparseBuildFlightFixture(t)
			var (
				observedGeneration int64
				observedOpen       bool
				observedCache      int64
				observedCheckpoint int64
				observedShape      bool
			)
			fixture.request.PrePublish = func(_ context.Context, generationID int64) error {
				observedGeneration, observedOpen = fixture.store.InGenerationBulkLoad()
				observedCache, observedCheckpoint, observedShape = fixture.store.GenerationBulkLoadShape()
				if observedGeneration != generationID {
					return errors.New("generation bulk window owns the wrong generation")
				}
				return tt.prePublish
			}

			generationID, report, err := fixture.builder.Build(context.Background(), fixture.request)
			if tt.prePublish == nil {
				if err != nil {
					t.Fatalf("build sparse generation: %v", err)
				}
				if report.GenerationID != generationID {
					t.Fatalf("report generation = %d, want %d", report.GenerationID, generationID)
				}
			} else if !errors.Is(err, tt.prePublish) {
				t.Fatalf("build error = %v, want %v", err, tt.prePublish)
			}
			if !observedOpen {
				t.Fatal("pre-publish hook did not observe a generation bulk window")
			}
			if !observedShape {
				t.Fatal("pre-publish hook could not read generation bulk shape")
			}
			if observedCache != -262144 {
				t.Fatalf("bulk cache_size = %d, want -262144", observedCache)
			}
			if observedCheckpoint != 0 {
				t.Fatalf("bulk wal_autocheckpoint = %d, want 0", observedCheckpoint)
			}
			if active, open := fixture.store.InGenerationBulkLoad(); open {
				t.Fatalf("generation bulk window leaked after build: generation %d", active)
			}
		})
	}
}

func TestSparseGenerationBulkWindowFallsBackForPopulatedAdoption(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	generationID, handle, err := fixture.store.BeginPayloadGeneration(
		context.Background(), payloadRequestForBuild(fixture.request))
	if err != nil {
		t.Fatalf("begin payload generation: %v", err)
	}
	if err := handle.AddBatchChecked([]*graph.Node{{
		ID:         "populated-adoption",
		Name:       "populated-adoption",
		Kind:       graph.KindFile,
		FilePath:   "populated-adoption.go",
		RepoPrefix: builderRepoPrefix,
	}}, nil); err != nil {
		t.Fatalf("populate adopted generation: %v", err)
	}

	window := &generationBulkWindow{
		loader: fixture.store, generationID: generationID, logger: fixture.builder.Logger,
	}
	if err := window.open(); err != nil {
		t.Fatalf("open populated sparse-generation window: %v", err)
	}
	if window.opened {
		t.Fatal("populated sparse generation unexpectedly opened a bulk window")
	}
}
