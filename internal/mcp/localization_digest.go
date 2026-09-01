package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Terminal evidence retention.
//
// The localize handler builds a byte-budgeted evidence envelope once and
// retains a compact projection for host-side fallback and deterministic replay.
// A post-terminal navigation call returns the same successful ready-to-emit
// answer instead of an error that would invite another recovery loop.

const (
	// localizationDigestMaxBytes bounds retained session state independently of
	// the original envelope budget. Sized to hold the full replay window below
	// without shedding: a retention cap tighter than the page silently undoes
	// any widening of the page.
	localizationDigestMaxBytes = 12288
	// localizationReplayEvidenceLimit preserves the complete twelve-symbol
	// refinement window plus four graph-relationship slots. The retained byte
	// cap remains authoritative when long identities cannot all fit.
	localizationReplayEvidenceLimit = 16
	// localizationFinalResponseMaxBytes bounds the ready-to-emit answer that
	// accompanies the retained digest on terminal responses and replays.
	localizationFinalResponseMaxBytes = 8192
	// This canonical envelope is deliberately carried in MCP _meta. Adapting
	// hosts may render its ordered evidence deterministically without exposing
	// retained rows to model-visible text or structuredContent.
	localizationHostMetaKey = "gortex/localization"
)

// localizationEvidenceDigest is the compact, session-retained projection of
// an answer envelope: ranked candidate evidence without source bodies.
type localizationEvidenceDigest struct {
	Files    []string                `json:"files,omitempty"`
	Symbols  []string                `json:"symbols,omitempty"`
	Evidence []localizationDigestRow `json:"evidence,omitempty"`

	// finalResponse is derived from Evidence and excluded from digest JSON so
	// the retained-state byte cap does not count the same identities twice.
	finalResponse string
	// provisionalResponse is the same rows rendered for a state that has not
	// proven its answer yet. A caller can run out of turns at any point, and a
	// session that ends holding nothing is the one outcome with no recovery, so
	// every state carries a page — labelled for what it is.
	provisionalResponse string
	// primaryIDs is the exact bounded PRIMARY projection used to render the
	// final response. It stays out of retained digest JSON to avoid counting the
	// same identities twice, but is copied into authenticated host authority.
	primaryIDs []string
}

