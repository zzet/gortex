// Package svc is the runner layer that ties an llm.Provider to the
// agent tool-loop (package llm/agent) and the search-assist passes
// (assist.go). It lives in its own package to break the import cycle
// that would otherwise exist between `llm` and `llm/agent`.
//
// svc is pure Go: the `-tags llama` build-tag split is contained
// entirely within the provider packages. The daemon links the same
// Service whether or not the tag is set — without it only the `local`
// provider is unavailable; the HTTP providers still work, and a
// disabled service degrades cleanly (Enabled() reports false).
package svc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/agent"
	"github.com/zzet/gortex/internal/llm/conversationlog"
	"github.com/zzet/gortex/internal/llm/provider"
	"github.com/zzet/gortex/internal/savings"
)

// loadable is implemented by a provider whose backing model is loaded
// lazily and can be unloaded when idle (the local llama provider). The
// service consults it via ModelLoaded so the search-assist gate can
// avoid triggering an expensive cold load while enrichment is running.
// Providers with no cold-load cost (the HTTP backends) do not implement
// it and are always treated as ready.
type loadable interface {
	Loaded() bool
}

// loggerConfigurable is implemented by a provider that emits lifecycle
// diagnostics (model load / idle unload) to a structured logger the
// service supplies after construction.
type loggerConfigurable interface {
	SetLogger(*zap.Logger)
}

// ServiceOption customises a Service at construction. Options are
// applied before the provider is built so a WithLogger logger can be
// propagated into a lifecycle-aware provider.
type ServiceOption func(*Service)

// WithBusyPredicate installs the enrichment-busy predicate the
// search-assist gate consults: when it reports true and the local model
// is not yet loaded, an implicit assist request defers its cold load
// rather than contend with the in-flight enrichment pass. A nil
// predicate (the default) always reads as "not busy".
func WithBusyPredicate(fn func() bool) ServiceOption {
	return func(s *Service) { s.busy = fn }
}

// WithLogger attaches the structured logger the service propagates into
// a lifecycle-aware provider (the local model's load / idle-unload
// diagnostics).
func WithLogger(l *zap.Logger) ServiceOption {
	return func(s *Service) { s.logger = l }
}

// errServiceUnavailable is returned by operational methods when no
// provider could be constructed (disabled config, build without
// `-tags llama` for the local provider, missing API key, ...).
var errServiceUnavailable = errors.New("llm: service unavailable — no provider configured")

// Service is the reusable LLM access point. It wraps a constructed
// llm.Provider plus a Backend (typically an InProcessBackend pointing
// at the daemon's *query.Engine). Three consumption shapes:
//
//   - Generate: one-shot prompt → text. Freeform completion.
//   - RunAgent: the grammar/schema-constrained tool-calling loop that
//     uses the Backend's tools to navigate the graph. Backs the MCP
//     `ask` tool.
//   - ExpandQuery / RerankSymbols / VerifyRelevance: the search-assist
//     passes — short structured completions backing the `search_symbols`
//     `assist` argument (see assist.go).
//
// The active provider is chosen by llm.Config.Provider. The prompt
// tier (profile) is derived from the active provider — a custom
// provider's prompt_profile, else the built-in's fixed tier — so the
// assist passes prompt small local models and hosted frontier models
// differently; see llm.ProfileFor.
type Service struct {
	cfg         llm.Config
	backend     llm.Backend
	provider    llm.Provider
	providerErr error
	profile     llm.PromptProfile

	expandCache *assistCache
	rerankCache *assistCache
	verifyCache *assistCache

	// routedProviders caches one provider per distinct model id when
	// llm.routing is enabled — see providerForModel. Guarded by
	// routedMu; closed by Close.
	routedMu        sync.Mutex
	routedProviders map[string]llm.Provider

	// convLog records the exact request/response of each completion as
	// JSONL when a conversation-log directory is configured. Opt-in
	// (records raw LLM I/O): a nil/disabled logger is a no-op.
	convLog *conversationlog.Logger

	// busy, when set, reports whether a background enrichment pass is in
	// flight — the signal the search-assist gate uses to defer a local
	// model cold load. Nil reads as "not busy". Installed via
	// WithBusyPredicate.
	busy func() bool

	// logger receives the provider's lifecycle diagnostics (model load /
	// idle unload). Installed via WithLogger; nil degrades to a no-op.
	logger *zap.Logger
}

