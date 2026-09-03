package graphview

import (
	"errors"
	"slices"
	"testing"
)

func TestKnownCapabilitiesCoverTheVocabulary(t *testing.T) {
	// The wire strings are spelled out rather than referenced, so renaming a
	// constant cannot quietly rename the value clients switch on.
	vocabulary := []struct {
		id   CapabilityID
		wire string
	}{
		{CapSourceSnapshot, "source.snapshot"},
		{CapSourceConfig, "source.config"},
		{CapSyntaxGraph, "graph.syntax"},
		{CapResolutionLocal, "graph.resolution.local"},
		{CapResolutionCrossRepo, "graph.resolution.cross_repo"},
		{CapIncomingEdges, "graph.incoming_edges"},
		{CapSimilarity, "graph.similarity"},
		{CapSearchSymbols, "search.symbols"},
		{CapSearchContent, "search.content"},
		{CapSearchVector, "search.vector"},
		{CapSearchText, "search.text"},
		{CapLSPReferences, "lsp.references"},
		{CapLSPDiagnostics, "lsp.diagnostics"},
		{CapLSPHover, "lsp.hover"},
		{CapLSPRename, "lsp.rename"},
		{CapLSPCodeActions, "lsp.code_actions"},
	}
	want := make([]CapabilityID, 0, len(vocabulary))
	for _, v := range vocabulary {
		if string(v.id) != v.wire {
			t.Errorf("capability %q carries the wire value %q", v.wire, string(v.id))
		}
		want = append(want, CapabilityID(v.wire))
	}
	got := KnownCapabilities()
	if !slices.Equal(got, want) {
		t.Fatalf("KnownCapabilities() = %v, want %v", got, want)
	}
	for _, id := range got {
		if !id.Valid() {
			t.Errorf("%q is listed but not Valid", id)
		}
	}
	if CapabilityID("graph.nonsense").Valid() {
		t.Error("an undefined capability reported itself Valid")
	}
	if CapabilityID("").Valid() {
		t.Error("the empty capability reported itself Valid")
	}

	// The returned slice is a copy: mutating it must not corrupt the package
	// vocabulary for the next caller.
	got[0] = "mutated"
	if KnownCapabilities()[0] != CapSourceSnapshot {
		t.Error("KnownCapabilities() handed out the backing array")
	}
}

func TestCapabilityStateValidAndTerminal(t *testing.T) {
	tests := []struct {
		state    CapabilityState
		valid    bool
		terminal bool
	}{
		{StateComplete, true, false},
		{StateIncomplete, true, false},
		{StateBuilding, true, false},
		{StateUnavailable, true, true},
		{StateDisabledByConfig, true, true},
		{CapabilityState("nonsense"), false, false},
		{CapabilityState(""), false, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.state.Terminal(); got != tc.terminal {
				t.Errorf("Terminal() = %v, want %v", got, tc.terminal)
			}
		})
	}
}

