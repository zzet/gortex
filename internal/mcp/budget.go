package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/respbudget"
)

// The budget arithmetic (default cap, token ratio, opt-out semantics,
// structural trim) lives in internal/respbudget so the daemon's
// federation merge applies the exact same rules to the final merged
// representation. The aliases below keep this package's call sites
// unchanged.
const defaultMaxBytes = respbudget.DefaultMaxBytes

const avgBytesPerToken = respbudget.AvgBytesPerToken

// tokenBudgetParamDescription is the canonical description string
// for the `max_tokens` parameter wired onto every list-shaped tool.
// Centralised so the wording stays consistent across the registry.
const tokenBudgetParamDescription = "Cap the marshaled response at approximately this many tokens (3.5 bytes/token heuristic; composable with max_bytes — tighter wins). Use when you have a token budget instead of a byte budget. Omit for no cap; pass 0 to opt out explicitly."

// tokensToBytes converts a token budget into a byte cap; see
// respbudget.TokensToBytes for the ratio's calibration notes.
func tokensToBytes(maxTokens int) int {
	return respbudget.TokensToBytes(maxTokens)
}

// bytesToTokens is the inverse of tokensToBytes.
func bytesToTokens(byteCount int) int {
	return respbudget.BytesToTokens(byteCount)
}

// numArgInt extracts an integer arg from the request arguments map;
// see respbudget.NumArgInt for the absent-vs-malformed semantics.
func numArgInt(args map[string]any, key string) (int, bool) {
	return respbudget.NumArgInt(args, key)
}

// budgetTruncatedKey is the meta flag appended to a payload trimmed
// by applyBudget so callers can branch on truncation without scanning
// for shape-specific signals. Mirrored on the GCX path through the
// `truncated_by_budget=true` header meta.
const budgetTruncatedKey = respbudget.TruncatedKey

// applyBudget enforces a marshaled-size cap on payload by trimming
// top-level lists in longest-first order until the result fits; the
// algorithm and its truncation-meta contract live in respbudget.Apply,
// shared with the daemon's federation merge.
func applyBudget(payload any, maxBytes int) (any, bool) {
	return respbudget.Apply(payload, maxBytes)
}

// applyFieldsFilter returns a copy of payload with only the fields
// listed in `fields` retained on each list element (and on top-level
// scalar fields). Empty `fields` returns payload unchanged. Designed
// for sparse fieldsets — the caller asks for `id,line` and gets only
// those keys, dropping verbose `meta`, `doc`, etc.
//
// Filtering happens after the budget guard so a sparse request fits
// even more rows under the same cap.
func applyFieldsFilter(payload any, fields []string) any {
	if len(fields) == 0 || payload == nil {
		return payload
	}
	keep := make(map[string]bool, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			keep[f] = true
		}
	}
	if len(keep) == 0 {
		return payload
	}

	// Round-trip through JSON for the same uniform-shape reason as
	// applyBudget. This also mirrors the GCX/TOON pipeline so the
	// fields filter behaves identically across formats.
	bytes, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	var generic map[string]any
	if err := json.Unmarshal(bytes, &generic); err != nil {
		return payload
	}
	for k, v := range generic {
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(arr))
		for _, row := range arr {
			rowMap, ok := row.(map[string]any)
			if !ok {
				filtered = append(filtered, row)
				continue
			}
			out := make(map[string]any, len(keep))
			for f := range keep {
				if val, ok := rowMap[f]; ok {
					out[f] = val
				}
			}
			filtered = append(filtered, out)
		}
		generic[k] = filtered
	}
	return generic
}

