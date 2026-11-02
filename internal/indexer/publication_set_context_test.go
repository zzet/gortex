package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilePublicationCarrierCopiesExactMembers(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	owner := NewPublicationPhaseRecorder(8, 2)
	first := owner.Begin("", "first", PublicationSourceMCPEdit, time.Now())
	second := owner.Begin("", "second", PublicationSourceMCPEdit, time.Now())
	input := map[string]*PublicationPhaseRecord{a: first, b: second}
	ctx := WithFilePublicationRecords(context.Background(), input)
	input[a] = second
	delete(input, b)
	require.Same(t, first, filePublicationRecordFrom(ctx, a))
	require.Same(t, second, filePublicationRecordFrom(ctx, b))
	require.Nil(t, filePublicationRecordFrom(ctx, filepath.Join(root, "foreign.go")))
}

func TestFilePublicationCarrierRejectsAliasesWithoutSingleRecordBorrow(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	owner := NewPublicationPhaseRecorder(8, 2)
	first := owner.Begin("", "first", PublicationSourceMCPEdit, time.Now())
	for _, records := range []map[string]*PublicationPhaseRecord{
		{a: first, b: first},
		{a: first, filepath.Join(root, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "a.go": first},
		{a: nil, "relative.go": first},
	} {
		ctx := WithFilePublicationRecords(WithPublicationRecord(context.Background(), first), records)
		require.Nil(t, filePublicationRecordFrom(ctx, a))
		require.Nil(t, filePublicationRecordFrom(ctx, b))
		require.False(t, hasFilePublicationRecords(ctx))
	}
}

func TestEmptyFilePublicationCarrierShadowsEveryPriorOwner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	owner := NewPublicationPhaseRecorder(8, 2)
	record := owner.Begin("", "first", PublicationSourceMCPEdit, time.Now())
	plain := context.Background()
	require.Equal(t, plain, WithFilePublicationRecords(plain, nil))
	for _, prior := range []context.Context{
		WithPublicationRecord(plain, record),
		WithFilePublicationRecords(plain, map[string]*PublicationPhaseRecord{path: record}),
		WithFilePublicationRecords(WithPublicationRecord(plain, record), map[string]*PublicationPhaseRecord{path: record}),
	} {
		for _, empty := range []map[string]*PublicationPhaseRecord{nil, {}} {
			ctx := WithFilePublicationRecords(prior, empty)
			require.Nil(t, filePublicationRecordFrom(ctx, path))
			require.False(t, hasFilePublicationRecords(ctx))
		}
	}
}

func TestFilePublicationMixedAliasesAreOrderIndependent(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	alias := filepath.Join(root, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + "a.go"
	owner := NewPublicationPhaseRecorder(8, 2)
	first := owner.Begin("", "first", PublicationSourceMCPEdit, time.Now())
	second := owner.Begin("", "second", PublicationSourceMCPEdit, time.Now())
	for range 100 {
		ctx := WithFilePublicationRecords(context.Background(), map[string]*PublicationPhaseRecord{a: first, alias: second, b: second})
		require.Nil(t, filePublicationRecordFrom(ctx, a))
		require.Nil(t, filePublicationRecordFrom(ctx, b), "ambiguous normalized path cannot hide another use of this record")
	}
}
