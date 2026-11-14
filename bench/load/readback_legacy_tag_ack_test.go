//go:build acceptance

package main

import (
	"crypto/sha1" // Git's original blob ID, not a security or producer-SHA256 substitute.
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"
)

const v06410TagRevision = "defa9f8e7a670792b5b693744d88488f741a8160"
const v06410TagTree = "655071eb061f2647bd819ef30e0f42116f1dfcbf"
const v06410PublishedArchiveSHA = "d36835c90ccdfcb6546757602bd82b520ce8d98593eda460faebdbe660d301a2"
const v06410PublishedBinarySHA = "51e84c3e133d530f21661623ec86633b741a3c75507ec3304b1f14a8a6d5c6be"
const v06410ArtifactAuthoritySHA = "7db2df96bf5144ed940c9797136c16003b1dd12e152e8f1899ddcf0ab969b408"
const v06410LegacyPolicyEvidence = "legacy_default_unavailable: original v0.64.10 ACK has no ack_mode/content_sha256; emitted Git-blob new_sha verified"

var readbackLegacyTagACK = flag.Bool("readback-reference-v06410-original-ack", false, "decode only the authenticated published v0.64.10 ARM64 original ACK for default cell72; never a current-producer policy override")

type v06410ACKContract struct {
	Version                 int    `json:"version"`
	SourceRevision          string `json:"source_revision"`
	SourceTree              string `json:"source_tree"`
	ArtifactSHA256          string `json:"artifact_sha256"`
	ArchiveSHA256           string `json:"archive_sha256"`
	ArtifactAuthoritySHA256 string `json:"artifact_authority_sha256"`
	AssetID                 int    `json:"asset_id"`
	CellIndex               int    `json:"cell_index"`
	Kind                    string `json:"kind"`
	Policy                  string `json:"policy"`
}

func bindV06410ACKContract(plan readbackReferencePlan, schedule readbackReferenceSchedule, role string) (v06410ACKContract, readbackReferenceACKDecoder, error) {
	empty := v06410ACKContract{}
	expected := readbackReferenceCell{Agents: 32, Offers: 100, Kind: "add_crossfile_call", Tool: "edit.file"}
	if role != "under_edit" || schedule.Cell != expected || len(schedule.Arrival) != 100 || plan.Version != 1 || plan.Language != "" || plan.Seed != 1 || plan.EditMeanNS != 15000000000 || plan.ProductDeadlineSeconds != 120 || plan.WarmSeconds != 1800 || plan.StopSeconds != 30 || plan.ToolSurface != readbackReferenceCompatibilitySurface {
		return empty, nil, errors.New("legacy baseline supports only original Go90 cell72/default32/N100/seed1 and unchanged policy")
	}
	if schedule.OfferedTailNS != schedule.Arrival[len(schedule.Arrival)-1].DueNS || schedule.PhaseBoundNS != schedule.OfferedTailNS+int64(240*time.Second) {
		return empty, nil, errors.New("legacy baseline schedule/tail bounds changed")
	}
	if plan.BinaryRevision != v06410TagRevision || plan.BinarySHA256 != v06410PublishedBinarySHA {
		return empty, nil, errors.New("legacy original ACK requires the actual authenticated published tag artifact")
	}
	// Existing native preflight checks the actual external plan.Binary bytes and
	// embedded revision. The selected published member is authenticated by archive
	// d368... and the immutable host acquisition/closure authority7db2...; no
	// per-run self-asserted role JSON or modern projection-off surrogate is used.
	metadata := v06410ACKContract{Version: 1, SourceRevision: v06410TagRevision, SourceTree: v06410TagTree, ArtifactSHA256: v06410PublishedBinarySHA, ArchiveSHA256: v06410PublishedArchiveSHA, ArtifactAuthoritySHA256: v06410ArtifactAuthoritySHA, AssetID: 669760344, CellIndex: 72, Kind: expected.Kind, Policy: "legacy_default_unavailable"}
	return metadata, decodeV06410OriginalACK, nil
}

