//go:build acceptance

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestProjectionLatencyNativeMembershipManifestIsRequired(t *testing.T) {
	manifest := projectionLatencyPopulationManifest{
		Revision: strings.Repeat("a", 40), Policy: projectionLatencySelectionPolicy,
		Requested: 1, Admitted: 1, MaxFiles: projectionMaxFiles, MaxBytes: projectionMaxBytes,
		ChargedBytes: 40, IndexedFiles: 12, NativeSymbols: 20, EligibleCount: 8,
		SelectedNodes: 1, SelectedFuncs: 1, InputsVerified: true,
		Entries: []projectionLatencyCandidate{{Path: "a/one.go", ChargedBytes: 40, RawSHA256: strings.Repeat("a", 64), SourceBytes: 20, Nodes: 1, Functions: 1}},
	}
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	for _, test := range []struct{ name, digest, encoding string }{
		{"missing", "", ""},
		{"invalid_hash", "unknown", "UTF-8 sorted IDs joined by LF with terminal LF"},
		{"different_domain", strings.Repeat("a", 64), "JSON encoded IDs"},
		{"valid", strings.Repeat("a", 64), "UTF-8 sorted IDs joined by LF with terminal LF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields["native_symbols_sha256"] = test.digest
			fields["native_symbols_encoding"] = test.encoding
			raw, marshalErr := json.Marshal(fields)
			require.NoError(t, marshalErr)
			var candidate projectionLatencyPopulationManifest
			require.NoError(t, json.Unmarshal(raw, &candidate))
			err := validateProjectionLatencyPopulationManifest(candidate, candidate.Revision, 1)
			if test.name == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a scalar census cannot certify exact native membership")
			}
		})
	}
}

func TestProjectionLatencyNativeMembershipUsesExactGenerationRows(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ids := []string{"z", "é", "A"}
	for _, id := range ids {
		store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		require.NoError(t, store.UpsertSymbolFTS(id, id))
	}
	other := store.AtGeneration(7)
	other.AddNode(&graph.Node{ID: "foreign", Kind: graph.KindFunction, Name: "foreign"})
	require.NoError(t, other.UpsertSymbolFTS("foreign", "foreign"))
	count, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, 3, count)
	digest, err := projectionLatencyNativeMembership(t.Context(), store, count)
	require.NoError(t, err)
	expected := sha256.Sum256([]byte("A\nz\né\n"))
	require.Equal(t, hex.EncodeToString(expected[:]), digest, "UTF-8 IDs sorted by bytes with terminal LF, not JSON/token/other-generation rows")
	manifest := projectionLatencyPopulationManifest{IndexedFiles: 2, NativeSymbols: count, NativeSymbolsSHA256: digest}
	require.NoError(t, validateProjectionLatencyNativeCensus(manifest, 2, count, digest))
	require.Error(t, validateProjectionLatencyNativeCensus(manifest, 3, count, digest))
	require.Error(t, validateProjectionLatencyNativeCensus(manifest, 2, count+1, digest))
	require.Error(t, validateProjectionLatencyNativeCensus(manifest, 2, count, strings.Repeat("a", 64)), "same count does not credit changed membership")
	_, err = projectionLatencyNativeMembership(t.Context(), store, count+1)
	require.Error(t, err, "enumerated ownership count must match scalar")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = projectionLatencyNativeMembership(ctx, store, count)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, store.Close())
	_, err = projectionLatencyNativeMembership(t.Context(), store, count)
	require.Error(t, err, "a failed census cannot become a digest")
}

func TestProjectionLatencyNativeMembershipRetainsNewlineIDs(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, id := range []string{"a\nb", "c"} {
		store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		require.NoError(t, store.UpsertSymbolFTS(id, "symbol"))
	}
	count, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, 2, count)
	digest, err := projectionLatencyNativeMembership(t.Context(), store, count)
	require.NoError(t, err, "opaque ownership IDs containing LF must not be rejected or filtered")
	require.Len(t, digest, 64)
}