type localizationDigestRow struct {
	Rank       int      `json:"rank,omitempty"`
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	QualName   string   `json:"qual_name,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	Callers    []string `json:"callers,omitempty"`
	Callees    []string `json:"callees,omitempty"`
	Provenance string   `json:"provenance,omitempty"`

	// Kept only in the request-local/session-retained projection. They separate
	// truthful provenance from presentation authority without spending response
	// bytes on internal policy state. primaryCohortOrder freezes the ranked
	// PRIMARY projection before supplemental evidence is materialized.
	literalPrimaryEligible   bool
	taskCitedPrimaryEligible bool
	primaryCohortOrder       int
	supportingOnly           bool
	leadingFileDepth         bool
	authorizationPriority    bool
}

// newLocalizationEvidenceDigestForTask retains only concrete ranked evidence
// rows. Files and Symbols are rebuilt from those rows, so an item that was shed
// by the replay limit or byte budget cannot survive as an unsupported answer
// candidate. Exact and refinement-authorized rows form a stable protected
// prefix in retained state only; the visible envelope keeps its original order.
// The request is rendered into the ready-to-emit answer, so a page completing
// on its first call presents the same task-scored rows a later merge would.
func newLocalizationEvidenceDigestForTask(task string, envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	priorityIDs := localizationDigestPriorityIDs(envelope.Completion, envelope.Evidence)

	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	appendRow := func(row localizationEvidence) bool {
		if row.ID == "" || row.File == "" || len(digest.Evidence) >= localizationReplayEvidenceLimit {
			return false
		}
		if _, exists := seen[row.ID]; exists {
			return false
		}
		seen[row.ID] = struct{}{}
		_, authorizationPriority := priorityIDs[row.ID]
		digest.Evidence = append(digest.Evidence, localizationDigestRow{
			Rank:       row.Rank,
			ID:         row.ID,
			Name:       row.Name,
			QualName:   row.QualName,
			Kind:       row.Kind,
			File:       row.File,
			Line:       row.Line,
			Signature:  row.Signature,
			Callers:    append([]string(nil), row.Callers...),
			Callees:    append([]string(nil), row.Callees...),
			Provenance: row.Provenance,

			literalPrimaryEligible:   row.literalPrimaryEligible,
			taskCitedPrimaryEligible: row.taskCitedPrimaryEligible,
			primaryCohortOrder:       row.primaryCohortOrder,
			supportingOnly:           row.supportingOnly || localizationSupportingOnlyProvenance(row.Provenance),
			leadingFileDepth:         row.leadingFileDepth,
			authorizationPriority:    authorizationPriority,
		})
		return true
	}
	// Proof/authorization identities and the one task-cited supplemental
	// PRIMARY remain the immutable prefix. The latter is presentation evidence,
	// not authorization, so it is reserved without entering priorityIDs.
	for _, row := range envelope.Evidence {
		_, prioritized := priorityIDs[row.ID]
		if prioritized || localizationEvidenceTaskCitedPrimary(row) {
			appendRow(row)
		}
	}
	// Reserve tail capacity for real body-derived declarations before ordinary
	// non-priority rows fill the replay window. Visible envelope rows are never
	// removed; only the weakest retained digest tail yields.
	bodyIDs := make(map[string]struct{}, localizationBodyMentionCap)
	for _, row := range envelope.Evidence {
		if row.Provenance == localizationProvenanceBodyMention && row.ID != "" {
			if _, already := seen[row.ID]; !already {
				bodyIDs[row.ID] = struct{}{}
			}
		}
	}
	bodyReserve := min(len(bodyIDs), localizationReplayEvidenceLimit-len(digest.Evidence))
	ordinaryLimit := localizationReplayEvidenceLimit - bodyReserve
	for _, row := range envelope.Evidence {
		if len(digest.Evidence) >= ordinaryLimit {
			break
		}
		if localizationSupportingOnlyProvenance(row.Provenance) {
			continue
		}
		if _, prioritized := priorityIDs[row.ID]; !prioritized && !localizationEvidenceTaskCitedPrimary(row) {
			appendRow(row)
		}
	}
	for _, row := range envelope.Evidence {
		if localizationSupportingOnlyProvenance(row.Provenance) {
			appendRow(row)
		}
	}

	for {
		rebuildLocalizationDigestSkeleton(digest)
		refreshLocalizationDigestResponses(digest, task, nil)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes && len(digest.finalResponse) <= localizationFinalResponseMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}
		if retained, removed := shedLocalizationDigestSupportingOnly(digest.Evidence); removed {
			digest.Evidence = retained
			continue
		}
		last := len(digest.Evidence) - 1
		if shedLocalizationDigestOptionalFields(digest.Evidence) {
			continue
		}
		// The identity and file are the irreducible row. If even one pathological
		// row cannot fit, prefer a bounded empty answer over retaining oversized
		// session state or emitting a truncated, misleading identity.
		if last == 0 {
			digest.Evidence = nil
			continue
		}
		digest.Evidence = digest.Evidence[:last]
	}
}

// localizationDigestPriorityIDs protects every identity required to justify a
// live authorization, not only the identity the client may read. Dependencies
// still remain subject to the hard byte cap; post-pack reconciliation below
// removes or downgrades any authorization whose complete proof did not fit.
func localizationDigestPriorityIDs(completion localizationCompletion, evidence []localizationEvidence) map[string]struct{} {
	priority := make(map[string]struct{}, 1+len(completion.AllowedSymbols)+len(completion.refinementRoutes)*3)
	add := func(symbol string) {
		if symbol = strings.TrimSpace(symbol); symbol != "" {
			priority[symbol] = struct{}{}
		}
	}
	add(completion.ExactSymbol)
	for _, symbol := range completion.AllowedSymbols {
		add(symbol)
	}
	for symbol, route := range completion.refinementRoutes {
		add(symbol)
		add(route.implementationSymbol)
		add(route.proofSymbol)
	}
	for _, row := range evidence {
		switch row.Provenance {
		case localizationProvenanceSourceLiteralCallee,
			localizationProvenanceDivergentDefault,
			localizationProvenanceDivergentDefaultType,
			localizationProvenanceImplementationRoute,
			localizationProvenanceImplementationTarget:
			add(row.ID)
		}
	}
	return priority
}

func cloneLocalizationDigestRows(rows []localizationDigestRow) []localizationDigestRow {
	if len(rows) == 0 {
		return nil
	}
	cloned := make([]localizationDigestRow, len(rows))
	for index, row := range rows {
		cloned[index] = row
		cloned[index].Callers = append([]string(nil), row.Callers...)
		cloned[index].Callees = append([]string(nil), row.Callees...)
	}
	return cloned
}

const localizationMergedRelationCap = exploreRingCap * 2

func mergeLocalizationDigestStrings(primary, supplementary []string) []string {
	merged := make([]string, 0, min(localizationMergedRelationCap, len(primary)+len(supplementary)))
	seen := make(map[string]struct{}, cap(merged))
	for _, values := range [][]string{primary, supplementary} {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
			if len(merged) == localizationMergedRelationCap {
				return merged
			}
		}
	}
	return merged
}

// localizationProvenanceMergeStrength orders provenance labels for merge
// retention. A literal observation is a claim a later graph hop cannot
// substitute: once a row has been seen as a content or source literal, a
// re-observation through adjacency must not erase that mark.
func localizationProvenanceMergeStrength(provenance string) int {
	switch provenance {
	case localizationProvenanceSourceLiteralCallee:
		return 6
	case localizationProvenanceContentLiteral:
		return 5
	case localizationProvenanceDivergentDefault,
		localizationProvenanceImplementationTarget,
		localizationProvenanceTypedAnchorProjection,
		localizationProvenanceDivergentDefaultType,
		localizationProvenancePermittedReadSource,
		localizationProvenanceImplementationRoute:
		return 4
	case localizationProvenanceDirectAdjacency, "direct_caller", "direct_callee":
		return 2
	case "":
		return 0
	default:
		return 3
	}
}

// mergeLocalizationDigestRowEvidence preserves the first-ranked identity while
// filling metadata and unioning bounded graph evidence from a later observation.
// File disagreement is deliberately not repaired here: one ID cannot authorize
// two declaration files, so the first tuple remains authoritative.
func mergeLocalizationDigestRowEvidence(primary, supplementary localizationDigestRow) localizationDigestRow {
	if strings.TrimSpace(primary.File) != strings.TrimSpace(supplementary.File) {
		return primary
	}
	if primary.Name == "" {
		primary.Name = supplementary.Name
	}
	if primary.QualName == "" {
		primary.QualName = supplementary.QualName
	}
	if primary.Kind == "" {
		primary.Kind = supplementary.Kind
	}
	if primary.Line == 0 {
		primary.Line = supplementary.Line
	}
	if primary.Signature == "" {
		primary.Signature = supplementary.Signature
	}
	if localizationProvenanceMergeStrength(supplementary.Provenance) >
		localizationProvenanceMergeStrength(primary.Provenance) {
		primary.Provenance = supplementary.Provenance
	}
	primary.literalPrimaryEligible = primary.literalPrimaryEligible || supplementary.literalPrimaryEligible
	primary.taskCitedPrimaryEligible = primary.taskCitedPrimaryEligible || supplementary.taskCitedPrimaryEligible
	primary.authorizationPriority = primary.authorizationPriority || supplementary.authorizationPriority
	// An identity that was independently ranked remains cohort-eligible when a
	// later supplemental observation of the same row is merged into it.
	primary.supportingOnly = primary.supportingOnly && supplementary.supportingOnly
	primary.leadingFileDepth = primary.leadingFileDepth || supplementary.leadingFileDepth
	if primary.primaryCohortOrder == 0 ||
		(supplementary.primaryCohortOrder > 0 && supplementary.primaryCohortOrder < primary.primaryCohortOrder) {
		primary.primaryCohortOrder = supplementary.primaryCohortOrder
	}
	primary.Callers = mergeLocalizationDigestStrings(primary.Callers, supplementary.Callers)
	primary.Callees = mergeLocalizationDigestStrings(primary.Callees, supplementary.Callees)
	return primary
}

// localizationFreshEvidenceReserve bounds how much of the retained digest the
// terminalizing call's own output may claim. The remainder is exactly the full
// refinement authorization window, so one broad recovery page cannot evict a
// candidate that the preceding contract said was safe to inspect.
const localizationFreshEvidenceReserve = localizationReplayEvidenceLimit - localizationRefinementAllowedSymbolCap

// mergeLocalizationEvidenceDigest puts evidence returned by the terminalizing
// permitted call first, then fills the bounded tail from the retained localize
// digest. Files, Symbols, and Evidence are rebuilt from the same rows so their
// ordinal positions can never diverge. Fresh rows lead but cannot crowd the
// retained ranking out of its own answer.
func mergeLocalizationEvidenceDigest(current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	seen := make(map[string]int, localizationReplayEvidenceLimit)
	appendRows := func(rows []localizationDigestRow, limit int) {
		if limit > localizationReplayEvidenceLimit {
			limit = localizationReplayEvidenceLimit
		}
		for _, row := range rows {
			row.ID = strings.TrimSpace(row.ID)
			row.File = strings.TrimSpace(row.File)
			if row.ID == "" || row.File == "" {
				continue
			}
			if index, exists := seen[row.ID]; exists {
				digest.Evidence[index] = mergeLocalizationDigestRowEvidence(digest.Evidence[index], row)
				continue
			}
			if len(digest.Evidence) >= limit {
				return
			}
			seen[row.ID] = len(digest.Evidence)
			row.Callers = append([]string(nil), row.Callers...)
			row.Callees = append([]string(nil), row.Callees...)
			digest.Evidence = append(digest.Evidence, row)
		}
	}
	freshLead := localizationReplayEvidenceLimit
	if retained != nil && len(retained.Evidence) > 0 {
		freshLead = localizationFreshEvidenceReserve
	}
	appendRows(current, freshLead)
	if retained != nil {
		appendRows(retained.Evidence, localizationReplayEvidenceLimit)
	}
	// Any capacity the retained rows did not need goes back to the fresh page.
	appendRows(current, localizationReplayEvidenceLimit)
	for {
		rebuildLocalizationDigestSkeleton(digest)
		refreshLocalizationDigestResponses(digest, "", nil)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes && len(digest.finalResponse) <= localizationFinalResponseMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}
		if retained, removed := shedLocalizationDigestSupportingOnly(digest.Evidence); removed {
			digest.Evidence = retained
			continue
		}
		last := len(digest.Evidence) - 1
		if shedLocalizationDigestOptionalFields(digest.Evidence) {
			continue
		}
		if last == 0 {
			digest.Evidence = nil
			continue
		}
		digest.Evidence = digest.Evidence[:last]
	}
}

const localizationSameOwnerEvidenceReserve = 3

// mergeLocalizationEvidenceDigestForTask preserves a small coherent method
// cohort around the terminalizing read before byte shedding considers the
// unrelated tail. It only reorders already-retained rows; cardinality and byte
// limits remain centralized in mergeLocalizationEvidenceDigest.
func mergeLocalizationEvidenceDigestForTask(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	ordered := &localizationEvidenceDigest{Evidence: localizationTaskAwareRetainedRows(task, current, retained)}
	digest := mergeLocalizationEvidenceDigest(current, ordered)
	finalResponse := renderLocalizationFinalResponseForTask(task, current, digest.Evidence)
	if len(finalResponse) <= localizationFinalResponseMaxBytes {
		digest.finalResponse = finalResponse
		digest.provisionalResponse = renderLocalizationProvisionalResponseForTask(task, current, digest.Evidence)
	}
	return digest
}

func localizationTaskAwareRetainedRows(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) []localizationDigestRow {
	if retained == nil || len(retained.Evidence) == 0 {
		return nil
	}
	rows := retained.Evidence
	ordered := make([]localizationDigestRow, 0, len(rows))
	selected := make(map[string]struct{}, len(rows)+len(current))
	ownerKeys := make(map[string]struct{}, len(current))
	seenFiles := make(map[string]struct{}, len(current))
	for _, row := range current {
		if id := strings.TrimSpace(row.ID); id != "" {
			selected[id] = struct{}{}
		}
		if file := strings.TrimSpace(row.File); file != "" {
			seenFiles[file] = struct{}{}
		}
		if key := localizationDigestRowOwnerKey(row); key != "" {
			ownerKeys[key] = struct{}{}
		}
	}
	appendWhere := func(limit int, keep func(localizationDigestRow) bool) {
		added := 0
		for _, row := range rows {
			id := strings.TrimSpace(row.ID)
			if id == "" {
				continue
			}
			if _, exists := selected[id]; exists || !keep(row) {
				continue
			}
			selected[id] = struct{}{}
			ordered = append(ordered, row)
			if file := strings.TrimSpace(row.File); file != "" {
				seenFiles[file] = struct{}{}
			}
			added++
			if limit > 0 && added == limit {
				return
			}
		}
	}
	sameOwner := func(row localizationDigestRow) bool {
		key := localizationDigestRowOwnerKey(row)
		_, exists := ownerKeys[key]
		return key != "" && exists
	}
	appendWhere(0, func(row localizationDigestRow) bool {
		return localizationDigestRowTaskCited(task, row)
	})
	appendWhere(localizationSameOwnerEvidenceReserve, sameOwner)
	taskTerms := exploreTerminalTerms(task)
	appendWhere(0, func(row localizationDigestRow) bool {
		return !sameOwner(row) && localizationDigestRowTaskAligned(task, taskTerms, row)
	})
	appendWhere(0, func(row localizationDigestRow) bool {
		_, seen := seenFiles[strings.TrimSpace(row.File)]
		return !seen
	})
	appendWhere(0, func(localizationDigestRow) bool { return true })
	return ordered
}

func localizationIdentifierRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$'
}

func localizationTaskExactIdentifierOffset(task, value string) int {
	value = strings.TrimSpace(value)
	if value == "" || len(task) < len(value) {
		return -1
	}
	quoted := false
	for _, quote := range []string{"`", "'", "\""} {
		if strings.Contains(task, quote+value+quote) {
			quoted = true
			break
		}
	}
	if !localizationRecoveryConcreteIdentifier(value) &&
		!exploreQueryHasCallAnchor(task, value) && !quoted {
		return -1
	}
	for offset := 0; offset <= len(task)-len(value); {
		relative := strings.Index(task[offset:], value)
		if relative < 0 {
			return -1
		}
		start := offset + relative
		end := start + len(value)
		leftBoundary := start == 0
		if !leftBoundary {
			r, _ := utf8.DecodeLastRuneInString(task[:start])
			leftBoundary = !localizationIdentifierRune(r)
		}
		rightBoundary := end == len(task)
		if !rightBoundary {
			r, _ := utf8.DecodeRuneInString(task[end:])
			rightBoundary = !localizationIdentifierRune(r)
		}
		if leftBoundary && rightBoundary {
			return start
		}
		offset = start + 1
	}
	return -1
}

