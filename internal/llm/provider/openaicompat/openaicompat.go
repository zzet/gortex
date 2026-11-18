// Package openaicompat is the shared OpenAI Chat Completions client
// used by every provider that speaks the OpenAI wire format: the hosted
// `openai` provider, the `azure` (Azure OpenAI Service) provider, and
// user-registered custom OpenAI-compatible endpoints (see the provider
// registry).
//
// Those providers differ only in how a request is addressed and
// authenticated — the request/response bodies, the structured-output
// mechanism, and the hollow-200 retry are identical. Client captures
// that shared half: each provider supplies the absolute request URL,
// the auth headers, and (for endpoints that don't implement strict
// json_schema) the structured-output strategy. The result is a single
// OpenAI-compatible core behind several thin provider constructors
// rather than the same ~150 lines copied per backend.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/httpx"
)

// SchemaMode selects how a Client satisfies a structured-output
// request (any req.Shape other than ShapeFreeform).
type SchemaMode int

const (
	// SchemaJSONSchema uses the OpenAI `response_format: {type:
	// json_schema}` field — strict for the fixed list shapes, lenient
	// for the open-ended tool-call shape. This is the canonical path
	// for OpenAI and Azure OpenAI; the response is already valid JSON
	// so no post-extraction is needed.
	SchemaJSONSchema SchemaMode = iota
	// SchemaJSONObject uses `response_format: {type: json_object}` and
	// rides the JSON Schema in on the prompt. For OpenAI-compatible
	// gateways that honour json_object but not strict json_schema
	// (many proxies, some self-hosted servers). The reply is run
	// through llm.ExtractJSON.
	SchemaJSONObject
	// SchemaPromptOnly sends no response_format at all and relies
	// entirely on a prompt rider + llm.ExtractJSON. The safe floor for
	// an endpoint whose response_format support is unknown.
	SchemaPromptOnly
)

// DefaultMaxTokensField is the request-body key for the output token
// cap. OpenAI's current API uses max_completion_tokens (max_tokens is
// deprecated and rejected by reasoning models); custom endpoints that
// only understand the legacy key can override via Client.MaxTokensField.
const DefaultMaxTokensField = "max_completion_tokens"

// Client is the shared OpenAI Chat Completions backend. It implements
// llm.Provider directly: the openai / azure / custom constructors each
// build a configured Client and return it.
type Client struct {
	// ProviderID is the provider identifier returned by Name(). For
	// Azure and custom OpenAI-compatible endpoints this is "azure" /
	// "custom"; a custom endpoint's prompt tier comes from its
	// prompt_profile (see llm.ProfileFor), not from this name.
	ProviderID string
	// Tag is the short label used in the exhausted-retry error message
	// and httpx diagnostics. May be more specific than Name (e.g.
	// "custom:groq").
	Tag string
	// Model is the model identifier sent in the request body.
	Model string
	// URL is the absolute Chat Completions endpoint. The caller bakes
	// in any path segments and query string (Azure folds the
	// deployment into the path and api-version into the query).
	URL string
	// Headers are applied to every request — at minimum the auth
	// header (Bearer for OpenAI, api-key for Azure).
	Headers map[string]string
	// HTTPClient issues the requests. Required.
	HTTPClient *http.Client
	// SchemaMode selects the structured-output strategy.
	SchemaMode SchemaMode
	// MaxTokensField overrides the output-token-cap body key. Empty
	// uses DefaultMaxTokensField.
	MaxTokensField string
	// Temperature, when non-nil, is sent as the `temperature` body
	// field. Nil leaves it unset (the endpoint's own default applies).
	Temperature *float64
	// ReasoningEffort, when non-empty, is sent as `reasoning_effort`
	// (OpenAI o-series / reasoning models, and gateways that proxy
	// them). Ignored by endpoints that don't recognise it.
	ReasoningEffort string
	// ExtraBody carries provider-specific top-level request fields
	// merged verbatim into the body (e.g. a custom endpoint that needs
	// a `provider` routing hint). Applied after the standard fields so
	// it can override them.
	ExtraBody map[string]any
}

