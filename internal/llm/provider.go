// Package llm — provider abstraction.
//
// Provider isolates every LLM operation (the agent tool-loop and the
// three search-assist passes) from where inference actually runs.
// Nine implementations live under internal/llm/provider/: a llama.cpp
// `local` provider (CGO, `-tags llama`), six pure-Go HTTP providers
// (`anthropic`, `openai`, `ollama`, `gemini`, `bedrock` — SigV4-signed,
// no AWS SDK — and `deepseek`), and two subprocess CLI providers,
// `claudecli` and `codex`, that shell out to the user's `claude` /
// `codex` binary (reusing their existing Claude Code / Codex sign-in —
// no API key needed). They are swapped via the `llm.provider` config
// key — see Config.
//
// The whole surface is a single method, Complete: one structured
// single-turn call. The agent loop is just repeated Complete calls
// with a growing Messages slice; the assist passes are one Complete
// call each. Keeping the interface to one method is what lets the
// HTTP providers stay small and the build-tag split stay contained to
// the `local` package.
package llm

import "context"

// Role identifies who produced a Message in a provider conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a provider conversation. Content is the plain
// text payload — for a RoleAssistant message that represents a tool
// call it holds the raw tool-call JSON; for a RoleTool message it
// holds the tool's observation and ToolName names the tool that
// produced it.
//
// The conversation is provider-neutral: the `local` provider flattens
// it through a llama chat template, the HTTP providers map it onto
// their native messages array. Tool calls are carried as plain text
// (the "emulated" protocol the local model already uses), not via any
// provider's native tool-use wire format — that keeps a single
// Message shape working across all four providers.
type Message struct {
	Role     Role
	Content  string
	ToolName string // set on RoleTool messages: which tool produced Content
}

// ToolSpec describes one callable tool to a provider. When a request
// carries Shape == ShapeToolCall the provider constrains the emitted
// "tool" field to exactly these names.
type ToolSpec struct {
	Name        string
	Description string
}

// JSONShape names the structured-output schema a provider must enforce
// on a completion. ShapeFreeform applies no constraint; every other
// value corresponds to a concrete JSON object shape (see
// JSONSchemaFor) that the provider guarantees the response conforms
// to — via a GBNF grammar (local) or a json-schema / forced-tool
// mechanism (HTTP providers).
type JSONShape int

const (
	ShapeFreeform    JSONShape = iota // no structured constraint
	ShapeExpandTerms                  // {"terms":[<string>...]}
	ShapeRerankOrder                  // {"order":[<string>...]}
	ShapeVerifyKeep                   // {"keep":[<string>...]}
	ShapeToolCall                     // {"tool":<enum>,"args":{...}}
)

// CompletionRequest is one single-turn request to a Provider. The
// provider flattens Messages into its native wire format, applies the
// structured-output mechanism implied by Shape, and returns the raw
// model text.
type CompletionRequest struct {
	// Messages is the conversation so far, oldest first. The last
	// message is whatever the model should respond to.
	Messages []Message
	// MaxTokens caps generation length. 0 lets the provider pick a
	// sensible default.
	MaxTokens int
	// Shape is the structured-output contract for the response.
	Shape JSONShape
	// Tools is consulted only when Shape == ShapeToolCall: the provider
	// constrains the emitted "tool" field to these names.
	Tools []ToolSpec
}

// TokenUsage carries the token accounting for a single completion: the
// prompt (input) tokens billed, the generated (output) tokens, and the
// prompt-cache read / write tokens when the provider reports them. A
// provider that does not surface usage leaves every field zero.
type TokenUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Add folds u' into u in place, accumulating each token field. Used by
// the agent loop to sum usage across the tool-calling steps of one run.
func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

// IsZero reports whether no usage was recorded — the signal a provider
// could not report token counts (subprocess / not-yet-decoded HTTP).
func (u TokenUsage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

// CompletionResponse is a Provider's single-turn output. Text is the
// raw model text — JSON conforming to the requested Shape when Shape
// is not ShapeFreeform. Usage carries the provider's token accounting
// when it reports one (zero otherwise).
type CompletionResponse struct {
	Text  string
	Usage TokenUsage
}

// Provider is a single-turn LLM completion backend. The agent loop and
// the search-assist passes are both built on repeated Complete calls.
type Provider interface {
	// Name returns the provider's short identifier — one of "local",
	// "anthropic", "openai", "ollama", "claudecli", "codex", "gemini",
	// "bedrock", "deepseek", "requesty". Used to pick the prompt tier (see
	// PromptProfile) and for diagnostics.
	Name() string
	// Complete runs one single-turn completion, honouring req.Shape
	// with whatever structured-output mechanism the provider has.
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	// Close releases any held resources (model weights, idle HTTP
	// connections). Safe to call multiple times.
	Close() error
}
