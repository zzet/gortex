//go:build acceptance

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
