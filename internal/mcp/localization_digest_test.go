package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func testEvidenceDigest() *localizationEvidenceDigest {
	return newLocalizationEvidenceDigest(localizationExploreEnvelope{
		Files:   []string{"repo/storage/disk.go", "repo/storage/cloud.go"},
		Symbols: []string{"repo/storage/disk.go::DiskStorage.Load", "repo/storage/cloud.go::CloudStorage.Load"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", File: "repo/storage/disk.go", Line: 42, Signature: "func (s *DiskStorage) Load(key string) ([]byte, error)"},
			{Rank: 2, ID: "repo/storage/cloud.go::CloudStorage.Load", Name: "Load", File: "repo/storage/cloud.go", Line: 17},
		},
	})
}

func requireLocalizationTerminalReplay(t *testing.T, result *mcpgo.CallToolResult, _, _ string) localizationTerminalContract {
	t.Helper()
	return requireLocalizationReplayClosing(t, result, localizationAnswerReadyDirective, []string{
		"Localization for this task is complete",
		"Answer now",
		"naming the files and symbols you rely on",
		"another navigation call is not",
	})
}

// A localization that spent its recovery allowances without corroborating
// anything replays the same structure under the closing line that says so.
func requireLocalizationUnconfirmedReplay(t *testing.T, result *mcpgo.CallToolResult) localizationTerminalContract {
	t.Helper()
	return requireLocalizationReplayClosing(t, result, localizationUnconfirmedAnswerDirective, []string{
		"bounded localization allowance is spent",
		"Answer now",
		"say plainly that the match is unconfirmed",
	})
}

func requireLocalizationReplayClosing(
	t *testing.T, result *mcpgo.CallToolResult, directive string, required []string,
) localizationTerminalContract {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("terminal replay = %#v, want successful MCP result", result)
	}
	text, ok := singleTextContent(result)
	if !ok || !strings.Contains(text, directive) {
		t.Fatalf("terminal replay content = %#v", result.Content)
	}
	for _, required := range required {
		if !strings.Contains(text, required) {
			t.Fatalf("terminal replay text %q does not contain %q", text, required)
		}
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("terminal replay structured content = %#v", result.StructuredContent)
	}
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("marshal terminal replay: %v", err)
	}
	var contract localizationTerminalContract
	if err := json.Unmarshal(encoded, &contract); err != nil {
		t.Fatalf("decode terminal replay contract: %v", err)
	}
	if !contract.Terminal || contract.Completion.State != localizationStateAnswerReady ||
		contract.Completion.ContractVersion != localizationTerminalContractV2 ||
		contract.Completion.AllowedToolCalls != 0 || contract.Completion.FinalResponse == "" {
		t.Fatalf("terminal replay contract = %#v", contract)
	}
	// The answer rides the completion once; the top level carries only the
	// pointer to it, so the same block is never billed twice on the wire.
	if _, duplicated := structured["final_response"]; duplicated ||
		structured["directive"] != directive ||
		contract.Completion.FinalResponse == "" {
		t.Fatalf("terminal replay stable keys = %#v", structured)
	}
	if text != contract.Completion.FinalResponse {
		t.Fatalf("terminal replay text diverged from final_response: %q", text)
	}
	if !strings.HasSuffix(contract.Completion.FinalResponse, directive) {
		t.Fatalf("terminal convergence directive is outside final_response: %q", contract.Completion.FinalResponse)
	}
	return contract
}

func TestPostTerminalNavigationReturnsByteIdenticalSuccessfulReplay(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	var canonical []byte
	var canonicalFinalResponse string
	for _, facade := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
		for repeat := 0; repeat < 3; repeat++ {
			result, reserved := state.authorize(facade, "any_operation", nil)
			if reserved {
				t.Fatalf("%s repeat %d reserved a handler call", facade, repeat)
			}
			contract := requireLocalizationTerminalReplay(t, result, facade, "any_operation")
			if canonicalFinalResponse == "" {
				canonicalFinalResponse = contract.Completion.FinalResponse
			} else if contract.Completion.FinalResponse != canonicalFinalResponse {
				t.Fatalf("%s repeat %d changed final_response bytes", facade, repeat)
			}
			if contract.Completion.Enforceable {
				t.Fatalf("%s repeat %d unexpectedly upgraded advisory evidence", facade, repeat)
			}
			visible, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal %s result: %v", facade, err)
			}
			if !strings.Contains(string(visible), "repo/storage/disk.go") {
				t.Fatalf("%s replay omitted retained evidence: %s", facade, visible)
			}
			if canonical == nil {
				canonical = append([]byte(nil), visible...)
			} else if !reflect.DeepEqual(visible, canonical) {
				t.Fatalf("%s repeat %d replay changed by route", facade, repeat)
			}
		}
	}
	for _, facade := range []string{"change", "edit", "refactor", "workspace", "session", "recall", "remember", "capabilities"} {
		if result, reserved := state.authorize(facade, "any_operation", nil); result != nil || reserved {
			t.Fatalf("%s must remain dispatchable after answer_ready: result=%#v reserved=%v", facade, result, reserved)
		}
	}
}

func TestRepeatLocalizeAgainstTerminalContractReturnsCompactStop(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	token, blocked := state.beginLocalize("find the storage load implementations again", false)
	if token != 0 {
		t.Fatal("repeat localize must not reserve the handler slot")
	}
	requireLocalizationTerminalReplay(t, blocked, "explore", "localize")
}

func TestRefinementPromotionRetainsDigestForReplay(t *testing.T) {
	state := &localizationTerminalState{}
	candidate := "repo/storage/disk.go::DiskStorage.Load"
	state.armRefinementRoutesForTask(
		"find the storage load implementations",
		candidate,
		[]string{candidate},
		map[string]localizationRefinementRoute{candidate: {enforceable: true}},
		testEvidenceDigest(),
	)

	args := map[string]any{"target": map[string]any{"symbol": candidate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("permitted refinement read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)

	terminal, reserved := state.authorize("search", "symbols", nil)
	if reserved {
		t.Fatal("post-promotion navigation reserved a handler")
	}
	contract := requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
	if !strings.Contains(contract.Completion.FinalResponse, "- PRIMARY — repo/storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load") ||
		!strings.Contains(contract.Completion.FinalResponse, "- PRIMARY — repo/storage/cloud.go:17 — repo/storage/cloud.go::CloudStorage.Load") {
		t.Fatalf("promotion lost retained aligned evidence: %q", contract.Completion.FinalResponse)
	}
}

func TestRefinementAllowsOneAlternateRankedCandidateRead(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/storage/disk.go::DiskStorage.Load"
	alternate := "repo/storage/cloud.go::CloudStorage.Load"
	state.armRefinementRoutesForTask(
		"find the storage load implementations",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {enforceable: true},
			alternate: {enforceable: true},
		},
		testEvidenceDigest(),
	)

	completion := state.completionLocked()
	if !strings.Contains(completion.RequiredAction, preferred) ||
		!strings.Contains(completion.RequiredAction, alternate) ||
		!strings.Contains(completion.RequiredAction, "any returned candidate") {
		t.Fatalf("required action does not name the ranked alternates it authorizes: %q", completion.RequiredAction)
	}
	// Obeying the directive has to stay productive when the top candidate is
	// wrong, so it names what to do next instead of ending at one read.
	if !strings.Contains(completion.RequiredAction, "does not match the task's anchor terms") ||
		!strings.Contains(completion.RequiredAction, "one bounded search") {
		t.Fatalf("required action ends at the first candidate: %q", completion.RequiredAction)
	}
	args := map[string]any{"target": map[string]any{"symbol": alternate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("alternate ranked candidate read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)
	if result, reserved := state.authorize("read", "source", args); reserved {
		t.Fatal("second read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestDigestLifecycleAndLegacyFallback(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")
	state.keepOpenForTask("new unrelated work")
	if blocked, _ := state.authorize("search", "symbols", nil); blocked != nil {
		t.Fatalf("inactive state must not block, got %+v", blocked)
	}
	if state.digest != nil {
		t.Fatal("keepOpenForTask must clear the retained digest")
	}

	withoutDigest := &localizationTerminalState{}
	withoutDigest.armForTask(newLocalizationCompletion(true, ""), "task without digest")
	blocked, _ := withoutDigest.authorize("search", "symbols", nil)
	requireLocalizationTerminalReplay(t, blocked, "search", "symbols")
}

func TestOnlyAuthenticatedSourceLiteralCalleeReservesOnePrimarySeat(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/d.go::Semantic", Name: "Semantic", File: "repo/d.go"},
		{ID: "repo/e.go::RankedTwo", Name: "RankedTwo", File: "repo/e.go"},
		{ID: "repo/f.go::RankedThree", Name: "RankedThree", File: "repo/f.go"},
		{ID: "repo/g.go::RankedFour", Name: "RankedFour", File: "repo/g.go"},
		{ID: "repo/h.go::RankedFive", Name: "RankedFive", File: "repo/h.go"},
		{ID: "repo/b.go::FirstCallee", Name: "FirstCallee", File: "repo/b.go", Provenance: localizationProvenanceSourceLiteralCallee, literalPrimaryEligible: true},
		{ID: "repo/c.go::SecondCallee", Name: "SecondCallee", File: "repo/c.go", Provenance: localizationProvenanceSourceLiteralCallee},
		{ID: "repo/a.go::Content", Name: "Content", File: "repo/a.go", Provenance: localizationProvenanceContentLiteral},
	}

	presented := localizationFinalResponseRows("find the semantic handler", nil, rows)
	reservedLiteralPrimaries := 0
	contentLiteralPrimary := false
	semanticPrimary := false
	for _, row := range presented {
		if !row.primary {
			continue
		}
		if row.row.Provenance == localizationProvenanceSourceLiteralCallee {
			reservedLiteralPrimaries++
		}
		if row.row.Provenance == localizationProvenanceContentLiteral {
			contentLiteralPrimary = true
		}
		if row.row.ID == "repo/d.go::Semantic" {
			semanticPrimary = true
		}
	}
	if reservedLiteralPrimaries != localizationFinalResponseLiteralReserve {
		t.Fatalf("reserved literal PRIMARY rows = %d, want %d", reservedLiteralPrimaries, localizationFinalResponseLiteralReserve)
	}
	if contentLiteralPrimary {
		t.Fatal("generic content literal gained PRIMARY authority from the literal alone")
	}
	if !semanticPrimary {
		t.Fatal("literal reserve consumed the semantic PRIMARY opportunity")
	}
}

func TestFrozenPrimaryCohortSurvivesSupplementalRows(t *testing.T) {
	cases := []struct {
		name string
		task string
		gold string
	}{
		{name: "aiohttp anchored import", task: "import aiohttp fails when zstd is not installed", gold: "aiohttp/compression_utils.py::ZSTDDecompressor.__init__"},
		{name: "fmt anchored print", task: "ostream print fails with constant conditional warning", gold: "include/fmt/ostream.h::print"},
		{name: "vue anchored hydrate", task: "skip detached async component hydration", gold: "packages/runtime-core/src/apiAsyncComponent.ts::performHydrate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{
				{ID: tc.gold, Name: "gold", File: strings.Split(tc.gold, "::")[0]},
				{ID: "repo/second.go::Second", Name: "Second", File: "repo/second.go"},
				{ID: "repo/third.go::Third", Name: "Third", File: "repo/third.go"},
				{ID: "repo/fourth.go::Fourth", Name: "Fourth", File: "repo/fourth.go"},
				{ID: "repo/fifth.go::Fifth", Name: "Fifth", File: "repo/fifth.go"},
			}}
			digest := newLocalizationEvidenceDigestForTask(tc.task, envelope)
			before := append([]string(nil), digest.primaryIDs...)
			freezeLocalizationPrimaryCohort(tc.task, &envelope, digest)
			for index := 0; index < 4; index++ {
				envelope.Evidence = append(envelope.Evidence, localizationEvidence{
					ID:   fmt.Sprintf("repo/supplement%02d.go::HydratePrint", index),
					Name: "HydratePrint", File: fmt.Sprintf("repo/supplement%02d.go", index),
					Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true,
				})
			}
			afterDigest := newLocalizationEvidenceDigestForTask(tc.task, envelope)
			after := localizationFinalResponsePrimaryIDs(tc.task, nil, afterDigest.Evidence)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("PRIMARY cohort changed after supplemental rows:\nbefore=%v\nafter=%v", before, after)
			}
			if len(after) != localizationFinalResponsePrimaryLimit || after[0] != tc.gold {
				t.Fatalf("PRIMARY cohort lost anchored candidate or count: %v", after)
			}
		})
	}
}

func TestSupplementalRowsStaySupportingAcrossEverySelectorLoop(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/base.go::First", Name: "First", File: "repo/base.go", primaryCohortOrder: 1},
		{ID: "repo/base.go::Second", Name: "Second", File: "repo/base.go", primaryCohortOrder: 2},
		{ID: "repo/base.go::Third", Name: "Third", File: "repo/base.go", primaryCohortOrder: 3},
		{ID: "repo/base.go::Fourth", Name: "Fourth", File: "repo/base.go", primaryCohortOrder: 4},
		{ID: "repo/supp.go::ExactTaskMatch", Name: "ExactTaskMatch", Kind: "method", File: "repo/supp.go", supportingOnly: true},
		{ID: "repo/body.go::Body", Name: "Body", File: "repo/body.go", Provenance: localizationProvenanceBodyMention, supportingOnly: true},
		{ID: "repo/adj.go::Adjacent", Name: "Adjacent", File: "repo/adj.go", Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true},
	}
	current := []localizationDigestRow{{ID: "repo/supp.go::Current", Name: "Current", QualName: "Owner.Current", Kind: "method", File: "repo/supp.go"}}
	first := localizationFinalResponseRows("ExactTaskMatch", current, rows)
	second := localizationFinalResponseRows("ExactTaskMatch", current, rows)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("selector output is not deterministic")
	}
	for _, item := range first {
		if item.primary && localizationFinalResponseSupportingOnly(item.row) {
			t.Fatalf("supplemental row became PRIMARY: %+v", item.row)
		}
	}
}