func localizationTaskCitesExactIdentifier(task, value string) bool {
	return localizationTaskExactIdentifierOffset(task, value) >= 0
}

func localizationEvidenceTaskCitationOffset(task string, row localizationEvidence) int {
	best := -1
	for _, value := range []string{row.ID, row.Name, row.QualName} {
		offset := localizationTaskExactIdentifierOffset(task, value)
		if offset >= 0 && (best < 0 || offset < best) {
			best = offset
		}
	}
	return best
}

func localizationEvidenceTaskCited(task string, row localizationEvidence) bool {
	return localizationEvidenceTaskCitationOffset(task, row) >= 0
}

func localizationEvidenceTaskCitedPrimary(row localizationEvidence) bool {
	return row.taskCitedPrimaryEligible && row.primaryCohortOrder > 0 &&
		row.Provenance == localizationProvenanceDirectAdjacency
}

func localizationDigestRowIdentifierTaskCited(task string, row localizationDigestRow) bool {
	for _, value := range []string{row.ID, row.Name, row.QualName} {
		if localizationTaskCitesExactIdentifier(task, value) {
			return true
		}
	}
	return false
}

func localizationDigestRowTaskCited(task string, row localizationDigestRow) bool {
	for _, value := range []string{row.ID, row.Name, row.QualName, row.File, row.Signature} {
		if localizationTaskCitesConcreteEvidence(task, value) {
			return true
		}
	}
	return false
}

func localizationDigestRowTaskAligned(task string, taskTerms map[string]struct{}, row localizationDigestRow) bool {
	if localizationDigestRowTaskCited(task, row) {
		return true
	}
	values := []string{row.ID, row.Name, row.QualName, row.File, row.Signature}
	for term := range exploreTerminalTerms(strings.Join(values, " ")) {
		if _, aligned := taskTerms[term]; aligned {
			return true
		}
	}
	return false
}

func localizationDigestRowOwnerKey(row localizationDigestRow) string {
	if !strings.EqualFold(strings.TrimSpace(row.Kind), "method") {
		return ""
	}
	file := strings.TrimSpace(row.File)
	owner := strings.TrimSpace(row.QualName)
	if owner == "" {
		owner = strings.TrimSpace(row.ID)
		if prefix := file + "::"; file != "" && strings.HasPrefix(owner, prefix) {
			owner = strings.TrimPrefix(owner, prefix)
		}
	}
	if cut := strings.LastIndex(owner, "."); cut > 0 {
		owner = owner[:cut]
	} else if cut := strings.LastIndex(owner, "::"); cut > 0 {
		owner = owner[:cut]
	} else {
		return ""
	}
	owner = strings.TrimSpace(owner)
	if file == "" || owner == "" {
		return ""
	}
	return file + "\x00" + owner
}

func shedLocalizationDigestRowOptionalFields(row *localizationDigestRow) bool {
	if row == nil {
		return false
	}
	if len(row.Callers) > 0 || len(row.Callees) > 0 {
		row.Callers = nil
		row.Callees = nil
		return true
	}
	if row.Signature != "" {
		row.Signature = ""
		return true
	}
	if row.QualName != "" {
		row.QualName = ""
		return true
	}
	if row.Name != "" || row.Kind != "" {
		row.Name = ""
		row.Kind = ""
		return true
	}
	return false
}

// shedLocalizationDigestOptionalFields compacts supplementary detail across the
// complete retained window before any declaration identity is removed. Walking
// from the tail preserves richer metadata on higher-ranked rows when the byte
// cap can be met without stripping the whole page.
func shedLocalizationDigestOptionalFields(rows []localizationDigestRow) bool {
	for index := len(rows) - 1; index >= 0; index-- {
		if shedLocalizationDigestRowOptionalFields(&rows[index]) {
			return true
		}
	}
	return false
}

// Supplemental rows yield before richer metadata or independently ranked
// evidence under retained-state pressure, regardless of where a later merge
// placed them. An identity required by the live completion is protected even
// when its only visible observation has supplemental provenance.
func shedLocalizationDigestSupportingOnly(rows []localizationDigestRow) ([]localizationDigestRow, bool) {
	for index := len(rows) - 1; index >= 0; index-- {
		if !localizationFinalResponseSupportingOnly(rows[index]) || rows[index].authorizationPriority {
			continue
		}
		copy(rows[index:], rows[index+1:])
		rows[len(rows)-1] = localizationDigestRow{}
		return rows[:len(rows)-1], true
	}
	return rows, false
}

func rebuildLocalizationDigestSkeleton(digest *localizationEvidenceDigest) {
	digest.Files = digest.Files[:0]
	digest.Symbols = digest.Symbols[:0]
	for index := range digest.Evidence {
		row := &digest.Evidence[index]
		row.Rank = index + 1
		// Keep these arrays positional, including repeated files. A consumer can
		// now pair FILES #N, SYMBOLS #N, and EVIDENCE #N without guessing.
		digest.Files = append(digest.Files, row.File)
		digest.Symbols = append(digest.Symbols, row.ID)
	}
}

func localizationFinalResponseField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

const (
	// Five primary slots: sessions convert a primary-seated answer at a far
	// higher rate than any other placement, and the observed gold rank is
	// usually within the top five. Slots below stay SUPPORTING so the page
	// keeps an explicit confidence order.
	localizationFinalResponsePrimaryLimit = 5
	// Authenticated literal rows may pre-seat ahead of the ranked cohort. The
	// second seat is granted only for a second proven file, so a literal
	// registered in one place and consumed in another presents both sites
	// while same-file corroboration still competes through ordinary ranking.
	localizationFinalResponseLiteralReserve = 2
	// A row is task-named only when the raw task text carries its identifier
	// as a whole word, case-sensitively. The rune floors keep short prose
	// words from naming whatever row happens to share them: five for the
	// row's own name, six for an owner segment.
	localizationTaskNameMinRunes  = 5
	localizationTaskOwnerMinRunes = 6
	// Task-named rows never displace a ranked seat. They may extend the
	// primary block past its ranked width by at most this many extra seats:
	// a proven ranked conversion is never traded for a hoped-for one.
	localizationTaskAlignedExtraSeats = 2
	// localizationDigestShrinkFloorRows is the smallest answer the envelope
	// shed loop may shrink the digest to under extreme budget pressure. It is
	// deliberately narrower than the primary block: the primary width is what
	// a roomy page seats, while this floor is the minimum worth returning at
	// all — and the tight-budget wire bound must hold at any primary width.
	localizationDigestShrinkFloorRows = 3
	// localizationAnswerPageAllowanceBytes is reserved during evidence-row
	// admission for the answer block reconciled after packing: the rendered
	// floor-sized primary block, its excerpts, and the directive.
	localizationAnswerPageAllowanceBytes = 1024
	// The answer asks the caller to reproduce these lines verbatim, so the
	// presentation must cover every row the digest retained: a retained row
	// that never reaches the answer is evidence the page found and then hid.
	localizationFinalResponseSupportingLimit = localizationReplayEvidenceLimit - localizationFinalResponsePrimaryLimit
)

