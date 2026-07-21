package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const (
	localizationStateInactive          = ""
	localizationStateNeedsExactRead    = "needs_exact_read"
	localizationStateNeedsRefinement   = "needs_refinement"
	localizationStateNeedsRecovery     = "needs_recovery"
	localizationStateRefineInFlight    = "refinement_in_flight"
	localizationStateExactReadInFlight = "exact_read_in_flight"
	localizationStateRecoveryInFlight  = "recovery_in_flight"
	localizationStateAnswerReady       = "answer_ready"
	localizationTerminalContractV2     = 2
)

var localizationRecoveryOperations = []string{"search.text", "search.symbols", "read.source"}

// localizationCompletion is the host-neutral terminality contract returned by
// explore(operation:"localize"). Hosts may stop the turn from this payload;
// the server also enforces it for later Gortex navigation calls in the same
// MCP session.
type localizationCompletion struct {
	State             string   `json:"state"`
	Scope             string   `json:"scope"`
	RequiredAction    string   `json:"required_action"`
	Instruction       string   `json:"instruction,omitempty"`
	FinalResponse     string   `json:"final_response,omitempty"`
	AllowedToolCalls  int      `json:"allowed_tool_calls"`
	ContractVersion   int      `json:"contract_version"`
	Enforceable       bool     `json:"enforceable"`
	ExactSymbol       string   `json:"exact_symbol,omitempty"`
	AllowedSymbols    []string `json:"allowed_symbols,omitempty"`
	AllowedOperations []string `json:"allowed_operations,omitempty"`

	// Route hops stay session-only, while AllowedSymbols exposes the exact
	// bounded authorization set carried by the wire contract.
	refinementSymbol  string
	refinementSymbols []string
	refinementRoutes  map[string]localizationRefinementRoute
	// correctionSymbol is the one ranked alternate fixed when the contract is
	// armed. It is session-only: after a successful advisory read the wire
	// contract exposes it as ExactSymbol instead of opening another search.
	correctionSymbol string
	correctionRoute  localizationRefinementRoute
	// enforceableOnAnswerReady is session-only provenance. A non-terminal
	// completion may carry a prevalidated future verdict through its one
	// authorized read without claiming that the current response is terminal.
	// It defaults false until the evidence policy explicitly opts in.
	enforceableOnAnswerReady bool

	// digest is the bounded evidence projection carried session-only through
	// reservation staging (see localization_digest.go). It rides the completion
	// through reservation staging into commitLocalizationLocked, which covers
	// the direct-arm and facade finishLocalize paths alike. Wire contracts expose
	// only a deep-cloned projection and its deterministic final response.
	digest *localizationEvidenceDigest
}

// localizationRefinementRoute is session-only. A zero implementation symbol
// marks a concrete refinement candidate; a non-empty symbol is the one exact
// concrete hop prevalidated for a generic forwarder.
type localizationRefinementRoute struct {
	implementationSymbol string
	// proofSymbol names the generic wrapper that uniquely and completely
	// resolved to this concrete target. It is empty for ordinary concrete
	// hydration and for the wrapper side of the same route.
	proofSymbol string
	// enforceable is set only by the centralized evidence policy after it has
	// proved the entire route. A successful read alone never upgrades trust.
	enforceable bool
}

// localizationTerminalContract is the single wire shape used in visible MCP
// payloads and authoritative host-only metadata. Hosts must treat _meta as the
// authority; the visible copy remains useful to agents and legacy harnesses.
type localizationTerminalContract struct {
	Completion localizationCompletion `json:"completion"`
	Terminal   bool                   `json:"terminal"`
}

func localizationContractFor(completion localizationCompletion) localizationTerminalContract {
	if completion.ContractVersion == 0 {
		completion.ContractVersion = localizationTerminalContractV2
	}
	if completion.State != localizationStateAnswerReady {
		completion.Enforceable = false
		completion.FinalResponse = ""
	} else {
		completion.Instruction = localizationReplayDirective
		if completion.digest != nil || completion.FinalResponse == "" {
			completion.FinalResponse = buildLocalizationFinalResponse(completion.digest)
		}
	}
	// The digest is state-only. Its stable, bounded wire projections are
	// FinalResponse and the typed evidence_digest payload.
	completion.digest = nil
	return localizationTerminalContract{
		Completion: completion,
		Terminal:   completion.State == localizationStateAnswerReady,
	}
}

func newLocalizationRecoveryCompletion() localizationCompletion {
	return localizationCompletion{
		State:             localizationStateNeedsRecovery,
		Scope:             "localization",
		RequiredAction:    "recover_once",
		Instruction:       `Make exactly one bounded Gortex recovery call: search(operation:"text" or "symbols", query:<specific task anchor>) or read(operation:"source", target:{symbol:<exact id>}); then respond from the returned evidence.`,
		AllowedToolCalls:  1,
		ContractVersion:   localizationTerminalContractV2,
		AllowedOperations: append([]string(nil), localizationRecoveryOperations...),
	}
}

