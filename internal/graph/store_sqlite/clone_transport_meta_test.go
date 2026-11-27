package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithoutCloneTransportMetaKeepsStoredFacts(t *testing.T) {
	meta := map[string]any{
		"clone_shingles": []uint64{1, 2, 3}, "clone_tokens": 42,
		"clone_body": "body-fingerprint", "clone_sig": "final-signature",
		"semantic_type": "func() int", "semantic_source": "go-types",
		"signature": "func Caller() int", "other": int64(9007199254740993),
	}
	stored := WithoutCloneTransportMeta(meta)
	require.Equal(t, map[string]any{
		"clone_body": "body-fingerprint", "clone_sig": "final-signature",
		"semantic_type": "func() int", "semantic_source": "go-types",
		"signature": "func Caller() int", "other": int64(9007199254740993),
	}, stored)
	require.Len(t, meta, 8, "the comparison must not remove sidecar input from the corpus")
	require.Contains(t, meta, "clone_shingles")
	require.Contains(t, meta, "clone_tokens")
	require.Nil(t, WithoutCloneTransportMeta(map[string]any{"clone_shingles": []uint64{1}}))
}
