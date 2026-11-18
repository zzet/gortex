package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Bind the ACK to the SAME exact selected-checkout path sent by the supported
// whole-connection compatibility adapter. Basenames/suffixes cannot authorize
// a different checkout or file, even if offered bytes match.
func TestReadbackBatchACKMatchesQualifiedCompatibilityPath(t *testing.T) {
	repo := privateRepo{Path: t.TempDir(), Prefix: "selected"}
	for _, kind := range []string{"modify_caller_body", "change_signature"} {
		t.Run(kind, func(t *testing.T) {
			trial, err := readbackReferenceTrialFor(readbackReferenceCell{Agents: 1, Kind: kind, Tool: "edit.batch", Offers: 100}, editProbeArrival{}, 0)
			require.NoError(t, err)
			params, err := readbackReferenceCompatibilityParams(trial.Request, repo)
			require.NoError(t, err)
			require.Equal(t, "batch_edit", params["name"])
			edit := params["arguments"].(map[string]any)["edits"].([]any)[0].(map[string]any)
			wirePath := edit["path"].(string)
			require.Equal(t, repo.Prefix+"/"+trial.ProbeFile, wirePath)
			for _, fault := range []string{"matched", "missing_prefix", "foreign_prefix", "foreign_result", "foreign_absolute", "wrong_after_hash", "wrong_transaction"} {
				t.Run(fault, func(t *testing.T) {
					file := map[string]any{"path": filepath.Join(repo.Path, trial.ProbeFile), "relative_path": wirePath, "after_sha256": readbackReferenceSHA(trial.After[trial.ProbeFile].Bytes)}
					result := map[string]any{"op": "edit_file", "path": wirePath, "status": "applied"}
					body := map[string]any{"transaction_id": trial.TransactionID, "status": "committed", "disk_status": "committed", "files": []any{file}, "results": []any{result}}
					switch fault {
					case "missing_prefix":
						file["relative_path"], result["path"] = trial.ProbeFile, trial.ProbeFile
					case "foreign_prefix":
						file["relative_path"], result["path"] = "foreign/"+trial.ProbeFile, "foreign/"+trial.ProbeFile
					case "foreign_result":
						result["path"] = "foreign/" + trial.ProbeFile
					case "foreign_absolute":
						file["path"] = filepath.Join(repo.Path, "foreign.go")
					case "wrong_after_hash":
						file["after_sha256"] = readbackReferenceSHA("other offered bytes")
					case "wrong_transaction":
						body["transaction_id"] = "foreign-transaction"
					}
					err := readbackReferenceACKSource(body, repo, trial)
					if fault == "matched" {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				})
			}
		})
	}
}