func newLocalizationCompletion(answerReady bool, exactSymbol string) localizationCompletion {
	if answerReady {
		return localizationCompletion{
			State:            localizationStateAnswerReady,
			Scope:            "localization",
			RequiredAction:   "respond",
			AllowedToolCalls: 0,
			ContractVersion:  localizationTerminalContractV2,
		}
	}
	return newLocalizationExactReadCompletion(exactSymbol, false)
}

func newLocalizationExactReadCompletion(exactSymbol string, correction bool) localizationCompletion {
	instruction := fmt.Sprintf(`Call Gortex MCP read(operation:"source", target:{symbol:%q}); then respond.`, exactSymbol)
	if correction {
		instruction = fmt.Sprintf(`Call Gortex MCP read(operation:"source", target:{symbol:%q}); this is the only permitted corrective read; then follow the returned completion.`, exactSymbol)
	}
	return localizationCompletion{
		State:            localizationStateNeedsExactRead,
		Scope:            "localization",
		RequiredAction:   "read_exact",
		Instruction:      instruction,
		AllowedToolCalls: 1,
		ContractVersion:  localizationTerminalContractV2,
		ExactSymbol:      exactSymbol,
	}
}

func newLocalizationOpenCompletion() localizationCompletion {
	return localizationCompletion{
		State:            localizationStateInactive,
		Scope:            "localization",
		RequiredAction:   "continue",
		AllowedToolCalls: 0,
		ContractVersion:  localizationTerminalContractV2,
	}
}

// newLocalizationRefinementCompletion keeps uncertain localization successful
// and bounded. The ranked evidence remains usable, while the server permits
// exactly one source read selected from the explicit wire authorization set.
func newLocalizationRefinementCompletion(preferredSymbol string) localizationCompletion {
	return newLocalizationRefinementCompletionForSymbols(preferredSymbol, []string{preferredSymbol})
}

func newLocalizationRefinementCompletionForSymbols(preferredSymbol string, allowedSymbols []string) localizationCompletion {
	preferredSymbol = strings.TrimSpace(preferredSymbol)
	allowedSymbols = append([]string(nil), allowedSymbols...)
	return localizationCompletion{
		State:             localizationStateNeedsRefinement,
		Scope:             "localization",
		RequiredAction:    fmt.Sprintf(localizationRefinementRequiredActionFormat, preferredSymbol),
		AllowedToolCalls:  1,
		ContractVersion:   localizationTerminalContractV2,
		AllowedSymbols:    allowedSymbols,
		refinementSymbol:  preferredSymbol,
		refinementSymbols: append([]string(nil), allowedSymbols...),
	}
}

func localizationRankedCorrection(
	preferredSymbol string,
	symbols []string,
	routes map[string]localizationRefinementRoute,
) (string, localizationRefinementRoute) {
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" || symbol == preferredSymbol {
			continue
		}
		route, authorized := routes[symbol]
		if !authorized || (!route.enforceable && route.implementationSymbol == "") {
			continue
		}
		return symbol, route
	}
	return "", localizationRefinementRoute{}
}

// localizationTerminalState is intentionally session-local. It bounds only
// localization navigation; mutation, workspace, session, memory, and
// capability tools remain usable after answer_ready and across later work.
type localizationTerminalState struct {
	mu                sync.Mutex
	state             string
	exactSymbol       string
	refinementSymbol  string
	refinementSymbols []string
	refinementRoutes  map[string]localizationRefinementRoute
	correctionSymbol  string
	correctionRoute   localizationRefinementRoute
	// inFlightImplementationSymbol is selected from refinementRoutes when the
	// actual requested candidate is authorized. It is never inferred from the
	// read result.
	inFlightImplementationSymbol string
	inFlightEnforceable          bool
	inFlightCorrectionSymbol     string
	exactReadIsCorrection        bool
	exactReadRoute               localizationRefinementRoute
	correctionRetriesRemaining   uint8
	refinementRetriesRemaining   uint8
	recoveryRetriesRemaining     uint8
	// Read reservations are tokenized independently of localization calls. A
	// reset or newly armed task invalidates an old token, so a late read cannot
	// finish (or decorate itself with) a newer task's contract.
	nextReadReservation  uint64
	readReservationToken uint64
	readReservationGen   uint64
	// enforceableOnAnswerReady persists a proven verdict across an authorized
	// exact/refinement read. Its zero value is deliberately advisory.
	enforceableOnAnswerReady bool
	taskFingerprint          string
	generation               uint64
	nextReservation          uint64
	reservation              *localizationReservation
	// digest is the evidence retained for the live contract; nil when the
	// contract is inactive or predates digest capture. Promotions through
	// finishReservedRead keep it — the evidence was stashed when the
	// contract was armed, before the permitted read ran.
	digest *localizationEvidenceDigest
}