func TestContentLiteralProvenanceDoesNotBecomeStrongTerminalProof(t *testing.T) {
	rows := map[string]localizationDigestRow{
		"repo/a.go::First": {
			ID: "repo/a.go::First", File: "repo/a.go", Provenance: localizationProvenanceContentLiteral,
		},
	}
	if localizationDigestStrongProofRetained(rows, "repo/a.go::First") {
		t.Fatal("ordinary content evidence became enforceable terminal proof")
	}
}

func TestDigestByteCapShedsOptionalMetadataBeforeEvidenceTail(t *testing.T) {
	envelope := localizationExploreEnvelope{}
	for i := 0; i < 400; i++ {
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank:      i + 1,
			ID:        fmt.Sprintf("repo/big/file.go::Sym%03d", i),
			Name:      strings.Repeat("n", 40),
			File:      "repo/big/file.go",
			Line:      i,
			Signature: strings.Repeat("s", 2000),
		})
	}
	digest := newLocalizationEvidenceDigest(envelope)
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(digest.Evidence) != localizationReplayEvidenceLimit {
		t.Fatalf("byte cap retained %d rows, want all %d declaration identities", len(digest.Evidence), localizationReplayEvidenceLimit)
	}
	stripped := 0
	for _, row := range digest.Evidence {
		if row.Signature == "" {
			stripped++
		}
	}
	if stripped == 0 {
		t.Fatal("byte cap retained every optional signature instead of compacting metadata")
	}
	if len(digest.Files) != len(digest.Evidence) || len(digest.Symbols) != len(digest.Evidence) {
		t.Fatalf("files=%d symbols=%d evidence=%d, want positional rows", len(digest.Files), len(digest.Symbols), len(digest.Evidence))
	}
	for index, file := range digest.Files {
		if file != "repo/big/file.go" || digest.Evidence[index].Rank != index+1 {
			t.Fatalf("unaligned compacted row %d: file=%q evidence=%#v", index, file, digest.Evidence[index])
		}
	}
	if len(digest.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("final_response = %d bytes, want <= %d", len(digest.finalResponse), localizationFinalResponseMaxBytes)
	}
}

func TestDigestPressureShedsSupplementalRowsBeforeRankedEvidence(t *testing.T) {
	const (
		protectedID = "repo/protected.go::Protected"
		bodyID      = "repo/body.go::BodyMention"
		adjacentID  = "repo/adjacent.go::Adjacent"
		ordinary    = 13
	)
	largeSignature := strings.Repeat("large optional signature ", 120)
	completion := newLocalizationRefinementCompletion(protectedID)
	evidence := []localizationEvidence{{
		Rank: 1, ID: protectedID, Name: "Protected", Kind: "function",
		File: "repo/protected.go", Signature: largeSignature,
		Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true,
	}}
	for index := 0; index < ordinary; index++ {
		evidence = append(evidence, localizationEvidence{
			Rank: len(evidence) + 1,
			ID:   fmt.Sprintf("repo/ordinary%02d.go::Ordinary%02d", index, index),
			Name: fmt.Sprintf("Ordinary%02d", index), Kind: "function",
			File: fmt.Sprintf("repo/ordinary%02d.go", index), Signature: largeSignature,
		})
	}
	evidence = append(evidence,
		localizationEvidence{Rank: len(evidence) + 1, ID: bodyID, Name: "BodyMention", Kind: "function", File: "repo/body.go", Signature: largeSignature, Provenance: localizationProvenanceBodyMention, supportingOnly: true},
		localizationEvidence{Rank: len(evidence) + 2, ID: adjacentID, Name: "Adjacent", Kind: "function", File: "repo/adjacent.go", Signature: largeSignature, Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true},
	)

	assertRetained := func(t *testing.T, digest *localizationEvidenceDigest) {
		t.Helper()
		ids := make(map[string]localizationDigestRow, len(digest.Evidence))
		for index, row := range digest.Evidence {
			ids[row.ID] = row
			if row.Rank != index+1 || digest.Files[index] != row.File || digest.Symbols[index] != row.ID {
				t.Fatalf("unaligned retained row %d: %#v", index+1, row)
			}
		}
		if _, exists := ids[bodyID]; exists {
			t.Fatal("body supplement survived retained-state pressure")
		}
		if _, exists := ids[adjacentID]; exists {
			t.Fatal("direct adjacency supplement survived retained-state pressure")
		}
		protected, exists := ids[protectedID]
		if !exists || !protected.authorizationPriority {
			t.Fatalf("authorized supplemental identity was shed or lost priority: %#v", protected)
		}
		for index := 0; index < ordinary; index++ {
			id := fmt.Sprintf("repo/ordinary%02d.go::Ordinary%02d", index, index)
			if _, exists := ids[id]; !exists {
				t.Fatalf("independently ranked row %q was shed before supplements", id)
			}
		}
		encoded, err := json.Marshal(digest)
		if err != nil || len(encoded) > localizationDigestMaxBytes || len(digest.finalResponse) > localizationFinalResponseMaxBytes {
			t.Fatalf("unbounded digest bytes=%d final=%d err=%v", len(encoded), len(digest.finalResponse), err)
		}
	}

	t.Run("initial digest", func(t *testing.T) {
		digest := newLocalizationEvidenceDigestForTask("find Protected", localizationExploreEnvelope{
			Completion: completion,
			Evidence:   evidence,
		})
		assertRetained(t, digest)
	})
	t.Run("post-read merge", func(t *testing.T) {
		rows := make([]localizationDigestRow, 0, len(evidence))
		for _, row := range evidence {
			rows = append(rows, localizationDigestRow{
				Rank: row.Rank, ID: row.ID, Name: row.Name, Kind: row.Kind,
				File: row.File, Signature: row.Signature, Provenance: row.Provenance,
				supportingOnly:        row.supportingOnly,
				authorizationPriority: row.ID == protectedID,
			})
		}
		assertRetained(t, mergeLocalizationEvidenceDigest(nil, &localizationEvidenceDigest{Evidence: rows}))
	})
}