// NewService constructs the service and its provider. Provider
// construction is cheap for every backend — the local provider only
// validates its config here and defers the model mmap to the first
// call. A disabled or misconfigured config yields a Service whose
// Enabled() reports false; the construction error is retained and
// surfaced via ProviderErr.
func NewService(cfg llm.Config, backend llm.Backend, opts ...ServiceOption) *Service {
	cfg = cfg.ApplyDefaults()
	s := &Service{
		cfg:             cfg,
		backend:         backend,
		expandCache:     newAssistCache(256),
		rerankCache:     newAssistCache(256),
		verifyCache:     newAssistCache(256),
		routedProviders: map[string]llm.Provider{},
		convLog:         conversationlog.New(conversationlog.DirFromEnv()),
	}
	for _, opt := range opts {
		opt(s)
	}
	if cfg.IsEnabled() && backend != nil {
		p, err := provider.New(cfg)
		if err != nil {
			s.providerErr = err
		} else {
			s.provider = p
			s.profile = llm.ProfileFor(cfg)
			s.configureProvider(p)
		}
	}
	return s
}

// configureProvider propagates the service's logger into a
// lifecycle-aware provider (the local model). A provider without the
// SetLogger method, or a nil logger, is left untouched.
func (s *Service) configureProvider(p llm.Provider) {
	if s.logger == nil {
		return
	}
	if lc, ok := p.(loggerConfigurable); ok {
		lc.SetLogger(s.logger)
	}
}

// ModelLoaded reports whether the active provider's backing model is
// resident. A provider with no cold-load cost (the HTTP backends), or no
// provider at all, always reports true — there is nothing to load.
func (s *Service) ModelLoaded() bool {
	if s == nil || s.provider == nil {
		return true
	}
	if l, ok := s.provider.(loadable); ok {
		return l.Loaded()
	}
	return true
}

// EnrichmentBusy reports whether the configured busy predicate fires —
// i.e. a background enrichment pass is currently running. A nil
// predicate (never configured) reports false.
func (s *Service) EnrichmentBusy() bool {
	if s == nil || s.busy == nil {
		return false
	}
	return s.busy()
}

// SetConversationDir enables (or disables) the conversation-log sink at
// runtime. A non-empty dir turns recording on; "" turns it off. This is
// the opt-in wiring point used by the daemon/server when the operator
// configures a conversation-log location.
func (s *Service) SetConversationDir(dir string) {
	if s == nil {
		return
	}
	if s.convLog != nil {
		_ = s.convLog.Close()
	}
	s.convLog = conversationlog.New(dir)
}

// ConversationDir returns the active conversation-log directory ("" when
// the sink is off).
func (s *Service) ConversationDir() string {
	if s == nil || s.convLog == nil {
		return ""
	}
	return s.convLog.Dir()
}

// recordConversation appends one Record to the conversation-log sink
// when it is enabled. The labels (session/repo/file/phase) ride on the
// context via conversationlog.WithMeta. A nil/disabled logger is a
// no-op; recording never disturbs the completion.
func (s *Service) recordConversation(ctx context.Context, req []llm.Message, resp string, usage llm.TokenUsage, model string, elapsedMs int64, callErr error) {
	if s == nil || s.convLog == nil || !s.convLog.Enabled() {
		return
	}
	meta := conversationlog.MetaFromContext(ctx)
	rec := conversationlog.Record{
		Session:   meta.Session,
		Repo:      meta.Repo,
		File:      meta.File,
		Phase:     meta.Phase,
		Provider:  s.ProviderName(),
		Model:     model,
		Request:   req,
		Response:  resp,
		ElapsedMs: elapsedMs,
	}
	if usage.IsZero() {
		// No provider usage available — estimate from char counts so the
		// inspector still shows a magnitude, flagged as an estimate.
		rec.InputTokens = estimateTokens(req)
		rec.OutputTokens = len(resp) / 4
		rec.Estimated = true
	} else {
		rec.InputTokens = usage.InputTokens
		rec.OutputTokens = usage.OutputTokens
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	}
	s.convLog.Record(rec)
}