func selectedV06410ACKContract(plan readbackReferencePlan, schedule readbackReferenceSchedule, role string) (json.RawMessage, readbackReferenceACKDecoder, error) {
	if !*readbackLegacyTagACK {
		return nil, nil, nil
	}
	if *readbackReferenceCellIndex != 72 {
		return nil, nil, errors.New("legacy original ACK requires exact source-selected cell72")
	}
	metadata, decoder, err := bindV06410ACKContract(plan, schedule, role)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(metadata)
	return encoded, decoder, err
}

func v06410GitBlobSHA(source string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(source), source)))
	return hex.EncodeToString(sum[:])
}
func legacyACKCount(value any, expected int) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	n, err := number.Int64()
	return err == nil && n == int64(expected)
}
func decodeV06410OriginalACK(body map[string]any, repo privateRepo, trial readbackReferenceTrial) (string, string, error) {
	fail := func(reason string) (string, string, error) {
		return "", "legacy_default_unavailable", errors.New(reason)
	}
	if trial.Kind != "add_crossfile_call" || trial.Request.name() != "edit.file" || repo.Prefix == "" || trial.Absent {
		return fail("unsupported legacy original mutation shape")
	}
	before, beforeOK := trial.Before[trial.ProbeFile]
	after, afterOK := trial.After[trial.ProbeFile]
	if !beforeOK || !afterOK || before.Bytes == after.Bytes || before.Bytes == "" || after.Bytes == "" {
		return fail("legacy baseline is not a genuine differing before/after mutation")
	}
	params, err := readbackReferenceCompatibilityParams(trial.Request, repo)
	if err != nil || params["name"] != "edit_file" {
		return fail("legacy original request adapter unavailable")
	}
	args, ok := params["arguments"].(map[string]any)
	if !ok || args["path"] != repo.Prefix+"/"+trial.ProbeFile || args["old_string"] != before.Bytes || args["new_string"] != after.Bytes {
		return fail("legacy mutation request differs from independent fixture")
	}
	if _, present := body["ack_mode"]; present {
		return fail("legacy response unexpectedly claims modern ACK policy")
	}
	if _, present := body["content_sha256"]; present {
		return fail("legacy Gitblob response cannot borrow producer SHA256")
	}
	if body["path"] != args["path"] || body["status"] != "applied" || body["disk_status"] != "committed" || body["dry_run"] != nil && body["dry_run"] != false || !legacyACKCount(body["replacements"], 1) || !legacyACKCount(body["bytes_written"], len(after.Bytes)) {
		return fail("original legacy result/target/count is not the applied commit")
	}
	if body["new_sha"] != v06410GitBlobSHA(after.Bytes) || body["new_sha"] == v06410GitBlobSHA(before.Bytes) {
		return fail("original legacy Gitblob does not identify the actual after-image")
	}
	if body["reindex_error"] != nil && body["reindex_error"] != "" {
		return fail("original legacy ACK reports reindex failure")
	}
	switch body["graph_status"] {
	case "fresh":
		if body["reindexed"] != true || body["reindex_pending"] != nil && body["reindex_pending"] != false {
			return fail("legacy fresh state contradicts original indexing flags")
		}
	case "pending":
		if body["reindex_pending"] != true || body["reindexed"] != false {
			return fail("legacy pending state contradicts original indexing flags")
		}
	case "stale":
		if body["reindexed"] != false || body["reindex_pending"] != nil && body["reindex_pending"] != false {
			return fail("legacy stale state contradicts original indexing flags")
		}
	default:
		return fail("original legacy graph state failed or unknown")
	}
	// Full source/summary/context/file/metadata oracles and final disk checks are
	// unchanged. This ACK proves no modern ACKmode, producerSHA256 or P/B timing.
	return "", v06410LegacyPolicyEvidence, nil
}