type localizationFinalResponseRow struct {
	row     localizationDigestRow
	primary bool
}

type localizationFinalResponseTaskScore struct {
	matched  int
	longest  int
	callable bool
}

func localizationFinalResponsePrimaryProvenance(row localizationDigestRow) bool {
	switch row.Provenance {
	case localizationProvenanceContentLiteral:
		return row.literalPrimaryEligible
	case localizationProvenanceSourceLiteralCallee,
		localizationProvenanceDivergentDefault,
		localizationProvenanceImplementationTarget,
		localizationProvenanceTypedAnchorProjection,
		localizationProvenancePermittedReadSource:
		return true
	default:
		return false
	}
}

func localizationFinalResponseSupportingOnly(row localizationDigestRow) bool {
	if row.taskCitedPrimaryEligible && row.primaryCohortOrder > 0 &&
		row.Provenance == localizationProvenanceDirectAdjacency {
		return false
	}
	return row.supportingOnly || localizationSupportingOnlyProvenance(row.Provenance)
}

func localizationFinalResponseSupportingProvenance(provenance string) bool {
	switch provenance {
	case localizationProvenanceDivergentDefaultType,
		localizationProvenanceImplementationRoute,
		localizationProvenanceDirectAdjacency,
		"direct_caller", "direct_callee":
		return true
	default:
		return false
	}
}

func localizationFinalResponseIdentifierText(row localizationDigestRow) string {
	id := strings.TrimSpace(row.ID)
	if cut := strings.LastIndex(id, "::"); cut >= 0 {
		id = id[cut+2:]
	}
	return strings.ToLower(strings.Join([]string{row.Name, row.QualName, id}, " "))
}

func scoreLocalizationFinalResponseTask(taskTerms map[string]struct{}, row localizationDigestRow) localizationFinalResponseTaskScore {
	score := localizationFinalResponseTaskScore{
		callable: strings.EqualFold(strings.TrimSpace(row.Kind), "function") ||
			strings.EqualFold(strings.TrimSpace(row.Kind), "method"),
	}
	text := localizationFinalResponseIdentifierText(row)
	for term := range taskTerms {
		if !exploreConceptTermPresent(text, term) {
			continue
		}
		score.matched++
		if len(term) > score.longest {
			score.longest = len(term)
		}
	}
	return score
}

func localizationFinalResponseBetterTaskScore(left, right localizationFinalResponseTaskScore) bool {
	if left.matched != right.matched {
		return left.matched > right.matched
	}
	if left.callable != right.callable {
		return left.callable
	}
	return left.longest > right.longest
}

// localizationFinalResponseBareName is the identifier an answer is most likely
// to reuse: the last segment of the row's name, stripped of its owner and file.
// Two rows agreeing on it are the pair a caller can confuse.
func localizationFinalResponseBareName(row localizationDigestRow) string {
	name := strings.TrimSpace(row.Name)
	if name == "" {
		name = strings.TrimSpace(row.QualName)
	}
	if name == "" {
		name = strings.TrimSpace(row.ID)
	}
	if cut := strings.LastIndex(name, "::"); cut >= 0 {
		name = name[cut+2:]
	}
	if cut := strings.LastIndex(name, "."); cut >= 0 {
		name = name[cut+1:]
	}
	return name
}