type localizationReservation struct {
	token                  uint64
	generation             uint64
	pendingCompletion      localizationCompletion
	pendingTaskFingerprint string
	staged                 bool
}

func newLocalizationTerminalState() *localizationTerminalState {
	return &localizationTerminalState{}
}

func (s *localizationTerminalState) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.generation++
	s.state = localizationStateInactive
	s.exactSymbol = ""
	s.refinementSymbol = ""
	s.refinementSymbols = nil
	s.refinementRoutes = nil
	s.correctionSymbol = ""
	s.correctionRoute = localizationRefinementRoute{}
	s.inFlightImplementationSymbol = ""
	s.inFlightEnforceable = false
	s.inFlightCorrectionSymbol = ""
	s.exactReadIsCorrection = false
	s.exactReadRoute = localizationRefinementRoute{}
	s.correctionRetriesRemaining = 0
	s.refinementRetriesRemaining = 0
	s.recoveryRetriesRemaining = 0
	s.readReservationToken = 0
	s.readReservationGen = 0
	s.enforceableOnAnswerReady = false
	s.taskFingerprint = ""
	s.digest = nil
	// Keep an in-flight reservation until its owner finishes. Its captured
	// generation is now stale, so finishLocalize cannot commit it, while a
	// second localization cannot race ahead of the still-running handler.
	s.mu.Unlock()
}

func (s *localizationTerminalState) arm(completion localizationCompletion) {
	s.armForTask(completion, "")
}

func (s *localizationTerminalState) armForTask(completion localizationCompletion, task string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fingerprint := localizationTaskFingerprint(task)
	if s.reservation != nil {
		s.reservation.pendingCompletion = completion
		s.reservation.pendingTaskFingerprint = fingerprint
		s.reservation.staged = true
		return
	}
	s.commitLocalizationLocked(completion, fingerprint)
}

// keepOpenForTask transactionally replaces any prior terminal contract with
// inactive navigation state. Under facade dispatch the inactive state is
// staged until the localization response succeeds; direct handlers commit it
// immediately.
func (s *localizationTerminalState) keepOpenForTask(task string) {
	s.armForTask(newLocalizationOpenCompletion(), task)
}

func (s *localizationTerminalState) armRefinementForTask(task, preferredSymbol string, symbols []string, digest *localizationEvidenceDigest) {
	routes := make(map[string]localizationRefinementRoute, len(symbols))
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol != "" {
			routes[symbol] = localizationRefinementRoute{}
		}
	}
	s.armRefinementRoutesForTask(task, preferredSymbol, symbols, routes, digest)
}

func (s *localizationTerminalState) armRefinementRoutesForTask(
	task, preferredSymbol string,
	symbols []string,
	routes map[string]localizationRefinementRoute,
	digest *localizationEvidenceDigest,
) {
	preferredSymbol = strings.TrimSpace(preferredSymbol)
	seen := make(map[string]struct{}, min(len(symbols), localizationRefinementAllowedSymbolCap))
	refinementSymbols := make([]string, 0, min(len(symbols), localizationRefinementAllowedSymbolCap))
	refinementRoutes := make(map[string]localizationRefinementRoute, min(len(symbols), localizationRefinementAllowedSymbolCap))
	for _, symbol := range symbols {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			continue
		}
		route, authorized := routes[symbol]
		if !authorized {
			continue
		}
		route.implementationSymbol = strings.TrimSpace(route.implementationSymbol)
		if route.implementationSymbol == symbol {
			continue
		}
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		refinementSymbols = append(refinementSymbols, symbol)
		refinementRoutes[symbol] = route
		if len(refinementSymbols) == localizationRefinementAllowedSymbolCap {
			break
		}
	}
	if len(refinementSymbols) == 0 {
		s.keepOpenForTask(task)
		return
	}
	if _, exists := seen[preferredSymbol]; !exists {
		s.keepOpenForTask(task)
		return
	}
	completion := newLocalizationRefinementCompletionForSymbols(preferredSymbol, refinementSymbols)
	completion.refinementRoutes = refinementRoutes
	completion.correctionSymbol, completion.correctionRoute = localizationRankedCorrection(
		preferredSymbol,
		refinementSymbols,
		refinementRoutes,
	)
	// Stashed now, not at promotion: when the permitted read succeeds,
	// finishReservedRead flips this contract to answer_ready and the
	// evidence must already be retained for replay.
	completion.digest = digest
	s.armForTask(completion, task)
}

