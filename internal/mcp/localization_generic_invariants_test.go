package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestConceptAnchorChoosesBoundedRefinementWhileFocusedLookupStaysExact(t *testing.T) {
	const exact = "fixture/worker.go::Worker.Run"
	cases := []struct {
		name        string
		answerReady bool
		task        string
		causalExact string
		want        bool
	}{
		{
			name: "anchor embedded in prose",
			task: "Trace why Worker.Run keeps retrying requests after the queue has already acknowledged successful delivery",
			want: true,
		},
		{name: "focused qualified lookup", task: exact, want: false},
		{name: "focused signature lookup", task: "func (Worker) Run() error", want: false},
		{
			name:        "graph proven causal identity",
			task:        "Trace why Worker.Run keeps retrying requests after the queue has already acknowledged successful delivery",
			causalExact: "fixture/config.go::newWorker",
			want:        false,
		},
		{
			name:        "already answer ready",
			answerReady: true,
			task:        "Trace why Worker.Run keeps retrying requests after the queue has already acknowledged successful delivery",
			want:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exploreLocalizationUsesConceptRefinement(tc.answerReady, tc.task, exact, tc.causalExact); got != tc.want {
				t.Fatalf("concept refinement = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContractReconcilesAfterLateDigestShedding(t *testing.T) {
	const wrapper = "fixture/wrapper.go::Forwarder.Forward"
	const implementation = "fixture/engine.go::Engine.Execute"
	completion := newLocalizationRefinementCompletion(wrapper)
	completion.refinementRoutes = map[string]localizationRefinementRoute{
		wrapper: {implementationSymbol: implementation, enforceable: true},
	}
	digest := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		{ID: wrapper, Name: "Forward", Kind: "method", File: "fixture/wrapper.go", Provenance: localizationProvenanceImplementationRoute},
		{ID: implementation, Name: "Execute", Kind: "method", File: "fixture/engine.go", Provenance: localizationProvenanceImplementationTarget},
	}}
	rebuildLocalizationDigestSkeleton(digest)
	refreshLocalizationDigestResponses(digest, "forward execution", nil)

	contract := localizationContractReconciledWithDigest(completion, digest)
	if contract.Terminal || contract.Completion.State != localizationStateNeedsRefinement {
		t.Fatalf("complete proof was not retained as refinement: %#v", contract)
	}

	// Model the final envelope-fit loop dropping a proof dependency after the
	// first reconciliation pass.
	digest.Evidence = digest.Evidence[:1]
	rebuildLocalizationDigestSkeleton(digest)
	refreshLocalizationDigestResponses(digest, "forward execution", nil)
	contract = localizationContractReconciledWithDigest(contract.Completion, digest)
	if contract.Terminal || contract.Completion.State != localizationStateLocalized || len(contract.Completion.AllowedSymbols) != 0 {
		t.Fatalf("late proof shedding left stale authorization: %#v", contract)
	}
}

func TestDirectRelationProjectionPreservesAuthorizedDirectFloor(t *testing.T) {
	targets := make([]exploreTarget, 0, localizationReplayEvidenceLimit)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		node := &graph.Node{
			ID:       fmt.Sprintf("fixture/direct_%02d.go::Candidate%02d", index, index),
			Name:     fmt.Sprintf("Candidate%02d", index),
			Kind:     graph.KindFunction,
			FilePath: fmt.Sprintf("fixture/direct_%02d.go", index),
		}
		targets = append(targets, exploreTarget{node: node})
	}
	implementation := &graph.Node{
		ID: "fixture/engine.go::ExecuteRequest", Name: "ExecuteRequest",
		Kind: graph.KindFunction, FilePath: "fixture/engine.go",
	}
	targets[0].callees = []*graph.Node{implementation}

	got := interleaveLocalizationDirectRelations("execute request behavior", "", targets)
	if len(got) != localizationReplayEvidenceLimit {
		t.Fatalf("projection length = %d, want %d", len(got), localizationReplayEvidenceLimit)
	}
	seen := make(map[string]bool, len(got))
	for _, target := range got {
		seen[target.node.ID] = true
	}
	for index := 0; index < localizationRefinementAllowedSymbolCap; index++ {
		if id := targets[index].node.ID; !seen[id] {
			t.Fatalf("authorized direct candidate %d was evicted: %s", index+1, id)
		}
	}
	if !seen[implementation.ID] {
		t.Fatalf("bounded relationship slot did not retain implementation %s", implementation.ID)
	}
}

func TestRecoveryMergePreservesPriorAuthorizationWindow(t *testing.T) {
	retained := &localizationEvidenceDigest{}
	for index := 0; index < localizationRefinementAllowedSymbolCap; index++ {
		retained.Evidence = append(retained.Evidence, localizationDigestRow{
			ID:        fmt.Sprintf("fixture/retained_%02d.go::Candidate%02d", index, index),
			File:      fmt.Sprintf("fixture/retained_%02d.go", index),
			Name:      fmt.Sprintf("Candidate%02d", index),
			Signature: strings.Repeat("parameter metadata ", 45),
			Callers:   []string{strings.Repeat("fixture/caller.go::Dispatch", 20)},
			Callees:   []string{strings.Repeat("fixture/store.go::Commit", 20)},
		})
	}
	current := make([]localizationDigestRow, 5)
	for index := range current {
		current[index] = localizationDigestRow{
			ID:        fmt.Sprintf("fixture/recovery_%02d.go::Result%02d", index, index),
			File:      fmt.Sprintf("fixture/recovery_%02d.go", index),
			Signature: strings.Repeat("recovery detail ", 45),
		}
	}
	// A fresh observation of the first retained declaration expands its graph
	// metadata, exercising duplicate union as well as the near-cap window.
	current[0].ID = retained.Evidence[0].ID
	current[0].File = retained.Evidence[0].File
	current[0].Callers = []string{strings.Repeat("fixture/queue.go::Enqueue", 20)}

	merged := mergeLocalizationEvidenceDigest(current, retained)
	seen := make(map[string]bool, len(merged.Evidence))
	for _, row := range merged.Evidence {
		seen[row.ID] = true
	}
	for _, row := range retained.Evidence {
		if !seen[row.ID] {
			t.Fatalf("recovery evicted previously authorized candidate %s", row.ID)
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("merged digest = %d bytes, cap %d", len(encoded), localizationDigestMaxBytes)
	}
}

func TestRecoveryMergeUnionsDuplicateEvidenceMetadata(t *testing.T) {
	const id = "fixture/worker.go::Worker.Run"
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{{
		ID: id, File: "fixture/worker.go", Signature: "func (Worker) Run() error",
		Callers:    []string{"fixture/app.go::Start"},
		Callees:    []string{"fixture/store.go::Store.Flush"},
		Provenance: localizationProvenanceImplementationTarget,
	}}}
	current := []localizationDigestRow{{
		ID: id, Name: "Run", Kind: "method", File: "fixture/worker.go", Line: 21,
		Callers: []string{"fixture/queue.go::Dispatch", "fixture/app.go::Start"},
	}}

	merged := mergeLocalizationEvidenceDigest(current, retained)
	if len(merged.Evidence) != 1 {
		t.Fatalf("duplicate declaration produced %d rows", len(merged.Evidence))
	}
	row := merged.Evidence[0]
	if row.Signature == "" || row.Provenance != localizationProvenanceImplementationTarget {
		t.Fatalf("retained declaration metadata was lost: %#v", row)
	}
	if got, want := row.Callers, []string{"fixture/queue.go::Dispatch", "fixture/app.go::Start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("callers = %#v, want %#v", got, want)
	}
	if got, want := row.Callees, []string{"fixture/store.go::Store.Flush"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("callees = %#v, want %#v", got, want)
	}
}

func TestRecoveryInterceptionCannotInvalidateConcurrentNewUserLocalizeReservation(t *testing.T) {
	stageReplacement := func(t *testing.T, state *localizationTerminalState) uint64 {
		t.Helper()
		state.armForTask(newLocalizationRecoveryCompletion(), "old task")
		token, blocked := state.beginLocalize("replacement task", true)
		if token == 0 || blocked != nil {
			t.Fatalf("replacement reservation = (%d, %#v)", token, blocked)
		}
		state.armForTask(newLocalizationCompletion(true, ""), "replacement task")
		return token
	}
	assertReplacementCommitted := func(t *testing.T, state *localizationTerminalState, token uint64) {
		t.Helper()
		if !state.finishLocalize(token, true) {
			t.Fatal("replacement localization was invalidated by recovery interception")
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.state != localizationStateAnswerReady || state.reservation != nil {
			t.Fatalf("replacement completion was not committed: state=%q reservation=%#v", state.state, state.reservation)
		}
	}

	t.Run("unsupported navigation", func(t *testing.T) {
		state := newLocalizationTerminalState()
		token := stageReplacement(t, state)
		blocked, generation := state.interceptAnswerReady("search", "files", map[string]any{"query": "worker"})
		if blocked == nil || generation != 0 {
			t.Fatalf("concurrent unsupported recovery = (%#v, %d), want in-progress block", blocked, generation)
		}
		assertReplacementCommitted(t, state, token)
	})

	t.Run("schema-invalid recovery ticket", func(t *testing.T) {
		state := newLocalizationTerminalState()
		state.armForTask(newLocalizationRecoveryCompletion(), "old anchor task")
		_, generation := state.interceptAnswerReady("search", "text", map[string]any{"query": "old anchor"})
		if generation == 0 {
			t.Fatal("valid recovery request did not receive a schema-validation ticket")
		}
		token, blocked := state.beginLocalize("replacement task", true)
		if token == 0 || blocked != nil {
			t.Fatalf("replacement reservation = (%d, %#v)", token, blocked)
		}
		state.armForTask(newLocalizationCompletion(true, ""), "replacement task")
		if completion, consumed := state.consumeInvalidRecovery("search", "text", generation); consumed || completion.State != localizationStateInactive {
			t.Fatalf("stale schema ticket consumed replacement reservation: (%#v, %v)", completion, consumed)
		}
		assertReplacementCommitted(t, state, token)
	})
}

// A reserved read that ends in the advisory release commits, and committing
// bumps the generation that a concurrently reserved localize staged against.
// The newer request owns the session's next contract; the finishing read must
// not silently discard it.
func TestAdvisoryReleaseCannotInvalidateConcurrentLocalizeReservation(t *testing.T) {
	state := newLocalizationTerminalState()
	state.armForTask(newLocalizationRecoveryCompletion(), "old task")

	blocked, readToken := state.authorizeWithToken("search", "text",
		map[string]any{"query": "old task"})
	if blocked != nil || readToken == 0 {
		t.Fatalf("recovery authorization = (%#v, %d), want a reserved read token", blocked, readToken)
	}

	localizeToken, refused := state.beginLocalize("replacement task", true)
	if localizeToken == 0 || refused != nil {
		t.Fatalf("replacement reservation = (%d, %#v)", localizeToken, refused)
	}
	state.armForTask(newLocalizationCompletion(true, ""), "replacement task")

	// The recovery page returns evidence that does not prove a task-aligned
	// declaration, so the bounded workflow ends advisory, not answer_ready.
	completion := state.finishReservedReadTokenWithDigest(readToken, true,
		[]localizationDigestRow{{ID: "fixture/other.go::Other.Run", File: "fixture/other.go", Name: "Run"}},
		true)
	if completion.State == localizationStateAnswerReady {
		t.Fatalf("weak recovery terminalized: %#v", completion)
	}

	if !state.finishLocalize(localizeToken, true) {
		t.Fatal("advisory release invalidated the concurrently reserved localize")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.state != localizationStateAnswerReady || state.reservation != nil {
		t.Fatalf("replacement completion was not committed: state=%q reservation=%#v",
			state.state, state.reservation)
	}
}

func TestAcceptedEmptyRecoveryKeepsOneAllowanceThenTerminatesUnconfirmed(t *testing.T) {
	state := newLocalizationTerminalState()
	completion := newLocalizationRecoveryCompletion()
	completion.digest = &localizationEvidenceDigest{Evidence: []localizationDigestRow{{
		ID: "fixture/storage.go::Commit", File: "fixture/storage.go",
	}}}
	state.armForTask(completion, "storage commit stalls")

	blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
	if blocked != nil || token == 0 {
		t.Fatalf("recovery authorization = (%#v, %d)", blocked, token)
	}
	retry := state.finishReservedReadTokenWithDigest(token, true, nil, true)
	if retry.State != localizationStateNeedsRecovery || retry.AllowedToolCalls != 1 || retry.Enforceable {
		t.Fatalf("empty accepted recovery did not keep one further allowance: %#v", retry)
	}

	blocked, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "storage"})
	if blocked != nil || token == 0 {
		t.Fatalf("second recovery authorization = (%#v, %d)", blocked, token)
	}
	spent := state.finishReservedReadTokenWithDigest(token, true, nil, true)
	if spent.State != localizationStateAnswerReady || spent.AllowedToolCalls != 0 || spent.Enforceable {
		t.Fatalf("spent recovery allowance did not terminate the session: %#v", spent)
	}
	if !strings.HasPrefix(spent.FinalResponse, localizationProvisionalHeading) ||
		!strings.HasSuffix(spent.FinalResponse, localizationUnconfirmedAnswerDirective) {
		t.Fatalf("uncorroborated terminal page claimed proof: %q", spent.FinalResponse)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.state != localizationStateAnswerReady {
		t.Fatalf("terminal recovery left session navigable: %q", state.state)
	}
}
