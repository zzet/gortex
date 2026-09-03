package graphview

import (
	"fmt"
	"slices"
	"strings"
)

// CapabilityID names one thing a view can answer. A view is rarely complete
// all at once: source bytes are readable long before the syntax graph is
// resolved, and vector search may never be enabled at all. Callers declare
// what they need and get a typed error naming exactly what is missing rather
// than a silently thin answer.
type CapabilityID string

const (
	// CapSourceSnapshot reads file bytes at the view's content snapshot.
	CapSourceSnapshot CapabilityID = "source.snapshot"
	// CapSourceConfig reads the repository configuration that applies to the
	// view (ignore rules, artifact declarations, language settings).
	CapSourceConfig CapabilityID = "source.config"
	// CapSyntaxGraph reads the parsed syntax graph: files, symbols, and the
	// structural edges between them.
	CapSyntaxGraph CapabilityID = "graph.syntax"
	// CapResolutionLocal reads references resolved within one repository.
	CapResolutionLocal CapabilityID = "graph.resolution.local"
	// CapResolutionCrossRepo reads references resolved across repositories.
	CapResolutionCrossRepo CapabilityID = "graph.resolution.cross_repo"
	// CapIncomingEdges reads the reverse index — who points at a node.
	CapIncomingEdges CapabilityID = "graph.incoming_edges"
	// CapSimilarity reads the near-duplicate relation between bodies. It is
	// separate from the syntax graph because it is not derived from one file:
	// the pass that writes it ranks every body against every other, so how
	// much of the repository the producer saw decides what it emits.
	CapSimilarity CapabilityID = "graph.similarity"
	// CapSearchSymbols runs symbol search over the view.
	CapSearchSymbols CapabilityID = "search.symbols"
	// CapSearchContent runs content search over the view.
	CapSearchContent CapabilityID = "search.content"
	// CapSearchVector runs embedding / vector search over the view.
	CapSearchVector CapabilityID = "search.vector"
	// CapSearchText runs trigram-indexed literal and regex search.
	CapSearchText CapabilityID = "search.text"
	// CapLSPReferences answers language-server reference queries.
	CapLSPReferences CapabilityID = "lsp.references"
	// CapLSPDiagnostics reports language-server diagnostics.
	CapLSPDiagnostics CapabilityID = "lsp.diagnostics"
	// CapLSPHover answers language-server hover queries.
	CapLSPHover CapabilityID = "lsp.hover"
	// CapLSPRename performs language-server renames.
	CapLSPRename CapabilityID = "lsp.rename"
	// CapLSPCodeActions lists and applies language-server code actions.
	CapLSPCodeActions CapabilityID = "lsp.code_actions"
)

// knownCapabilities is the closed set of capability ids, in the order
// KnownCapabilities reports them: source, graph, search, then LSP.
var knownCapabilities = []CapabilityID{
	CapSourceSnapshot,
	CapSourceConfig,
	CapSyntaxGraph,
	CapResolutionLocal,
	CapResolutionCrossRepo,
	CapIncomingEdges,
	CapSimilarity,
	CapSearchSymbols,
	CapSearchContent,
	CapSearchVector,
	CapSearchText,
	CapLSPReferences,
	CapLSPDiagnostics,
	CapLSPHover,
	CapLSPRename,
	CapLSPCodeActions,
}

// KnownCapabilities returns every defined capability id, in a stable order.
func KnownCapabilities() []CapabilityID { return slices.Clone(knownCapabilities) }

// Valid reports whether id is a defined capability.
func (id CapabilityID) Valid() bool { return slices.Contains(knownCapabilities, id) }

// CapabilityState is how far along a capability is for one view.
type CapabilityState string

const (
	// StateComplete: the capability answers fully for this view.
	StateComplete CapabilityState = "complete"
	// StateIncomplete: the capability answers, but from partial data — the
	// answer may be missing results.
	StateIncomplete CapabilityState = "incomplete"
	// StateBuilding: the capability is being populated right now; retrying
	// later can turn it complete.
	StateBuilding CapabilityState = "building"
	// StateUnavailable: the capability cannot be served for this view.
	StateUnavailable CapabilityState = "unavailable"
	// StateDisabledByConfig: the capability is switched off by configuration,
	// so it will not become available by waiting.
	StateDisabledByConfig CapabilityState = "disabled_by_config"
)

