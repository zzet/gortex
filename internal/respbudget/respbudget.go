// Package respbudget holds the response-budget arithmetic shared by
// every layer that assembles a tool response: the MCP server applies it
// per handler, and the daemon's federation merge reapplies it to the
// final merged representation. One implementation, so the two layers
// cannot drift on defaults, opt-out semantics, or the token ratio.
package respbudget

import (
	"encoding/json"
	"sort"
)

// DefaultMaxBytes is the upper bound on a single tool response.
// Empirically the agent harness (claude-code at the time of writing)
// starts spilling responses to a side file around ~50 KB of wire
// text. The MCP `tools/call` envelope wraps our payload as
// `{"content":[{"type":"text","text":"<payload>"}]}`, then JSON-RPC
// itself adds one more layer of escaping when serialised across the
// stdio bridge — round-trip overhead averages 25–30 % on top of the
// raw payload bytes for our shapes. Capping the inner payload at
// 40 KB keeps the wire form comfortably under the 50 KB threshold,
// leaving headroom for the rare row that has unusually heavy meta.
//
// Lower this number cautiously: every drop here means more rows get
// trimmed across every list-shaped tool. Raise it only after
// re-measuring the harness threshold; "no spill" beats "more rows"
// because spilled output forces a cold re-read for the agent.
const DefaultMaxBytes = 40_000

// AvgBytesPerToken is the calibration constant used to translate the
// `max_tokens` parameter into an effective byte cap. Empirically:
//
//   - dense JSON / TOON rows (`{"id":"...","kind":"function",...}`)
//     tokenise at ~3.0–3.4 chars per BPE token on cl100k_base. The
//     punctuation density (quotes, colons, braces) drags the ratio
//     down vs. natural English.
//   - GCX1 rows (`id\tkind\tname\t...`) tokenise at ~4.0–4.5 chars
//     per token because tabs collapse into single tokens and the
//     identifier-heavy payload tokenises more efficiently than JSON.
//   - smart_context / get_editing_context source-bearing payloads
//     average ~3.6 chars per token because the source lines push
//     the ratio toward English text.
//
// 3.5 is the safe midpoint: it slightly UNDER-counts tokens for JSON
// (so the resulting byte cap is tighter than strictly necessary —
// erring on the side of "fits the budget") and approximately matches
// the GCX row case. Token estimation is necessarily imperfect across
// model tokenisers; the budget guard's job is to ride a safe margin,
// not to count exactly. A caller who needs precise token-counting
// should run their own tokenizer post-hoc.
const AvgBytesPerToken = 3.5

// TruncatedKey is the meta flag appended to a payload trimmed by
// Apply so callers can branch on truncation without scanning for
// shape-specific signals.
const TruncatedKey = "_truncated_by_budget"

// TokensToBytes converts a token budget into a byte cap using the
// AvgBytesPerToken ratio. Returns 0 for non-positive inputs so
// `max_tokens: 0` is honoured as "opt out" with the same semantics
// as `max_bytes: 0`.
func TokensToBytes(maxTokens int) int {
	if maxTokens <= 0 {
		return 0
	}
	return int(float64(maxTokens) * AvgBytesPerToken)
}

// BytesToTokens is the inverse of TokensToBytes. Used to render a
// human-readable token-equivalent on the truncation meta so callers
// see "kept ~N tokens" alongside the raw byte cap. Returns 0 for
// non-positive inputs.
func BytesToTokens(byteCount int) int {
	if byteCount <= 0 {
		return 0
	}
	return int(float64(byteCount) / AvgBytesPerToken)
}

// NumArgInt extracts an integer arg from a request arguments map.
// JSON numbers arrive as float64; some test harnesses pass int.
// Returns (value, present). When the arg is the wrong type, returns
// (0, true) so callers can distinguish "absent" from "malformed
// zero" — important because a zero value is meaningful here (it is
// the opt-out signal for both max_bytes and max_tokens).
func NumArgInt(args map[string]any, key string) (int, bool) {
	raw, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := raw.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, true
	}
}

// EffectiveFromArgs resolves the per-call byte budget from a tool's
// arguments map. Budget-by-default — every list-shaped tool runs
// through graceful degradation so the agent gets a usable in-band
// response instead of a transport spill. Resolution order:
//
//   - `max_bytes` and / or `max_tokens` set explicitly: the tighter
//     cap wins. `max_tokens` is converted via TokensToBytes and then
//     min-merged with `max_bytes`. Passing 0 (or negative) on either
//     axis opts OUT of that axis; opting out of one axis still
//     respects the other. Opting out of both yields no cap.
//   - Nothing set: the project default.
func EffectiveFromArgs(args map[string]any) int {
	rawBytes, bytesPresent := NumArgInt(args, "max_bytes")
	rawTokens, tokensPresent := NumArgInt(args, "max_tokens")

	if !bytesPresent && !tokensPresent {
		return DefaultMaxBytes
	}

	// Per-axis opt-out: a non-positive value means "skip THIS axis".
	bytesOptOut := bytesPresent && rawBytes <= 0
	tokensOptOut := tokensPresent && rawTokens <= 0

	// Both axes opted out → no cap.
	if bytesPresent && tokensPresent && bytesOptOut && tokensOptOut {
		return 0
	}
	// Only one axis present and it opted out → no cap.
	if bytesPresent && !tokensPresent && bytesOptOut {
		return 0
	}
	if tokensPresent && !bytesPresent && tokensOptOut {
		return 0
	}

	tokensBytes := TokensToBytes(rawTokens) // 0 when token axis absent or opt-out

	switch {
	case bytesPresent && tokensPresent:
		switch {
		case bytesOptOut:
			return tokensBytes
		case tokensOptOut:
			return rawBytes
		case rawBytes < tokensBytes:
			return rawBytes
		default:
			return tokensBytes
		}
	case bytesPresent:
		return rawBytes
	default: // tokensPresent
		return tokensBytes
	}
}

