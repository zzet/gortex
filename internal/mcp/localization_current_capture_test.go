package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func captureTestRow(id, file string) localizationDigestRow {
	return localizationDigestRow{
		ID:         id,
		Name:       id,
		Kind:       "method",
		File:       file,
		Line:       17,
		Provenance: "capture_test",
	}
}

func TestMergeLocalizationEvidenceDigestReservesPriorWindowAndStaysAligned(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("repo/storage/base.go::Storage.Load", "repo/storage/base.go"),
		captureTestRow("repo/storage/cloud.go::CloudStorage.Load", "repo/storage/cloud.go"),
		captureTestRow("repo/storage/cache.go::Cache.Load", "repo/storage/cache.go"),
		captureTestRow("repo/storage/archive.go::Archive.Load", "repo/storage/archive.go"),
	}, nil)
	current := []localizationDigestRow{
		captureTestRow("repo/storage/disk.go::DiskStorage.Load", "repo/storage/disk.go"),
		captureTestRow("repo/storage/base.go::Storage.Load", "repo/storage/base.go"),
		captureTestRow("repo/storage/mirror.go::Mirror.Load", "repo/storage/mirror.go"),
	}
	merged := mergeLocalizationEvidenceDigest(current, retained)
	if merged == nil {
		t.Fatal("merge returned nil digest")
	}
	if got, want := len(merged.Evidence), 6; got != want {
		t.Fatalf("evidence count = %d, want %d", got, want)
	}
	// The whole current page fits inside the fresh-evidence reserve
	// (localizationFreshEvidenceReserve rows), so every current row leads —
	// base.go dedup-merges into its retained twin — and the retained window
	// follows in its original order.
	wantIDs := []string{
		"repo/storage/disk.go::DiskStorage.Load",
		"repo/storage/base.go::Storage.Load",
		"repo/storage/mirror.go::Mirror.Load",
		"repo/storage/cloud.go::CloudStorage.Load",
		"repo/storage/cache.go::Cache.Load",
		"repo/storage/archive.go::Archive.Load",
	}
	for index, want := range wantIDs {
		row := merged.Evidence[index]
		if row.ID != want || merged.Symbols[index] != want {
			t.Fatalf("position %d IDs = row %q symbols %q, want %q", index+1, row.ID, merged.Symbols[index], want)
		}
		if merged.Files[index] != row.File {
			t.Fatalf("position %d file = %q, evidence file %q", index+1, merged.Files[index], row.File)
		}
		if row.Rank != index+1 {
			t.Fatalf("position %d rank = %d", index+1, row.Rank)
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("encoded digest = %d bytes, cap %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(merged.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("final response = %d bytes, cap %d", len(merged.finalResponse), localizationFinalResponseMaxBytes)
	}
}

func TestLocalizationTypedCaptureIsTokenPrivateAndDefensivelyCopied(t *testing.T) {
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 41)
	input := []localizationDigestRow{captureTestRow("repo/storage/disk.go::DiskStorage.Load", "repo/storage/disk.go")}
	captureLocalizationRows(ctx, input)
	input[0].ID = "mutated"

	rows, recorded := localizationCapturedEvidence(ctx, 41)
	if !recorded || len(rows) != 1 {
		t.Fatalf("capture = %#v, recorded %v", rows, recorded)
	}
	if rows[0].ID != "repo/storage/disk.go::DiskStorage.Load" {
		t.Fatalf("captured ID = %q", rows[0].ID)
	}
	rows[0].ID = "mutated-copy"
	again, recorded := localizationCapturedEvidence(ctx, 41)
	if !recorded || again[0].ID != "repo/storage/disk.go::DiskStorage.Load" {
		t.Fatalf("capture was not defensively copied: %#v", again)
	}
	if _, recorded := localizationCapturedEvidence(ctx, 42); recorded {
		t.Fatal("mismatched reservation token read captured evidence")
	}
	if _, recorded := localizationCapturedEvidence(context.Background(), 41); recorded {
		t.Fatal("unreserved context exposed captured evidence")
	}
	zero := withLocalizationPermittedEvidenceCapture(context.Background(), 0)
	captureLocalizationRows(zero, []localizationDigestRow{captureTestRow("repo/storage/cloud.go::CloudStorage.Load", "repo/storage/cloud.go")})
	if _, recorded := localizationCapturedEvidence(zero, 1); recorded {
		t.Fatal("zero reservation installed a capture sink")
	}
}