// parseFields splits the comma-separated `fields` arg into a clean
// slice. Whitespace is stripped; empty tokens are dropped.
func parseFields(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trimGCXBytes shrinks a GCX1 payload that exceeds maxBytes by
// dropping rows from the tail of the LAST section, leaving the
// header and any earlier sections intact. Each GCX row ends at `\n`
// (embedded newlines in field values are escaped to a literal `\n`
// sequence in the wire format, so a raw byte 0x0A unambiguously
// marks a row boundary), which makes byte-level trimming safe and
// cheap. Multi-section payloads — flow_between, taint_paths,
// contracts.check, get_editing_context, smart_context — start each
// section with a fresh `GCX1 tool=` header line; we trim only the
// last section because earlier sections usually carry the summary
// metadata the caller needs to interpret what was dropped.
//
// A trailing comment line records the truncation:
//
//	# truncated_by_budget=true original_rows=N kept_rows=K
//
// Agents that decode GCX with `@gortex/wire` see this as a comment
// and skip it; agents reading the raw text get a clear hint.
//
// Returns the trimmed payload (always ≤ maxBytes when there's at
// least one droppable row) and a flag indicating whether trimming
// occurred. Unchanged when payload was already under the cap.
func trimGCXBytes(payload []byte, maxBytes int) ([]byte, bool) {
	if maxBytes <= 0 || len(payload) <= maxBytes {
		return payload, false
	}

	// Locate the last section header (`GCX1 tool=`). The first
	// section header is always at offset 0; subsequent headers start
	// after a `\n` and begin with `GCX1`. We scan to find the last
	// one so single-section payloads (the common case) work without
	// branching.
	const tag = "GCX1 tool="
	lastSectionStart := 0
	for i := 0; i+len(tag) <= len(payload); i++ {
		if i > 0 && payload[i-1] != '\n' {
			continue
		}
		if string(payload[i:i+len(tag)]) == tag {
			lastSectionStart = i
		}
	}

	// Find the end of the last section's header line (first `\n`
	// after lastSectionStart). Header is preserved verbatim — we
	// never trim it.
	headerEnd := lastSectionStart
	for headerEnd < len(payload) && payload[headerEnd] != '\n' {
		headerEnd++
	}
	if headerEnd >= len(payload) {
		return payload, false
	}
	headerEnd++ // skip past `\n`

	// Walk row boundaries from the tail. Each row ends at `\n` that
	// is NOT followed by a header line. We collect the cumulative
	// byte offset of every row terminator within the last section
	// so we can binary-truncate at the largest prefix that fits.
	rowEnds := make([]int, 0, 64)
	originalRowCount := 0
	for i := headerEnd; i < len(payload); i++ {
		if payload[i] != '\n' {
			continue
		}
		// Skip comment-line endings (preserve all comments — they
		// carry tool-author hints) and section-boundary newlines.
		// Comments start with `#`. Section boundary detection: the
		// next byte after the trailing `\n` is the start of `GCX1`.
		// We never split across that boundary; lastSectionStart is
		// the actual upper bound for our slice anyway.
		rowEnds = append(rowEnds, i+1)
		// Increment row count only for non-comment rows so the
		// trailing meta is faithful. Find the start of this row.
		rowStart := headerEnd
		if len(rowEnds) > 1 {
			rowStart = rowEnds[len(rowEnds)-2]
		}
		if rowStart < len(payload) && payload[rowStart] != '#' {
			originalRowCount++
		}
	}
	if len(rowEnds) == 0 {
		return payload, false
	}

	// Build the truncation comment first so we can subtract its
	// length from the budget when deciding how many rows to keep.
	commentTemplate := "# truncated_by_budget=true original_rows=%d kept_rows=%d\n"
	// We don't yet know kept_rows. Use a pessimistic placeholder:
	// the comment with the max digit count we'll need.
	maxComment := []byte(fmt.Sprintf(commentTemplate, originalRowCount, originalRowCount))
	roomForRows := maxBytes - len(maxComment)
	if roomForRows < headerEnd {
		// Header alone (plus the truncation comment) exceeds the
		// cap. Return header + comment; pathological case for
		// extremely tight caps but keeps the response valid GCX.
		out := make([]byte, 0, headerEnd+len(maxComment))
		out = append(out, payload[:headerEnd]...)
		out = append(out, []byte(fmt.Sprintf(commentTemplate, originalRowCount, 0))...)
		return out, true
	}

	// Find the largest k such that payload[:rowEnds[k-1]] fits in
	// roomForRows. Linear scan from the tail is O(rows) and rows are
	// already in ascending offset order; we walk forward picking the
	// last row whose end is ≤ roomForRows.
	keep := 0
	for i, end := range rowEnds {
		if end > roomForRows {
			break
		}
		keep = i + 1
	}
	// Floor: when the input had any rows but every row crosses the
	// cap, keep one anyway. A response with zero rows + a
	// truncated_by_budget marker is uninformative — the caller can't
	// see the row shape, can't decide whether to re-issue with a
	// tighter scope, and can't even tell which fields are populated.
	// One overflowing row tells them more than zero. The overshoot is
	// bounded (one row's worth of bytes) and the truncation comment
	// rides on the response so downstream parsers know we overran.
	if keep == 0 && len(rowEnds) > 0 {
		keep = 1
	}

	// Compute kept_rows = non-comment rows in the prefix [0, keep).
	keptRows := 0
	rowStart := headerEnd
	for i := 0; i < keep; i++ {
		if rowStart < len(payload) && payload[rowStart] != '#' {
			keptRows++
		}
		rowStart = rowEnds[i]
	}

	out := make([]byte, 0, maxBytes)
	if keep == 0 {
		out = append(out, payload[:headerEnd]...)
	} else {
		out = append(out, payload[:rowEnds[keep-1]]...)
	}
	out = append(out, []byte(fmt.Sprintf(commentTemplate, originalRowCount, keptRows))...)
	return out, true
}

// effectiveBudget resolves the per-call byte budget. Budget-by-default
// — every list-shaped tool runs through graceful degradation so the
// agent gets a usable in-band response instead of a transport spill
// that the model learns to route around. Resolution order:
//
//   - `max_bytes` and / or `max_tokens` set explicitly: the tighter
//     cap wins. `max_tokens` is converted via tokensToBytes and then
//     min-merged with `max_bytes`. Passing 0 (or negative) on either
//     axis opts OUT of that axis; opting out of one axis still
//     respects the other. Opting out of both yields no cap.
//   - `paginate: true`: shorthand for "I'll follow next_cursor; cap
//     each page at the project default". Same effective budget as
//     the default, but advertises the caller's iteration intent.
//   - Nothing set: the project default. Spill becomes a true edge
//     case rather than the routine outcome on real-world payloads.
//
// The opt-out is intentional friction: agents that need exhaustive
// data can pass `max_bytes: 0` (or `max_tokens: 0`), but the default
// prefers a partial, inline answer over a spilled file the agent has
// to re-read.
func effectiveBudget(req mcp.CallToolRequest) int {
	return respbudget.EffectiveFromArgs(req.GetArguments())
}

// budgetSource describes which arg drove the resolved byte cap. Used
// purely to decorate truncation metadata — the cap value itself is
// already returned by effectiveBudget. "none" means no cap was
// applied (default budget or explicit opt-out); the other values
// match the parameter name that won the min-merge.
type budgetSource string

const (
	budgetSourceNone    budgetSource = "none"
	budgetSourceBytes   budgetSource = "max_bytes"
	budgetSourceTokens  budgetSource = "max_tokens"
	budgetSourceDefault budgetSource = "default"
)

// resolveBudgetSource reports which axis was the tighter constraint
// for this request. Mirrors effectiveBudget's resolution order but
// returns the *origin* rather than the value. When max_tokens drove
// the cap, callers can surface that in truncation meta so the agent
// sees "trimmed because of your max_tokens=N" instead of a generic
// "too big" hint. Returns budgetSourceDefault when no caller arg was
// present and the project default applied.
func resolveBudgetSource(req mcp.CallToolRequest) (budgetSource, int) {
	args := req.GetArguments()
	rawBytes, bytesPresent := numArgInt(args, "max_bytes")
	rawTokens, tokensPresent := numArgInt(args, "max_tokens")

	if !bytesPresent && !tokensPresent {
		return budgetSourceDefault, 0
	}

	bytesOptOut := bytesPresent && rawBytes <= 0
	tokensOptOut := tokensPresent && rawTokens <= 0

	if bytesPresent && tokensPresent && bytesOptOut && tokensOptOut {
		return budgetSourceNone, 0
	}
	if bytesPresent && !tokensPresent && bytesOptOut {
		return budgetSourceNone, 0
	}
	if tokensPresent && !bytesPresent && tokensOptOut {
		return budgetSourceNone, 0
	}

	tokensBytes := tokensToBytes(rawTokens)
	switch {
	case bytesPresent && tokensPresent:
		switch {
		case bytesOptOut:
			return budgetSourceTokens, rawTokens
		case tokensOptOut:
			return budgetSourceBytes, rawBytes
		case rawBytes < tokensBytes:
			return budgetSourceBytes, rawBytes
		default:
			return budgetSourceTokens, rawTokens
		}
	case bytesPresent:
		return budgetSourceBytes, rawBytes
	default:
		return budgetSourceTokens, rawTokens
	}
}

// decorateTokenBudgetJSON folds caller-supplied max_tokens info into
// a payload's truncation metadata. Only mutates when (a) max_tokens
// was supplied and (b) the budget guard actually fired — under both
// conditions a `_max_tokens` and (when tokens drove the cap) a
// `_truncated_by_tokens` marker ride alongside the existing
// `_truncated_by_budget` flag. Callers without max_tokens see the
// unchanged byte-only meta.
//
// Best-effort: payload that isn't a map[string]any is returned
// untouched (no shape to decorate).
func decorateTokenBudgetJSON(payload any, req mcp.CallToolRequest) any {
	if payload == nil {
		return payload
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return payload
	}
	// Only decorate when the budget actually fired.
	flag, hasFlag := m[budgetTruncatedKey].(bool)
	if !hasFlag || !flag {
		return payload
	}
	source, value := resolveBudgetSource(req)
	if source == budgetSourceTokens && value > 0 {
		m["_max_tokens"] = value
		m["_truncated_by_tokens"] = true
		return m
	}
	// Even when bytes drove the cap, surface a tokens-equivalent if
	// max_tokens was supplied so the agent can see what fraction of
	// its token budget was actually used.
	if rawTokens, present := numArgInt(req.GetArguments(), "max_tokens"); present && rawTokens > 0 {
		m["_max_tokens"] = rawTokens
	}
	return m
}

// tokenBudgetDecorationReserve is the worst-case byte cost of the
// token-budget decoration the JSON and GCX renderers append AFTER
// their fit (the `_max_tokens` / `_truncated_by_tokens` fields, the
// GCX `# max_tokens=…` comment). Reserving it from the effective
// budget up front keeps the decorated payload inside the caller's cap
// — the decoration must never be the bytes that break the contract it
// documents. Zero when the caller did not pass max_tokens.
func tokenBudgetDecorationReserve(req mcp.CallToolRequest) int {
	rawTokens, present := numArgInt(req.GetArguments(), "max_tokens")
	if !present || rawTokens <= 0 {
		return 0
	}
	return 48 + len(strconv.Itoa(rawTokens))
}

// decorateTokenBudgetGCX appends a trailing comment to a GCX payload
// recording the max_tokens annotation. Only fires when max_tokens
// was supplied AND the payload already shows the byte-trim
// `# truncated_by_budget=true` marker — otherwise we'd spam every
// response with budget metadata. The decoder (@gortex/wire, gcx-go)
// treats trailing comments as no-ops, so the appended line is
// non-invasive.
func decorateTokenBudgetGCX(payload []byte, req mcp.CallToolRequest) []byte {
	if len(payload) == 0 {
		return payload
	}
	// Only decorate when the GCX trim path already fired.
	if !bytes.Contains(payload, []byte("# truncated_by_budget=true")) {
		return payload
	}
	rawTokens, present := numArgInt(req.GetArguments(), "max_tokens")
	if !present || rawTokens <= 0 {
		return payload
	}
	source, _ := resolveBudgetSource(req)
	extra := fmt.Sprintf("# max_tokens=%d", rawTokens)
	if source == budgetSourceTokens {
		extra += " truncated_by_tokens=true"
	}
	extra += "\n"
	// Avoid duplicate decoration on retries that re-trim an already-
	// trimmed payload (cheap idempotency).
	if bytes.Contains(payload, []byte(fmt.Sprintf("max_tokens=%d", rawTokens))) {
		return payload
	}
	return append(payload, []byte(extra)...)
}

// DegradeShape registers a per-tool graceful-degradation policy.
// The cascade applied by applyDegradation when a payload exceeds the
// budget runs in priority order: meta-strip first (cheapest signal
// to drop), then tier-3 row drops, then tier-2 row drops, then
// finally a longest-list tail-trim as the last-resort fallback.
//
// The TierFunc is invoked per row. Lower numbers mean "keep" — 1 is
// the must-keep tier, 2 is dropped second, 3 is dropped first.
// Rows whose row-map shape doesn't fit the policy (e.g. a non-map
// value) default to tier 1 (kept) so the policy can never accidentally
// strip a payload to nothing.
type DegradeShape struct {
	// MetaStrip lists the keys to remove from each list-row before
	// any row drops. Use this for high-bytes / low-signal columns
	// like `doc`, `signature` body, raw `meta` blobs.
	MetaStrip []string
	// TierFunc returns the priority tier for a row (1 = keep, 2 =
	// drop second, 3 = drop first). Implementations typically read
	// `row["kind"]` and switch on the value.
	TierFunc func(row map[string]any) int
}

// degradeShapes is the registry consulted by respondJSONOrTOON when
// a payload needs trimming. Per-tool shapes live next to their
// handlers in init() blocks, so the policy and the data shape stay
// co-located.
var degradeShapes = map[string]DegradeShape{}

// registerDegradeShape installs a per-tool policy. Idempotent — a
// re-register replaces the previous entry, so adapter packages can
// override the default policy if their tool variant needs different
// priorities.
func registerDegradeShape(toolName string, shape DegradeShape) {
	degradeShapes[toolName] = shape
}

// applyDegradation runs the priority-aware trim cascade for a tool
// whose handler registered a DegradeShape. Steps:
//
//  1. Marshal payload to JSON; under cap → return as-is.
//  2. Strip MetaStrip keys from every list row; under cap → return
//     with `_meta_stripped` flag.
//  3. Drop rows where TierFunc == 3 across every list; under cap →
//     return with `_dropped_tier_3_<key>` counters.
//  4. Drop rows where TierFunc == 2; under cap → return with
//     `_dropped_tier_2_<key>` counters.
//  5. Fall through to applyBudget (longest-list tail-trim) on
//     whatever survives, marking `_truncated_by_budget`.
//
// Each escape adds metadata so the agent sees what was dropped at
// which step. The trim is monotone: every step either fits the cap
// (return) or progresses to a more aggressive step.
func applyDegradation(payload any, shape DegradeShape, maxBytes int) (any, bool) {
	if maxBytes <= 0 || payload == nil {
		return payload, false
	}
	if shape.TierFunc == nil && len(shape.MetaStrip) == 0 {
		return applyBudget(payload, maxBytes)
	}

	bytes, err := json.Marshal(payload)
	if err != nil || len(bytes) <= maxBytes {
		return payload, false
	}

	var generic map[string]any
	if err := json.Unmarshal(bytes, &generic); err != nil {
		return payload, false
	}

	// Step 1: strip verbose meta keys from every row.
	if len(shape.MetaStrip) > 0 {
		stripped := false
		for k, v := range generic {
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			for i, row := range arr {
				rowMap, ok := row.(map[string]any)
				if !ok {
					continue
				}
				for _, ms := range shape.MetaStrip {
					if _, has := rowMap[ms]; has {
						delete(rowMap, ms)
						stripped = true
					}
				}
				arr[i] = rowMap
			}
			generic[k] = arr
		}
		if stripped {
			if size, _ := json.Marshal(generic); len(size) <= maxBytes {
				generic[budgetTruncatedKey] = true
				generic["_meta_stripped"] = shape.MetaStrip
				return generic, true
			}
		}
	}

	// Step 2: drop tier-3 rows, then tier-2 rows.
	if shape.TierFunc != nil {
		for tier := 3; tier >= 2; tier-- {
			anyDropped := false
			for k, v := range generic {
				arr, ok := v.([]any)
				if !ok {
					continue
				}
				kept := make([]any, 0, len(arr))
				originalLen := len(arr)
				droppedCount := 0
				for _, row := range arr {
					rowMap, ok := row.(map[string]any)
					if !ok {
						kept = append(kept, row)
						continue
					}
					if shape.TierFunc(rowMap) >= tier {
						droppedCount++
						continue
					}
					kept = append(kept, row)
				}
				if droppedCount > 0 {
					generic[k] = kept
					generic[fmt.Sprintf("_dropped_tier_%d_%s", tier, k)] = droppedCount
					generic[fmt.Sprintf("_original_count_%s", k)] = originalLen
					anyDropped = true
				}
			}
			_ = anyDropped
			if size, _ := json.Marshal(generic); len(size) <= maxBytes {
				generic[budgetTruncatedKey] = true
				return generic, true
			}
		}
	}

	// Step 3: last-resort tail-trim of the longest remaining list.
	return applyBudget(generic, maxBytes)
}

// encodeCursor packs an opaque cursor value (currently {offset:N}) as
// base64-encoded JSON so it is safe to round-trip across MCP tool
// args. Opaque on purpose — callers must not parse it. We can change
// the encoding later without breaking callers as long as we keep the
// "round-trip what the server gave you" contract.
func encodeCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	raw, err := json.Marshal(map[string]any{"offset": offset})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor returns the offset encoded in cursor or 0 if cursor is
// empty / malformed. Malformed cursors degrade to "start from row 0"
// rather than failing the call — defensive against agents that might
// reuse stale cursors after a server restart.
func decodeCursor(cursor string) int {
	if cursor == "" {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	var p struct {
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}
	if p.Offset < 0 {
		return 0
	}
	return p.Offset
}

// applyOffsetLimit slices a generic []any according to offset/limit
// and returns the windowed slice plus the next-cursor (empty when no
// more rows). Defensive: an offset past the end yields an empty slice
// rather than an error.
func applyOffsetLimit(rows []any, offset, limit int) ([]any, string) {
	if offset >= len(rows) {
		return []any{}, ""
	}
	end := offset + limit
	if limit <= 0 || end > len(rows) {
		end = len(rows)
	}
	next := ""
	if end < len(rows) {
		next = encodeCursor(end)
	}
	return rows[offset:end], next
}