func (s *localizationTerminalState) commitLocalizationLocked(completion localizationCompletion, fingerprint string) {
	s.generation++
	s.state = completion.State
	s.exactSymbol = completion.ExactSymbol
	s.refinementSymbol = ""
	s.refinementSymbols = nil
	s.refinementRoutes = nil
	s.inFlightImplementationSymbol = ""
	s.inFlightEnforceable = false
	s.inFlightCorrectionSymbol = ""
	s.exactReadIsCorrection = false
	s.exactReadRoute = localizationRefinementRoute{}
	s.correctionRetriesRemaining = 0
	s.refinementRetriesRemaining = 0
	s.recoveryRetriesRemaining = 0
	s.readReservationToken = 0
	s.readReservationGen = 0
	s.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
	if completion.State == localizationStateAnswerReady {
		s.enforceableOnAnswerReady = completion.Enforceable
	}
	if completion.State == localizationStateNeedsRefinement {
		s.refinementSymbol = completion.refinementSymbol
		s.refinementSymbols = append([]string(nil), completion.refinementSymbols...)
		s.refinementRoutes = cloneLocalizationRefinementRoutes(completion.refinementRoutes)
		s.correctionSymbol = completion.correctionSymbol
		s.correctionRoute = completion.correctionRoute
		s.refinementRetriesRemaining = 1
	} else {
		s.correctionSymbol = ""
		s.correctionRoute = localizationRefinementRoute{}
	}
	if completion.State == localizationStateNeedsRecovery {
		s.recoveryRetriesRemaining = 1
	}
	s.taskFingerprint = fingerprint
	// The digest follows the contract: an inactive commit (keepOpenForTask)
	// carries nil and clears it; every localize commit replaces it.
	s.digest = cloneLocalizationEvidenceDigest(completion.digest)
}

func (s *localizationTerminalState) completionLocked() localizationCompletion {
	var completion localizationCompletion
	switch s.state {
	case localizationStateNeedsRefinement, localizationStateRefineInFlight:
		completion = newLocalizationRefinementCompletionForSymbols(s.refinementSymbol, s.refinementSymbols)
		completion.refinementRoutes = cloneLocalizationRefinementRoutes(s.refinementRoutes)
		if s.state == localizationStateRefineInFlight {
			completion.State = localizationStateRefineInFlight
			completion.AllowedToolCalls = 0
		}
	case localizationStateNeedsExactRead, localizationStateExactReadInFlight:
		completion = newLocalizationExactReadCompletion(s.exactSymbol, s.exactReadIsCorrection)
	case localizationStateNeedsRecovery, localizationStateRecoveryInFlight:
		completion = newLocalizationRecoveryCompletion()
		if s.state == localizationStateRecoveryInFlight {
			completion.State = localizationStateRecoveryInFlight
			completion.RequiredAction = "wait"
			completion.Instruction = "The bounded Gortex recovery call is already in progress."
			completion.AllowedToolCalls = 0
		}
	case localizationStateInactive:
		completion = newLocalizationOpenCompletion()
	default:
		completion = newLocalizationCompletion(true, "")
	}
	completion.enforceableOnAnswerReady = s.enforceableOnAnswerReady
	if completion.State == localizationStateAnswerReady {
		completion.Enforceable = s.enforceableOnAnswerReady
	}
	completion.digest = cloneLocalizationEvidenceDigest(s.digest)
	if completion.State == localizationStateAnswerReady {
		digest := completion.digest
		completion = localizationContractFor(completion).Completion
		completion.digest = digest
	}
	return completion
}

// replayAnswerReady handles malformed boundary metadata without changing a
// pre-terminal recovery/refinement state. Once terminal, every navigation shape
// receives the same successful replay before argument validation can emit an
// error.
func (s *localizationTerminalState) replayAnswerReady(facade, operation string) *mcpgo.CallToolResult {
	if s == nil || !localizationNavigationFacade(facade) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != localizationStateAnswerReady {
		return nil
	}
	return localizationTerminalResult(s.completionLocked(), facade, operation)
}

