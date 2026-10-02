// Package llm — prompt tiers and structured-output schemas.
//
// The search-assist passes (expand / rerank / verify) need different
// prompting depending on the model behind the active provider. A
// small local GGUF model needs verbose, rule-heavy, example-laden
// instructions; a hosted frontier model reasons well with light
// direction and is measurably *hurt* by over-constraining prompts.
//
// Rather than carry a prompt set per provider, prompts are keyed by a
// capability *tier* (PromptProfile): two sets to maintain, not four.
// The svc layer maps a provider to its tier via ProfileForProvider.
package llm

// PromptProfile selects a prompt tier.
type PromptProfile int

const (
	// ProfileSmall is the verbose, rule-heavy tier — tuned for small
	// local GGUF models and small Ollama coder models, which need
	// explicit rules and blocklists to behave.
	ProfileSmall PromptProfile = iota
	// ProfileFrontier is the terse tier — tuned for hosted frontier
	// models (Anthropic, OpenAI) that reason well with light direction
	// and lose quality when over-constrained.
	ProfileFrontier
)

// ProfileForProvider maps a provider's Name() to its prompt tier.
// "local" and "ollama" run small models → ProfileSmall; the hosted
// providers and the CLI providers ("claudecli", "codex" — which shell
// out to Claude / Codex) run frontier models → ProfileFrontier.
// Unknown names fall back to ProfileSmall (the safe, more-explicit
// tier).
func ProfileForProvider(name string) PromptProfile {
	switch name {
	case "anthropic", "openai", "azure", "claudecli", "codex", "copilot", "cursor", "opencode", "gemini", "bedrock", "deepseek", "requesty":
		return ProfileFrontier
	default:
		return ProfileSmall
	}
}

// --- Expand -----------------------------------------------------------------

const expandSystemSmall = `You expand a code-search query into a small set of CONCRETE identifier-style terms a programmer would actually grep for. ` +
	`Output strict JSON: {"terms":["<term1>","<term2>",...]}. ` +
	`Include 2 to 5 terms. Each term MUST be a single word with no spaces and no punctuation other than underscores. ` +
	`
RULES:
1. Prefer DOMAIN-SPECIFIC terms over generic English. ` +
	`GOOD examples: bcrypt, argon2, scrypt, sha256, hmac, jwt, oauth, pbkdf2, kdf, salt. ` +
	`BAD examples (NEVER emit): function, library, algorithm, code, system, data, service, value, info, content, thing, stuff, name, general, common, logic, process, handler, flow, action, helper, util, utility. ` +
	`
2. Prefer terms that are likely SYMBOL names in a codebase (camelCase / snake_case / PascalCase fragments), library or protocol names, well-known acronyms. ` +
	`
3. Do NOT echo the original query words. ` +
	`
4. If the query has no obvious domain-specific neighbours, emit FEWER terms (or an empty array) — quality over quantity.`

const expandSystemFrontier = `Expand a code-search query into 2-5 concrete identifier-style terms a programmer would grep for: library and protocol names, well-known acronyms, camelCase / snake_case symbol fragments. ` +
	`Skip generic English nouns (function, data, handler, ...). Do not echo the query words. Fewer strong terms beat many weak ones — an empty list is fine when there are no good neighbours. ` +
	`Output JSON: {"terms":["<term>",...]}.`

// ExpandSystemPrompt returns the system prompt for the query-expansion
// pass at the given tier.
func ExpandSystemPrompt(p PromptProfile) string {
	if p == ProfileFrontier {
		return expandSystemFrontier
	}
	return expandSystemSmall
}

// --- Rerank -----------------------------------------------------------------

const rerankSystemSmall = `You rerank code-search results by relevance to a natural-language task. ` +
	`Given a query and a list of candidate symbols (id | name | optional signature), output strict JSON: {"order":["id1","id2",...]} ` +
	`with the most relevant candidates first. ` +
	`Use ONLY the provided ids verbatim. Do not invent ids. You may drop ids that are clearly unrelated.`