func authorizationAwareDigestFixture() (localizationExploreEnvelope, []string) {
	allowed := []string{
		"repo/storage/level.go::StorageLevel.Normalize",
		"repo/storage/batch_gate.go::BatchGate.FlushPending",
		"repo/storage/batch_gate.go::BatchGate.ClearPending",
	}
	longPathSegment := strings.Repeat("very-long-path-segment-", 8)
	evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		file := fmt.Sprintf("src/%s/%02d.go", longPathSegment, index)
		id := fmt.Sprintf(
			"repo/%s/%02d.go::Unrelated%02d.%s",
			longPathSegment, index, index, strings.Repeat("LongMethod", 6),
		)
		row := localizationEvidence{
			Rank: index + 1, ID: id, Name: fmt.Sprintf("Unrelated%02d", index),
			QualName: fmt.Sprintf("Unrelated%02d.%s", index, strings.Repeat("LongMethod", 6)),
			Kind:     "method", File: file, Line: index + 1,
		}
		switch index {
		case 0:
			row.ID = allowed[0]
			row.Name = "Normalize"
			row.QualName = "StorageLevel.Normalize"
			row.File = "repo/storage/level.go"
			row.Line = 55
		case 2:
			row.ID = allowed[1]
			row.Name = "FlushPending"
			row.QualName = "BatchGate.FlushPending"
			row.File = "repo/storage/batch_gate.go"
			row.Line = 81
		case 8:
			row.ID = allowed[2]
			row.Name = "ClearPending"
			row.QualName = "BatchGate.ClearPending"
			row.File = "repo/storage/batch_gate.go"
			row.Line = 72
		}
		evidence = append(evidence, row)
	}
	completion := newLocalizationRefinementCompletionForSymbols(allowed[0], allowed)
	completion.refinementRoutes = map[string]localizationRefinementRoute{
		allowed[0]: {enforceable: true},
		allowed[1]: {enforceable: true},
		allowed[2]: {enforceable: true},
	}
	envelope := localizationExploreEnvelope{
		Completion: completion,
		Files:      make([]string, len(evidence)),
		Symbols:    make([]string, len(evidence)),
		Evidence:   evidence,
	}
	for index, row := range evidence {
		envelope.Files[index] = row.File
		envelope.Symbols[index] = row.ID
	}
	return envelope, allowed
}

func TestDigestPrioritizesAuthorizedRowsBeforeUnrelatedByteShedding(t *testing.T) {
	envelope, allowed := authorizationAwareDigestFixture()
	before, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	digest := newLocalizationEvidenceDigest(envelope)
	after, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal fixture after digest: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("digest construction mutated the visible envelope")
	}
	if len(digest.Evidence) >= len(envelope.Evidence) {
		t.Fatalf("fixture did not force byte shedding: retained=%d visible=%d", len(digest.Evidence), len(envelope.Evidence))
	}
	if len(digest.Evidence) < len(allowed) {
		t.Fatalf("retained rows=%d, want all %d authorized rows", len(digest.Evidence), len(allowed))
	}
	for index, want := range allowed {
		if digest.Evidence[index].ID != want {
			t.Fatalf("authorized row %d = %q, want %q; all=%#v", index+1, digest.Evidence[index].ID, want, digest.Evidence)
		}
	}
	authorized := make(map[string]struct{}, len(allowed))
	for _, symbol := range allowed {
		authorized[symbol] = struct{}{}
	}
	seenUnrelated := false
	for index, row := range digest.Evidence {
		_, isAuthorized := authorized[row.ID]
		if !isAuthorized {
			seenUnrelated = true
		} else if seenUnrelated {
			t.Fatalf("authorized row %q followed unrelated evidence at position %d", row.ID, index+1)
		}
		if row.Rank != index+1 || digest.Files[index] != row.File || digest.Symbols[index] != row.ID {
			t.Fatalf("unaligned retained row %d: %#v", index+1, row)
		}
	}
	encoded, err := json.Marshal(digest)
	if err != nil || len(encoded) > localizationDigestMaxBytes || len(digest.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("bounded digest bytes=%d final=%d err=%v", len(encoded), len(digest.finalResponse), err)
	}
}

func TestAuthorizedLateOwnerMethodSurvivesInitialDigestAndPostReadMerge(t *testing.T) {
	envelope, allowed := authorizationAwareDigestFixture()
	initial := newLocalizationEvidenceDigest(envelope)
	current := localizationDigestRow{
		ID: allowed[1], Name: "FlushPending", QualName: "BatchGate.FlushPending",
		Kind: "method", File: "repo/storage/batch_gate.go", Line: 81,
		Signature: strings.Repeat("current argument ", 70),
	}
	merged := mergeLocalizationEvidenceDigestForTask(
		"Investigate BatchGate.ClearPending() after FlushPending()",
		[]localizationDigestRow{current},
		initial,
	)
	if len(merged.Evidence) < 2 || merged.Evidence[0].ID != allowed[1] || merged.Evidence[1].ID != allowed[2] {
		t.Fatalf("post-read owner cohort = %#v, want flushBuffer then clear", merged.Evidence)
	}
	encoded, err := json.Marshal(merged)
	if err != nil || len(encoded) > localizationDigestMaxBytes || len(merged.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("post-read digest bytes=%d final=%d err=%v", len(encoded), len(merged.finalResponse), err)
	}
	for index, row := range merged.Evidence {
		if row.Rank != index+1 || merged.Files[index] != row.File || merged.Symbols[index] != row.ID {
			t.Fatalf("unaligned post-read row %d: %#v", index+1, row)
		}
	}
}

func TestLocalizationCompletionReconcilesShedAllowedSymbols(t *testing.T) {
	t.Run("retains preferred and drops oversized alternate", func(t *testing.T) {
		preferred := "repo/pkg/preferred.go::Preferred.Run"
		alternate := "repo/" + strings.Repeat("oversized-segment-", 400) + "::Alternate.Run"
		completion := newLocalizationRefinementCompletionForSymbols(preferred, []string{preferred, alternate})
		completion.refinementRoutes = map[string]localizationRefinementRoute{
			preferred: {enforceable: true},
			alternate: {enforceable: true},
		}
		envelope := localizationExploreEnvelope{
			Completion: completion,
			Evidence: []localizationEvidence{
				{Rank: 1, ID: preferred, Name: "Run", Kind: "method", File: "pkg/preferred.go", Line: 10},
				{Rank: 2, ID: alternate, Name: "Run", Kind: "method", File: "pkg/alternate.go", Line: 20},
			},
		}
		digest := newLocalizationEvidenceDigest(envelope)
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		bounded = localizationCompletionWithDigest(bounded, digest)
		if bounded.State != localizationStateNeedsRefinement || !reflect.DeepEqual(bounded.AllowedSymbols, []string{preferred}) {
			t.Fatalf("bounded completion = %#v, want preferred-only refinement", bounded)
		}
		if !reflect.DeepEqual(bounded.refinementSymbols, bounded.AllowedSymbols) || len(bounded.refinementRoutes) != 1 {
			t.Fatalf("session refinement state diverged: symbols=%#v routes=%#v", bounded.refinementSymbols, bounded.refinementRoutes)
		}
		if _, exists := bounded.refinementRoutes[preferred]; !exists {
			t.Fatalf("preferred route missing after reconciliation: %#v", bounded.refinementRoutes)
		}
		if !strings.Contains(bounded.RequiredAction, preferred) {
			t.Fatalf("preferred refinement action was not rebuilt: %#v", bounded)
		}
		// The page a refinement hands back is the unconfirmed one: it must exist,
		// and it must not read as an answer the caller may stop on.
		if !strings.HasPrefix(bounded.FinalResponse, localizationProvisionalHeading) ||
			strings.Contains(bounded.FinalResponse, localizationAnswerReadyDirective) {
			t.Fatalf("bounded refinement page was not provisional: %q", bounded.FinalResponse)
		}
		if len(digest.Symbols) != 1 || digest.Symbols[0] != preferred {
			t.Fatalf("oversized alternate survived digest: %#v", digest.Symbols)
		}
	})

	t.Run("downgrades when no authorized identity fits", func(t *testing.T) {
		oversized := "repo/" + strings.Repeat("x", localizationDigestMaxBytes) + "::Only.Run"
		completion := newLocalizationRefinementCompletion(oversized)
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{
			Completion: completion,
			Evidence: []localizationEvidence{{
				Rank: 1, ID: oversized, Name: "Run", Kind: "method", File: "pkg/only.go", Line: 1,
			}},
		})
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		bounded = localizationCompletionWithDigest(bounded, digest)
		if len(digest.Evidence) != 0 {
			t.Fatalf("pathological identity survived digest: %#v", digest.Evidence)
		}
		if bounded.State != localizationStateLocalized || bounded.RequiredAction != "continue_task" || bounded.AllowedToolCalls != 0 || len(bounded.AllowedSymbols) != 0 {
			t.Fatalf("empty authorization did not use nonterminal advisory fallback: %#v", bounded)
		}
		if bounded.FinalResponse != "" {
			t.Fatalf("empty evidence should not manufacture a provisional identity page: %q", bounded.FinalResponse)
		}
	})
}