// interceptAnswerReady is the cheap pre-validation gate used by facade
// dispatch. It makes localization terminality independent of operation
// validity, and consumes an unsupported advisory recovery attempt before a
// schema error can create an unbounded retry loop. Non-navigation facades stay
// untouched.
func (s *localizationTerminalState) interceptAnswerReady(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, uint64) {
	if s == nil || !localizationNavigationFacade(facade) {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case localizationStateAnswerReady:
		return localizationTerminalResult(s.completionLocked(), facade, operation), 0
	case localizationStateNeedsRecovery:
		if s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			// Carry the task generation through later schema validation. A stale
			// invalid request must never consume a newly committed task's recovery.
			return nil, s.generation
		}
		if localizationRecoveryAllows(facade, operation, arguments) {
			// A concrete search with a weak task correlation has not spent the
			// bounded recovery call. Let the caller correct the anchor once it has
			// better evidence instead of terminalizing the localization session.
			return localizationRecoveryMisalignedResult(s.completionLocked(), facade, operation), 0
		}
		s.state = localizationStateAnswerReady
		s.recoveryRetriesRemaining = 0
		return localizationRecoveryRejectedResult(s.completionLocked(), facade, operation), 0
	default:
		return nil, 0
	}
}

func (s *localizationTerminalState) refinementAllowsLocked(symbol string) bool {
	if symbol == "" {
		return false
	}
	_, authorized := s.refinementRoutes[symbol]
	return authorized
}

func localizationRecoveryAllows(facade, operation string, arguments map[string]any) bool {
	switch facade + "." + operation {
	case "search.text", "search.symbols":
		query, _ := arguments["query"].(string)
		return strings.TrimSpace(query) != ""
	case "read.source":
		return exactLocalizationSymbol(arguments) != ""
	default:
		return false
	}
}

// localizationRecoveryAllowsLocked keeps the one-shot correction anchored to
// the user request. Without this check an agent can invent a nearby generic
// declaration name, receive valid-but-unrelated text hits, and mistake their
// existence for causal evidence. Exact source reads remain available because
// their symbol identity is itself a bounded declaration target.
//
// s.mu must be held by the caller.
func (s *localizationTerminalState) localizationRecoveryAllowsLocked(facade, operation string, arguments map[string]any) bool {
	if !localizationRecoveryAllows(facade, operation, arguments) {
		return false
	}
	if facade != "search" {
		return true
	}
	query, _ := arguments["query"].(string)
	return localizationRecoveryQueryAligned(s.taskFingerprint, query)
}

func localizationRecoveryQueryAligned(task, query string) bool {
	task = strings.TrimSpace(task)
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	if task == "" {
		// Empty task fingerprints occur only in direct state tests and older
		// sessions. Production recovery is always armed with a localize task.
		return true
	}
	lowerTask := strings.ToLower(task)
	lowerQuery := strings.ToLower(query)
	// Preserve compact quoted literals and command-line flags that are too
	// short for identifier tokenization but occur verbatim in the request.
	trimmedAnchor := strings.Trim(lowerQuery, " \t\r\n`'\"")
	if len(trimmedAnchor) >= 2 && strings.Contains(lowerTask, trimmedAnchor) {
		return true
	}
	taskTerms := exploreTerminalTerms(task)
	for term := range exploreTerminalTerms(query) {
		if _, aligned := taskTerms[term]; aligned {
			return true
		}
	}
	return localizationRecoverySpecificAnchor(query)
}

// localizationRecoverySpecificAnchor admits compact path-like literals that
// are sufficiently concrete to bound a recovery search on their own. This
// covers metadata paths such as `".jj/"` whose semantic class (VCS state) may
// appear in the task without the literal directory name. Generic declaration
// fragments remain subject to task-term alignment.
func localizationRecoverySpecificAnchor(query string) bool {
	anchor := strings.Trim(strings.TrimSpace(query), "`'\"")
	if len(anchor) < 2 || strings.ContainsAny(anchor, " \t\r\n") {
		return false
	}
	return strings.ContainsAny(anchor, "/\\") || strings.HasPrefix(anchor, ".")
}

func localizationRecoveryOperationAllowed(facade, operation string) bool {
	switch facade + "." + operation {
	case "search.text", "search.symbols", "read.source":
		return true
	default:
		return false
	}
}

func (s *localizationTerminalState) consumeInvalidRecovery(facade, operation string, generation uint64) (localizationCompletion, bool) {
	if s == nil || generation == 0 || !localizationRecoveryOperationAllowed(facade, operation) {
		return newLocalizationOpenCompletion(), false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != localizationStateNeedsRecovery || s.generation != generation {
		return newLocalizationOpenCompletion(), false
	}
	s.state = localizationStateAnswerReady
	s.recoveryRetriesRemaining = 0
	return s.completionLocked(), true
}

func localizationRecoveryMisalignedResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   "the recovery search query is not specific to the localization task; use a task term or a concrete path/literal anchor; the recovery allowance is still available",
		Retriable: true,
		Data: map[string]any{
			"contract":           localizationContractFor(completion),
			"facade":             facade,
			"operation":          operation,
			"allowed_operations": append([]string(nil), localizationRecoveryOperations...),
		},
	}, true)
}

func localizationRecoveryRejectedResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationTerminal,
		Message:   "the one bounded localization recovery call must be search.text or search.symbols with a task-aligned query, or read.source with a concrete symbol; localization is now terminal",
		Retriable: false,
		Data: map[string]any{
			"contract":           localizationContractFor(completion),
			"facade":             facade,
			"operation":          operation,
			"allowed_operations": append([]string(nil), localizationRecoveryOperations...),
		},
	}, true)
}

// beginLocalize reserves the only localization handler slot for this session.
// An inactive session admits its first localization without a boundary flag.
// Once a contract exists, only the first explore call for a genuinely new user
// request may cross it, and the caller must say so explicitly. Localize stages
// its returned completion; task stages inactive navigation. The old contract
// remains live until finishLocalize commits the successful replacement.
func (s *localizationTerminalState) beginLocalize(task string, newUserTask bool) (uint64, *mcpgo.CallToolResult) {
	if s == nil {
		return 0, nil
	}
	fingerprint := localizationTaskFingerprint(task)
	if fingerprint == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservation != nil {
		return 0, localizationInProgressResult()
	}
	if s.state != localizationStateInactive && !newUserTask {
		completion := s.completionLocked()
		if s.state == localizationStateNeedsRecovery {
			s.state = localizationStateAnswerReady
			s.recoveryRetriesRemaining = 0
			return 0, localizationRecoveryRejectedResult(s.completionLocked(), "explore", "localize")
		}
		// A repeat localize against a terminal contract gets the same compact,
		// typed non-retriable signal as every other post-terminal navigation
		// call. The original successful result already holds the evidence.
		if s.state == localizationStateAnswerReady {
			return 0, localizationTerminalResult(completion, "explore", "localize")
		}
		return 0, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeLocalizationComplete,
			Message:   "this user request already has a localization completion contract; follow it instead of starting another localize call",
			Data: map[string]any{
				"completion": completion,
				"facade":     "explore",
				"operation":  "localize",
			},
		})
	}
	s.nextReservation++
	if s.nextReservation == 0 {
		s.nextReservation++
	}
	token := s.nextReservation
	s.reservation = &localizationReservation{token: token, generation: s.generation}
	return token, nil
}

// finishLocalize commits only the completion staged by the matching reservation
// and only if no reset changed its generation. Errors and panics pass success=false
// and leave the prior contract untouched. A stale finisher can never clear or
// overwrite a newer reservation.
func (s *localizationTerminalState) finishLocalize(token uint64, success bool) bool {
	if s == nil || token == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation := s.reservation
	if reservation == nil || reservation.token != token {
		return false
	}
	s.reservation = nil
	if !success || !reservation.staged || reservation.generation != s.generation {
		return false
	}
	s.commitLocalizationLocked(reservation.pendingCompletion, reservation.pendingTaskFingerprint)
	return true
}

func localizationInProgressResult() *mcpgo.CallToolResult {
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   "a localization request is already in progress for this session",
		Data: map[string]any{
			"completion": map[string]any{
				"state": "localization_in_progress", "scope": "localization",
				"required_action": "wait", "allowed_tool_calls": 0,
				"contract_version": localizationTerminalContractV2,
				"enforceable":      false,
			},
			"facade": "explore", "operation": "localize",
		},
	})
}

func localizationTaskFingerprint(task string) string {
	return strings.Join(strings.Fields(task), " ")
}

// authorize checks a navigation call and reserves the single permitted
// localization read when applicable. The caller must finish the reservation
// after invocation so a failed read restores the allowance instead of silently
// consuming it.
func (s *localizationTerminalState) authorize(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, bool) {
	blocked, token := s.authorizeWithToken(facade, operation, arguments)
	return blocked, token != 0
}