// estimateTokens approximates the prompt token count from message
// content lengths (char/4) when no provider usage is available.
func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n / 4
}

// Enabled reports whether the service can do real work — a provider
// was constructed and a backend is wired. Callers gate feature /
// tool registration on this.
func (s *Service) Enabled() bool {
	return s != nil && s.provider != nil && s.backend != nil
}

// Provider returns the underlying LLM provider, or nil when none was
// constructed. Lets callers that need raw completion (e.g. the wiki
// narrative enhancer) reuse the service's already-built provider instead
// of constructing a second one.
func (s *Service) Provider() llm.Provider {
	if s == nil {
		return nil
	}
	return s.provider
}

// ProviderErr returns the error from provider construction, if any.
// Enabled() is false whenever this is non-nil; the daemon entrypoint
// surfaces it as a startup warning so a misconfigured `llm:` block
// (unset API key, model file missing) isn't silently ignored.
func (s *Service) ProviderErr() error {
	if s == nil {
		return nil
	}
	return s.providerErr
}

// ProviderName returns the active provider's name, or "" when no
// provider was constructed.
func (s *Service) ProviderName() string {
	if s == nil || s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

// Close releases the provider's resources (model weights, idle HTTP
// connections), including any providers constructed for model routing.
// Safe to call multiple times and on a disabled service.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.routedMu.Lock()
	for _, p := range s.routedProviders {
		_ = p.Close()
	}
	s.routedProviders = map[string]llm.Provider{}
	s.routedMu.Unlock()
	if s.convLog != nil {
		_ = s.convLog.Close()
	}
	if s.provider == nil {
		return nil
	}
	return s.provider.Close()
}

// providerForModel returns the llm.Provider for the given model id,
// constructing and caching one provider per distinct model. An empty
// model id — or one equal to the active provider's configured model —
// returns the base provider untouched. Routed providers are closed by
// Close. Used by model routing (see RunAgent).
func (s *Service) providerForModel(model string) (llm.Provider, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == strings.TrimSpace(s.cfg.ActiveModel()) {
		return s.provider, nil
	}
	s.routedMu.Lock()
	defer s.routedMu.Unlock()
	if p, ok := s.routedProviders[model]; ok {
		return p, nil
	}
	p, err := provider.New(s.cfg.WithModel(model))
	if err != nil {
		return nil, err
	}
	s.configureProvider(p)
	s.routedProviders[model] = p
	return p, nil
}

// repoCount reports how many repos the backend currently exposes — the
// graph-breadth signal Classify uses for routing. Returns 0 when the
// backend is unavailable or the lookup fails.
func (s *Service) repoCount(ctx context.Context) int {
	if s.backend == nil {
		return 0
	}
	repos, err := s.backend.ListRepos(ctx)
	if err != nil {
		return 0
	}
	return len(repos)
}