func TestDigestRetainsAuthorizationDependenciesUnderPressure(t *testing.T) {
	t.Run("refinement routes", func(t *testing.T) {
		const (
			wrapper        = "repo/storage/facade.go::StorageFacade.Flush"
			implementation = "repo/storage/batch_gate.go::BatchGate.FlushPending"
			concrete       = "repo/storage/batch_gate.go::BatchGate.ClearPending"
			proof          = "repo/storage/facade.go::StorageFacade.Clear"
		)
		longSegment := strings.Repeat("unrelated-metadata-", 18)
		evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
		for index := 0; index < localizationReplayEvidenceLimit; index++ {
			row := localizationEvidence{
				Rank: index + 1,
				ID:   fmt.Sprintf("repo/noise/%02d.go::Noise%02d.%s", index, index, longSegment),
				Name: fmt.Sprintf("Noise%02d", index), QualName: longSegment,
				Kind: "method", File: fmt.Sprintf("repo/noise/%02d/%s.go", index, longSegment),
				Signature: strings.Repeat("argument ", 30),
			}
			switch index {
			case 0:
				row.ID, row.Name, row.QualName, row.File, row.Provenance = wrapper, "Flush", "StorageFacade.Flush", "repo/storage/facade.go", localizationProvenanceImplementationRoute
			case 2:
				row.ID, row.Name, row.QualName, row.File, row.Provenance = concrete, "ClearPending", "BatchGate.ClearPending", "repo/storage/batch_gate.go", localizationProvenanceImplementationTarget
			case 8:
				row.ID, row.Name, row.QualName, row.File, row.Provenance = implementation, "FlushPending", "BatchGate.FlushPending", "repo/storage/batch_gate.go", localizationProvenanceImplementationTarget
			case 9:
				row.ID, row.Name, row.QualName, row.File, row.Provenance = proof, "Clear", "StorageFacade.Clear", "repo/storage/facade.go", localizationProvenanceImplementationRoute
			}
			evidence = append(evidence, row)
		}
		completion := newLocalizationRefinementCompletionForSymbols(wrapper, []string{wrapper, concrete})
		completion.refinementRoutes = map[string]localizationRefinementRoute{
			wrapper:  {implementationSymbol: implementation, enforceable: true},
			concrete: {proofSymbol: proof, enforceable: true},
		}
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Completion: completion, Evidence: evidence})
		if len(digest.Evidence) >= len(evidence) {
			t.Fatalf("fixture did not force digest shedding: retained=%d visible=%d", len(digest.Evidence), len(evidence))
		}
		retained := localizationDigestRowsByID(digest)
		for _, symbol := range []string{wrapper, implementation, concrete, proof} {
			if _, exists := retained[symbol]; !exists {
				t.Fatalf("authorization dependency %q was shed: %#v", symbol, digest.Symbols)
			}
		}
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		if bounded.State != localizationStateNeedsRefinement || len(bounded.refinementRoutes) != 2 {
			t.Fatalf("bounded refinement lost retained routes: %#v", bounded)
		}
		for symbol, route := range bounded.refinementRoutes {
			if !route.enforceable {
				t.Fatalf("retained route %q was downgraded: %#v", symbol, route)
			}
		}
	})

	t.Run("divergent exact support", func(t *testing.T) {
		const (
			owner  = "repo/storage/batch_gate.go::BatchGate.New"
			typeID = "repo/storage/batch_gate.go::BatchGate"
		)
		longSegment := strings.Repeat("unrelated-metadata-", 18)
		evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
		for index := 0; index < localizationReplayEvidenceLimit; index++ {
			row := localizationEvidence{
				Rank: index + 1,
				ID:   fmt.Sprintf("repo/noise/%02d.go::Noise%02d.%s", index, index, longSegment),
				Name: fmt.Sprintf("Noise%02d", index), QualName: longSegment,
				Kind: "method", File: fmt.Sprintf("repo/noise/%02d/%s.go", index, longSegment),
				Signature: strings.Repeat("argument ", 30),
			}
			switch index {
			case 0:
				row.ID, row.Name, row.QualName, row.File, row.Provenance = owner, "New", "BatchGate.New", "repo/storage/batch_gate.go", localizationProvenanceDivergentDefault
			case 8:
				row.ID, row.Name, row.QualName, row.Kind, row.File, row.Provenance = typeID, "BatchGate", "BatchGate", "type", "repo/storage/batch_gate.go", localizationProvenanceDivergentDefaultType
			}
			evidence = append(evidence, row)
		}
		completion := newLocalizationCompletion(false, owner)
		completion.enforceableOnAnswerReady = true
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Completion: completion, Evidence: evidence})
		if len(digest.Evidence) >= len(evidence) {
			t.Fatalf("fixture did not force digest shedding: retained=%d visible=%d", len(digest.Evidence), len(evidence))
		}
		retained := localizationDigestRowsByID(digest)
		for _, symbol := range []string{owner, typeID} {
			if _, exists := retained[symbol]; !exists {
				t.Fatalf("exact-read proof dependency %q was shed: %#v", symbol, digest.Symbols)
			}
		}
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		if bounded.State != localizationStateNeedsExactRead || !bounded.enforceableOnAnswerReady {
			t.Fatalf("complete retained proof was downgraded: %#v", bounded)
		}
	})
}

func TestDigestDowngradesAuthorizationWhenDependencyCannotFit(t *testing.T) {
	t.Run("generic route loses missing implementation", func(t *testing.T) {
		const wrapper = "repo/storage/facade.go::StorageFacade.Flush"
		implementation := "repo/storage/" + strings.Repeat("implementation-", localizationDigestMaxBytes) + "::BatchGate.FlushPending"
		completion := newLocalizationRefinementCompletion(wrapper)
		completion.refinementRoutes = map[string]localizationRefinementRoute{
			wrapper: {implementationSymbol: implementation, enforceable: true},
		}
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{
			Completion: completion,
			Evidence: []localizationEvidence{
				{Rank: 1, ID: wrapper, Name: "Flush", Kind: "method", File: "repo/storage/facade.go", Provenance: localizationProvenanceImplementationRoute},
				{Rank: 2, ID: implementation, Name: "FlushPending", Kind: "method", File: "repo/storage/batch_gate.go", Provenance: localizationProvenanceImplementationTarget},
			},
		})
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		if _, retained := localizationDigestRowsByID(digest)[implementation]; retained {
			t.Fatal("pathological implementation unexpectedly fit the digest")
		}
		if bounded.State != localizationStateLocalized || len(bounded.AllowedSymbols) != 0 || bounded.AllowedToolCalls != 0 {
			t.Fatalf("missing implementation proof did not release an advisory completion: %#v", bounded)
		}
	})

	t.Run("concrete route loses missing proof", func(t *testing.T) {
		const concrete = "repo/storage/batch_gate.go::BatchGate.ClearPending"
		proof := "repo/storage/" + strings.Repeat("proof-", localizationDigestMaxBytes) + "::StorageFacade.Clear"
		completion := newLocalizationRefinementCompletion(concrete)
		completion.refinementRoutes = map[string]localizationRefinementRoute{
			concrete: {proofSymbol: proof, enforceable: true},
		}
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{
			Completion: completion,
			Evidence: []localizationEvidence{
				{Rank: 1, ID: concrete, Name: "ClearPending", Kind: "method", File: "repo/storage/batch_gate.go", Provenance: localizationProvenanceImplementationTarget},
				{Rank: 2, ID: proof, Name: "Clear", Kind: "method", File: "repo/storage/facade.go", Provenance: localizationProvenanceImplementationRoute},
			},
		})
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		route, exists := bounded.refinementRoutes[concrete]
		if bounded.State != localizationStateNeedsRefinement || !exists || route.enforceable || route.proofSymbol != "" {
			t.Fatalf("concrete route did not downgrade after proof shedding: %#v", bounded)
		}
	})

	t.Run("divergent exact trust loses missing type", func(t *testing.T) {
		const owner = "repo/storage/batch_gate.go::BatchGate.New"
		typeID := "repo/storage/" + strings.Repeat("type-", localizationDigestMaxBytes) + "::BatchGate"
		completion := newLocalizationCompletion(false, owner)
		completion.enforceableOnAnswerReady = true
		digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{
			Completion: completion,
			Evidence: []localizationEvidence{
				{Rank: 1, ID: owner, Name: "New", Kind: "method", File: "repo/storage/batch_gate.go", Provenance: localizationProvenanceDivergentDefault},
				{Rank: 2, ID: typeID, Name: "BatchGate", Kind: "type", File: "repo/storage/batch_gate.go", Provenance: localizationProvenanceDivergentDefaultType},
			},
		})
		bounded := localizationCompletionBoundedByDigest(completion, digest)
		if bounded.State != localizationStateNeedsExactRead || bounded.ExactSymbol != owner || bounded.enforceableOnAnswerReady {
			t.Fatalf("exact read retained trust without divergent support: %#v", bounded)
		}
		ready := newLocalizationCompletion(true, "")
		ready.Enforceable = true
		ready = localizationCompletionBoundedByDigest(ready, digest)
		if ready.State != localizationStateAnswerReady || ready.Enforceable {
			t.Fatalf("answer-ready contract retained enforcement without divergent support: %#v", ready)
		}
	})
}

func TestTaskAwareDigestMergePromotesLongTailSameOwnerCohortWithoutIdentityLoss(t *testing.T) {
	const (
		currentID = "repo/gate.go::BatchGate.drainPending"
		targetID  = "repo/gate.go::BatchGate.discardPending"
	)
	row := func(id, name, qualName, kind, file string) localizationDigestRow {
		return localizationDigestRow{
			ID: id, Name: name, QualName: qualName, Kind: kind, File: file,
			Signature: strings.Repeat("argument ", 22),
		}
	}
	retainedRows := []localizationDigestRow{
		row("repo/level.go::Level.Normalize", "Normalize", "Level.Normalize", "method", "repo/level.go"),
		row("repo/filter.go::Filter.Accept", "Accept", "Filter.Accept", "method", "repo/filter.go"),
		row(currentID, "drainPending", "BatchGate.drainPending", "method", "repo/gate.go"),
		row("repo/gate.go::BatchGate.resolveSink", "resolveSink", "BatchGate.resolveSink", "method", "repo/gate.go"),
		row("repo/gate.go::BatchGate", "BatchGate", "BatchGate", "type", "repo/gate.go"),
		row("repo/gate.go::BatchGate.newGate", "newGate", "BatchGate.newGate", "method", "repo/gate.go"),
		row("repo/logger.go::Logger.Flush", "Flush", "Logger.Flush", "method", "repo/logger.go"),
		row("repo/trace.go::Trace.Record", "Record", "Trace.Record", "method", "repo/trace.go"),
		row(targetID, "discardPending", "BatchGate.discardPending", "method", "repo/gate.go"),
		row("repo/metrics.go::Metrics.Emit", "Emit", "Metrics.Emit", "method", "repo/metrics.go"),
	}
	// Pad with unrelated single-owner rows until the retained window is full,
	// so the cohort promotion below happens under a genuinely tight cap.
	for index := len(retainedRows); index < localizationReplayEvidenceLimit; index++ {
		retainedRows = append(retainedRows, row(
			fmt.Sprintf("repo/filler%02d.go::Filler%02d.Run", index, index),
			fmt.Sprintf("Run%02d", index),
			fmt.Sprintf("Filler%02d.Run", index),
			"method",
			fmt.Sprintf("repo/filler%02d.go", index),
		))
	}
	retained := mergeLocalizationEvidenceDigest(nil, &localizationEvidenceDigest{Evidence: retainedRows})
	if len(retained.Evidence) != localizationReplayEvidenceLimit {
		t.Fatalf("fixture retained %d rows, want %d", len(retained.Evidence), localizationReplayEvidenceLimit)
	}
	current := row(currentID, "drainPending", "BatchGate.drainPending", "method", "repo/gate.go")
	current.Signature = strings.Repeat("current argument ", 70)

	contains := func(digest *localizationEvidenceDigest, id string) bool {
		for _, evidence := range digest.Evidence {
			if evidence.ID == id {
				return true
			}
		}
		return false
	}
	baseline := mergeLocalizationEvidenceDigest([]localizationDigestRow{current}, retained)
	if !contains(baseline, targetID) {
		t.Fatal("byte compaction shed a declaration identity before optional metadata")
	}

	digest := mergeLocalizationEvidenceDigestForTask(
		"Batch gate drops queued entries during shutdown",
		[]localizationDigestRow{current},
		retained,
	)
	if len(digest.Evidence) < 4 || digest.Evidence[0].ID != currentID || digest.Evidence[3].ID != targetID {
		t.Fatalf("same-owner cohort was not reserved after current evidence: %#v", digest.Evidence)
	}
	if !contains(digest, targetID) {
		t.Fatalf("same-owner target was shed from task-aware digest: %#v", digest.Evidence)
	}
	encoded, err := json.Marshal(digest)
	if err != nil || len(encoded) > localizationDigestMaxBytes || len(digest.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("task-aware digest exceeded bounds: bytes=%d final=%d err=%v", len(encoded), len(digest.finalResponse), err)
	}
	if len(digest.Files) != len(digest.Evidence) || len(digest.Symbols) != len(digest.Evidence) {
		t.Fatalf("task-aware digest lost positional arrays: files=%d symbols=%d evidence=%d", len(digest.Files), len(digest.Symbols), len(digest.Evidence))
	}
	for index, evidence := range digest.Evidence {
		if evidence.Rank != index+1 || digest.Files[index] != evidence.File || digest.Symbols[index] != evidence.ID {
			t.Fatalf("task-aware row %d is not ordinally aligned: %#v", index, evidence)
		}
	}
}

