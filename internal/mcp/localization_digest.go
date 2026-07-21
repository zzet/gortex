package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Terminal evidence retention.
//
// The localize handler builds a byte-budgeted evidence envelope once and
// retains a compact projection for a deterministic terminal replay. A
// post-terminal navigation call receives that projection as a successful,
// actionable result rather than an error that invites another verification
// loop.

const (
	// localizationDigestMaxBytes bounds retained session state independently of
	// the original envelope budget.
	localizationDigestMaxBytes = 4096
	// localizationFinalResponseMaxBytes bounds the ready-to-emit answer
	// independently of the retained digest. Typical responses are much smaller;
	// the cap protects repeated terminal calls from inflating token usage.
	localizationFinalResponseMaxBytes = 4096
	// localizationReplayEvidenceLimit prevents a broad localization envelope
	// from becoming an exhaustive, implicitly endorsed answer during replay.
	// Five keeps the promoted structural/literal candidates reserved by the
	// envelope builder while bounding repeat-turn cost.
	localizationReplayEvidenceLimit = 5
	// This canonical envelope is deliberately carried in MCP _meta. Adapting
	// hosts may render its ordered evidence deterministically without exposing
	// retained rows to model-visible text or structuredContent.
	localizationHostMetaKey   = "gortex/localization"
	localizationReplayVersion = 1
)

const localizationReplayDirective = "You already hold the localization answer — respond now using final_response. Do not call another tool."

// localizationEvidenceDigest is the compact, session-retained projection of
// an answer envelope: ranked candidate evidence without source bodies.
type localizationEvidenceDigest struct {
	Files    []string                `json:"files,omitempty"`
	Symbols  []string                `json:"symbols,omitempty"`
	Evidence []localizationDigestRow `json:"evidence,omitempty"`
}