// Apply enforces a marshaled-size cap on payload by trimming
// top-level lists in longest-first order until the result fits.
// Returns the (possibly trimmed) payload and a flag indicating
// whether trimming happened. The trimmed payload carries inline
// metadata so callers can surface "narrow your filter" hints:
//
//   - _truncated_by_budget: true
//   - _max_returned_<field>: N
//   - _original_count_<field>: M (one pair per trimmed list)
//
// Multi-list payloads (`nodes` + `edges` for get_file_summary, etc.)
// are trimmed iteratively: the longest list is binary-searched first;
// if the result still exceeds the cap, the next-longest list is
// trimmed too, and so on.
//
// FLOOR: the enforceable minimum for a structured payload is its
// scalar skeleton — the marshaled size with every top-level list
// emptied, truncation meta included. A budget below that floor still
// gets every list emptied and the meta stamped, and the response then
// exceeds the cap by the skeleton's size: scalars are not trimmable
// without discarding the answer itself, and a byte-level cut would
// corrupt the JSON. Callers that need a hard ceiling at any size
// (the text renderers) enforce it with their own grammar-aware trim;
// for JSON the cap is a contract above the floor and best-effort
// below it. TestApplyScalarSkeletonFloor pins this exact shape.
//
// Best-effort: if no top-level list is found in the marshaled JSON,
// the payload is returned unchanged.
func Apply(payload any, maxBytes int) (any, bool) {
	if maxBytes <= 0 || payload == nil {
		return payload, false
	}
	bytes, err := json.Marshal(payload)
	if err != nil || len(bytes) <= maxBytes {
		return payload, false
	}

	// Re-shape into a generic map so we can manipulate any payload
	// type uniformly (struct, *query.SubGraph, map[string]any). The
	// JSON round-trip costs one extra alloc — cheap given we already
	// know we are over budget — and it is load-bearing twice over:
	// it normalizes TYPED slices (a handler's []*graph.Edge inside a
	// map[string]any) into the []any the trimmer recognizes, and it
	// yields a fresh map so the caller's payload — which callers may
	// reuse — stays untouched. Do not shortcut it with a map
	// assertion: a shallow clone keeps the typed slices and the trim
	// silently returns the oversized payload.
	var generic map[string]any
	if err := json.Unmarshal(bytes, &generic); err != nil {
		return payload, false
	}

	trimmed := false
	// Each non-fitting pass empties the then-longest list, so the
	// number of top-level keys bounds the loop: a payload whose
	// scalars alone exceed the cap terminates with every list empty
	// (the documented floor), never spins.
	for pass := 0; pass < len(generic); pass++ {
		longestKey := findLongestSliceKey(generic)
		if longestKey == "" {
			break
		}
		longest := genericSlice(generic, longestKey)
		originalLen := len(longest)
		// Binary search for the largest prefix that fits.
		lo, hi := 0, originalLen
		for lo < hi {
			mid := (lo + hi + 1) / 2
			generic[longestKey] = longest[:mid]
			generic[TruncatedKey] = true
			generic["_max_returned_"+longestKey] = mid
			generic["_original_count_"+longestKey] = originalLen
			candidate, err := json.Marshal(generic)
			if err != nil {
				break
			}
			if len(candidate) <= maxBytes {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		generic[longestKey] = longest[:lo]
		generic[TruncatedKey] = true
		generic["_max_returned_"+longestKey] = lo
		generic["_original_count_"+longestKey] = originalLen
		trimmed = true

		final, _ := json.Marshal(generic)
		if len(final) <= maxBytes {
			return generic, true
		}
	}
	if !trimmed {
		// No slice candidate was actually trimmed — return the
		// original payload type intact so callers comparing against
		// concrete Go types (int vs json's float64, etc.) keep
		// working unchanged.
		return payload, false
	}
	return generic, trimmed
}

// Trimmed reports whether raw marshaled JSON carries Apply's
// truncation marker (TruncatedKey). Lives beside the constant so
// every layer probing for the marker shares one definition.
func Trimmed(raw []byte) bool {
	var probe struct {
		TruncatedByBudget bool `json:"_truncated_by_budget"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.TruncatedByBudget
}

// findLongestSliceKey returns the top-level field name whose value is
// the longest []any. Empty string when no slices are present. Used by
// Apply to pick the trimming target without per-tool config.
func findLongestSliceKey(m map[string]any) string {
	var key string
	maxLen := 0
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Stable iteration so ties resolve deterministically.
	sort.Strings(keys)
	for _, k := range keys {
		arr, ok := m[k].([]any)
		if !ok {
			continue
		}
		if len(arr) > maxLen {
			maxLen = len(arr)
			key = k
		}
	}
	return key
}

func genericSlice(m map[string]any, key string) []any {
	if arr, ok := m[key].([]any); ok {
		return arr
	}
	return nil
}