func TestTaskAwareDigestMergeKeepsExplicitFourthOwnerMethodUnderTightCap(t *testing.T) {
	const (
		currentID  = "repo/gate.go::BatchGate.run"
		explicitID = "repo/gate.go::BatchGate.forceDiscard"
	)
	row := func(id, name, qualName, file string) localizationDigestRow {
		return localizationDigestRow{
			ID: id, Name: name, QualName: qualName, Kind: "method", File: file,
			Signature: strings.Repeat("argument ", 22),
		}
	}
	retainedRows := []localizationDigestRow{
		row("repo/level.go::Level.normalize", "normalize", "Level.normalize", "repo/level.go"),
		row(currentID, "run", "BatchGate.run", "repo/gate.go"),
		row("repo/gate.go::BatchGate.prepare", "prepare", "BatchGate.prepare", "repo/gate.go"),
		row("repo/filter.go::Filter.accept", "accept", "Filter.accept", "repo/filter.go"),
		row("repo/gate.go::BatchGate.resolve", "resolve", "BatchGate.resolve", "repo/gate.go"),
		row("repo/logger.go::Logger.flush", "flush", "Logger.flush", "repo/logger.go"),
		row("repo/gate.go::BatchGate.drain", "drain", "BatchGate.drain", "repo/gate.go"),
		row("repo/trace.go::Trace.record", "record", "Trace.record", "repo/trace.go"),
		row(explicitID, "forceDiscard", "BatchGate.forceDiscard", "repo/gate.go"),
		row("repo/metrics.go::Metrics.emit", "emit", "Metrics.emit", "repo/metrics.go"),
	}
	// Pad with unrelated rows to fill the retained window; the explicitly
	// cited fourth owner method must survive a genuinely tight cap.
	for index := len(retainedRows); index < localizationReplayEvidenceLimit; index++ {
		retainedRows = append(retainedRows, row(
			fmt.Sprintf("repo/filler%02d.go::Filler%02d.run", index, index),
			fmt.Sprintf("run%02d", index),
			fmt.Sprintf("Filler%02d.run", index),
			fmt.Sprintf("repo/filler%02d.go", index),
		))
	}
	retained := mergeLocalizationEvidenceDigest(nil, &localizationEvidenceDigest{Evidence: retainedRows})
	if len(retained.Evidence) != localizationReplayEvidenceLimit {
		t.Fatalf("fixture retained %d rows, want %d", len(retained.Evidence), localizationReplayEvidenceLimit)
	}
	current := row(currentID, "run", "BatchGate.run", "repo/gate.go")
	current.Signature = strings.Repeat("current argument ", 70)
	task := "Investigate BatchGate.forceDiscard() during shutdown"
	if !localizationDigestRowTaskCited(task, retainedRows[8]) {
		t.Fatal("qualified fourth owner method was not recognized as concretely cited")
	}
	digest := mergeLocalizationEvidenceDigestForTask(task, []localizationDigestRow{current}, retained)
	wantHead := []string{
		currentID,
		explicitID,
		"repo/gate.go::BatchGate.prepare",
		"repo/gate.go::BatchGate.resolve",
		"repo/gate.go::BatchGate.drain",
	}
	if len(digest.Evidence) < len(wantHead) {
		t.Fatalf("tight digest retained %d rows, want at least %d: %#v", len(digest.Evidence), len(wantHead), digest.Evidence)
	}
	for index, want := range wantHead {
		if digest.Evidence[index].ID != want {
			t.Fatalf("tight digest row %d = %q, want %q; all=%#v", index, digest.Evidence[index].ID, want, digest.Evidence)
		}
	}
	encoded, err := json.Marshal(digest)
	if err != nil || len(encoded) > localizationDigestMaxBytes || len(digest.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("explicit-priority digest exceeded bounds: bytes=%d final=%d err=%v", len(encoded), len(digest.finalResponse), err)
	}
	if len(digest.Files) != len(digest.Evidence) || len(digest.Symbols) != len(digest.Evidence) {
		t.Fatalf("explicit-priority digest lost positional arrays: files=%d symbols=%d evidence=%d", len(digest.Files), len(digest.Symbols), len(digest.Evidence))
	}
	for index, evidence := range digest.Evidence {
		if evidence.Rank != index+1 || digest.Files[index] != evidence.File || digest.Symbols[index] != evidence.ID {
			t.Fatalf("explicit-priority row %d is not aligned: %#v", index, evidence)
		}
	}
}

func TestTaskAwareDigestMergeCapsOwnerMethodPriority(t *testing.T) {
	method := func(id, name, qualName, file string) localizationDigestRow {
		return localizationDigestRow{ID: id, Name: name, QualName: qualName, Kind: "method", File: file}
	}
	current := method("repo/gate.go::BatchGate.run", "run", "BatchGate.run", "repo/gate.go")
	first := method("repo/gate.go::BatchGate.prepare", "prepare", "BatchGate.prepare", "repo/gate.go")
	second := method("repo/gate.go::BatchGate.resolve", "resolve", "BatchGate.resolve", "repo/gate.go")
	third := method("repo/gate.go::BatchGate.drain", "drain", "BatchGate.drain", "repo/gate.go")
	overflow := method("repo/gate.go::BatchGate.shutdownQueue", "shutdownQueue", "BatchGate.shutdownQueue", "repo/gate.go")
	distinctTask := method("repo/planner.go::ShutdownPlanner.apply", "apply", "ShutdownPlanner.apply", "repo/planner.go")
	distinctFile := method("repo/trace.go::Trace.record", "record", "Trace.record", "repo/trace.go")
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		first, second, third, overflow, distinctTask, distinctFile,
	}}
	ordered := localizationTaskAwareRetainedRows("shutdown planner selection fails", []localizationDigestRow{current}, retained)
	wantHead := []string{first.ID, second.ID, third.ID, distinctTask.ID}
	for index, want := range wantHead {
		if len(ordered) <= index || ordered[index].ID != want {
			t.Fatalf("priority row %d = %#v, want %q; all=%#v", index, ordered, want, ordered)
		}
	}
	overflowIndex := -1
	distinctIndex := -1
	for index, row := range ordered {
		switch row.ID {
		case overflow.ID:
			overflowIndex = index
		case distinctTask.ID:
			distinctIndex = index
		}
	}
	if overflowIndex <= distinctIndex {
		t.Fatalf("owner overflow outranked distinct task evidence: %#v", ordered)
	}
}

func TestTaskAwareDigestMergeDoesNotCohortQualifiedFunctions(t *testing.T) {
	current := localizationDigestRow{
		ID: "repo/queue.go::queue.flush", Name: "flush", QualName: "queue.flush", Kind: "function", File: "repo/queue.go",
	}
	packageFunction := localizationDigestRow{
		ID: "repo/queue.go::queue.discard", Name: "discard", QualName: "queue.discard", Kind: "function", File: "repo/queue.go",
	}
	distinctTask := localizationDigestRow{
		ID: "repo/planner.go::ShutdownPlanner.apply", Name: "apply", QualName: "ShutdownPlanner.apply", Kind: "method", File: "repo/planner.go",
	}
	ordered := localizationTaskAwareRetainedRows(
		"shutdown planner selection fails",
		[]localizationDigestRow{current},
		&localizationEvidenceDigest{Evidence: []localizationDigestRow{packageFunction, distinctTask}},
	)
	if len(ordered) != 2 || ordered[0].ID != distinctTask.ID || ordered[1].ID != packageFunction.ID {
		t.Fatalf("package-qualified functions formed a false owner cohort: %#v", ordered)
	}
}