const rerankSystemFrontier = `Reorder the candidate symbols by relevance to the query, most relevant first. ` +
	`Use the provided ids verbatim; drop ones that are clearly unrelated. Output JSON: {"order":["<id>",...]}.`

// RerankSystemPrompt returns the system prompt for the rerank pass at
// the given tier.
func RerankSystemPrompt(p PromptProfile) string {
	if p == ProfileFrontier {
		return rerankSystemFrontier
	}
	return rerankSystemSmall
}

// --- Verify -----------------------------------------------------------------

const verifySystemSmall = `You filter code-search candidates by reading their BODY, SIGNATURE, and CALLERS, and keeping every one whose code is genuinely about the user's query. ` +
	`Each candidate is presented as:

<id> | <name> | <signature>
body:
<code body, truncated>
callers:
- <caller_name> | <caller_signature>
- ...
---

Output strict JSON: {"keep":["id1","id2",...]} listing EVERY id whose code is meaningfully related to the query, in your preferred order (most relevant first).

RULES (follow exactly):
1. Evaluate EACH candidate INDEPENDENTLY. Multiple candidates can be valid matches — keep them all.
2. A name that contains a query word is not enough by itself — read what the code DOES.
3. Cross-reference the CALLERS and the SIGNATURE's parameter types against the query DOMAIN. If a function hashes data but is only called from a "publishDiagnostics" or "renderLog" path with a non-password parameter type, it is NOT about hashing passwords — DROP it.
4. Be GENEROUS, not restrictive: if a candidate's body AND callers AND signature are all plausibly about the query, KEEP it. The user wants signal, not a single "best" pick.
5. Drop a candidate when its body, signature, or callers reveal the operation is on the wrong KIND of data for the query.
6. Returning {"keep":[]} is valid ONLY when NO candidate is genuinely about the query.
7. Use ONLY the provided ids verbatim. Never invent or modify an id.`

const verifySystemFrontier = `Filter code-search candidates: keep every one whose body, signature, and callers show it genuinely concerns the query; drop the rest. ` +
	`Judge by what the code DOES and the data domain its callers imply — not by name overlap with the query. Keep all genuine matches, not just the single best. ` +
	`An empty result {"keep":[]} is valid and correct when nothing genuinely matches. Use the provided ids verbatim. Output JSON: {"keep":["<id>",...]}.`

// VerifySystemPrompt returns the system prompt for the body-grounded
// verification pass at the given tier.
func VerifySystemPrompt(p PromptProfile) string {
	if p == ProfileFrontier {
		return verifySystemFrontier
	}
	return verifySystemSmall
}

// --- Structured-output schemas ----------------------------------------------

// JSONSchemaFor returns a provider-agnostic JSON Schema (as a
// marshalable map) describing the response shape for a JSONShape. The
// HTTP providers feed it to their native structured-output mechanism
// (Anthropic forced-tool input_schema, OpenAI json_schema, Ollama
// format); the local provider ignores it and uses a GBNF grammar
// instead. Returns nil for ShapeFreeform.
//
// tools is consulted only for ShapeToolCall, where the "tool" field is
// constrained to an enum of the tool names.
func JSONSchemaFor(shape JSONShape, tools []ToolSpec) map[string]any {
	stringArray := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	switch shape {
	case ShapeExpandTerms:
		return listSchema("terms", stringArray)
	case ShapeRerankOrder:
		return listSchema("order", stringArray)
	case ShapeVerifyKeep:
		return listSchema("keep", stringArray)
	case ShapeToolCall:
		names := make([]any, len(tools))
		for i, t := range tools {
			names[i] = t.Name
		}
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool": map[string]any{"type": "string", "enum": names},
				"args": map[string]any{"type": "object"},
			},
			"required":             []any{"tool", "args"},
			"additionalProperties": false,
		}
	default:
		return nil
	}
}

// listSchema builds the schema for a single-key object whose value is
// a JSON array — the shape shared by expand / rerank / verify.
func listSchema(key string, arraySchema map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{key: arraySchema},
		"required":             []any{key},
		"additionalProperties": false,
	}
}
