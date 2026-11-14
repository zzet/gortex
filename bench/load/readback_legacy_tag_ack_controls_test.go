//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func legacyTagControlBinding(t *testing.T) (readbackReferencePlan, readbackReferenceSchedule) {
	t.Helper()
	p := readbackReferencePlan{Version: 1, BinaryRevision: v06410TagRevision, BinarySHA256: v06410PublishedBinarySHA, Seed: 1, EditMeanNS: 15000000000, ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30, ToolSurface: readbackReferenceCompatibilitySurface}
	schedules, err := readbackReferenceSchedules(p)
	require.NoError(t, err)
	return p, schedules[72]
}

func TestLegacyTagACKSelectsOnlyBoundOriginalDefaultCell72(t *testing.T) {
	p, s := legacyTagControlBinding(t)
	metadata, decoder, err := bindV06410ACKContract(p, s, "under_edit")
	require.NoError(t, err)
	require.NotNil(t, decoder)
	require.Equal(t, "legacy_default_unavailable", metadata.Policy)
	require.Equal(t, 72, metadata.CellIndex)
	require.Equal(t, "add_crossfile_call", metadata.Kind)
	require.Equal(t, "51e84c3e133d530f21661623ec86633b741a3c75507ec3304b1f14a8a6d5c6be", metadata.ArtifactSHA256)
}
func TestLegacyTagACKRefusesWrongSourceArtifactOrPolicy(t *testing.T) {
	for _, name := range []string{"wrong_revision", "wrong_binary_hash", "rebuilt_source_artifact", "local", "other_kind", "other_role", "wrong_seed", "short_schedule", "wrong_warm_bound", "wrong_surface", "changed_tail"} {
		t.Run(name, func(t *testing.T) {
			p, s := legacyTagControlBinding(t)
			role := "under_edit"
			switch name {
			case "wrong_revision":
				p.BinaryRevision = strings.Repeat("c", 40)
			case "wrong_binary_hash":
				p.BinarySHA256 = strings.Repeat("c", 64)
			case "rebuilt_source_artifact":
				p.BinarySHA256 = "722d4a4a16d3a858a96677cb03d50254c80fb9c3c09f03a976a39ee5159e238f"
			case "local":
				s.Cell.ACKMode = "local"
			case "other_kind":
				s.Cell.Kind = "relocate"
				s.Cell.Tool = "refactor.move"
			case "other_role":
				role = "protocol_preflight"
			case "wrong_seed":
				p.Seed = 2
			case "short_schedule":
				s.Arrival = s.Arrival[:1]
			case "wrong_warm_bound":
				p.WarmSeconds = 1801
			case "wrong_surface":
				p.ToolSurface = readbackReferenceFacadeSurface
			case "changed_tail":
				s.OfferedTailNS++
			}
			_, _, err := bindV06410ACKContract(p, s, role)
			require.Error(t, err)
		})
	}
}
func legacyOriginalACKFixture(t *testing.T) (privateRepo, readbackReferenceTrial, map[string]any) {
	t.Helper()
	cell := readbackReferenceCell{Agents: 32, Offers: 100, Kind: "add_crossfile_call", Tool: "edit.file"}
	trial, err := readbackReferenceTrialFor(cell, editProbeArrival{Editor: 0}, 0)
	require.NoError(t, err)
	image := trial.After[trial.ProbeFile]
	body := map[string]any{"path": "owned/" + trial.ProbeFile, "status": "applied", "disk_status": "committed", "new_sha": v06410GitBlobSHA(image.Bytes), "replacements": json.Number("1"), "bytes_written": json.Number(strconv.Itoa(len(image.Bytes))), "graph_status": "pending", "reindex_pending": true, "reindexed": false}
	return privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, body
}
func TestLegacyTagACKChecksOriginalGitBlobWithoutInventingModernFields(t *testing.T) {
	repo, trial, body := legacyOriginalACKFixture(t)
	mode, evidence, err := decodeV06410OriginalACK(body, repo, trial)
	require.NoError(t, err)
	require.Empty(t, mode)
	require.Equal(t, v06410LegacyPolicyEvidence, evidence)
	require.NotContains(t, body, "ack_mode")
	require.NotContains(t, body, "content_sha256")
	_, _, currentErr := readbackReferenceACKPolicy(body, trial)
	require.Error(t, currentErr, "default current policy remains strict")
	require.Error(t, readbackReferenceACKSource(body, repo, trial), "Gitblob is not producerSHA256")
}
func TestLegacyTagACKRejectsNoopWrongTargetAndUnsupportedShapes(t *testing.T) {
	for _, name := range []string{"noop", "wrong_path", "wrong_sha", "missing_sha", "dry_run", "refused", "failed", "unknown_graph", "reindex_error", "modern_mode", "modern_sha", "wrong_count", "short_bytes", "unsupported_kind", "false_fresh"} {
		t.Run(name, func(t *testing.T) {
			repo, trial, body := legacyOriginalACKFixture(t)
			switch name {
			case "noop":
				body["new_sha"] = v06410GitBlobSHA(trial.Before[trial.ProbeFile].Bytes)
			case "wrong_path":
				body["path"] = "foreign/" + trial.ProbeFile
			case "wrong_sha":
				body["new_sha"] = strings.Repeat("0", 40)
			case "missing_sha":
				delete(body, "new_sha")
			case "dry_run":
				body["dry_run"] = true
			case "refused":
				body["status"] = "refused"
			case "failed":
				body["graph_status"] = "failed"
			case "unknown_graph":
				body["graph_status"] = "unknown"
			case "reindex_error":
				body["reindex_error"] = "actual failure"
			case "modern_mode":
				body["ack_mode"] = "full"
			case "modern_sha":
				body["content_sha256"] = readbackReferenceSHA(trial.After[trial.ProbeFile].Bytes)
			case "wrong_count":
				body["replacements"] = json.Number("0")
			case "short_bytes":
				body["bytes_written"] = json.Number("1")
			case "unsupported_kind":
				trial.Kind = "relocate"
			case "false_fresh":
				body["graph_status"] = "fresh"
			}
			_, _, err := decodeV06410OriginalACK(body, repo, trial)
			require.Error(t, err)
		})
	}
}

