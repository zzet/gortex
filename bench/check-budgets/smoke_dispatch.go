package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Manual measurements retain their real dispatch identity. They never borrow a
// synthetic PR ref or silently fall back to another revision or artifact.
type smokeDispatchPins struct {
	WorkflowRef            string `json:"workflow_ref"`
	WorkflowRevision       string `json:"workflow_revision"`
	BaselineRevision       string `json:"baseline_revision"`
	BaselineArtifactID     int64  `json:"baseline_artifact_id"`
	BaselineArtifactDigest string `json:"baseline_artifact_digest"`
	CandidateRevision      string `json:"candidate_revision"`
}

func currentSmokeDispatchPins(event string) smokeDispatchPins {
	if event != "workflow_dispatch" || os.Getenv("GITHUB_JOB") != "smoke" {
		return smokeDispatchPins{}
	}
	id, _ := strconv.ParseInt(os.Getenv("PERF_SMOKE_BASELINE_ARTIFACT_ID"), 10, 64)
	return smokeDispatchPins{WorkflowRef: os.Getenv("GITHUB_WORKFLOW_REF"), WorkflowRevision: os.Getenv("GITHUB_WORKFLOW_SHA"), BaselineRevision: os.Getenv("PERF_SMOKE_BASELINE_REVISION"), BaselineArtifactID: id, BaselineArtifactDigest: os.Getenv("PERF_SMOKE_BASELINE_ARTIFACT_DIGEST"), CandidateRevision: os.Getenv("PERF_SMOKE_CANDIDATE_REVISION")}
}

func validSmokeDispatchWorkflow(workflow workflowProvenance) bool {
	pins := workflow.SmokeDispatch
	return workflow.Job == "smoke" && strings.HasPrefix(workflow.RunURL, "https://github.com/zzet/gortex/actions/runs/") && workflow.Ref == "refs/heads/main" && pins != (smokeDispatchPins{}) && pins.WorkflowRef == "zzet/gortex/.github/workflows/perf-gate.yml@refs/heads/main" && pins.WorkflowRevision == workflow.Revision && pins.CandidateRevision == workflow.Revision && revisionPattern.MatchString(pins.BaselineRevision) && pins.BaselineArtifactID > 0 && validDigest(pins.BaselineArtifactDigest)
}

func validateSmokeDispatchBinding(workflow workflowProvenance, proof smokeProvenance) error {
	if workflow.Event != "workflow_dispatch" {
		return nil
	}
	pins := workflow.SmokeDispatch
	if !validSmokeDispatchWorkflow(workflow) || pins.BaselineRevision != proof.Main.Revision || pins.BaselineArtifactID != proof.Main.Artifact.ID || pins.BaselineArtifactDigest != proof.Main.Artifact.Digest || pins.CandidateRevision != proof.CandidateBinary.Revision || !planPattern.MatchString(proof.CandidateBinary.BinarySHA256) {
		return errors.New("manual smoke role source/artifact facts differ from explicit trusted-main pins")
	}
	return nil
}