// Valid reports whether s is a defined capability state.
func (s CapabilityState) Valid() bool {
	switch s {
	case StateComplete, StateIncomplete, StateBuilding, StateUnavailable, StateDisabledByConfig:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is a state that waiting cannot improve.
func (s CapabilityState) Terminal() bool {
	return s == StateUnavailable || s == StateDisabledByConfig
}

// denialRank orders the states by how hard a denial each one is. It is the
// lattice a union over several declarations resolves a disagreement in: the
// hardest denial any of them names is the one the caller has to be told
// about, because the softer one would promise something the view cannot keep.
//
// complete denies nothing and ranks lowest. building is the softest denial —
// it is the one a caller clears by waiting. incomplete outranks it because a
// build that finishes cannot repair data another declaration already truncated,
// so answering "building" over "incomplete" would sell a retry that never pays
// off. Both terminal states outrank both of those for the reason Evaluate
// already prefers them: no amount of waiting clears them, and a caller told to
// retry a capability that is switched off retries forever. Between the two,
// disabled_by_config wins over unavailable because it names a cause the caller
// can act on, which a bare "cannot serve" would erase.
//
// A state outside the vocabulary ranks with unavailable, matching what
// Completeness.State reports for one: a view never serves a capability it
// cannot vouch for.
func (s CapabilityState) denialRank() int {
	switch s {
	case StateComplete:
		return 0
	case StateBuilding:
		return 1
	case StateIncomplete:
		return 2
	case StateDisabledByConfig:
		return 4
	default: // StateUnavailable and anything unrecognised
		return 3
	}
}

// worst returns whichever of s and other is the harder denial.
func (s CapabilityState) worst(other CapabilityState) CapabilityState {
	if other.denialRank() > s.denialRank() {
		return other
	}
	return s
}

// Completeness is what a view can currently answer. A capability absent from
// the map counts as StateUnavailable: a view never serves a capability it has
// not declared.
type Completeness map[CapabilityID]CapabilityState

// State returns the state recorded for id, or StateUnavailable when the view
// declares nothing for it.
func (c Completeness) State(id CapabilityID) CapabilityState {
	if st, ok := c[id]; ok && st.Valid() {
		return st
	}
	return StateUnavailable
}

// IsComplete reports whether id is fully served by this view.
func (c Completeness) IsComplete(id CapabilityID) bool {
	return c.State(id) == StateComplete
}

// CapabilityStatus pairs one capability with the state a view found it in. It
// is what a caller reports for a capability that did not fail the request but
// still shaped the answer.
type CapabilityStatus struct {
	Capability CapabilityID    `json:"capability"`
	State      CapabilityState `json:"state"`
}

// Degraded reports which of caps this view does not serve completely, in the
// order given and with the state each was found in. It is the optional half of
// Evaluate: the same inspection, reported instead of refused.
func (c Completeness) Degraded(caps []CapabilityID) []CapabilityStatus {
	var out []CapabilityStatus
	for _, id := range caps {
		if st := c.State(id); st != StateComplete {
			out = append(out, CapabilityStatus{Capability: id, State: st})
		}
	}
	return out
}

// ParseCapability resolves a wire capability name, refusing anything outside
// the vocabulary so a typo names itself instead of silently requiring nothing.
func ParseCapability(name string) (CapabilityID, error) {
	id := CapabilityID(strings.TrimSpace(name))
	if !id.Valid() {
		return "", NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("%q is not a capability this server knows; see %v", name, knownCapabilities))
	}
	return id, nil
}

// Evaluate checks a request's capability requirements against the view.
//
// It returns nil when every required capability is complete. A required
// capability in a terminal state (unavailable, disabled by configuration, or
// undeclared) yields *CapabilityUnavailableError; a required capability that is
// merely building or incomplete yields *RequiredCapabilityIncompleteError. When
// both kinds are present the terminal one wins, because retrying could never
// clear it. Optional capabilities are reported by neither — they exist in the
// signature so a caller can state its full requirement set in one place.
func (c Completeness) Evaluate(required, optional []CapabilityID) error {
	_ = optional // optional capabilities never fail a request

	var terminal, pending []CapabilityID
	states := make(map[CapabilityID]CapabilityState)
	for _, id := range required {
		st := c.State(id)
		switch {
		case st == StateComplete:
			continue
		case st.Terminal():
			terminal = append(terminal, id)
		default:
			pending = append(pending, id)
		}
		states[id] = st
	}

	switch {
	case len(terminal) > 0:
		return newCapabilityUnavailable(terminal, capabilityStates(terminal, states))
	case len(pending) > 0:
		return newRequiredCapabilityIncomplete(pending, capabilityStates(pending, states))
	default:
		return nil
	}
}

// capabilityStates narrows the collected states down to the reported ids.
func capabilityStates(ids []CapabilityID, states map[CapabilityID]CapabilityState) map[CapabilityID]CapabilityState {
	out := make(map[CapabilityID]CapabilityState, len(ids))
	for _, id := range ids {
		out[id] = states[id]
	}
	return out
}