func localizationIdentifierByte(b byte) bool {
	return b == '_' || b == '$' ||
		('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// localizationTaskNamesIdentifier reports whether the raw task text contains
// identifier as a whole word, case-sensitively. Substring hits inside a longer
// identifier do not count: a task about "starting" does not name "start".
func localizationTaskNamesIdentifier(task, identifier string, minRunes int) bool {
	if identifier == "" || task == "" || utf8.RuneCountInString(identifier) < minRunes {
		return false
	}
	for offset := 0; offset+len(identifier) <= len(task); {
		index := strings.Index(task[offset:], identifier)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(identifier)
		if (start == 0 || !localizationIdentifierByte(task[start-1])) &&
			(end == len(task) || !localizationIdentifierByte(task[end])) {
			return true
		}
		offset = start + 1
	}
	return false
}

// localizationTaskNamesRow reports whether the task names the row itself or
// the type the row is declared on. Only the immediate declaring owner counts:
// qualified names carry namespace and package segments, and the repository's
// own name sits in almost every namespace chain and almost every task, so a
// wider match would name the whole page.
func localizationTaskNamesRow(task string, row localizationDigestRow) bool {
	name := localizationFinalResponseBareName(row)
	if localizationTaskNamesIdentifier(task, name, localizationTaskNameMinRunes) {
		return true
	}
	segments := strings.FieldsFunc(strings.TrimSpace(row.QualName), func(r rune) bool {
		return r == '.' || r == ':'
	})
	if len(segments) < 2 {
		return false
	}
	owner := segments[len(segments)-2]
	if owner == name || strings.EqualFold(owner, localizationDigestRowRepoIdentifier(row)) {
		return false
	}
	return localizationTaskNamesIdentifier(task, owner, localizationTaskOwnerMinRunes)
}

// localizationDigestRowRepoIdentifier extracts the repository name that
// prefixes the row's node ID, without any trailing checkout discriminator.
func localizationDigestRowRepoIdentifier(row localizationDigestRow) string {
	id := strings.TrimSpace(row.ID)
	slash := strings.IndexByte(id, '/')
	if slash <= 0 {
		return ""
	}
	repo := id[:slash]
	if dash := strings.LastIndexByte(repo, '-'); dash > 0 {
		digits := repo[dash+1:]
		numeric := digits != ""
		for _, r := range digits {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			repo = repo[:dash]
		}
	}
	return repo
}

// localizationFinalResponseAnchorRank orders the provenance flags that can name
// the file the page is actually about. A divergent default owner is the
// strongest such claim, a causal literal callee next, then a proven
// implementation target and a typed anchor projection.
func localizationFinalResponseAnchorRank(provenance string) int {
	switch provenance {
	case localizationProvenanceDivergentDefault:
		return 6
	case localizationProvenanceSourceLiteralCallee:
		return 5
	case localizationProvenanceImplementationTarget:
		return 4
	case localizationProvenanceTypedAnchorProjection:
		return 3
	case localizationProvenanceDivergentDefaultType:
		return 2
	case localizationProvenancePermittedReadSource:
		return 1
	default:
		return 0
	}
}

func localizationFinalResponseAnchorFile(rows []localizationDigestRow) string {
	anchor, rank := "", 0
	for _, row := range rows {
		file := strings.TrimSpace(row.File)
		if file == "" {
			continue
		}
		if candidate := localizationFinalResponseAnchorRank(row.Provenance); candidate > rank {
			anchor, rank = file, candidate
		}
		if anchor == "" {
			// No provenance flag has spoken yet: the top-ranked row's file is the
			// only thing the page knows about where the answer lives.
			anchor = file
		}
	}
	return anchor
}

// localizationFinalResponseHomonymOrder gives the first slot of each same-name
// group to the row whose file the provenance chain points at. It reorders a
// presentation copy only; retained evidence and its positional arrays are
// untouched.
func localizationFinalResponseHomonymOrder(rows []localizationDigestRow) []localizationDigestRow {
	if len(rows) < 2 {
		return rows
	}
	anchor := localizationFinalResponseAnchorFile(rows)
	if anchor == "" {
		return rows
	}
	counts := make(map[string]int, len(rows))
	preferred := make(map[string]int, len(rows))
	for index, row := range rows {
		name := localizationFinalResponseBareName(row)
		if name == "" {
			continue
		}
		counts[name]++
		if _, exists := preferred[name]; !exists && strings.TrimSpace(row.File) == anchor {
			preferred[name] = index
		}
	}
	ordered := make([]localizationDigestRow, 0, len(rows))
	emitted := make([]bool, len(rows))
	for index, row := range rows {
		if emitted[index] {
			continue
		}
		name := localizationFinalResponseBareName(row)
		if target, exists := preferred[name]; exists && counts[name] > 1 && !emitted[target] {
			ordered = append(ordered, rows[target])
			emitted[target] = true
		}
		if emitted[index] {
			continue
		}
		ordered = append(ordered, row)
		emitted[index] = true
	}
	return ordered
}

func localizationFinalResponseNeighborContains(ids []string, id string) bool {
	for _, candidate := range ids {
		if strings.TrimSpace(candidate) == id {
			return true
		}
	}
	return false
}

func localizationFinalResponseDirectRelation(row localizationDigestRow, primaries []localizationDigestRow) bool {
	if localizationFinalResponseSupportingProvenance(row.Provenance) {
		return true
	}
	id := strings.TrimSpace(row.ID)
	for _, primary := range primaries {
		primaryID := strings.TrimSpace(primary.ID)
		if localizationFinalResponseNeighborContains(primary.Callers, id) ||
			localizationFinalResponseNeighborContains(primary.Callees, id) ||
			localizationFinalResponseNeighborContains(row.Callers, primaryID) ||
			localizationFinalResponseNeighborContains(row.Callees, primaryID) {
			return true
		}
	}
	return false
}

// localizationFinalResponseRows selects a bounded model-facing projection
// without changing the retained evidence rows or their positional JSON arrays.
func localizationFinalResponseRows(task string, current, rows []localizationDigestRow) []localizationFinalResponseRow {
	if len(rows) == 0 {
		return nil
	}
	rows = localizationFinalResponseHomonymOrder(rows)
	primaries := make([]localizationDigestRow, 0, localizationFinalResponsePrimaryLimit)
	supporting := make([]localizationDigestRow, 0, localizationFinalResponseSupportingLimit)
	selected := make(map[string]struct{}, localizationFinalResponsePrimaryLimit+localizationFinalResponseSupportingLimit)
	appendRow := func(dst *[]localizationDigestRow, limit int, row localizationDigestRow) bool {
		id := strings.TrimSpace(row.ID)
		if id == "" || strings.TrimSpace(row.File) == "" || len(*dst) >= limit {
			return false
		}
		if _, exists := selected[id]; exists {
			return false
		}
		selected[id] = struct{}{}
		*dst = append(*dst, row)
		return true
	}

	rowsByID := make(map[string]localizationDigestRow, len(rows))
	for _, row := range rows {
		rowsByID[strings.TrimSpace(row.ID)] = row
	}
	// Fresh rows already lead the merged evidence order. Once the initial ranked
	// cohort has been frozen, later supplemental projections cannot reshuffle it:
	// replay and permitted-read merges recover the exact original order first.
	appendPrimary := func(row localizationDigestRow) bool {
		if localizationFinalResponseSupportingOnly(row) {
			return false
		}
		return appendRow(&primaries, localizationFinalResponsePrimaryLimit, row)
	}
	for order := 1; order <= localizationFinalResponsePrimaryLimit; order++ {
		for _, row := range rows {
			if row.primaryCohortOrder == order {
				appendPrimary(row)
				break
			}
		}
	}

	// Authenticated literal rows reserve bounded seats before ordinary
	// task/rank selection. The second seat must bring a second proven file:
	// two owners of the same registration site add nothing over one.
	literalReserveUsed := 0
	literalReserveFiles := make(map[string]struct{}, localizationFinalResponseLiteralReserve)
	claimReserve := func(row localizationDigestRow) {
		literalReserveUsed++
		if file := strings.TrimSpace(row.File); file != "" {
			literalReserveFiles[file] = struct{}{}
		}
	}
	for _, row := range primaries {
		if row.Provenance == localizationProvenanceSourceLiteralCallee && row.literalPrimaryEligible {
			claimReserve(row)
		}
	}
	for _, row := range rows {
		if !localizationFinalResponsePrimaryProvenance(row) {
			continue
		}
		literal := row.Provenance == localizationProvenanceContentLiteral ||
			row.Provenance == localizationProvenanceSourceLiteralCallee
		if literal {
			if literalReserveUsed >= localizationFinalResponseLiteralReserve {
				continue
			}
			if literalReserveUsed > 0 {
				if _, sameFile := literalReserveFiles[strings.TrimSpace(row.File)]; sameFile {
					continue
				}
			}
			if appendPrimary(row) {
				claimReserve(row)
			}
			continue
		}
		appendPrimary(row)
	}

	taskTerms := exploreTerminalTerms(task)
	ownerKeys := make(map[string]struct{}, len(current))
	for _, row := range current {
		key := localizationDigestRowOwnerKey(row)
		if key == "" {
			if retained, exists := rowsByID[strings.TrimSpace(row.ID)]; exists {
				key = localizationDigestRowOwnerKey(retained)
			}
		}
		if key != "" {
			ownerKeys[key] = struct{}{}
		}
	}
	bestSameOwner := -1
	var bestSameOwnerScore localizationFinalResponseTaskScore
	if len(primaries) < localizationFinalResponsePrimaryLimit && len(ownerKeys) > 0 {
		for index, row := range rows {
			if _, exists := selected[strings.TrimSpace(row.ID)]; exists {
				continue
			}
			key := localizationDigestRowOwnerKey(row)
			if _, sameOwner := ownerKeys[key]; key == "" || !sameOwner {
				continue
			}
			score := scoreLocalizationFinalResponseTask(taskTerms, row)
			if bestSameOwner < 0 || localizationFinalResponseBetterTaskScore(score, bestSameOwnerScore) {
				bestSameOwner, bestSameOwnerScore = index, score
			}
		}
		if bestSameOwner >= 0 {
			appendPrimary(rows[bestSameOwner])
		}
	}

	bestTaskMatch := -1
	var bestTaskScore localizationFinalResponseTaskScore
	if len(primaries) < localizationFinalResponsePrimaryLimit && len(taskTerms) > 0 {
		for index, row := range rows {
			if _, exists := selected[strings.TrimSpace(row.ID)]; exists {
				continue
			}
			score := scoreLocalizationFinalResponseTask(taskTerms, row)
			if score.matched == 0 {
				continue
			}
			if bestTaskMatch < 0 || localizationFinalResponseBetterTaskScore(score, bestTaskScore) {
				bestTaskMatch, bestTaskScore = index, score
			}
		}
		if bestTaskMatch >= 0 {
			appendPrimary(rows[bestTaskMatch])
		}
	}

	for _, row := range rows {
		// Further authenticated literal callees may still win through ordinary
		// rank; only the pre-seated reserve above is capped at one.
		appendPrimary(row)
	}

	// After every ranked seat is filled, remaining task-named rows extend the
	// block by bounded extra seats. The admission lane must not decide
	// presentation authority: a declaration the task names is what the caller
	// asked about, whether it arrived through ranked retrieval, leading-file
	// completeness, or a body mention. Naming means the raw task carries the
	// row's identifier as a whole word, case-sensitively — prose substrings
	// do not extend the answer. Adjacency neighbors stay supporting: sharing
	// an identifier with the task does not make a graph hop the subject of
	// the report.
	if strings.TrimSpace(task) != "" {
		extendedLimit := localizationFinalResponsePrimaryLimit + localizationTaskAlignedExtraSeats
		type taskAlignedCandidate struct {
			index int
			score localizationFinalResponseTaskScore
		}
		aligned := make([]taskAlignedCandidate, 0, len(rows))
		for index, row := range rows {
			if _, exists := selected[strings.TrimSpace(row.ID)]; exists {
				continue
			}
			if row.Provenance == localizationProvenanceDirectAdjacency {
				continue
			}
			// Only the leading-file-depth writer of supportingOnly may be
			// overridden by task naming; a supplemental row stays supporting
			// no matter how well it matches the task.
			if localizationFinalResponseSupportingOnly(row) && !row.leadingFileDepth {
				continue
			}
			if !localizationTaskNamesRow(task, row) {
				continue
			}
			aligned = append(aligned, taskAlignedCandidate{
				index: index, score: scoreLocalizationFinalResponseTask(taskTerms, row),
			})
		}
		sort.SliceStable(aligned, func(left, right int) bool {
			return localizationFinalResponseBetterTaskScore(aligned[left].score, aligned[right].score)
		})
		for _, candidate := range aligned {
			appendRow(&primaries, extendedLimit, rows[candidate.index])
		}
	}

	supportingLimit := localizationReplayEvidenceLimit - len(primaries)
	for _, row := range rows {
		if localizationFinalResponseDirectRelation(row, primaries) {
			appendRow(&supporting, supportingLimit, row)
		}
	}
	for _, row := range rows {
		appendRow(&supporting, supportingLimit, row)
	}

	presented := make([]localizationFinalResponseRow, 0, len(primaries)+len(supporting))
	for _, row := range primaries {
		presented = append(presented, localizationFinalResponseRow{row: row, primary: true})
	}
	for _, row := range supporting {
		presented = append(presented, localizationFinalResponseRow{row: row})
	}
	return presented
}

func localizationFinalResponsePrimaryIDs(task string, current, rows []localizationDigestRow) []string {
	presented := localizationFinalResponseRows(task, current, rows)
	ids := make([]string, 0, localizationFinalResponsePrimaryLimit)
	for _, item := range presented {
		if !item.primary {
			break
		}
		if id := strings.TrimSpace(item.row.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// freezeLocalizationPrimaryCohort records the exact initial PRIMARY projection
// immediately before supplemental evidence is materialized. The order is
// request-local policy state: it is retained across digest rebuilds and reads,
// but never serialized into the response payload.
func freezeLocalizationPrimaryCohort(
	task string,
	envelope *localizationExploreEnvelope,
	digest *localizationEvidenceDigest,
) {
	if envelope == nil || digest == nil {
		return
	}
	primaryIDs := append([]string(nil), digest.primaryIDs...)
	if len(primaryIDs) == 0 {
		primaryIDs = localizationFinalResponsePrimaryIDs(task, nil, digest.Evidence)
	}
	if len(primaryIDs) > localizationFinalResponsePrimaryLimit {
		primaryIDs = primaryIDs[:localizationFinalResponsePrimaryLimit]
	}
	orders := make(map[string]int, len(primaryIDs))
	for index, id := range primaryIDs {
		if id = strings.TrimSpace(id); id != "" {
			orders[id] = index + 1
		}
	}
	for index := range envelope.Evidence {
		row := &envelope.Evidence[index]
		if order := orders[strings.TrimSpace(row.ID)]; order > 0 && !row.supportingOnly &&
			!localizationSupportingOnlyProvenance(row.Provenance) {
			row.primaryCohortOrder = order
		}
	}
	for index := range digest.Evidence {
		row := &digest.Evidence[index]
		if order := orders[strings.TrimSpace(row.ID)]; order > 0 && !localizationFinalResponseSupportingOnly(*row) {
			row.primaryCohortOrder = order
		}
	}
	refreshLocalizationDigestResponses(digest, task, nil)
}

func renderLocalizationFinalResponse(rows []localizationDigestRow) string {
	return renderLocalizationFinalResponseForTask("", nil, rows)
}

func renderLocalizationFinalResponseForTask(task string, current, rows []localizationDigestRow) string {
	presented := localizationFinalResponseRows(task, current, rows)
	return renderLocalizationAnswerPage(presented, localizationAnswerHeading,
		"No bounded localization evidence was found.", localizationAnswerReadyDirective, true)
}

// renderLocalizationProvisionalResponseForTask renders the same rows for a
// state that has not proven its answer yet. The heading and closing line are
// the whole difference: a caller must be able to tell a proven answer from the
// best one available so far, or the page teaches it to trust both equally.
// Unlike the proven page, this one trims its own tail to stay inside the byte
// cap — it is additional payload on responses that previously carried none, so
// it may not push an envelope over budget on its way in.
func renderLocalizationProvisionalResponseForTask(task string, current, rows []localizationDigestRow) string {
	presented := localizationFinalResponseRows(task, current, rows)
	if len(presented) > localizationProvisionalRowLimit {
		presented = presented[:localizationProvisionalRowLimit]
	}
	excerpts := true
	for {
		page := renderLocalizationAnswerPage(presented, localizationProvisionalHeading,
			"No localization evidence has been retained for this request yet.",
			localizationProvisionalDirective, excerpts)
		if len(page) <= localizationFinalResponseMaxBytes || len(presented) == 0 {
			return page
		}
		if excerpts {
			// An excerpt proves which declaration a row names; a row is the answer
			// itself. Drop every excerpt before the first located identity.
			excerpts = false
			continue
		}
		presented = presented[:len(presented)-1]
	}
}

// localizationFinalResponseExcerptMaxRunes bounds one declaration excerpt. The
// excerpt exists to show which declaration a row names, not to reproduce it.
const localizationFinalResponseExcerptMaxRunes = 200

// localizationFinalResponseExcerptRowLimit bounds how many primary rows carry
// an excerpt line. Deliberately narrower than the primary block itself: every
// primary's full body already ships in the page's prose section, and the
// rendered answer must hold the tight-budget wire bound at any primary width.
const localizationFinalResponseExcerptRowLimit = 3

func localizationFinalResponseExcerpt(row localizationDigestRow) string {
	return compactLocalizationField(
		localizationFinalResponseField(row.Signature), localizationFinalResponseExcerptMaxRunes,
	)
}

// localizationFinalResponseSymbolLabel keeps same-named rows distinguishable by
// the identity string alone. Graph identities already embed their file, so this
// only qualifies the bare identities a caller could otherwise not tell apart.
func localizationFinalResponseSymbolLabel(row localizationDigestRow, qualify bool) string {
	id := localizationFinalResponseField(row.ID)
	file := localizationFinalResponseField(row.File)
	if !qualify || id == "" || file == "" || strings.Contains(id, file) {
		return localizationDisplayIdentity(id, row.Line)
	}
	return file + "::" + localizationDisplayIdentity(id, row.Line)
}

// localizationDisplayIdentity renders one retained identity for prose. A graph
// ID carries two spellings no answer can say: the keyword a constructor is
// indexed under, and the positional suffix that separates same-named
// declarations. Both are presentation-only. The row's ID, the graph, and every
// machine field keep their exact spelling — only this label changes.
func localizationDisplayIdentity(identity string, line int) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return identity
	}
	prefix := ""
	member := identity
	if cut := strings.LastIndex(identity, "::"); cut >= 0 {
		prefix, member = identity[:cut+2], identity[cut+2:]
	}
	member = localizationConstructorProse(localizationTrimPositionalSuffix(member, line))
	if member == "" {
		return identity
	}
	return prefix + member
}

// localizationTrimPositionalSuffix drops the `_L<line>` / `@<line>` tie-break
// extractors append when one file declares the same name twice. The rendered
// row already carries the line in its own column. The trailing number must be
// that line: a declaration genuinely named `CACHE_L2` cannot satisfy it, and
// emitters disagree by one on where a declaration starts.
func localizationTrimPositionalSuffix(member string, line int) string {
	digits := len(member)
	for digits > 0 && member[digits-1] >= '0' && member[digits-1] <= '9' {
		digits--
	}
	if digits == len(member) {
		return member
	}
	suffix, err := strconv.Atoi(member[digits:])
	if err != nil || (line > 0 && suffix != line && suffix != line-1 && suffix != line+1) {
		return member
	}
	switch {
	case digits >= 2 && member[digits-2] == '_' && member[digits-1] == 'L':
		return member[:digits-2]
	case digits >= 1 && member[digits-1] == '@':
		return member[:digits-1]
	}
	return member
}

// localizationConstructorProse spells a keyword-indexed constructor the way an
// answer has to say it. The extractor names it `<Type>.<init>` across Java, C#
// and Swift; a caller repeating that back is naming a symbol no reader
// recognizes.
func localizationConstructorProse(member string) string {
	const keyword = "<init>"
	switch {
	case member == keyword:
		return "constructor"
	case strings.HasSuffix(member, "."+keyword):
		return strings.TrimSuffix(member, "."+keyword) + " constructor"
	default:
		return member
	}
}

func renderLocalizationAnswerPage(
	presented []localizationFinalResponseRow, heading, empty, directive string, excerpts bool,
) string {
	if len(presented) == 0 {
		return heading + "\n" + empty + "\n\n" + directive
	}
	homonyms := make(map[string]int, len(presented))
	for _, item := range presented {
		homonyms[localizationFinalResponseBareName(item.row)]++
	}
	var response strings.Builder
	response.WriteString(heading)
	response.WriteString("\n")
	response.WriteString(localizationAnswerShapeDirective)
	response.WriteString("\n")
	excerpted := 0
	for _, item := range presented {
		role := "SUPPORTING"
		if item.primary {
			role = "PRIMARY"
		}
		file := localizationFinalResponseField(item.row.File)
		id := localizationFinalResponseSymbolLabel(item.row,
			homonyms[localizationFinalResponseBareName(item.row)] > 1)
		if item.row.Line > 0 {
			fmt.Fprintf(&response, "- %s — %s:%d — %s\n", role, file, item.row.Line, id)
		} else {
			fmt.Fprintf(&response, "- %s — %s — %s\n", role, file, id)
		}
		// The excerpt rides its own line so the role/file/symbol tuple stays one
		// aligned, parseable row. Only the leading primaries carry one: the
		// full bodies for every primary already ship in the prose section, and
		// the page must stay inside the tight-budget wire bound however wide
		// the primary block is.
		if !excerpts || !item.primary || excerpted >= localizationFinalResponseExcerptRowLimit {
			continue
		}
		if excerpt := localizationFinalResponseExcerpt(item.row); excerpt != "" {
			response.WriteString("  ")
			response.WriteString(excerpt)
			response.WriteString("\n")
			excerpted++
		}
	}
	response.WriteString("\n")
	response.WriteString(directive)
	return response.String()
}

// refreshLocalizationDigestResponses keeps the proven and provisional pages
// derived from one row set. They are rebuilt together at every site that
// reshapes Evidence, so a shed row can never survive in one page after it has
// left the other.
func refreshLocalizationDigestResponses(digest *localizationEvidenceDigest, task string, current []localizationDigestRow) {
	if digest == nil {
		return
	}
	digest.primaryIDs = localizationFinalResponsePrimaryIDs(task, current, digest.Evidence)
	digest.finalResponse = renderLocalizationFinalResponseForTask(task, current, digest.Evidence)
	digest.provisionalResponse = renderLocalizationProvisionalResponseForTask(task, current, digest.Evidence)
}

// localizationAnswerClaimDiscipline closes the remaining gap between a page
// that names the right symbol and an answer that does. It rides the terminal
// directive only: a page still prescribing a step must keep asking for that
// step, and the refinement contract it carries is byte-budgeted.
const localizationAnswerClaimDiscipline = "Name only symbols this page lists or a permitted call already returned."

// The directive is the only instruction the caller sees on a terminal page, so
// it names the one failure mode measurement keeps finding: an answer that
// paraphrases the located identifier into a neighbouring one it inferred from
// the request text, losing the located symbol.
// The conclusion is bounded deliberately. Output is the most expensive token
// class in a session, and asking for the located lines verbatim already moved
// median output up measurably; an unbounded "explain" invites the model to
// restate evidence it has just quoted.
// Wording matters more than it looks. Ordering a caller to reproduce a page it
// judges wrong reads as coercion: measured across 336 sessions, pages carrying
// the word "verbatim" drew an explicit manipulation accusation in 30% of the
// caller's own statements against 2% on pages without it. Ask for the answer,
// name what the answer should carry, and leave the caller free to disagree —
// its disagreement is right more often than not.
const localizationAnswerReadyDirective = "Localization for this task is complete. Answer now from this evidence, naming the files and symbols you rely on. If it does not fit the request, say so and name what does — your judgement about the code is welcome, another navigation call is not. " + localizationAnswerClaimDiscipline

// The same instruction as a closing line is read after the answer is already
// shaped, which is why it is obeyed at a rate indistinguishable from absence.
// It leads the page instead, and it names the shape rather than describing it:
// how many candidates, what each row must carry, and the one thing a caller
// must not leave out when nothing corroborated the match. Two lines is the
// budget — every byte here is a byte the evidence rows do not get.
const localizationAnswerShapeDirective = "ANSWER WITH: the 2-3 strongest candidates below, each as its file plus a bare qualified identifier (Class.method), nothing trailing it.\n" +
	"If the match is unconfirmed, say so in the answer."

// localizationUnconfirmedAnswerDirective closes a page that terminates without
// corroboration. It must not read as the proven directive: the caller has to
// know the bounded allowance ran out and the candidates were never confirmed.
const localizationUnconfirmedAnswerDirective = "The bounded localization allowance is spent and nothing corroborated these candidates. Answer now from them, naming the files and symbols you rely on, and say plainly that the match is unconfirmed. " + localizationAnswerClaimDiscipline

const (
	localizationAnswerHeading      = "LOCALIZATION:"
	localizationProvisionalHeading = "LOCALIZATION (UNCONFIRMED):"
)

// The provisional page exists because a session can stop at any turn: the
// caller may run out of steps, decide it has enough, or give up. Until now
// every state but one handed back no answer at all, so those sessions ended
// with nothing — measurably, one in five of them.
// It is deliberately terse. The envelope budget is 6400 bytes and a real page
// already fills most of it, so a wordy fallback is a fallback that gets shed
// before it ever reaches a caller. Say the three things that matter — these
// are unconfirmed, the prescribed step is still the better move, and stopping
// with them beats stopping with nothing — and stop.
const localizationProvisionalDirective = "Unconfirmed. Prefer the step this response prescribes; if you stop here instead, answer with these candidates and say they are unconfirmed."

// localizationProvisionalRowLimit keeps the unconfirmed page to the rows most
// likely to be the answer. Every identity it lists is already in the
// envelope's evidence, so a longer page buys repetition, not information.
const localizationProvisionalRowLimit = localizationFinalResponsePrimaryLimit

func localizationDigestRowsByID(digest *localizationEvidenceDigest) map[string]localizationDigestRow {
	retained := make(map[string]localizationDigestRow)
	if digest == nil {
		return retained
	}
	for _, row := range digest.Evidence {
		if symbol := strings.TrimSpace(row.ID); symbol != "" {
			retained[symbol] = row
		}
	}
	return retained
}

func localizationDigestHasProvenance(rows map[string]localizationDigestRow, provenance string) bool {
	for _, row := range rows {
		if row.Provenance == provenance {
			return true
		}
	}
	return false
}

func localizationDigestStrongProofRetained(rows map[string]localizationDigestRow, symbol string) bool {
	row, exists := rows[strings.TrimSpace(symbol)]
	if !exists {
		return false
	}
	switch row.Provenance {
	case localizationProvenanceSourceLiteralCallee:
		return true
	case localizationProvenanceDivergentDefault:
		return localizationDigestHasProvenance(rows, localizationProvenanceDivergentDefaultType)
	case localizationProvenanceImplementationRoute:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationTarget)
	case localizationProvenanceImplementationTarget:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationRoute)
	default:
		return false
	}
}

func localizationDigestAnyStrongProofRetained(rows map[string]localizationDigestRow) bool {
	for symbol := range rows {
		if localizationDigestStrongProofRetained(rows, symbol) {
			return true
		}
	}
	return false
}

// localizationDigestReconcileRoute preserves a concrete advisory read when its
// optional proof was shed, but never preserves a generic-wrapper hop without
// the concrete implementation that makes the route useful.
func localizationDigestReconcileRoute(
	symbol string,
	route localizationRefinementRoute,
	rows map[string]localizationDigestRow,
) (localizationRefinementRoute, bool) {
	row, retained := rows[symbol]
	if !retained {
		return localizationRefinementRoute{}, false
	}
	if route.implementationSymbol != "" {
		implementation, implementationRetained := rows[route.implementationSymbol]
		if !implementationRetained {
			return localizationRefinementRoute{}, false
		}
		if route.enforceable && (row.Provenance != localizationProvenanceImplementationRoute ||
			implementation.Provenance != localizationProvenanceImplementationTarget) {
			route.enforceable = false
		}
	}
	if route.proofSymbol != "" {
		proof, proofRetained := rows[route.proofSymbol]
		if !proofRetained || proof.Provenance != localizationProvenanceImplementationRoute ||
			row.Provenance != localizationProvenanceImplementationTarget {
			route.proofSymbol = ""
			route.enforceable = false
		}
	}
	if route.enforceable && route.implementationSymbol == "" && route.proofSymbol == "" &&
		row.Provenance != localizationProvenanceSourceLiteralCallee {
		route.enforceable = false
	}
	return route, true
}

func localizationCompletionBoundedByDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		return completion
	}
	retained := localizationDigestRowsByID(digest)
	advisory := func() localizationCompletion {
		bounded := newLocalizationAdvisoryCompletion()
		bounded.taskLead = completion.taskLead
		return bounded
	}

	switch completion.State {
	case localizationStateAnswerReady:
		if completion.Enforceable && !localizationDigestAnyStrongProofRetained(retained) {
			completion.Enforceable = false
		}
	case localizationStateNeedsExactRead:
		if _, exists := retained[completion.ExactSymbol]; !exists || completion.ExactSymbol == "" {
			return advisory()
		}
		if completion.enforceableOnAnswerReady &&
			!localizationDigestStrongProofRetained(retained, completion.ExactSymbol) {
			completion.enforceableOnAnswerReady = false
		}
	case localizationStateNeedsRefinement:
		allowed := make([]string, 0, len(completion.AllowedSymbols))
		seen := make(map[string]struct{}, len(completion.AllowedSymbols))
		var routes map[string]localizationRefinementRoute
		if len(completion.refinementRoutes) > 0 {
			routes = make(map[string]localizationRefinementRoute, len(completion.refinementRoutes))
		}
		for _, symbol := range completion.AllowedSymbols {
			symbol = strings.TrimSpace(symbol)
			if symbol == "" {
				continue
			}
			if _, exists := retained[symbol]; !exists {
				continue
			}
			if _, duplicate := seen[symbol]; duplicate {
				continue
			}
			if route, exists := completion.refinementRoutes[symbol]; exists {
				reconciled, usable := localizationDigestReconcileRoute(symbol, route, retained)
				if !usable {
					continue
				}
				routes[symbol] = reconciled
			}
			seen[symbol] = struct{}{}
			allowed = append(allowed, symbol)
		}
		if len(allowed) == 0 {
			return advisory()
		}
		preferred := strings.TrimSpace(completion.refinementSymbol)
		if _, exists := seen[preferred]; !exists {
			preferred = allowed[0]
		}
		bounded := newLocalizationRefinementCompletionForSymbols(preferred, allowed)
		bounded.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
		bounded.taskLead = completion.taskLead
		if routes != nil {
			bounded.refinementRoutes = routes
			bounded.correctionSymbol, bounded.correctionRoute = localizationRankedCorrection(
				preferred, allowed, bounded.refinementRoutes,
			)
		}
		return bounded
	}
	return completion
}