func TestLocalizationReadSourceCaptureUsesValidatedTypedNode(t *testing.T) {
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 7)
	node := &graph.Node{
		ID:        "repo/storage/disk.go::DiskStorage.Load",
		Name:      "Load",
		QualName:  "DiskStorage.Load",
		Kind:      graph.KindMethod,
		FilePath:  "repo/storage/disk.go",
		StartLine: 23,
		Meta:      map[string]any{"signature": "func (d *DiskStorage) Load() error"},
	}
	captureLocalizationReadSource(ctx, node)
	rows, recorded := localizationEvidenceForPermittedCall(ctx, "read", "source", 7)
	if !recorded || len(rows) != 1 {
		t.Fatalf("typed source capture = %#v, recorded %v", rows, recorded)
	}
	if rows[0].ID != node.ID || rows[0].Line != node.StartLine || rows[0].Provenance != "permitted_read_source" {
		t.Fatalf("typed source row = %#v", rows[0])
	}
	if _, recorded := localizationEvidenceForPermittedCall(ctx, "read", "file", 7); recorded {
		t.Fatal("read.file consumed a reserved evidence sink")
	}
}

func TestLocalizationCaptureContextsRemainIsolatedConcurrently(t *testing.T) {
	const workers = 32
	var wg sync.WaitGroup
	errors := make(chan string, workers)
	for index := 1; index <= workers; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := uint64(index)
			id := fmt.Sprintf("repo/storage/disk_%d.go::DiskStorage.Load", index)
			ctx := withLocalizationPermittedEvidenceCapture(context.Background(), token)
			captureLocalizationRows(ctx, []localizationDigestRow{captureTestRow(id, fmt.Sprintf("repo/storage/disk_%d.go", index))})
			rows, recorded := localizationCapturedEvidence(ctx, token)
			if !recorded || len(rows) != 1 || rows[0].ID != id {
				errors <- fmt.Sprintf("token %d capture = %#v recorded=%v", token, rows, recorded)
			}
			if _, recorded := localizationCapturedEvidence(ctx, token+workers); recorded {
				errors <- fmt.Sprintf("token %d accepted mismatched token", token)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestFinishReservedReadWithDigestMergesOnlyOnAnswerReady(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("repo/storage/base.go::Storage.Load", "repo/storage/base.go"),
	}, nil)
	state := &localizationTerminalState{
		state:                localizationStateExactReadInFlight,
		exactSymbol:          "repo/storage/disk.go::DiskStorage.Load",
		generation:           3,
		readReservationToken: 11,
		readReservationGen:   3,
		digest:               retained,
	}
	exact := []localizationDigestRow{captureTestRow("repo/storage/disk.go::DiskStorage.Load", "repo/storage/disk.go")}
	completion := state.finishReservedReadTokenWithDigest(11, true, exact, true)
	if completion.State != localizationStateNeedsRecovery {
		t.Fatalf("exact completion state = %q", completion.State)
	}
	if len(state.digest.Evidence) != 1 || state.digest.Evidence[0].ID != "repo/storage/base.go::Storage.Load" {
		t.Fatalf("preterminal digest was changed: %#v", state.digest)
	}

	state.state = localizationStateRecoveryInFlight
	state.readReservationToken = 12
	state.readReservationGen = state.generation
	state.recoveryRetriesRemaining = 1
	recovery := []localizationDigestRow{captureTestRow("repo/storage/cloud.go::CloudStorage.Load", "repo/storage/cloud.go")}
	completion = state.finishReservedReadTokenWithDigest(12, true, recovery, true)
	if completion.State != localizationStateAnswerReady {
		t.Fatalf("recovery completion state = %q", completion.State)
	}
	if len(state.digest.Evidence) != 2 || state.digest.Evidence[0].ID != recovery[0].ID || state.digest.Evidence[1].ID != retained.Evidence[0].ID {
		t.Fatalf("terminal digest is not current-first: %#v", state.digest)
	}
	if completion.digest != state.digest {
		t.Fatal("terminal completion did not publish merged digest")
	}
}