func TestDigestByteCapRetainsSingleMandatoryRowAfterSheddingOptionalFields(t *testing.T) {
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1,
		ID:   "repo/registry.go::Registry.Configure",
		// Each optional field scales with the retention cap so every shed
		// step is still forced: after callers, callees, and the signature go,
		// the qual-name alone still busts the cap and must go too.
		Name:      strings.Repeat("n", 1000),
		QualName:  strings.Repeat("q", localizationDigestMaxBytes),
		Kind:      "method",
		File:      "repo/registry.go",
		Line:      17,
		Signature: strings.Repeat("s", 2*localizationDigestMaxBytes),
		Callers:   []string{strings.Repeat("caller", localizationDigestMaxBytes/6)},
		Callees:   []string{strings.Repeat("callee", localizationDigestMaxBytes/6)},
	}}}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != 1 {
		t.Fatalf("mandatory evidence rows = %d, want 1", len(digest.Evidence))
	}
	row := digest.Evidence[0]
	if row.ID != envelope.Evidence[0].ID || row.File != envelope.Evidence[0].File || row.Line != envelope.Evidence[0].Line {
		t.Fatalf("mandatory row identity changed while shedding: %#v", row)
	}
	if row.Signature != "" || row.QualName != "" || len(row.Callers) != 0 || len(row.Callees) != 0 {
		t.Fatalf("largest optional fields were retained after the digest fit: %#v", row)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("shed digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
}

func TestDigestPathologicalIdentityFallsBackToBoundedEmptyEvidence(t *testing.T) {
	oversized := strings.Repeat("x", localizationDigestMaxBytes+1)
	digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1, ID: oversized, File: oversized, Line: 1,
	}}})
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal pathological digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("pathological digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(digest.Evidence) != 0 || len(digest.Files) != 0 || len(digest.Symbols) != 0 {
		t.Fatalf("oversized irreducible row survived fallback: %#v", digest)
	}
	want := renderLocalizationFinalResponse(nil)
	if digest.finalResponse != want {
		t.Fatalf("pathological fallback final_response = %q, want %q", digest.finalResponse, want)
	}
	completion := newLocalizationCompletion(true, "")
	completion.digest = digest
	result := localizationAnswerReadyResult(completion)
	contract := requireLocalizationTerminalReplay(t, result, "search", "symbols")
	if contract.Completion.FinalResponse != want {
		t.Fatalf("pathological replay final_response = %q, want %q", contract.Completion.FinalResponse, want)
	}
}

func TestPostTerminalReadsAreIntercepted(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	blocked, reserved := state.authorize("read", "source", map[string]any{
		"target": map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
	})
	if reserved {
		t.Fatal("post-terminal read reserved a handler")
	}
	requireLocalizationTerminalReplay(t, blocked, "read", "source")
}

func TestLocalizationDigestKeepsOnlyConcreteBoundedEvidence(t *testing.T) {
	envelope := localizationExploreEnvelope{
		Files:   []string{"pkg/unsupported.go", "pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go", "pkg/f.go"},
		Symbols: []string{"repo/pkg/unsupported.go::Unsupported", "repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E", "repo/pkg/f.go::F"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/pkg/a.go::A", Name: "A", Kind: "function", File: "pkg/a.go", Line: 10, Callers: []string{"repo/pkg/caller.go::CallA"}},
			{Rank: 2, ID: "repo/pkg/b.go::B", Name: "B", Kind: "method", File: "pkg/b.go", Line: 20},
			{Rank: 3, ID: "repo/pkg/c.go::C", Name: "C", Kind: "method", File: "pkg/c.go", Line: 30},
			{Rank: 4, ID: "repo/pkg/d.go::D", Name: "D", Kind: "method", File: "pkg/d.go", Line: 40},
			{Rank: 5, ID: "repo/pkg/e.go::E", Name: "E", Kind: "method", File: "pkg/e.go", Line: 50},
			{Rank: 6, ID: "repo/pkg/f.go::F", Name: "F", Kind: "method", File: "pkg/f.go", Line: 60},
			{Rank: 7, ID: "repo/pkg/missing.go::Missing", Name: "Missing"},
		},
	}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != 6 {
		t.Fatalf("retained evidence = %d, want 6", len(digest.Evidence))
	}
	wantFiles := []string{"pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go", "pkg/f.go"}
	wantSymbols := []string{"repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E", "repo/pkg/f.go::F"}
	if !reflect.DeepEqual(digest.Files, wantFiles) {
		t.Fatalf("digest files = %#v, want %#v", digest.Files, wantFiles)
	}
	if !reflect.DeepEqual(digest.Symbols, wantSymbols) {
		t.Fatalf("digest symbols = %#v, want %#v", digest.Symbols, wantSymbols)
	}
	if got := digest.Evidence[0].Callers; !reflect.DeepEqual(got, []string{"repo/pkg/caller.go::CallA"}) {
		t.Fatalf("causal provenance was dropped: %#v", got)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if strings.Contains(string(encoded), "final_response") || strings.Contains(string(encoded), "unsupported") {
		t.Fatalf("digest retained an unsupported or prewritten answer field: %s", encoded)
	}
}

func TestLocalizationFinalResponseKeepsDuplicateFilesPositionallyAligned(t *testing.T) {
	digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Evidence: []localizationEvidence{
		{Rank: 7, ID: "repo/pkg/shared.go::First", File: "pkg/shared.go", Line: 11, Source: "first body"},
		{Rank: 9, ID: "repo/pkg/shared.go::Second", File: "pkg/shared.go", Line: 27, Source: "second body"},
		{Rank: 12, ID: "repo/pkg/other.go::Third", File: "pkg/other.go", Line: 3, Source: "third body"},
	}})
	wantFiles := []string{"pkg/shared.go", "pkg/shared.go", "pkg/other.go"}
	wantSymbols := []string{"repo/pkg/shared.go::First", "repo/pkg/shared.go::Second", "repo/pkg/other.go::Third"}
	if !reflect.DeepEqual(digest.Files, wantFiles) || !reflect.DeepEqual(digest.Symbols, wantSymbols) {
		t.Fatalf("positional digest = files %#v symbols %#v", digest.Files, digest.Symbols)
	}
	for index, row := range digest.Evidence {
		marker := fmt.Sprintf("- PRIMARY — %s:%d — %s", row.File, row.Line, row.ID)
		if row.Rank != index+1 || !strings.Contains(digest.finalResponse, marker) {
			t.Fatalf("row %d not aligned in final_response:\n%s", index, digest.finalResponse)
		}
	}
	for _, parallelSection := range []string{"FILES:", "SYMBOLS:", "EVIDENCE:", "#1"} {
		if strings.Contains(digest.finalResponse, parallelSection) {
			t.Fatalf("final_response retained ambiguous parallel section %q:\n%s", parallelSection, digest.finalResponse)
		}
	}
	encoded, err := json.Marshal(digest)
	if err != nil || strings.Contains(string(encoded), "body") || strings.Contains(digest.finalResponse, "body") {
		t.Fatalf("retained digest leaked source: %s (%v)", encoded, err)
	}
}

func TestLocalizationFinalResponseCapsRolesAndPrioritizesProofRelations(t *testing.T) {
	rows := make([]localizationDigestRow, localizationReplayEvidenceLimit)
	for index := range rows {
		rows[index] = localizationDigestRow{
			ID:   fmt.Sprintf("repo/pkg/file%02d.go::Worker%02d.Run", index, index),
			Name: "Run", QualName: fmt.Sprintf("Worker%02d.Run", index),
			Kind: "method", File: fmt.Sprintf("pkg/file%02d.go", index), Line: index + 10,
		}
	}
	// Proof-bearing provenance sits in the window's tail, past every seat the
	// primary block could reach on rank alone.
	target, route, callee := len(rows)-3, len(rows)-2, len(rows)-1
	rows[target].Provenance = localizationProvenanceImplementationTarget
	rows[route].Provenance = localizationProvenanceImplementationRoute
	rows[callee].Provenance = "direct_callee"

	response := renderLocalizationFinalResponse(rows)
	if got := strings.Count(response, "- PRIMARY —"); got != localizationFinalResponsePrimaryLimit {
		t.Fatalf("primary rows = %d, want %d:\n%s", got, localizationFinalResponsePrimaryLimit, response)
	}
	if got := strings.Count(response, "- SUPPORTING —"); got != localizationFinalResponseSupportingLimit {
		t.Fatalf("supporting rows = %d, want %d:\n%s", got, localizationFinalResponseSupportingLimit, response)
	}
	for _, want := range []string{
		fmt.Sprintf("- PRIMARY — pkg/file%02d.go:%d — repo/pkg/file%02d.go::Worker%02d.Run", target, target+10, target, target),
		fmt.Sprintf("- SUPPORTING — pkg/file%02d.go:%d — repo/pkg/file%02d.go::Worker%02d.Run", route, route+10, route, route),
		fmt.Sprintf("- SUPPORTING — pkg/file%02d.go:%d — repo/pkg/file%02d.go::Worker%02d.Run", callee, callee+10, callee, callee),
	} {
		if !strings.Contains(response, want) {
			t.Fatalf("bounded response omitted prioritized tuple %q:\n%s", want, response)
		}
	}
	for _, line := range strings.Split(response, "\n") {
		if strings.HasPrefix(line, "- ") && strings.Count(line, " — ") != 2 {
			t.Fatalf("response row is not one aligned role/file/symbol tuple: %q", line)
		}
	}
	if !strings.HasSuffix(response, localizationAnswerReadyDirective) || strings.Contains(response, "#1") {
		t.Fatalf("response lost the stable directive or restored ordinal cross-references:\n%s", response)
	}
}

func TestLocalizationFinalResponsePromotesTaskAlignedSameOwnerMethod(t *testing.T) {
	current := localizationDigestRow{
		ID: "repo/gate.go::BatchGate.drainPending", Name: "drainPending",
		QualName: "BatchGate.drainPending", Kind: "method", File: "repo/gate.go", Line: 80,
	}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		{ID: "repo/gate.go::BatchGate.prepare", Name: "prepare", QualName: "BatchGate.prepare", Kind: "method", File: "repo/gate.go", Line: 25},
		{ID: "repo/gate.go::BatchGate.discardPending", Name: "discardPending", QualName: "BatchGate.discardPending", Kind: "method", File: "repo/gate.go", Line: 64},
		{ID: "repo/trace.go::Trace.record", Name: "record", QualName: "Trace.record", Kind: "method", File: "repo/trace.go", Line: 11},
	}}
	digest := mergeLocalizationEvidenceDigestForTask(
		"discard pending entries during shutdown",
		[]localizationDigestRow{current},
		retained,
	)
	for _, want := range []string{
		"- PRIMARY — repo/gate.go:80 — repo/gate.go::BatchGate.drainPending",
		"- PRIMARY — repo/gate.go:64 — repo/gate.go::BatchGate.discardPending",
	} {
		if !strings.Contains(digest.finalResponse, want) {
			t.Fatalf("same-owner response omitted %q:\n%s", want, digest.finalResponse)
		}
	}
}