// Generate runs one-shot freeform inference: prompt in, generated text
// out. No agent loop, no tools. maxTokens caps generation length; 0
// falls back to a sensible default.
func (s *Service) Generate(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if s.provider == nil {
		return "", errServiceUnavailable
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	messages := []llm.Message{{Role: llm.RoleUser, Content: prompt}}
	t0 := time.Now()
	resp, err := s.provider.Complete(ctx, llm.CompletionRequest{
		Messages:  messages,
		MaxTokens: maxTokens,
		Shape:     llm.ShapeFreeform,
	})
	s.recordConversation(ctx, messages, resp.Text, resp.Usage, s.cfg.ActiveModel(), time.Since(t0).Milliseconds(), err)
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// GenerateWithUsage is the usage-aware variant of Generate: it returns
// the provider's token accounting for the call alongside the generated
// text. The review flow threads this so its CostBreakdown reflects a real
// per-call token split; a provider that does not surface usage yields a
// zero TokenUsage (the caller's cost block is then zero / not Estimated).
func (s *Service) GenerateWithUsage(ctx context.Context, prompt string, maxTokens int) (string, llm.TokenUsage, error) {
	if s.provider == nil {
		return "", llm.TokenUsage{}, errServiceUnavailable
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	messages := []llm.Message{{Role: llm.RoleUser, Content: prompt}}
	t0 := time.Now()
	resp, err := s.provider.Complete(ctx, llm.CompletionRequest{
		Messages:  messages,
		MaxTokens: maxTokens,
		Shape:     llm.ShapeFreeform,
	})
	s.recordConversation(ctx, messages, resp.Text, resp.Usage, s.cfg.ActiveModel(), time.Since(t0).Milliseconds(), err)
	if err != nil {
		return "", llm.TokenUsage{}, err
	}
	return resp.Text, resp.Usage, nil
}

// Pricing returns the active provider's USD-per-1M-token rate card so a
// usage-aware caller (the review cost block) can price its token usage. A
// user-registered custom provider carries its own pricing; for a built-in
// provider the active model is matched against the savings rate table (a
// single per-model input rate, applied to both the input and output
// fields since the table carries no separate output rate). An unknown
// model or a disabled service yields a zero rate card — the cost block is
// still emitted, just with a zero USD estimate.
func (s *Service) Pricing() llm.ProviderPricing {
	if s == nil || s.provider == nil {
		return llm.ProviderPricing{}
	}
	if cp, ok := s.cfg.Custom[s.cfg.ProviderName()]; ok {
		return cp.Pricing
	}
	rate := savings.ModelRate(s.cfg.ActiveModel())
	if rate <= 0 {
		return llm.ProviderPricing{}
	}
	return llm.ProviderPricing{Input: rate, Output: rate}
}

// RunAgent runs the structured tool-calling agent loop. The agent
// issues tool calls against the configured Backend and synthesizes a
// final answer via the final_answer tool.
//
// The returned AgentAnswer always has at least Answer/Error populated
// — non-nil even on error paths.
func (s *Service) RunAgent(ctx context.Context, opts llm.RunAgentOptions) (*llm.AgentAnswer, error) {
	answer := &llm.AgentAnswer{Scope: opts.Scope, ChainMode: opts.Chain}
	if s.provider == nil {
		answer.Error = errServiceUnavailable.Error()
		return answer, errServiceUnavailable
	}
	if strings.TrimSpace(opts.Question) == "" {
		err := errors.New("llm: question is empty")
		answer.Error = err.Error()
		return answer, err
	}

	systemExtras := opts.SystemExtras
	if systemExtras == "" {
		if opts.Chain {
			systemExtras = promptChain
		} else {
			systemExtras = promptSimple
		}
	}

	var tools []agent.Tool
	if opts.Chain {
		tools = agent.GortexChainTools(s.backend, opts.Scope)
	} else {
		tools = agent.GortexTools(s.backend, opts.Scope)
	}

	// Model routing: when enabled, classify the run by graph-derived
	// task complexity and dispatch it to the cheaper or the more
	// capable model. A routing failure (a tier model that won't
	// construct) degrades cleanly to the base provider.
	activeProvider := s.provider
	answer.Model = s.cfg.ActiveModel()
	if s.cfg.Routing.Enabled {
		complexity := llm.Classify(llm.ComplexitySignals{
			Question:  opts.Question,
			Chain:     opts.Chain,
			Scoped:    opts.Scope.Repo != "" || opts.Scope.Project != "" || opts.Scope.Ref != "",
			RepoCount: s.repoCount(ctx),
		})
		answer.Complexity = complexity.String()
		routedModel := s.cfg.Routing.SimpleModel
		if complexity == llm.ComplexityComplex {
			routedModel = s.cfg.Routing.ComplexModel
		}
		if p, perr := s.providerForModel(routedModel); perr == nil && p != nil {
			activeProvider = p
			if m := strings.TrimSpace(routedModel); m != "" {
				answer.Model = m
			}
		}
	}

	ag, err := agent.New(activeProvider, tools)
	if err != nil {
		answer.Error = err.Error()
		return answer, err
	}

	t0 := time.Now()
	answerText, transcript, runErr := ag.Run(ctx, systemExtras, opts.Question, s.cfg.MaxSteps)
	answer.ElapsedMs = time.Since(t0).Milliseconds()
	answer.Answer = answerText

	// Token accounting: the agent summed per-step provider usage over the
	// loop. Stamp it on the answer and price it against the answer's
	// model. Zero/Estimated:false for providers that don't report usage.
	answer.Usage = ag.LastUsage()
	answer.Cost = estimateRunCost(answer.Usage, answer.Model)

	// Record the agent turn to the conversation-log sink when enabled:
	// the framed prompt (system extras + question) in, the final answer
	// out, with the run's summed token usage.
	s.recordConversation(ctx,
		[]llm.Message{
			{Role: llm.RoleSystem, Content: systemExtras},
			{Role: llm.RoleUser, Content: opts.Question},
		},
		answerText, answer.Usage, answer.Model, answer.ElapsedMs, runErr)

	steps := 0
	for _, st := range transcript {
		if st.Kind == "call" || st.Kind == "final" {
			steps++
		}
	}
	answer.Steps = steps

	if opts.IncludeTranscript {
		answer.Transcript = make([]llm.TranscriptStep, 0, len(transcript))
		for _, st := range transcript {
			answer.Transcript = append(answer.Transcript, llm.TranscriptStep{
				Kind: st.Kind, Raw: st.Raw, Tool: st.Tool,
			})
		}
	}
	if runErr != nil {
		answer.Error = runErr.Error()
	}
	return answer, runErr
}

// estimateRunCost prices a run's token usage against the savings pricing
// table for the given model. Input and output tokens are both billed at
// the model's listed input rate (the table carries a single per-model
// rate); an unknown model or zero usage yields zero. The cost is an
// estimate — the table is a list-price approximation, not a billed total.
func estimateRunCost(u llm.TokenUsage, model string) float64 {
	if model == "" || u.IsZero() {
		return 0
	}
	billable := int64(u.InputTokens + u.OutputTokens)
	return savings.CostAvoided(billable, model)
}

// promptSimple — tight system-prompt extras for single-hop /
// cross-repo lookups.
const promptSimple = `RULES (follow these exactly):
- If the user gives you only a bare name (not a path-qualified id like "pkg/x.Foo"), you MUST first call search_symbols to resolve it to an id before calling get_callers.
- For search_symbols, pass ONLY the bare symbol name as "query" — no prepositions, no package qualifiers, no extra words.
- search_symbols returns ranked matches; the FIRST few are best. Pick at most the top 1-3 that look like functions or methods.
- Make at least one real tool call before final_answer.
- Never call the same tool with the same args twice in a row.
- When you have enough information, call final_answer summarising what you found.`

// promptChain — chain-mode extras with the explicit "no get_callers"
// direction warning.
const promptChain = `RULES (follow these exactly):
- You are tracing a cross-system call chain. Output one tool call per turn.
- DIRECTION MATTERS. Only these tools are correct in chain mode:
    * contracts        — find producer↔consumer pairs across repos
    * get_dependencies — FORWARD direction: what does this symbol call/import?
    * final_answer     — emit the chain
  Do NOT use get_callers. get_callers walks BACKWARDS (who calls X), which is
  the WRONG direction for chain tracing and will lead you astray.
- For search_symbols and contracts, pass clean values (no extra words).
- Typical flow for "trace request X across systems":
  1) contracts({"role":"consumer","path":"<path>"}) — find the caller side.
  2) contracts({"role":"provider","path":"<path>"}) — find the handler.
  3) get_dependencies({"id":"<provider symbol_id>"}) — see what the handler calls.
  4) For deeper hops, call get_dependencies AGAIN on the most interesting result's id.
  5) Look for deps whose repo prefix differs from the handler's repo —
     those are the cross-repo downstream calls.
  6) Call final_answer with the chain as numbered steps.
- Never call the same tool with the same args twice in a row.
- final_answer.text should list each system hop with its symbol id and repo.`
