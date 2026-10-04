package store_sqlite

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
	"time"
)

func witnessBuilding(t *testing.T, s *Store, tag string) int64 {
	t.Helper()
	r := payloadRequest()
	r.LayerID = ""
	r.TreeOID = tag
	id, _, err := s.BeginPayloadGeneration(t.Context(), r)
	require.NoError(t, err)
	return id
}

func TestPayloadInputWitnessSelectedMutationRefusesReady(t *testing.T) {
	for _, kind := range []string{"base", "positive", "constant"} {
		t.Run(kind, func(t *testing.T) {
			s := openPayloadStore(t)
			seedPayloadControlPlane(t, s)
			input := witnessBuilding(t, s, "ancestor")
			output := witnessBuilding(t, s, "output")
			ids := []int64{0, input}
			w, err := s.CapturePayloadInputWitness(t.Context(), ids)
			require.NoError(t, err)
			switch kind {
			case "base":
				s.AddBatch([]*graph.Node{{ID: "repo/base", Name: "base", FilePath: "base.go", Kind: graph.KindFunction}}, nil)
			case "positive":
				s.AtGeneration(input).AddBatch([]*graph.Node{{ID: "repo/input", Name: "input", FilePath: "input.go", Kind: graph.KindFunction}}, nil)
			case "constant":
				require.NoError(t, s.AtGeneration(input).BulkSetConstantValues("repo", []graph.ConstantValueRow{{NodeID: "const", FilePath: "input.go", Value: "changed"}}))

			}
			require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), ErrPayloadInputChanged)
			row, found, err := s.Catalog().GetViewGeneration(t.Context(), output)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, ViewGenerationBuilding, row.State)
		})
	}
}

func TestPayloadInputWitnessExactSelectionAndOwnWrites(t *testing.T) {
	s := openPayloadStore(t)
	seedPayloadControlPlane(t, s)
	input := witnessBuilding(t, s, "ancestor")
	other := witnessBuilding(t, s, "other")
	output := witnessBuilding(t, s, "output")
	w, err := s.CapturePayloadInputWitness(t.Context(), []int64{input, input})
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{{ID: "base", Name: "base", Kind: graph.KindFunction}}, nil)
	require.NoError(t, s.AtGeneration(other).BulkSetConstantValues("repo", []graph.ConstantValueRow{{NodeID: "other", Value: "1"}}))
	s.AtGeneration(output).AddBatch([]*graph.Node{{ID: "own", Name: "own", Kind: graph.KindFunction}}, nil)
	require.NoError(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), "unselected base, unrelated generation and own output writes must not withdraw inputs")
}

func TestPayloadInputWitnessRejectsForeignAndCancelled(t *testing.T) {
	s := openPayloadStore(t)
	seedPayloadControlPlane(t, s)
	output := witnessBuilding(t, s, "output")
	foreign := openPayloadStore(t)
	w, err := foreign.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), ErrPayloadInputChanged)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.CapturePayloadInputWitness(ctx, []int64{0})
	require.ErrorIs(t, err, context.Canceled)
	own, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(ctx, output, 100, own), context.Canceled)
	require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, nil), ErrPayloadInputChanged)
	require.NoError(t, foreign.Close())
	_, err = foreign.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.Error(t, err)
}

func TestPayloadInputWitnessDoesNotHoldBuildGate(t *testing.T) {
	s := openPayloadStore(t)
	w, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		s.AddBatch([]*graph.Node{{ID: "during-build", Name: "during-build", Kind: graph.KindFunction}}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("capture retained write gate during simulated slow build")
	}
	s.writeMu.Lock()
	err = w.validateLocked(s)
	s.writeMu.Unlock()
	require.True(t, errors.Is(err, ErrPayloadInputChanged))
}

func TestPayloadInputWitnessActualDerivedCorrection(t *testing.T) {
	s, input := publishedStampGeneration(t)
	output := witnessBuilding(t, s, "output")
	w, err := s.CapturePayloadInputWitness(t.Context(), []int64{input})
	require.NoError(t, err)
	c, err := s.BeginDerivedCorrection(t.Context(), DerivedCorrectionRequest{GenerationID: input, Pass: "capability", FromVersion: 1, ToVersion: 2, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, c.ReplaceSourceEdges(t.Context(), []string{stampSrcA}, []*graph.Edge{{From: stampSrcA, To: stampField2, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 5}}, nil))
	// A committed correction chunk must withdraw the proof even before Finish.
	require.ErrorIs(t, s.PublishPayloadGenerationWithInputWitness(t.Context(), output, 100, w), ErrPayloadInputChanged)
	_, err = c.Finish(t.Context())
	require.NoError(t, err)
}

func TestPayloadInputWitnessCancelledWhileQueued(t *testing.T) {
	s := openPayloadStore(t)
	seedPayloadControlPlane(t, s)
	output := witnessBuilding(t, s, "output")
	w, err := s.CapturePayloadInputWitness(t.Context(), []int64{0})
	require.NoError(t, err)
	s.writeMu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Catalog().publishViewGenerationWithInputWitness(ctx, output, 100, w) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		s.writeMu.Unlock()
		t.Fatal("cancelled publication could not leave gate queue")
	}
	s.writeMu.Unlock()
	row, found, err := s.Catalog().GetViewGeneration(t.Context(), output)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ViewGenerationBuilding, row.State)
}

func TestPayloadInputWitnessCaptureDuringPinnedBulkWindow(t *testing.T) {
	s, handle, id := reparseWindowFixture(t)
	opened, err := s.BeginGenerationBulkLoad(id)
	require.NoError(t, err)
	require.True(t, opened)
	defer func() { require.NoError(t, handle.EndGenerationBulkLoadFor(id)) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	w, err := s.CapturePayloadInputWitness(ctx, []int64{0})
	require.NoError(t, err)
	require.NotNil(t, w)
	_, active := s.InGenerationBulkLoad()
	require.True(t, active, "capture must not require teardown of pinned writer")
}