func (s *localizationTerminalState) authorizeWithToken(facade, operation string, arguments map[string]any) (*mcpgo.CallToolResult, uint64) {
	if s == nil || !localizationNavigationFacade(facade) {
		return nil, 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservation != nil {
		return localizationInProgressResult(), 0
	}
	if s.state == localizationStateInactive {
		return nil, 0
	}
	// answer_ready terminates only localization navigation. Catch those facades
	// before their handlers can run and return a compact typed instruction;
	// unrelated work remains dispatchable through the early return above.
	if s.state == localizationStateAnswerReady {
		return localizationTerminalResult(s.completionLocked(), facade, operation), 0
	}
	if s.state == localizationStateNeedsRecovery {
		if s.localizationRecoveryAllowsLocked(facade, operation, arguments) {
			s.state = localizationStateRecoveryInFlight
			return nil, s.beginReadReservationLocked()
		}
		if localizationRecoveryAllows(facade, operation, arguments) {
			return localizationRecoveryMisalignedResult(s.completionLocked(), facade, operation), 0
		}
		s.state = localizationStateAnswerReady
		s.recoveryRetriesRemaining = 0
		return localizationRecoveryRejectedResult(s.completionLocked(), facade, operation), 0
	}
	if s.state == localizationStateNeedsExactRead && facade == "read" && operation == "source" && exactLocalizationSymbol(arguments) == s.exactSymbol {
		s.inFlightImplementationSymbol = s.exactReadRoute.implementationSymbol
		s.inFlightEnforceable = s.exactReadRoute.enforceable
		s.state = localizationStateExactReadInFlight
		return nil, s.beginReadReservationLocked()
	}
	refinementSymbol := exactLocalizationSymbol(arguments)
	if s.state == localizationStateNeedsRefinement && facade == "read" && operation == "source" && s.refinementAllowsLocked(refinementSymbol) {
		route := s.refinementRoutes[refinementSymbol]
		s.inFlightImplementationSymbol = route.implementationSymbol
		s.inFlightEnforceable = route.enforceable
		s.inFlightCorrectionSymbol = ""
		if refinementSymbol == s.refinementSymbol && !route.enforceable && route.implementationSymbol == "" && s.correctionSymbol != "" {
			s.inFlightCorrectionSymbol = s.correctionSymbol
		}
		s.state = localizationStateRefineInFlight
		return nil, s.beginReadReservationLocked()
	}

	completion := s.completionLocked()
	message := "localization is complete; return the existing evidence without another Gortex navigation call"
	switch s.state {
	case localizationStateNeedsExactRead:
		message = fmt.Sprintf("localization needs exactly one read(operation:\"source\") for %q; other navigation calls are blocked", s.exactSymbol)
	case localizationStateExactReadInFlight:
		message = "the permitted exact localization read is already in progress"
	case localizationStateNeedsRefinement:
		message = fmt.Sprintf("localization permits exactly one read(operation:\"source\") for %q; other navigation calls are blocked", s.refinementSymbol)
	case localizationStateRefineInFlight:
		message = "the permitted localization refinement read is already in progress"
	case localizationStateRecoveryInFlight:
		message = "the bounded localization recovery call is already in progress"
	}
	return newStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeLocalizationComplete,
		Message:   message,
		Retriable: false,
		Data: map[string]any{
			"completion": completion,
			"facade":     facade,
			"operation":  operation,
		},
	}, true), 0
}

func (s *localizationTerminalState) beginReadReservationLocked() uint64 {
	s.nextReadReservation++
	if s.nextReadReservation == 0 {
		s.nextReadReservation++
	}
	s.readReservationToken = s.nextReadReservation
	s.readReservationGen = s.generation
	return s.readReservationToken
}

// finishReservedRead is retained for direct state tests. Production dispatch
// carries the exact token returned by authorizeWithToken so a stale finisher
// can never consume a later task's read.
func (s *localizationTerminalState) finishReservedRead(success bool) localizationCompletion {
	if s == nil {
		return newLocalizationCompletion(true, "")
	}
	s.mu.Lock()
	token := s.readReservationToken
	s.mu.Unlock()
	return s.finishReservedReadToken(token, success)
}