// TestCapabilityStateWorst pins the severity lattice a union resolves a
// disagreement in: the hardest denial wins, in either argument order, and
// an unrecognised state is no softer than unavailable.
func TestCapabilityStateWorst(t *testing.T) {
	tests := []struct {
		a, b CapabilityState
		want CapabilityState
	}{
		{StateComplete, StateComplete, StateComplete},
		{StateComplete, StateBuilding, StateBuilding},
		{StateBuilding, StateIncomplete, StateIncomplete},
		{StateIncomplete, StateUnavailable, StateUnavailable},
		{StateIncomplete, StateDisabledByConfig, StateDisabledByConfig},
		{StateBuilding, StateDisabledByConfig, StateDisabledByConfig},
		{StateUnavailable, StateDisabledByConfig, StateDisabledByConfig},
		{StateComplete, CapabilityState("nonsense"), CapabilityState("nonsense")},
		{StateDisabledByConfig, CapabilityState("nonsense"), StateDisabledByConfig},
	}
	for _, tc := range tests {
		t.Run(string(tc.a)+"+"+string(tc.b), func(t *testing.T) {
			if got := tc.a.worst(tc.b); got != tc.want {
				t.Errorf("%q.worst(%q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
			// The lattice is symmetric: which side a declaration arrives
			// on cannot change which denial a caller is told about.
			if got := tc.b.worst(tc.a); got.denialRank() != tc.want.denialRank() {
				t.Errorf("%q.worst(%q) = %q, want a denial as hard as %q", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestCompletenessStateDefaultsToUnavailable(t *testing.T) {
	c := Completeness{
		CapSyntaxGraph:    StateComplete,
		CapSearchSymbols:  StateBuilding,
		CapLSPDiagnostics: CapabilityState("garbage"),
	}
	if got := c.State(CapSyntaxGraph); got != StateComplete {
		t.Errorf("State(syntax) = %q", got)
	}
	if got := c.State(CapSearchSymbols); got != StateBuilding {
		t.Errorf("State(search.symbols) = %q", got)
	}
	if got := c.State(CapSearchVector); got != StateUnavailable {
		t.Errorf("undeclared capability State = %q, want unavailable", got)
	}
	if got := c.State(CapLSPDiagnostics); got != StateUnavailable {
		t.Errorf("unparseable state = %q, want unavailable", got)
	}
	if !c.IsComplete(CapSyntaxGraph) {
		t.Error("IsComplete(syntax) = false")
	}
	if c.IsComplete(CapSearchSymbols) {
		t.Error("IsComplete(building capability) = true")
	}

	var nilMap Completeness
	if got := nilMap.State(CapSourceSnapshot); got != StateUnavailable {
		t.Errorf("nil Completeness State = %q, want unavailable", got)
	}
	if nilMap.IsComplete(CapSourceSnapshot) {
		t.Error("nil Completeness reported a complete capability")
	}
}

func TestCompletenessEvaluate(t *testing.T) {
	full := Completeness{
		CapSourceSnapshot:      StateComplete,
		CapSourceConfig:        StateComplete,
		CapSyntaxGraph:         StateComplete,
		CapResolutionLocal:     StateComplete,
		CapResolutionCrossRepo: StateBuilding,
		CapIncomingEdges:       StateIncomplete,
		CapSearchSymbols:       StateComplete,
		CapSearchContent:       StateComplete,
		CapSearchVector:        StateDisabledByConfig,
		CapSearchText:          StateUnavailable,
	}

	tests := []struct {
		name     string
		have     Completeness
		required []CapabilityID
		optional []CapabilityID
		wantCode string
		wantCaps []CapabilityID
	}{
		{
			name:     "no requirements",
			have:     full,
			required: nil,
			optional: []CapabilityID{CapSearchVector},
		},
		{
			name:     "all required complete",
			have:     full,
			required: []CapabilityID{CapSourceSnapshot, CapSyntaxGraph, CapResolutionLocal},
		},
		{
			name:     "optional failures never fail",
			have:     full,
			required: []CapabilityID{CapSyntaxGraph},
			optional: []CapabilityID{CapSearchVector, CapSearchText, CapResolutionCrossRepo, CapLSPRename},
		},
		{
			name:     "required unavailable",
			have:     full,
			required: []CapabilityID{CapSyntaxGraph, CapSearchText},
			wantCode: CodeCapabilityUnavailable,
			wantCaps: []CapabilityID{CapSearchText},
		},
		{
			name:     "required disabled by config",
			have:     full,
			required: []CapabilityID{CapSearchVector},
			wantCode: CodeCapabilityUnavailable,
			wantCaps: []CapabilityID{CapSearchVector},
		},
		{
			name:     "required undeclared counts as unavailable",
			have:     full,
			required: []CapabilityID{CapLSPHover},
			wantCode: CodeCapabilityUnavailable,
			wantCaps: []CapabilityID{CapLSPHover},
		},
		{
			name:     "required building",
			have:     full,
			required: []CapabilityID{CapResolutionCrossRepo},
			wantCode: CodeRequiredCapabilityIncomplete,
			wantCaps: []CapabilityID{CapResolutionCrossRepo},
		},
		{
			name:     "required incomplete",
			have:     full,
			required: []CapabilityID{CapIncomingEdges},
			wantCode: CodeRequiredCapabilityIncomplete,
			wantCaps: []CapabilityID{CapIncomingEdges},
		},
		{
			name:     "terminal beats pending",
			have:     full,
			required: []CapabilityID{CapResolutionCrossRepo, CapSearchText, CapIncomingEdges, CapSearchVector},
			wantCode: CodeCapabilityUnavailable,
			wantCaps: []CapabilityID{CapSearchText, CapSearchVector},
		},
		{
			name:     "every failing capability is reported",
			have:     full,
			required: []CapabilityID{CapResolutionCrossRepo, CapIncomingEdges},
			wantCode: CodeRequiredCapabilityIncomplete,
			wantCaps: []CapabilityID{CapResolutionCrossRepo, CapIncomingEdges},
		},
		{
			name:     "nil completeness fails every requirement",
			have:     nil,
			required: []CapabilityID{CapSourceSnapshot},
			wantCode: CodeCapabilityUnavailable,
			wantCaps: []CapabilityID{CapSourceSnapshot},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.have.Evaluate(tc.required, tc.optional)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Evaluate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Evaluate() = nil, want %s", tc.wantCode)
			}
			if got := CodeOf(err); got != tc.wantCode {
				t.Fatalf("CodeOf(Evaluate()) = %q, want %q", got, tc.wantCode)
			}

			var caps []CapabilityID
			var states map[CapabilityID]CapabilityState
			switch tc.wantCode {
			case CodeCapabilityUnavailable:
				typed, ok := errors.AsType[*CapabilityUnavailableError](err)
				if !ok {
					t.Fatalf("Evaluate() = %T, want *CapabilityUnavailableError", err)
				}
				if !errors.Is(err, ErrCapabilityUnavailable) {
					t.Error("error does not match the capability_unavailable sentinel")
				}
				caps, states = typed.Capabilities, typed.States
			case CodeRequiredCapabilityIncomplete:
				typed, ok := errors.AsType[*RequiredCapabilityIncompleteError](err)
				if !ok {
					t.Fatalf("Evaluate() = %T, want *RequiredCapabilityIncompleteError", err)
				}
				if !errors.Is(err, ErrRequiredCapabilityIncomplete) {
					t.Error("error does not match the required_capability_incomplete sentinel")
				}
				caps, states = typed.Capabilities, typed.States
			}

			if !slices.Equal(caps, tc.wantCaps) {
				t.Errorf("failing capabilities = %v, want %v", caps, tc.wantCaps)
			}
			if len(states) != len(tc.wantCaps) {
				t.Errorf("states = %v, want one entry per failing capability", states)
			}
			for _, id := range tc.wantCaps {
				if got, ok := states[id]; !ok || got != tc.have.State(id) {
					t.Errorf("states[%q] = %q (present=%v), want %q", id, got, ok, tc.have.State(id))
				}
			}
		})
	}
}

func TestEvaluateErrorMessageNamesTheCapabilities(t *testing.T) {
	err := Completeness{CapSearchText: StateUnavailable}.
		Evaluate([]CapabilityID{CapSearchText, CapSearchVector}, nil)
	const want = "capability_unavailable: search.text (unavailable), search.vector (unavailable)"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