func TestLegacyTagACKDefaultSelectorDoesNotAlterCurrentPath(t *testing.T) {
	require.False(t, *readbackLegacyTagACK)
	raw, decoder, err := selectedV06410ACKContract(readbackReferencePlan{}, readbackReferenceSchedule{}, "unknown")
	require.NoError(t, err)
	require.Empty(t, raw)
	require.Nil(t, decoder)
}

type legacyACKOracleCaller struct {
	mu     sync.Mutex
	next   int64
	names  []string
	ack    map[string]any
	bodies map[string]map[string]any
}

func (c *legacyACKOracleCaller) callWithIdentity(_ context.Context, _ string, args any) (json.RawMessage, wireCallIdentity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := args.(map[string]any)["name"].(string)
	c.names = append(c.names, name)
	c.next++
	now := time.Now()
	id := wireCallIdentity{ID: c.next, Started: now, WriteStarted: now, WriteFinished: now, ReplyDelivered: now, Finished: now}
	if name == "edit_file" {
		return readbackReferenceWire(c.ack), id, nil
	}
	return readbackReferenceWire(c.bodies[name]), id, nil
}
func legacyCompleteReadBodies(trial readbackReferenceTrial, repo privateRepo) map[string]map[string]any {
	image := trial.After[trial.ProbeFile]
	target := image.Symbols[trial.Target]
	names := make([]string, 0, len(image.Symbols))
	for name := range image.Symbols {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := []any{}
	var targetNode any
	for _, name := range names {
		symbol := image.Symbols[name]
		node := map[string]any{"id": repo.Prefix + "/" + image.File + "::" + name, "name": name, "kind": "function", "file_path": image.File, "start_line": symbol.StartLine, "end_line": symbol.EndLine}
		rows = append(rows, node)
		if name == trial.Target {
			targetNode = node
		}
	}
	return map[string]map[string]any{
		"get_symbol_source":   {"id": repo.Prefix + "/" + image.File + "::" + trial.Target, "name": trial.Target, "file_path": image.File, "source": target.Body, "start_line": target.StartLine, "end_line": target.EndLine},
		"get_file_summary":    {"nodes": rows, "total_nodes": len(rows), "truncated": false},
		"get_editing_context": {"file": map[string]any{"id": repo.Prefix + "/" + image.File}, "defines": rows, "graph_freshness": "pending", "reason": "store_busy"},
		"read_file":           {"path": image.File, "content": image.Bytes},
		"get_symbol":          {"node": targetNode, "in_edges": []any{}, "out_edges": []any{}},
	}
}
func TestLegacyTagACKKeepsEveryOriginalReadOracleAndDeadline(t *testing.T) {
	for _, broken := range []string{"", "get_symbol_source", "get_file_summary", "get_editing_context", "read_file", "get_symbol", "deadline"} {
		name := broken
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			repo, trial, body := legacyOriginalACKFixture(t)
			caller := &legacyACKOracleCaller{ack: body, bodies: legacyCompleteReadBodies(trial, repo)}
			if broken != "" && broken != "deadline" {
				caller.bodies[broken] = map[string]any{"state": "pending", "complete": false}
			}
			ctx := t.Context()
			if broken == "deadline" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			row := observeReadbackReferenceSurfaceWithACKDecoder(ctx, caller, repo, trial, "under_edit", time.Second, true, decodeV06410OriginalACK)
			require.Equal(t, broken == "", row.Correct, "%+v", row.Errors)
			require.Len(t, caller.names, 6, "one original mutation plus exactly5original reads, no retry")
			require.Equal(t, "edit_file", caller.names[0])
			require.Empty(t, row.ObservedACKMode)
			require.Equal(t, v06410LegacyPolicyEvidence, row.ACKModeEvidence)
			require.Len(t, row.Probes, 5)
			if broken == "deadline" {
				for _, probe := range row.Probes {
					require.NotEmpty(t, probe.Error)
				}
			}
		})
	}
}