// localizationContractReconciledWithDigest is the only projection boundary
// from retained evidence to a wire contract. Call it after every digest reshape:
// byte shedding may remove an authorized identity or one of its proof rows.
func localizationContractReconciledWithDigest(
	completion localizationCompletion,
	digest *localizationEvidenceDigest,
) localizationTerminalContract {
	completion = localizationCompletionBoundedByDigest(completion, digest)
	completion = localizationCompletionWithDigest(completion, digest)
	return localizationContractFor(completion)
}

// localizationStateCarriesEvidence reports whether a state is inside a
// localization flow that has ranked something. The inactive state is excluded
// deliberately: it rides responses that were never a localization request, and
// a candidate page there is payload with no question to answer.
func localizationStateCarriesEvidence(state string) bool {
	switch state {
	case localizationStateNeedsExactRead, localizationStateExactReadInFlight,
		localizationStateNeedsRefinement, localizationStateRefineInFlight,
		localizationStateNeedsRecovery, localizationStateRecoveryInFlight,
		localizationStateLocalized:
		return true
	default:
		return false
	}
}

func localizationCompletionWithDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		digest = completion.digest
	}
	completion.digest = digest
	if completion.State != localizationStateAnswerReady {
		// The rows are already ranked and packed here; only the confidence to
		// call them an answer is missing. Discarding the page left a caller that
		// stops early — out of steps, or satisfied — with nothing to say, which
		// is strictly worse than a candidate list that admits it is unconfirmed.
		// A state holding no evidence still gets nothing: boilerplate with no
		// identities in it is payload the caller cannot answer from.
		completion.FinalResponse = ""
		if localizationStateCarriesEvidence(completion.State) && digest != nil && len(digest.Evidence) > 0 {
			completion.FinalResponse = digest.provisionalResponse
		}
		return completion
	}
	if completion.provisionalAnswer {
		// A terminal state reached without corroboration keeps the unconfirmed
		// heading it earned — including when it retained nothing at all, which is
		// the one case a proven page would be furthest from true. Only the closing
		// line changes: no step remains to prefer, so the page says the allowance
		// is spent.
		var rows []localizationDigestRow
		page := ""
		if digest != nil {
			rows, page = digest.Evidence, digest.provisionalResponse
		}
		if page == "" {
			page = renderLocalizationProvisionalResponseForTask("", nil, rows)
		}
		completion.FinalResponse = localizationUnconfirmedTerminalPage(page)
		return completion
	}
	if digest != nil && digest.finalResponse != "" {
		completion.FinalResponse = digest.finalResponse
	} else if completion.FinalResponse == "" {
		completion.FinalResponse = renderLocalizationFinalResponse(nil)
	}
	return completion
}