func TestLocalizationFinalResponsePromotesIdentifierOnlyTaskStem(t *testing.T) {
	current := localizationDigestRow{
		ID: "repo/command.go::Command.run", Name: "run", QualName: "Command.run",
		Kind: "method", File: "repo/command.go", Line: 30,
	}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		{ID: "repo/writer.go::Writer.flush", Name: "flush", QualName: "Writer.flush", Kind: "method", File: "repo/writer.go", Line: 15},
		{ID: "repo/formatter.go::Formatter.applyAll", Name: "applyAll", QualName: "Formatter.applyAll", Kind: "method", File: "repo/formatter.go", Line: 52},
		{ID: "repo/planner.go::Planner.commit", Name: "commit", QualName: "Planner.commit", Kind: "method", File: "repo/planner.go", Line: 70},
	}}
	digest := mergeLocalizationEvidenceDigestForTask(
		"format the generated document",
		[]localizationDigestRow{current},
		retained,
	)
	want := "- PRIMARY — repo/formatter.go:52 — repo/formatter.go::Formatter.applyAll"
	if !strings.Contains(digest.finalResponse, want) {
		t.Fatalf("identifier task-stem response omitted %q:\n%s", want, digest.finalResponse)
	}
}

func TestLocalizationAnswerReadyResultReplaysAlignedStableContract(t *testing.T) {
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	result := localizationAnswerReadyResult(completion)
	contract := requireLocalizationTerminalReplay(t, result, "", "")
	wantRows := []string{
		"LOCALIZATION:\n",
		"- PRIMARY — repo/storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load",
		"- PRIMARY — repo/storage/cloud.go:17 — repo/storage/cloud.go::CloudStorage.Load",
	}
	for _, want := range wantRows {
		if !strings.Contains(contract.Completion.FinalResponse, want) {
			t.Fatalf("final_response omitted aligned rows %q:\n%s", want, contract.Completion.FinalResponse)
		}
	}
	visible, _ := singleTextContent(result)
	if strings.Contains(strings.ToLower(visible), "source") || len(contract.Completion.FinalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("terminal replay leaked source or exceeded cap: %q", visible)
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("terminal result omitted host-only metadata")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || host.Evidence == nil || host.FallbackFormat == "" || host.Evidence.Evidence[0].File != "repo/storage/disk.go" {
		t.Fatalf("host-only fallback envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	if host.Contract.Completion.FinalResponse != contract.Completion.FinalResponse {
		t.Fatalf("host and visible final_response disagree: host=%q visible=%q", host.Contract.Completion.FinalResponse, contract.Completion.FinalResponse)
	}
}

func TestAttachLocalizationHostEnvelopePreservesPreterminalStructuredContent(t *testing.T) {
	original := map[string]any{
		"payload":    map[string]any{"value": "kept"},
		"completion": map[string]any{"legacy": true},
		"terminal":   "unchanged",
	}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original structured content: %v", err)
	}
	result := mcpgo.NewToolResultText("preterminal payload")
	result.StructuredContent = original
	completion := newLocalizationRecoveryCompletion()
	attached := attachLocalizationHostEnvelope(result, completion, testEvidenceDigest())
	after, err := json.Marshal(attached.StructuredContent)
	if err != nil {
		t.Fatalf("marshal attached structured content: %v", err)
	}
	if !reflect.DeepEqual(attached.StructuredContent, original) || !reflect.DeepEqual(after, before) {
		t.Fatalf("preterminal structured content changed: before=%s after=%s shape=%#v", before, after, attached.StructuredContent)
	}
	host, ok := attached.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("preterminal host envelope missing: %#v", attached.Meta)
	}
	if host.Contract.Terminal || host.Contract.Completion.State != localizationStateNeedsRecovery {
		t.Fatalf("preterminal host contract was terminally enriched: %#v", host.Contract)
	}
	// A recovery state hands back its candidates, but as the unconfirmed page —
	// never carrying the directive that tells a caller to stop navigating.
	if strings.Contains(host.Contract.Completion.FinalResponse, localizationAnswerReadyDirective) {
		t.Fatalf("preterminal page carried the terminal directive: %q", host.Contract.Completion.FinalResponse)
	}
}

func TestDecorateLocalizationReadResultPreservesMultiContentStructuredAndMeta(t *testing.T) {
	structured := map[string]any{"source": "func Run() {}", "count": 2, "terminal": false}
	result := &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.NewTextContent(`{"source":"func Run() {}","completion":{"state":"stale"},"terminal":false}`),
			mcpgo.NewTextContent("secondary payload"),
		},
		StructuredContent: structured,
	}
	result.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"existing":              "kept",
		localizationHostMetaKey: "repository-controlled-spoof",
	})

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if decorated != result {
		t.Fatal("decorator must preserve the result object and its non-text payload")
	}
	if len(decorated.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(decorated.Content))
	}
	first, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok {
		t.Fatal("first content block stopped being text")
	}
	var firstPayload map[string]any
	if err := json.Unmarshal([]byte(first.Text), &firstPayload); err != nil {
		t.Fatalf("decorated first content is invalid JSON: %v", err)
	}
	if firstPayload["source"] != "func Run() {}" || firstPayload["completion"] == nil || firstPayload["terminal"] != true {
		t.Fatalf("decorated first payload = %#v", firstPayload)
	}
	if strings.Count(first.Text, `"completion"`) != 1 {
		t.Fatalf("decorated first payload contains duplicate completion keys: %s", first.Text)
	}
	second, ok := mcpgo.AsTextContent(decorated.Content[1])
	if !ok || second.Text != "secondary payload" {
		t.Fatalf("secondary content was lost: %#v", decorated.Content[1])
	}
	decoratedStructured, ok := decorated.StructuredContent.(map[string]any)
	if !ok || decoratedStructured["source"] != structured["source"] || decoratedStructured["completion"] == nil || decoratedStructured["terminal"] != true {
		t.Fatalf("structured payload was lost: %#v", decorated.StructuredContent)
	}
	if _, mutated := structured["completion"]; mutated {
		t.Fatal("decorator mutated the handler's structured map")
	}
	if decorated.Meta == nil || decorated.Meta.AdditionalFields["existing"] != "kept" {
		t.Fatalf("existing metadata was lost: %#v", decorated.Meta)
	}
	host, ok := decorated.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Contract.Terminal || host.Contract.Completion.ContractVersion != localizationTerminalContractV2 {
		t.Fatalf("authoritative metadata was not replaced: %#v", decorated.Meta.AdditionalFields[localizationHostMetaKey])
	}
}

func TestDecorateLocalizationReadResultPreservesStructuredOnlyPayload(t *testing.T) {
	payload := []any{"first", map[string]any{"second": true}}
	result := &mcpgo.CallToolResult{StructuredContent: payload}

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if len(decorated.Content) != 1 {
		t.Fatalf("completion content blocks = %d, want 1", len(decorated.Content))
	}
	completionText, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok || !strings.Contains(completionText.Text, `"state":"answer_ready"`) {
		t.Fatalf("structured-only result omitted visible completion: %#v", decorated.Content)
	}
	wrapped, ok := decorated.StructuredContent.(map[string]any)
	if !ok || !reflect.DeepEqual(wrapped["payload"], payload) || wrapped["completion"] == nil {
		t.Fatalf("structured-only payload was lost: %#v", decorated.StructuredContent)
	}
}

func TestDecorateLocalizationReadResultMirrorsContentOnlyPayloadForHosts(t *testing.T) {
	completion := newLocalizationCompletion(true, "")

	plain := mcpgo.NewToolResultText("func Load() {}")
	decoratedPlain := decorateLocalizationReadResult(plain, completion)
	plainStructured, ok := decoratedPlain.StructuredContent.(map[string]any)
	if !ok || plainStructured["text"] != "func Load() {}" || plainStructured["completion"] == nil {
		t.Fatalf("plain content was not mirrored for structured-first hosts: %#v", decoratedPlain.StructuredContent)
	}
	plainText, ok := singleTextContent(decoratedPlain)
	if !ok || !strings.Contains(plainText, "func Load() {}") {
		t.Fatalf("plain content block was lost: %#v", decoratedPlain.Content)
	}

	multi := &mcpgo.CallToolResult{Content: []mcpgo.Content{
		mcpgo.NewTextContent("primary"),
		mcpgo.NewTextContent("secondary"),
	}}
	decoratedMulti := decorateLocalizationReadResult(multi, completion)
	multiStructured, ok := decoratedMulti.StructuredContent.(map[string]any)
	mirrored, mirroredOK := multiStructured["content"].([]mcpgo.Content)
	if !ok || !mirroredOK || len(mirrored) != 2 || multiStructured["completion"] == nil {
		t.Fatalf("multi-content payload was not mirrored: %#v", decoratedMulti.StructuredContent)
	}
	if len(decoratedMulti.Content) != 2 {
		t.Fatalf("multi-content blocks = %d, want 2", len(decoratedMulti.Content))
	}
}