func TestFinishReservedReadWithDigestEmptySuccessSpendsOneAllowanceAndPreservesCurrentTaskDigest(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("repo/storage/base.go::Storage.Load", "repo/storage/base.go"),
	}, nil)
	state := &localizationTerminalState{
		state:                       localizationStateRecoveryInFlight,
		generation:                  5,
		readReservationToken:        21,
		readReservationGen:          5,
		recoveryRetriesRemaining:    1,
		recoveryAllowancesRemaining: localizationRecoveryAllowanceCap,
		digest:                      retained,
	}
	completion := state.finishReservedReadTokenWithDigest(21, true, nil, true)
	if completion.State != localizationStateNeedsRecovery || completion.AllowedToolCalls != 1 || completion.Enforceable {
		t.Fatalf("empty accepted recovery did not keep one further allowance: %#v", completion)
	}
	if state.recoveryAllowancesRemaining != localizationRecoveryAllowanceCap-1 {
		t.Fatalf("empty accepted recovery spent %d allowances", localizationRecoveryAllowanceCap-state.recoveryAllowancesRemaining)
	}
	if state.digest != retained || completion.digest != retained {
		t.Fatalf("uncorroborated recovery dropped retained evidence: state=%#v completion=%#v", state.digest, completion.digest)
	}
	if got := completion.digest.Evidence[0].ID; got != "repo/storage/base.go::Storage.Load" {
		t.Fatalf("same-task recovery changed retained identity: %q", got)
	}
}

func TestFinishReservedReadWithDigestRejectsStaleTokenAndGeneration(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("repo/storage/base.go::Storage.Load", "repo/storage/base.go"),
	}, nil)
	state := &localizationTerminalState{
		state:                localizationStateRecoveryInFlight,
		generation:           9,
		readReservationToken: 31,
		readReservationGen:   9,
		digest:               retained,
	}
	current := []localizationDigestRow{captureTestRow("repo/storage/disk.go::DiskStorage.Load", "repo/storage/disk.go")}
	completion := state.finishReservedReadTokenWithDigest(30, true, current, true)
	if completion.State != localizationStateInactive || state.state != localizationStateRecoveryInFlight {
		t.Fatalf("stale token changed state: completion=%#v state=%q", completion, state.state)
	}
	if state.digest.Evidence[0].ID != retained.Evidence[0].ID {
		t.Fatal("stale token changed digest")
	}

	state.readReservationGen = 8
	completion = state.finishReservedReadTokenWithDigest(31, true, current, true)
	if completion.State != localizationStateInactive || state.state != localizationStateRecoveryInFlight {
		t.Fatalf("stale generation changed state: completion=%#v state=%q", completion, state.state)
	}
}

func TestLocalizationRecoveryDoesNotAuthorizeReadFile(t *testing.T) {
	if localizationRecoveryOperationAllowed("read", "file") {
		t.Fatal("read.file was added to the recovery authorization set")
	}
	state := &localizationTerminalState{
		state:                    localizationStateNeedsRecovery,
		generation:               2,
		recoveryRetriesRemaining: 1,
	}
	// read.file is still not a recovery anchor: it can never spend the bounded
	// allowance or drive the contract to answer_ready. Without an explicit
	// target it is not a directed read either, so it stays refused.
	blocked, token := state.authorizeWithToken("read", "file", map[string]any{"path": "repo/storage/disk.go"})
	if token != 0 || blocked == nil {
		t.Fatalf("read.file authorization = token %d result %#v", token, blocked)
	}
	if localizationDirectedRead("read", "file", map[string]any{"path": "repo/storage/disk.go"}) {
		t.Fatal("an untargeted read.file was treated as a directed read")
	}
}