var _ llm.Provider = (*Client)(nil)

// Name implements llm.Provider — the identifier that picks the prompt
// tier and labels diagnostics. Falls back to Tag when Name is unset.
func (c *Client) Name() string {
	if c.ProviderID != "" {
		return c.ProviderID
	}
	return c.Tag
}

// Close releases idle HTTP connections. Safe to call multiple times.
func (c *Client) Close() error {
	if c.HTTPClient != nil {
		c.HTTPClient.CloseIdleConnections()
	}
	return nil
}

// wire types for the Chat Completions API.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *apiUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiUsage is the Chat Completions token accounting. prompt_tokens
// includes the cached prefix; prompt_tokens_details.cached_tokens
// breaks out the share served from the prompt cache.
type apiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// Complete implements llm.Provider. The body is marshalled once; the
// HTTP round-trip and parse run inside httpx.Complete, which retries a
// hollow HTTP-200 with bounded backoff. For the prompt-rider schema
// modes the structured reply is recovered with llm.ExtractJSON.
func (c *Client) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	if c.HTTPClient == nil {
		return llm.CompletionResponse{}, fmt.Errorf("%s: nil HTTP client", c.label())
	}

	structured := req.Shape != llm.ShapeFreeform
	messages := req.Messages
	if structured && c.SchemaMode != SchemaJSONSchema {
		// Lenient endpoints get the schema as a prompt rider rather
		// than a native response_format json_schema.
		messages = withSchemaRider(messages, req.Shape, req.Tools)
	}

	tokenField := c.MaxTokensField
	if tokenField == "" {
		tokenField = DefaultMaxTokensField
	}
	body := map[string]any{
		"model":    c.Model,
		"messages": mapMessages(messages),
	}
	if req.MaxTokens > 0 {
		body[tokenField] = req.MaxTokens
	}
	if c.Temperature != nil {
		body["temperature"] = *c.Temperature
	}
	if c.ReasoningEffort != "" {
		body["reasoning_effort"] = c.ReasoningEffort
	}
	if structured {
		switch c.SchemaMode {
		case SchemaJSONSchema:
			if rf := responseFormat(req.Shape, req.Tools); rf != nil {
				body["response_format"] = rf
			}
		case SchemaJSONObject:
			body["response_format"] = map[string]any{"type": "json_object"}
		}
	}
	maps.Copy(body, c.ExtraBody)

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("%s: marshal request: %w", c.label(), err)
	}

	text, usage, err := httpx.CompleteWithUsage(ctx, c.label(), func(ctx context.Context) httpx.Result {
		return c.attempt(ctx, raw)
	})
	if err != nil {
		return llm.CompletionResponse{}, err
	}

	if structured && c.SchemaMode != SchemaJSONSchema {
		extracted, ok := llm.ExtractJSON(text)
		if !ok {
			return llm.CompletionResponse{}, fmt.Errorf("%s: response carried no JSON: %s", c.label(), llm.Snippet([]byte(text)))
		}
		text = extracted
	}
	return llm.CompletionResponse{Text: text, Usage: toTokenUsage(usage)}, nil
}

// toTokenUsage maps the provider-neutral httpx.Usage onto llm.TokenUsage.
func toTokenUsage(u httpx.Usage) llm.TokenUsage {
	return llm.TokenUsage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

func (c *Client) label() string {
	if c.Tag != "" {
		return c.Tag
	}
	return c.Name()
}

// attempt issues one Chat Completions request and extracts the reply.
// A fresh body reader is built per call so httpx.Complete can retry.
func (c *Client) attempt(ctx context.Context, raw []byte) httpx.Result {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(raw))
	if err != nil {
		return httpx.Result{Err: fmt.Errorf("%s: build request: %w", c.label(), err)}
	}
	httpReq.Header.Set("content-type", "application/json")
	for k, v := range c.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return httpx.Result{Err: fmt.Errorf("%s: request failed: %w", c.label(), err)}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return httpx.Result{Err: fmt.Errorf("%s: read response: %w", c.label(), err)}
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return httpx.Result{Err: fmt.Errorf("%s: decode response (status %d): %w", c.label(), resp.StatusCode, err)}
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return httpx.Result{Err: fmt.Errorf("%s: API error (status %d): %s: %s", c.label(), resp.StatusCode, parsed.Error.Type, parsed.Error.Message)}
		}
		return httpx.Result{Err: fmt.Errorf("%s: API error (status %d): %s", c.label(), resp.StatusCode, snippet(payload))}
	}
	if len(parsed.Choices) == 0 {
		return httpx.Result{Hollow: true}
	}
	text := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if text == "" {
		return httpx.Result{Hollow: true}
	}
	return httpx.Result{Text: text, Usage: usageFrom(parsed.Usage)}
}