func TestPermittedRefinementReadInvokesHandlerAndPreservesPayload(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "refinement_read_payload")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	searchSpec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}

	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	readCalls := 0
	searchCalls := 0
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		readCalls++
		result := mcpgo.NewToolResultText(`{"source":"func (s *DiskStorage) Load() {}","language":"go"}`)
		result.Meta = mcpgo.NewMetaFromMap(map[string]any{"handler": "kept"})
		return result, nil
	})
	srv.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		searchCalls++
		return mcpgo.NewToolResultText("unexpected search result"), nil
	})
	srv.localizationFor(ctx).armRefinementRoutesForTask(
		"find the storage load implementation",
		symbol,
		[]string{symbol},
		map[string]localizationRefinementRoute{symbol: {enforceable: true}},
		testEvidenceDigest(),
	)

	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": symbol},
		},
	}}
	result, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("permitted refinement read = (%+v, %v)", result, err)
	}
	if readCalls != 1 {
		t.Fatalf("read handler calls = %d, want 1", readCalls)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	first, ok := mcpgo.AsTextContent(result.Content[0])
	if !ok || !strings.Contains(first.Text, `"source":"func (s *DiskStorage) Load() {}"`) ||
		!strings.Contains(first.Text, `"state":"answer_ready"`) {
		t.Fatalf("decorated first payload = %#v", result.Content[0])
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "func (s *DiskStorage) Load() {}" ||
		structured["language"] != "go" || structured["completion"] == nil {
		t.Fatalf("structured source payload was lost: %#v", result.StructuredContent)
	}
	if result.Meta == nil || result.Meta.AdditionalFields["handler"] != "kept" {
		t.Fatalf("handler metadata was lost: %#v", result.Meta)
	}
	// This completed read is the answer_ready PostToolUse event observed by a
	// host. Capture its exact terminal payload before deliberately issuing one
	// more navigation call below.
	initialEncoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal initial terminal contract: %v", err)
	}
	var initialContract localizationTerminalContract
	if err := json.Unmarshal(initialEncoded, &initialContract); err != nil {
		t.Fatalf("decode initial terminal contract: %v", err)
	}
	initialHost, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !initialContract.Terminal || initialContract.Completion.FinalResponse == "" {
		t.Fatalf("answer_ready PostToolUse envelope = contract %#v host %#v", initialContract, result.Meta)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal MCP result: %v", err)
	}
	var roundTrip struct {
		Content           []map[string]any `json:"content"`
		StructuredContent map[string]any   `json:"structuredContent"`
	}
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatalf("unmarshal MCP result: %v", err)
	}
	if len(roundTrip.Content) != 1 || roundTrip.StructuredContent["source"] != "func (s *DiskStorage) Load() {}" ||
		roundTrip.StructuredContent["language"] != "go" || roundTrip.StructuredContent["completion"] == nil {
		t.Fatalf("wire result lost source payload: %s", wire)
	}

	searchRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"operation": "  SyMbOlS  ", "query": "Load"},
	}}
	terminal, err := srv.handleFacade(ctx, "search", searchRequest)
	if err != nil {
		t.Fatalf("post-refinement navigation error = %v", err)
	}
	if searchCalls != 0 {
		t.Fatalf("search handler calls after answer_ready = %d, want 0", searchCalls)
	}
	replayContract := requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
	replayHost, ok := terminal.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("post-terminal replay host envelope missing: %#v", terminal.Meta)
	}
	if replayContract.Completion.FinalResponse != initialContract.Completion.FinalResponse ||
		replayHost.Contract.Completion.FinalResponse != initialHost.Contract.Completion.FinalResponse ||
		!reflect.DeepEqual(replayHost.Evidence, initialHost.Evidence) {
		t.Fatalf("host-ignored terminal replay diverged: initial=%#v/%#v replay=%#v/%#v", initialContract, initialHost, replayContract, replayHost)
	}
}

func TestAnswerReadyNavigationDispatchNeverInvokesLegacyHandler(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "terminal_handler_intercept")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	spec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}
	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	changeSpec, ok := srv.facades.operation("change", "detect")
	if !ok {
		t.Fatal("change.detect facade operation is missing")
	}
	handlerCalls := 0
	changeCalls := 0
	srv.facades.capture(mcpgo.NewTool(spec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("expensive", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("source", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(changeSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		changeCalls++
		return mcpgo.NewToolResultText(`{"changed":[]}`), nil
	})
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	srv.localizationFor(ctx).armForTask(completion, "find the storage load implementations")

	request := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "search",
		Arguments: map[string]any{
			"operation": "  SyMbOlS  ",
			"query":     "Load",
		},
	}}
	for repeat := 0; repeat < 10; repeat++ {
		result, err := srv.handleFacade(ctx, "search", request)
		if err != nil {
			t.Fatalf("repeat %d result = (%+v, %v)", repeat, result, err)
		}
		requireLocalizationTerminalReplay(t, result, "search", "symbols")
	}
	if handlerCalls != 0 {
		t.Fatalf("legacy handler invoked %d times after answer_ready", handlerCalls)
	}
	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
		},
	}}
	readResult, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil {
		t.Fatalf("post-terminal read = (%+v, %v)", readResult, err)
	}
	requireLocalizationTerminalReplay(t, readResult, "read", "source")
	if handlerCalls != 0 {
		t.Fatalf("read handler invoked %d times after answer_ready", handlerCalls)
	}

	// The pre-validation gate also catches malformed recovery attempts instead
	// of spending a turn on schema errors.
	request.Params.Arguments = map[string]any{"operation": "not_an_operation"}
	result, err := srv.handleFacade(ctx, "search", request)
	if err != nil {
		t.Fatalf("malformed post-terminal request = (%+v, %v)", result, err)
	}
	requireLocalizationTerminalReplay(t, result, "search", "not_an_operation")

	changeRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "change",
		Arguments: map[string]any{"operation": "detect"},
	}}
	changeResult, err := srv.handleFacade(ctx, "change", changeRequest)
	if err != nil || changeResult == nil || changeResult.IsError || changeCalls != 1 {
		t.Fatalf("post-terminal change.detect = (%+v, %v), calls=%d", changeResult, err, changeCalls)
	}
	if text, _ := singleTextContent(changeResult); strings.Contains(text, localizationAnswerReadyDirective) {
		t.Fatal("post-terminal change.detect was incorrectly intercepted")
	}

	// Capabilities has a dedicated handler and remains usable after localization.
	capabilitiesFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, capabilitiesFrame))
	if err != nil {
		t.Fatalf("marshal capabilities response: %v", err)
	}
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &called); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}
	if called.Error != nil || called.Result == nil || called.Result.IsError {
		t.Fatalf("terminal capabilities response = error %#v result %#v", called.Error, called.Result)
	}
	if text, _ := singleTextContent(called.Result); strings.Contains(text, localizationAnswerReadyDirective) {
		t.Fatalf("capabilities was incorrectly intercepted: %q", text)
	}
}

// The instruction that says what an answer must look like is the one
// instruction a closing line cannot deliver: by the time it is read, the
// answer is already shaped. It leads every page instead.
func TestAnswerPagesLeadWithTheAnswerShapeDirective(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	pages := map[string]string{
		"proven":      digest.finalResponse,
		"provisional": digest.provisionalResponse,
	}
	for name, page := range pages {
		lines := strings.Split(page, "\n")
		if len(lines) < 3 {
			t.Fatalf("%s page is too short to carry a leading directive: %q", name, page)
		}
		if got := lines[1] + "\n" + lines[2]; got != localizationAnswerShapeDirective {
			t.Fatalf("%s page does not lead with the answer shape: %q", name, got)
		}
		if strings.Index(page, localizationAnswerShapeDirective) > strings.Index(page, "- PRIMARY") {
			t.Fatalf("%s page buried the answer shape below its rows: %q", name, page)
		}
	}
	// Two lines is the whole budget: every byte here is a byte the rows lose.
	if strings.Count(localizationAnswerShapeDirective, "\n") != 1 {
		t.Fatalf("answer shape directive is not two lines: %q", localizationAnswerShapeDirective)
	}
	for _, required := range []string{"2-3 strongest candidates", "Class.method", "unconfirmed"} {
		if !strings.Contains(localizationAnswerShapeDirective, required) {
			t.Fatalf("answer shape directive is missing %q: %q", required, localizationAnswerShapeDirective)
		}
	}
}

// A caller repeating a keyword-indexed constructor or a positional tie-break
// suffix is naming a symbol no reader recognizes. Presentation drops both; the
// identities the graph and the host envelope carry keep their exact spelling.
func TestPresentedIdentitiesDropSyntheticSpellings(t *testing.T) {
	constructor := localizationDigestRow{
		Rank: 1, ID: "src/Foo.java::Foo.<init>_L42", Name: "Foo.<init>",
		QualName: "Foo.<init>", Kind: "method", File: "src/Foo.java", Line: 42,
	}
	positional := localizationDigestRow{
		Rank: 2, ID: "src/app.ts::handleRequest@17", Name: "handleRequest",
		Kind: "function", File: "src/app.ts", Line: 17,
	}
	digest := provisionalTestDigest(t, constructor, positional)

	for _, page := range []string{digest.finalResponse, digest.provisionalResponse} {
		for _, synthetic := range []string{"<init>", "_L42", "handleRequest@17"} {
			if strings.Contains(page, synthetic) {
				t.Fatalf("page presented the synthetic spelling %q: %q", synthetic, page)
			}
		}
		for _, presented := range []string{"src/Foo.java::Foo constructor", "src/app.ts::handleRequest"} {
			if !strings.Contains(page, presented) {
				t.Fatalf("page does not present %q: %q", presented, page)
			}
		}
	}
	if digest.Evidence[0].ID != constructor.ID || digest.Evidence[1].ID != positional.ID {
		t.Fatalf("presentation changed retained identities: %#v", digest.Evidence)
	}
	for _, id := range []string{constructor.ID, positional.ID} {
		if !slices.Contains(digest.Symbols, id) {
			t.Fatalf("machine symbol list lost %q: %#v", id, digest.Symbols)
		}
	}
}

// The suffix is stripped because it repeats the row's own line column. A name
// that merely ends in a number is a different declaration and keeps it.
func TestPositionalSuffixTrimRequiresTheRowsOwnLine(t *testing.T) {
	tests := []struct {
		name string
		row  localizationDigestRow
		want string
	}{
		{
			name: "declaration named for a cache level",
			row: localizationDigestRow{
				Rank: 1, ID: "src/cache.go::CACHE_L2", Name: "CACHE_L2",
				Kind: "field", File: "src/cache.go", Line: 87,
			},
			want: "src/cache.go::CACHE_L2",
		},
		{
			name: "overload disambiguated by its line",
			row: localizationDigestRow{
				Rank: 1, ID: "src/Cache.java::Cache.get_L87", Name: "Cache.get",
				Kind: "method", File: "src/Cache.java", Line: 87,
			},
			want: "src/Cache.java::Cache.get",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := localizationFinalResponseSymbolLabel(test.row, false); got != test.want {
				t.Fatalf("presented identity = %q, want %q", got, test.want)
			}
		})
	}
}
