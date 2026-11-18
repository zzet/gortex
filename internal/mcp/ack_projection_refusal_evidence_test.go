package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"weak"

	"github.com/stretchr/testify/require"
)

// The diagnostic must describe the guard evaluated while the original ACK
// was materialized. It cannot obtain authority or resample a later owner.
func TestAcknowledgementProjectionRefusalEvidence(t *testing.T) {
	for _, flag := range []string{"", "0", "true", "1"} {
		for _, mode := range []string{"missing_entry", "suspect_before", "owner_missing", "owner_replaced", "stat_error", "stamp_changed", "symlink", "directory", "tombstone_present"} {
			t.Run(flag+"/"+mode, func(t *testing.T) {
				t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", flag)
				selectedProjectionRegistry(t)
				root := t.TempDir()
				path := filepath.Join(root, "proof.go")
				raw := []byte("package proof\n")
				require.NoError(t, os.WriteFile(path, raw, 0600))
				if mode == "stat_error" {
					path += "\x00" // Rejected by OS path conversion before any lookup.
				}
				if mode == "symlink" {
					target := filepath.Join(root, "target.go")
					require.NoError(t, os.Rename(path, target))
					if err := os.Symlink(target, path); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				if mode == "directory" {
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.Mkdir(path, 0700))
				}
				stamp, stampErr := statDiskStamp(path)
				if mode == "stat_error" {
					require.Error(t, stampErr)
				} else {
					require.NoError(t, stampErr)
				}
				entry := syntheticProjectionEntry(path, "proof.go", "proof", 7, stamp, raw)
				if mode == "tombstone_present" {
					entry.tombstone = true
				}
				require.NoError(t, pendingFileProjections.publish(entry))
				guard, owner, suspect := mode, "not_observed", "clear"
				ownerCheck, suspectCheck := "before", "before"
				switch mode {
				case "missing_entry":
					pendingFileProjections.drop(entry)
					entry = nil
					suspect = "not_observed"
					ownerCheck, suspectCheck = "not_observed", "not_observed"
				case "suspect_before":
					entry.suspect.Store(true)
					suspect = "suspect"
					ownerCheck = "not_observed"
				case "owner_missing":
					pendingFileProjections.drop(entry)
					guard, owner = "owner_before", "missing"
				case "owner_replaced":
					next := syntheticProjectionEntry(path, "proof.go", "proof", 8, stamp, raw)
					require.NoError(t, pendingFileProjections.publish(next))
					guard, owner = "owner_before", "replaced"
				case "stamp_changed":
					require.NoError(t, os.WriteFile(path, []byte("package changed\n"), 0600))
					owner = "same"
				case "symlink", "directory":
					guard, owner = "not_regular", "same"
				case "tombstone_present":
					owner = "same"
				case "stat_error":
					owner = "same"
				}
				record := &mutationCommitRecord{disk: mutationDiskCommitted, commitSeq: 7, readbackExact: true, fileProjection: weak.Make(entry)}
				response := map[string]any{}
				record.attachFileProjection(response)
				if entry != nil {
					entry.suspect.Store(false) // A later event state cannot rewrite the first observation.
				}
				encoded, err := json.Marshal(response)
				require.NoError(t, err)
				var decoded map[string]any
				require.NoError(t, json.Unmarshal(encoded, &decoded))
				require.Equal(t, false, decoded["readback_exact"])
				view := decoded["file_view"].(map[string]any)
				require.Equal(t, "unavailable", view["state"])
				require.Equal(t, "projection_not_live", view["reason"])
				require.Equal(t, true, view["historical"])
				if flag != "1" {
					require.NotContains(t, decoded, "file_liveness_failure")
					return
				}
				evidence, ok := decoded["file_liveness_failure"].(map[string]any)
				require.True(t, ok, "original refusing ACK must expose the opt-in first predicate")
				require.Equal(t, guard, evidence["guard"])
				require.Equal(t, owner, evidence["owner"])
				require.Equal(t, suspect, evidence["suspect"])
				require.Equal(t, ownerCheck, evidence["owner_check"])
				require.Equal(t, suspectCheck, evidence["suspect_check"])
				if mode == "owner_replaced" {
					require.Equal(t, float64(8), evidence["owner_commit_seq"])
				}
				proof, err := json.Marshal(evidence)
				require.NoError(t, err)
				require.LessOrEqual(t, len(proof), 512)
				require.NotContains(t, string(proof), root)
				t.Logf("ACK_LIVENESS_CAUSAL mode=%s guard=%s owner=%s suspect=%s bytes=%d", mode, guard, owner, suspect, len(proof))
			})
		}
	}
}

func TestAcknowledgementProjectionEvidencePreservesLiveResponse(t *testing.T) {
	selectedProjectionRegistry(t)
	path := filepath.Join(t.TempDir(), "proof.go")
	raw := []byte("package proof\n")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	entry := syntheticProjectionEntry(path, "proof.go", "proof", 7, stamp, raw)
	require.NoError(t, pendingFileProjections.publish(entry))
	record := &mutationCommitRecord{disk: mutationDiskCommitted, commitSeq: 7, readbackExact: true, fileProjection: weak.Make(entry)}
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "")
	before := map[string]any{}
	record.attachFileProjection(before)
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	after := map[string]any{}
	record.attachFileProjection(after)
	require.Equal(t, before, after, "opt-in diagnostic must not alter a live ACK")
	require.Equal(t, true, after["readback_exact"])
	require.NotContains(t, after, "file_liveness_failure")
}

func TestAcknowledgementProjectionEvidenceKeepsFirstSnapshot(t *testing.T) {
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	selectedProjectionRegistry(t)
	path := filepath.Join(t.TempDir(), "proof.go")
	raw := []byte("package proof\n")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	entry := syntheticProjectionEntry(path, "proof.go", "proof", 7, stamp, raw)
	require.NoError(t, pendingFileProjections.publish(entry))
	entry.suspect.Store(true)
	record := &mutationCommitRecord{disk: mutationDiskCommitted, commitSeq: 7, readbackExact: true, fileProjection: weak.Make(entry)}
	response := map[string]any{}
	record.attachFileProjection(response)
	first, err := json.Marshal(response["file_liveness_failure"])
	require.NoError(t, err)
	require.NotEqual(t, "null", string(first))
	var firstFields map[string]any
	require.NoError(t, json.Unmarshal(first, &firstFields))
	require.Equal(t, "not_observed", firstFields["owner_check"], "suspect-before short-circuits before any owner load")
	require.Equal(t, "before", firstFields["suspect_check"])
	entry.suspect.Store(false)
	pendingFileProjections.drop(entry)
	record.attachFileProjection(response)
	later, err := json.Marshal(response["file_liveness_failure"])
	require.NoError(t, err)
	require.JSONEq(t, string(first), string(later), "later materialization must preserve the first recorded predicate")
	require.Equal(t, false, response["readback_exact"])
}