func localizationUnconfirmedTerminalPage(page string) string {
	if !strings.HasSuffix(page, localizationProvisionalDirective) {
		return page
	}
	return strings.TrimSuffix(page, localizationProvisionalDirective) + localizationUnconfirmedAnswerDirective
}

func localizationTerminalStructuredContent(payload any, contract localizationTerminalContract) map[string]any {
	var structured map[string]any
	switch existing := payload.(type) {
	case map[string]any:
		structured = make(map[string]any, len(existing)+4)
		for key, value := range existing {
			structured[key] = value
		}
	case nil:
		structured = make(map[string]any, 4)
	default:
		structured = map[string]any{"payload": existing}
	}
	structured["completion"] = contract.Completion
	structured["terminal"] = contract.Terminal
	if contract.Terminal && contract.Completion.FinalResponse != "" {
		// completion already carries the answer, and the directive is its closing
		// line. A second top-level copy is the same block billed twice at the
		// cache-write rate, which is the most expensive place to repeat oneself:
		// point at it instead.
		structured["directive"] = localizationAnswerReadyDirective
		if contract.Completion.provisionalAnswer {
			structured["directive"] = localizationUnconfirmedAnswerDirective
		}
	}
	return structured
}

// localizationHostEnvelope stores each retained row exactly once. Hosts render
// the ordered rows with fallback_format; no prewritten answer or duplicate row
// string crosses the wire.
type localizationHostEnvelope struct {
	Version        int                          `json:"version"`
	FallbackFormat string                       `json:"fallback_format"`
	Evidence       *localizationEvidenceDigest  `json:"evidence"`
	PrimaryIDs     []string                     `json:"primary_ids,omitempty"`
	Contract       localizationTerminalContract `json:"contract"`
}