func (s *localizationTerminalState) finishReservedReadToken(token uint64, success bool) localizationCompletion {
	if s == nil {
		return newLocalizationOpenCompletion()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == 0 || token != s.readReservationToken || s.readReservationGen != s.generation {
		return newLocalizationOpenCompletion()
	}
	s.readReservationToken = 0
	s.readReservationGen = 0
	switch s.state {
	case localizationStateRecoveryInFlight:
		if success {
			s.state = localizationStateAnswerReady
			s.recoveryRetriesRemaining = 0
			return s.completionLocked()
		}
		if s.recoveryRetriesRemaining > 0 {
			s.recoveryRetriesRemaining--
			s.state = localizationStateNeedsRecovery
			return s.completionLocked()
		}
		s.state = localizationStateAnswerReady
		return s.completionLocked()
	case localizationStateExactReadInFlight:
		implementationSymbol := s.inFlightImplementationSymbol
		routeEnforceable := s.inFlightEnforceable
		wasCorrection := s.exactReadIsCorrection
		s.inFlightImplementationSymbol = ""
		s.inFlightEnforceable = false
		s.inFlightCorrectionSymbol = ""
		if success {
			if routeEnforceable {
				s.enforceableOnAnswerReady = true
			}
			if wasCorrection && implementationSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = implementationSymbol
				s.exactReadRoute = localizationRefinementRoute{enforceable: routeEnforceable}
				return s.completionLocked()
			}
			s.exactSymbol = ""
			s.correctionSymbol = ""
			s.correctionRoute = localizationRefinementRoute{}
			s.exactReadIsCorrection = false
			s.exactReadRoute = localizationRefinementRoute{}
			s.correctionRetriesRemaining = 0
			if !wasCorrection && !s.enforceableOnAnswerReady {
				s.state = localizationStateNeedsRecovery
				s.recoveryRetriesRemaining = 1
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			return s.completionLocked()
		}
		s.enforceableOnAnswerReady = false
		if s.exactReadIsCorrection {
			if s.correctionRetriesRemaining > 0 {
				s.correctionRetriesRemaining--
				s.state = localizationStateNeedsExactRead
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			s.exactSymbol = ""
			s.exactReadIsCorrection = false
			s.exactReadRoute = localizationRefinementRoute{}
			s.correctionRetriesRemaining = 0
			return s.completionLocked()
		}
		s.state = localizationStateNeedsExactRead
	case localizationStateRefineInFlight:
		if success {
			implementationSymbol := s.inFlightImplementationSymbol
			enforceable := s.inFlightEnforceable
			correctionSymbol := s.inFlightCorrectionSymbol
			correctionRoute := s.correctionRoute
			s.inFlightImplementationSymbol = ""
			s.inFlightEnforceable = false
			s.inFlightCorrectionSymbol = ""
			s.enforceableOnAnswerReady = enforceable
			s.refinementSymbol = ""
			s.refinementSymbols = nil
			s.refinementRoutes = nil
			s.correctionSymbol = ""
			s.correctionRoute = localizationRefinementRoute{}
			s.refinementRetriesRemaining = 0
			if implementationSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = implementationSymbol
				s.exactReadIsCorrection = true
				s.exactReadRoute = localizationRefinementRoute{enforceable: enforceable}
				s.correctionRetriesRemaining = 1
				return s.completionLocked()
			}
			if correctionSymbol != "" {
				s.state = localizationStateNeedsExactRead
				s.exactSymbol = correctionSymbol
				s.exactReadIsCorrection = true
				s.exactReadRoute = correctionRoute
				s.correctionRetriesRemaining = 1
				return s.completionLocked()
			}
			if !enforceable {
				s.state = localizationStateNeedsRecovery
				s.recoveryRetriesRemaining = 1
				return s.completionLocked()
			}
			s.state = localizationStateAnswerReady
			return s.completionLocked()
		}
		s.inFlightImplementationSymbol = ""
		s.inFlightEnforceable = false
		s.inFlightCorrectionSymbol = ""
		s.enforceableOnAnswerReady = false
		if s.refinementRetriesRemaining > 0 {
			s.refinementRetriesRemaining--
			s.state = localizationStateNeedsRefinement
			return s.completionLocked()
		}
		s.state = localizationStateAnswerReady
		s.refinementSymbol = ""
		s.refinementSymbols = nil
		s.refinementRoutes = nil
		s.correctionSymbol = ""
		s.correctionRoute = localizationRefinementRoute{}
	}
	return s.completionLocked()
}

// localizationTerminalResult is the successful, idempotent replay returned
// after a localization response established answer_ready. Route details are
// intentionally omitted so every post-terminal navigation call receives the
// same actionable payload.
func localizationTerminalResult(completion localizationCompletion, facade, operation string) *mcpgo.CallToolResult {
	_ = facade
	_ = operation
	return localizationAnswerReadyResult(completion)
}

func cloneLocalizationRefinementRoutes(routes map[string]localizationRefinementRoute) map[string]localizationRefinementRoute {
	if len(routes) == 0 {
		return nil
	}
	cloned := make(map[string]localizationRefinementRoute, len(routes))
	for symbol, route := range routes {
		cloned[symbol] = route
	}
	return cloned
}

// block is retained for direct state checks; production dispatch uses
// authorize so it can finish a reserved exact read after handler completion.
func (s *localizationTerminalState) block(facade, operation string, arguments map[string]any) *mcpgo.CallToolResult {
	blocked, _ := s.authorize(facade, operation, arguments)
	return blocked
}

func localizationNavigationFacade(facade string) bool {
	switch facade {
	case "explore", "search", "read", "relations", "trace", "analyze":
		return true
	default:
		return false
	}
}

func exactLocalizationSymbol(arguments map[string]any) string {
	if target, ok := arguments["target"].(map[string]any); ok {
		return strings.TrimSpace(fmt.Sprint(target["symbol"]))
	}
	return strings.TrimSpace(fmt.Sprint(arguments["symbol"]))
}

func (s *Server) localizationFor(ctx context.Context) *localizationTerminalState {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.localization
	}
	return s.sessions.get(id).localization
}
