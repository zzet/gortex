package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditLoopSharedACKKeepsStrictIsolationAndEvidence(t *testing.T) {
	for _, mode := range []string{"shared", "strict_default", "linked", "unknown_scope", "single_writer", "missing_exact", "not_applied", "not_committed", "contradictory_exact", "foreign_mutation", "missing_commit", "missing_digest", "failed_edit", "capped_evidence", "incorrect_read"} {
		t.Run(mode, func(t *testing.T) {
			r := editLoopEvidenceFixture(t)
			var raw map[string]any
			if err := json.Unmarshal(r.Phases[0].EditProbe, &raw); err != nil {
				t.Fatal(err)
			}
			plan := raw["plan"].(map[string]any)
			plan["kind"], plan["tool"], plan["file"] = "add_intrafile_call", "edit.file", "sample.go"
			raw["physical_write_scope"] = map[string]any{"kind": "shared_worktree_file", "file": "sample.go", "editors": 4}
			records := raw["edits"].([]any)
			for i, item := range records {
				record := item.(map[string]any)
				record["editor"] = i % 4
				record["ack_readback_exact"] = false
				record["ack_status"], record["ack_disk_status"] = "applied", "committed"
				record["mutation_id"], record["ack_mutation_id"] = "mutation", "mutation"
				record["ack_commit_seq"], record["ack_content_sha256"] = i+1, strings.Repeat("a", 64)
			}
			first := records[0].(map[string]any)
			switch mode {
			case "strict_default":
				delete(raw, "physical_write_scope")
			case "linked":
				plan["linked_worktrees"] = 1
			case "unknown_scope":
				raw["physical_write_scope"].(map[string]any)["kind"] = "unknown"
			case "single_writer":
				for i, item := range records {
					item.(map[string]any)["editor"] = 0
					plan["steps"].([]any)[i].(map[string]any)["editor"] = 0
				}
			case "missing_exact":
				delete(first, "ack_readback_exact")
			case "not_applied":
				first["ack_status"] = "failed"
			case "not_committed":
				first["ack_disk_status"] = "pending"
			case "contradictory_exact":
				first["ack_readback_exact"], first["ack_disk_status"] = true, "pending"
			case "foreign_mutation":
				first["ack_mutation_id"] = "other"
			case "missing_commit":
				delete(first, "ack_commit_seq")
			case "missing_digest":
				delete(first, "ack_content_sha256")
			case "failed_edit":
				first["outcome"] = "diagnostic_failure"
			case "capped_evidence":
				first["outcome"] = "harness_evidence_incomplete"
			case "incorrect_read":
				first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = data
			value, err := measureEditLoopRow(r.Phases[0], "edit_ack_p95")
			wantACK := mode == "shared" || mode == "incorrect_read"
			if (err == nil) != wantACK || wantACK && value != .05 {
				t.Fatalf("ACK value=%v error=%v for %s", value, err, mode)
			}
			if mode == "incorrect_read" {
				correct, readErr := measureEditLoopRow(r.Phases[0], "readback_correct")
				if readErr != nil || correct >= 1 {
					t.Fatalf("shared ACK waived incorrect content: value=%v error=%v", correct, readErr)
				}
			}
		})
	}
}