type localizationDigestRow struct {
	Rank       int    `json:"rank,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	QualName   string `json:"qual_name,omitempty"`
	Kind       string `json:"kind,omitempty"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Signature  string `json:"signature,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

func cloneLocalizationEvidenceDigest(digest *localizationEvidenceDigest) *localizationEvidenceDigest {
	if digest == nil {
		return nil
	}
	return &localizationEvidenceDigest{
		Files:    append([]string(nil), digest.Files...),
		Symbols:  append([]string(nil), digest.Symbols...),
		Evidence: append([]localizationDigestRow(nil), digest.Evidence...),
	}
}

// buildLocalizationFinalResponse renders a deterministic answer in the same
// rank order as the served envelope. It never includes source bodies or graph
// expansion lists. If the prose exceeds its independent cap, optional fields
// and then the lowest-ranked rows are shed atomically; no partial UTF-8 line is
// emitted.
func buildLocalizationFinalResponse(digest *localizationEvidenceDigest) string {
	working := cloneLocalizationEvidenceDigest(digest)
	if working == nil {
		working = &localizationEvidenceDigest{}
	}
	for {
		response := renderLocalizationFinalResponse(working)
		if len(response) <= localizationFinalResponseMaxBytes {
			return response
		}
		for index := len(working.Evidence) - 1; index >= 0; index-- {
			if working.Evidence[index].Signature != "" {
				working.Evidence[index].Signature = ""
				goto retry
			}
			if working.Evidence[index].Name != "" {
				working.Evidence[index].Name = ""
				goto retry
			}
		}
		if len(working.Evidence) > 1 {
			working.Evidence = working.Evidence[:len(working.Evidence)-1]
			rebuildLocalizationDigestSkeleton(working)
			continue
		}
		// Production paths and symbol IDs are already bounded. Keep the response
		// total even for synthetic or legacy state with unusually large scalars.
		for len(working.Files) > 1 {
			working.Files = working.Files[:len(working.Files)-1]
		}
		for len(working.Symbols) > 1 {
			working.Symbols = working.Symbols[:len(working.Symbols)-1]
		}
		for index := range working.Files {
			working.Files[index] = truncateLocalizationReplayScalar(working.Files[index], 512)
		}
		for index := range working.Symbols {
			working.Symbols[index] = truncateLocalizationReplayScalar(working.Symbols[index], 768)
		}
		for index := range working.Evidence {
			working.Evidence[index].File = truncateLocalizationReplayScalar(working.Evidence[index].File, 512)
			working.Evidence[index].ID = truncateLocalizationReplayScalar(working.Evidence[index].ID, 768)
		}
		return renderLocalizationFinalResponse(working)
	retry:
	}
}

func renderLocalizationFinalResponse(digest *localizationEvidenceDigest) string {
	var builder strings.Builder
	builder.WriteString("FILES:\n")
	if digest == nil || len(digest.Files) == 0 {
		builder.WriteString("- (none)\n")
	} else {
		for _, file := range digest.Files {
			fmt.Fprintf(&builder, "- %s\n", compactLocalizationReplayScalar(file))
		}
	}
	builder.WriteString("\nSYMBOLS:\n")
	if digest == nil || len(digest.Symbols) == 0 {
		builder.WriteString("- (none)\n")
	} else {
		for _, symbol := range digest.Symbols {
			fmt.Fprintf(&builder, "- %s\n", compactLocalizationReplayScalar(symbol))
		}
	}
	builder.WriteString("\nEVIDENCE:\n")
	if digest == nil || len(digest.Evidence) == 0 {
		builder.WriteString("- (none)")
		return builder.String()
	}
	for index, row := range digest.Evidence {
		rank := row.Rank
		if rank <= 0 {
			rank = index + 1
		}
		location := compactLocalizationReplayScalar(row.File)
		if row.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, row.Line)
		}
		fmt.Fprintf(&builder, "- #%d %s — %s", rank, location, compactLocalizationReplayScalar(row.ID))
		if name := compactLocalizationReplayScalar(row.Name); name != "" {
			fmt.Fprintf(&builder, " — %s", name)
		}
		if signature := compactLocalizationReplayScalar(row.Signature); signature != "" {
			fmt.Fprintf(&builder, " — %s", signature)
		}
		if index+1 < len(digest.Evidence) {
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func compactLocalizationReplayScalar(value string) string {
	var builder strings.Builder
	spacePending := false
	for _, current := range strings.TrimSpace(value) {
		if unicode.IsSpace(current) || unicode.IsControl(current) {
			spacePending = builder.Len() > 0
			continue
		}
		if spacePending {
			builder.WriteByte(' ')
			spacePending = false
		}
		builder.WriteRune(current)
	}
	return builder.String()
}

func truncateLocalizationReplayScalar(value string, maxBytes int) string {
	value = compactLocalizationReplayScalar(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	const ellipsis = "…"
	var builder strings.Builder
	for _, current := range value {
		encoded := string(current)
		if builder.Len()+len(encoded)+len(ellipsis) > maxBytes {
			break
		}
		builder.WriteString(encoded)
	}
	builder.WriteString(ellipsis)
	return builder.String()
}

// newLocalizationEvidenceDigest retains only concrete ranked evidence rows.
// Files and Symbols are rebuilt from those rows, so an item that was shed by
// the replay limit or byte budget cannot survive as an unsupported answer
// candidate. The upstream ordering already reserves the strongest direct,
// exact, literal, and promoted structural targets before lower-ranked fan-out.
func newLocalizationEvidenceDigest(envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	for _, row := range envelope.Evidence {
		if len(digest.Evidence) >= localizationReplayEvidenceLimit {
			break
		}
		if row.ID == "" || row.File == "" {
			continue
		}
		if _, exists := seen[row.ID]; exists {
			continue
		}
		seen[row.ID] = struct{}{}
		digest.Evidence = append(digest.Evidence, localizationDigestRow{
			Rank:       row.Rank,
			ID:         row.ID,
			Name:       row.Name,
			QualName:   row.QualName,
			Kind:       row.Kind,
			File:       row.File,
			Line:       row.Line,
			Signature:  row.Signature,
			Provenance: row.Provenance,
		})
	}
	for {
		rebuildLocalizationDigestSkeleton(digest)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}
		last := len(digest.Evidence) - 1
		if shedLocalizationDigestRowOptionalFields(&digest.Evidence[last]) {
			continue
		}
		if last == 0 {
			// Keep a usable identity even for synthetic or legacy rows that exceed
			// production path/symbol bounds. Shrink both scalars gradually so the
			// retained projection remains a hard byte cap after JSON escaping.
			if shrinkLocalizationDigestRowIdentity(&digest.Evidence[0]) {
				continue
			}
			// The minimum identity sizes plus fixed JSON overhead fit comfortably
			// below the cap. This is a defensive fallback for marshal anomalies.
			digest.Evidence = nil
			continue
		}
		digest.Evidence = digest.Evidence[:last]
	}
}

func shrinkLocalizationDigestRowIdentity(row *localizationDigestRow) bool {
	if row == nil {
		return false
	}
	const minimumIdentityBytes = 32
	shrink := func(value string) string {
		value = compactLocalizationReplayScalar(value)
		if len(value) <= minimumIdentityBytes {
			return value
		}
		limit := len(value) * 3 / 4
		if limit < minimumIdentityBytes {
			limit = minimumIdentityBytes
		}
		return truncateLocalizationReplayScalar(value, limit)
	}
	file := shrink(row.File)
	id := shrink(row.ID)
	changed := file != row.File || id != row.ID
	row.File = file
	row.ID = id
	return changed
}

func shedLocalizationDigestRowOptionalFields(row *localizationDigestRow) bool {
	if row == nil {
		return false
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
	if row.Provenance != "" {
		row.Provenance = ""
		return true
	}
	return false
}

func rebuildLocalizationDigestSkeleton(digest *localizationEvidenceDigest) {
	digest.Files = digest.Files[:0]
	digest.Symbols = digest.Symbols[:0]
	seenFiles := make(map[string]struct{}, len(digest.Evidence))
	seenSymbols := make(map[string]struct{}, len(digest.Evidence))
	for _, row := range digest.Evidence {
		if _, exists := seenFiles[row.File]; !exists {
			seenFiles[row.File] = struct{}{}
			digest.Files = append(digest.Files, row.File)
		}
		if _, exists := seenSymbols[row.ID]; !exists {
			seenSymbols[row.ID] = struct{}{}
			digest.Symbols = append(digest.Symbols, row.ID)
		}
	}
}

// localizationHostEnvelope carries the authoritative completion contract used
// by installed hooks. Replay marks only intercepted post-terminal navigation;
// initial localization and the one permitted refinement read keep it false.
type localizationHostEnvelope struct {
	Version        int                          `json:"version"`
	FallbackFormat string                       `json:"fallback_format"`
	Evidence       *localizationEvidenceDigest  `json:"evidence"`
	Contract       localizationTerminalContract `json:"contract"`
	Replay         bool                         `json:"replay,omitempty"`
}

type localizationReplayPayload struct {
	Directive      string                      `json:"directive"`
	FinalResponse  string                      `json:"final_response"`
	Completion     localizationCompletion      `json:"completion"`
	Terminal       bool                        `json:"terminal"`
	EvidenceDigest *localizationEvidenceDigest `json:"evidence_digest"`
	ReplayVersion  int                         `json:"replay_version"`
}

// localizationReplayWirePayload is the compact model-visible replay. The full
// typed completion and retained digest remain in MCP metadata for adapters;
// repeating them in structuredContent makes the same answer look like protocol
// noise and materially increases every recovery turn.
type localizationReplayWirePayload struct {
	Directive     string `json:"directive"`
	FinalResponse string `json:"final_response"`
}

func localizationReplayFor(completion localizationCompletion) localizationReplayPayload {
	contract := localizationContractFor(completion)
	return localizationReplayPayload{
		ReplayVersion:  localizationReplayVersion,
		Completion:     contract.Completion,
		Terminal:       contract.Terminal,
		EvidenceDigest: cloneLocalizationEvidenceDigest(completion.digest),
		FinalResponse:  contract.Completion.FinalResponse,
		Directive:      localizationReplayDirective,
	}
}

func localizationTerminalStructuredFields(completion localizationCompletion) map[string]any {
	payload := localizationReplayFor(completion)
	return map[string]any{
		"replay_version":  payload.ReplayVersion,
		"completion":      payload.Completion,
		"terminal":        payload.Terminal,
		"evidence_digest": payload.EvidenceDigest,
		"final_response":  payload.FinalResponse,
		"directive":       payload.Directive,
	}
}

func mergeLocalizationTerminalStructuredFields(target map[string]any, completion localizationCompletion) map[string]any {
	if target == nil {
		target = make(map[string]any)
	}
	if completion.State != localizationStateAnswerReady {
		return target
	}
	for key, value := range localizationTerminalStructuredFields(completion) {
		target[key] = value
	}
	return target
}

// Initial localization and authorized reads call this only after byte-budget
// packing and evidence-policy finalization, so visible and authoritative host
// contracts always describe the same completion.
func attachLocalizationHostEnvelope(result *mcpgo.CallToolResult, completion localizationCompletion, digest *localizationEvidenceDigest) *mcpgo.CallToolResult {
	return attachLocalizationHostEnvelopeMode(result, completion, digest, false)
}

func attachLocalizationHostEnvelopeMode(result *mcpgo.CallToolResult, completion localizationCompletion, digest *localizationEvidenceDigest, replay bool) *mcpgo.CallToolResult {
	if result == nil {
		return result
	}
	if result.Meta == nil {
		result.Meta = &mcpgo.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	result.Meta.AdditionalFields[localizationHostMetaKey] = localizationHostEnvelope{
		Version:        1,
		FallbackFormat: "{file}:{line} — {id} ({signature})",
		Evidence:       cloneLocalizationEvidenceDigest(digest),
		Contract:       localizationContractFor(completion),
		Replay:         replay,
	}
	return result
}

func isLocalizationTerminalReplay(result *mcpgo.CallToolResult) bool {
	if result == nil || result.Meta == nil || result.Meta.AdditionalFields == nil {
		return false
	}
	switch envelope := result.Meta.AdditionalFields[localizationHostMetaKey].(type) {
	case localizationHostEnvelope:
		return envelope.Replay
	case *localizationHostEnvelope:
		return envelope != nil && envelope.Replay
	default:
		return false
	}
}

// localizationAnswerReadyResult is a successful, deterministic evidence
// replay. The visible text is immediately answerable by every host, while the
// canonical structured payload exposes stable fields for adapters. A fresh
// result and deep-cloned digest are built on every call so outer code cannot
// mutate future replays.
func localizationAnswerReadyResult(completion localizationCompletion) *mcpgo.CallToolResult {
	payload := localizationReplayFor(completion)
	text := payload.Directive + "\n\n" + payload.FinalResponse
	result := mcpgo.NewToolResultText(text)
	wire := localizationReplayWirePayload{
		Directive:     payload.Directive,
		FinalResponse: payload.FinalResponse,
	}
	if body, err := json.Marshal(wire); err == nil {
		result.StructuredContent = json.RawMessage(append([]byte(nil), body...))
	} else {
		result.StructuredContent = wire
	}
	return attachLocalizationHostEnvelopeMode(result, payload.Completion, payload.EvidenceDigest, true)
}