// usageFrom maps the Chat Completions usage block onto httpx.Usage. The
// cached-prompt share is reported as CacheReadTokens (a subset of
// prompt_tokens, which stays the full input count). A nil block yields
// zero usage. OpenAI's API does not report cache-write tokens.
func usageFrom(u *apiUsage) httpx.Usage {
	if u == nil {
		return httpx.Usage{}
	}
	return httpx.Usage{
		InputTokens:     u.PromptTokens,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: u.PromptTokensDetails.CachedTokens,
	}
}

// mapMessages flattens the provider-neutral conversation onto OpenAI
// chat roles. Tool observations become user turns (the emulated
// tool-call protocol shared by every provider).
func mapMessages(in []llm.Message) []apiMessage {
	out := make([]apiMessage, 0, len(in))
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, apiMessage{Role: "system", Content: m.Content})
		case llm.RoleAssistant:
			out = append(out, apiMessage{Role: "assistant", Content: m.Content})
		case llm.RoleTool:
			out = append(out, apiMessage{Role: "user", Content: renderToolResult(m)})
		default:
			out = append(out, apiMessage{Role: "user", Content: m.Content})
		}
	}
	return out
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}

// withSchemaRider returns a copy of messages with a JSON-Schema
// instruction appended, so a lenient endpoint without native
// json_schema support still emits the right shape. The rider is added
// to the final message when it is a user/tool turn, otherwise as a new
// trailing user turn — keeping the conversation valid for endpoints
// that reject a system turn in last position.
func withSchemaRider(in []llm.Message, shape llm.JSONShape, tools []llm.ToolSpec) []llm.Message {
	if len(in) == 0 {
		return []llm.Message{{Role: llm.RoleUser, Content: llm.AppendSchemaInstruction("", shape, tools)}}
	}
	out := append([]llm.Message(nil), in...)
	last := out[len(out)-1]
	if last.Role == llm.RoleUser {
		last.Content = llm.AppendSchemaInstruction(last.Content, shape, tools)
		out[len(out)-1] = last
		return out
	}
	return append(out, llm.Message{Role: llm.RoleUser, Content: llm.AppendSchemaInstruction("", shape, tools)})
}

// responseFormat builds the `response_format` payload for the native
// json_schema mode. The fixed list shapes get a strict json_schema;
// the tool-call shape gets a non-strict one because its `args` object
// is deliberately open-ended, which strict mode forbids. ShapeFreeform
// returns nil — no constraint.
func responseFormat(shape llm.JSONShape, tools []llm.ToolSpec) map[string]any {
	schema := llm.JSONSchemaFor(shape, tools)
	if schema == nil {
		return nil
	}
	strict := shape != llm.ShapeToolCall
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   schemaName(shape),
			"schema": schema,
			"strict": strict,
		},
	}
}

func schemaName(shape llm.JSONShape) string {
	switch shape {
	case llm.ShapeExpandTerms:
		return "expand_terms"
	case llm.ShapeRerankOrder:
		return "rerank_order"
	case llm.ShapeVerifyKeep:
		return "verify_keep"
	case llm.ShapeToolCall:
		return "tool_call"
	default:
		return "response"
	}
}

// snippet truncates a response body for inclusion in an error.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