// Initial localization and authorized reads call this only after byte-budget
// packing and evidence-policy finalization, so visible and authoritative host
// contracts always describe the same completion.
func attachLocalizationHostEnvelope(result *mcpgo.CallToolResult, completion localizationCompletion, digest *localizationEvidenceDigest) *mcpgo.CallToolResult {
	if result == nil {
		return result
	}
	completion = localizationCompletionWithDigest(completion, digest)
	contract := localizationContractFor(completion)
	// Preserve preterminal tool payloads byte-for-byte. Only answer_ready adds
	// the terminal host projection; refinement and recovery retain their
	// existing structuredContent shape and visible completion envelope.
	if completion.State == localizationStateAnswerReady {
		base := result.StructuredContent
		if base == nil {
			// A host that renders structured content in preference to text sees
			// only this projection. Replacing a nil payload would therefore erase
			// the tool's own answer — including the source an authorized read was
			// just permitted to fetch — so decode the text payload and keep it
			// underneath the terminal keys.
			if text, ok := singleTextContent(result); ok {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(text), &decoded); err == nil {
					base = decoded
				}
			}
		}
		result.StructuredContent = localizationTerminalStructuredContent(base, contract)
	}
	if result.Meta == nil {
		result.Meta = &mcpgo.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	primaryIDs := []string(nil)
	if digest != nil {
		primaryIDs = append(primaryIDs, digest.primaryIDs...)
	}
	result.Meta.AdditionalFields[localizationHostMetaKey] = localizationHostEnvelope{
		Version:        1,
		FallbackFormat: "{file}:{line} — {id} ({signature})",
		Evidence:       digest,
		PrimaryIDs:     primaryIDs,
		Contract:       contract,
	}
	return result
}

// localizationAnswerReadyResult is the successful, deterministic evidence
// replay. Hooked hosts may stop before dispatch; every other host receives the
// same ready-to-emit answer and directive on every post-terminal navigation.
func localizationAnswerReadyResult(completion localizationCompletion) *mcpgo.CallToolResult {
	completion = localizationCompletionWithDigest(completion, completion.digest)
	visible := completion.FinalResponse
	// Older retained completions may predate the in-response convergence cue.
	// Preserve their successful replay shape without duplicating the directive
	// for newly rendered terminal evidence.
	if !strings.HasSuffix(visible, localizationAnswerReadyDirective) &&
		!strings.HasSuffix(visible, localizationUnconfirmedAnswerDirective) {
		visible += "\n\n" + localizationAnswerReadyDirective
	}
	result := mcpgo.NewToolResultText(visible)
	return attachLocalizationHostEnvelope(result, completion, completion.digest)
}

// newLocalizationEvidenceDigest retains rows without a request in hand. Callers
// that know the request use newLocalizationEvidenceDigestForTask so the
// presented rows are scored against it.
func newLocalizationEvidenceDigest(envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	return newLocalizationEvidenceDigestForTask("", envelope)
}
