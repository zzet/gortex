package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// maxRetryAfterWait caps how long the API provider will sleep on an
// HTTP 429 Retry-After hint. A hostile or mis-set header should not be
// able to stall indexing indefinitely; past this the request just
// fails and the caller aborts to text-only search.
const maxRetryAfterWait = 60 * time.Second

// maxEmbedInputBytes caps each embedding input. OpenAI's embedding models
// reject inputs over 8192 tokens with a 400 that aborts the WHOLE batch
// (and the vector index). A BPE tokenizer never emits more tokens than
// input characters, and for single-byte (ASCII) source — the overwhelming
// majority of code — characters equal bytes, so capping the head at 8000
// bytes guarantees ≤8000 tokens, safely under the 8192 limit, regardless
// of how token-dense the snippet is. The truncated head still carries the
// symbol's signature and leading body — enough signal for nearest-neighbour
// search. Head-truncation beats dropping the whole index over a few giant
// generated symbols.
const maxEmbedInputBytes = 8000

// maxOllamaBatchSize bounds the number of inputs sent in one /api/embed
// request. Ollama fans an input array out internally, so forwarding the
// indexer's 500-item chunks unchanged can overwhelm or crash its runner.
// Keeping the provider-level cap here also protects direct EmbedBatch
// callers while preserving the indexer's configured outer concurrency.
const maxOllamaBatchSize = 64

// maxOllamaSingletonRetries bounds recovery once bisection isolates one
// input. The first retry is unchanged so a restarted runner can accept it;
// the next two halve the UTF-8-safe byte cap (8000 → 4000 → 2000) to handle
// Ollama runners that repeatedly die on one long embedding input.
const maxOllamaSingletonRetries = 3

func truncateUTF8Head(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && text[end]&0xC0 == 0x80 {
		end--
	}
	return text[:end]
}

// truncateEmbedInputs head-truncates any input over the byte cap, on a
// UTF-8 rune boundary so the JSON payload stays valid. Returns the same
// slice when nothing needed trimming (the common case).
func truncateEmbedInputs(texts []string) []string {
	var out []string
	for i, t := range texts {
		if len(t) <= maxEmbedInputBytes {
			continue
		}
		if out == nil {
			out = make([]string, len(texts))
			copy(out, texts)
		}
		out[i] = truncateUTF8Head(t, maxEmbedInputBytes)
	}
	if out == nil {
		return texts
	}
	return out
}

// APIProvider calls an external embedding API (Ollama or OpenAI-compatible).
type APIProvider struct {
	url    string
	model  string
	apiKey string
	client *http.Client
	dims   int
	format apiFormat

	// tokensUsed accumulates the `usage.total_tokens` reported by the
	// embedding backend across every request, so the indexer can log the
	// actual token spend of a paid embedding pass (otherwise invisible).
	// Touched from several goroutines under the concurrent embedding pool,
	// hence atomic.
	tokensUsed int64
}

// TokensUsed reports the total embedding tokens this provider has been
// billed for so far, summed from each response's usage.total_tokens.
// Returns 0 for backends that don't report usage (e.g. Ollama).
func (p *APIProvider) TokensUsed() int64 { return atomic.LoadInt64(&p.tokensUsed) }

type apiFormat int

const (
	formatOllama apiFormat = iota
	formatOpenAI
)

// NewAPIProvider creates a provider that calls an external embedding API.
// Auto-detects Ollama vs OpenAI format from the URL.
func NewAPIProvider(url, model string) *APIProvider {
	format := formatOpenAI
	if strings.Contains(url, "11434") || strings.Contains(url, "/api/") {
		format = formatOllama
	}

	if model == "" {
		if format == formatOllama {
			model = "nomic-embed-text"
		} else {
			model = "text-embedding-3-small"
		}
	}

	// API key for authenticated embedding backends (OpenAI, Azure, and
	// OpenAI-compatible gateways). Ollama on localhost is keyless, so the
	// key stays optional and an unset value just omits the header. Prefer
	// an explicit GORTEX_EMBEDDINGS_API_KEY; fall back to OPENAI_API_KEY
	// only when the endpoint is api.openai.com (and to REQUESTY_API_KEY
	// only for a router.requesty.ai host), so a stray vendor key can
	// never leak to an arbitrary third-party URL.
	apiKey := os.Getenv("GORTEX_EMBEDDINGS_API_KEY")
	if apiKey == "" && strings.Contains(url, "openai.com") {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" && strings.Contains(url, "requesty.ai") {
		apiKey = os.Getenv("REQUESTY_API_KEY")
	}

	return &APIProvider{
		url:    strings.TrimRight(url, "/"),
		model:  model,
		apiKey: apiKey,
		client: &http.Client{Timeout: 30 * time.Second},
		format: format,
	}
}

func (p *APIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedding API returned no results")
	}
	return vecs[0], nil
}

func (p *APIProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var (
		vecs [][]float32
		err  error
	)
	if p.format == formatOllama {
		vecs, err = p.embedOllama(ctx, texts)
	} else {
		vecs, err = p.embedOpenAI(ctx, texts)
	}
	if err != nil {
		return nil, err
	}
	// Width is learned lazily from the response (dims 0), so validate the count,
	// reject empty vectors, and check the batch is internally consistent without
	// asserting an absolute width.
	if err := validateBatch("api", texts, vecs, 0); err != nil {
		return nil, err
	}
	return vecs, nil
}

func (p *APIProvider) Dimensions() int { return p.dims }
func (p *APIProvider) Close() error    { return nil }

// ProbeDimensions makes one tiny embedding call to discover and cache the
// provider's vector width, so Dimensions() reports the true value *before*
// the first indexing pass. An APIProvider learns its width only from the
// first real embed (embedOpenAI / embedOllama set p.dims from the returned
// vector); until then Dimensions() returns 0, which has two concrete
// consequences at daemon startup: the "embeddings enabled" log mislabels
// the width as dim:0, and any gate comparing the configured width against
// already-persisted vectors sees a mismatch that isn't there and forces a
// needless full re-embed.
//
// Idempotent: a no-op once the width is known. Best-effort: on any
// transport/auth error it returns the error and leaves dims at 0 — the
// caller logs a warning, the lazy path still sets the width from the first
// real vector, and indexing degrades to BM25 if embeddings are truly
// unreachable. The probe also doubles as an early connectivity/credential
// check, surfacing a bad key or URL at startup instead of mid-index.
func (p *APIProvider) ProbeDimensions(ctx context.Context) (int, error) {
	if d := p.Dimensions(); d > 0 {
		return d, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	vec, err := p.Embed(pctx, "gortex embedding dimension probe")
	if err != nil {
		return 0, err
	}
	// Embed -> EmbedBatch -> embed{OpenAI,Ollama} already cached p.dims from
	// the response; fall back to the returned vector's length defensively.
	if p.dims == 0 && len(vec) > 0 {
		p.dims = len(vec)
	}
	return p.dims, nil
}

// Concurrent reports that this provider is safe — and worth — calling
// from several goroutines at once. An external HTTP embedding endpoint
// gains from overlapped round-trips; the indexer's embedding pool uses
// this to decide whether to parallelise.
func (p *APIProvider) Concurrent() bool { return true }

// doRequest issues req via the provider's HTTP client and returns the
// response. On an HTTP 429 it honours a Retry-After header (delta-
// seconds form) and retries once after sleeping — capped at
// maxRetryAfterWait so a bad header cannot stall indexing. The caller
// owns closing the returned body.
func (p *APIProvider) doRequest(ctx context.Context, req *http.Request, body []byte) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	// Rate limited — read the hint, drain and close this response,
	// then retry exactly once.
	wait := parseRetryAfter(resp.Header.Get("Retry-After"))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	if wait <= 0 {
		// No usable hint — let the caller surface the 429 by re-issuing
		// once without a sleep would just hammer the API, so fall back
		// to a short fixed backoff.
		wait = time.Second
	}
	if wait > maxRetryAfterWait {
		wait = maxRetryAfterWait
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Rebuild the request — a *http.Request body is single-use.
	retry, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	retry.Header = req.Header.Clone()
	return p.client.Do(retry)
}

// parseRetryAfter parses the delta-seconds form of an HTTP Retry-After
// header ("Retry-After: 12"). The HTTP-date form is not handled —
// embedding APIs use delta-seconds in practice — and returns 0, which
// the caller treats as "no usable hint".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// --- Ollama API ---

type ollamaRequest struct {
	Model    string `json:"model"`
	Input    any    `json:"input"` // string or []string
	Truncate bool   `json:"truncate"`
}

type ollamaResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type ollamaAPIError struct {
	statusCode int
	body       string
}

func (e *ollamaAPIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.statusCode, e.body)
}

func (p *APIProvider) embedOllama(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) <= maxOllamaBatchSize {
		return p.embedOllamaRecover(ctx, texts)
	}

	vecs := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxOllamaBatchSize {
		end := min(start+maxOllamaBatchSize, len(texts))
		batch, err := p.embedOllamaRecover(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		vecs = append(vecs, batch...)
	}
	return vecs, nil
}

func (p *APIProvider) embedOllamaRecover(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := p.embedOllamaOnce(ctx, texts)
	if err == nil {
		return vecs, nil
	}
	if !isOllamaRunnerTokenizerEOF(err) {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(texts) == 0 {
		return nil, err
	}
	if len(texts) == 1 {
		return p.recoverOllamaSingleton(ctx, texts[0], err)
	}

	mid := len(texts) / 2
	left, err := p.embedOllamaRecover(ctx, texts[:mid])
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	right, err := p.embedOllamaRecover(ctx, texts[mid:])
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func (p *APIProvider) recoverOllamaSingleton(ctx context.Context, text string, initialErr error) ([][]float32, error) {
	candidate := truncateUTF8Head(text, maxEmbedInputBytes)
	lastErr := initialErr
	attempts := 0

	for retry := 0; retry < maxOllamaSingletonRetries; retry++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if retry > 0 {
			shortened := truncateUTF8Head(candidate, len(candidate)/2)
			if shortened == "" || shortened == candidate {
				break
			}
			candidate = shortened
		}

		attempts++
		vecs, err := p.embedOllamaOnce(ctx, []string{candidate})
		if err == nil {
			return vecs, nil
		}
		if !isOllamaRunnerTokenizerEOF(err) {
			return nil, err
		}
		lastErr = err
	}

	return nil, fmt.Errorf(
		"ollama tokenizer runner failed for one input after %d recovery attempts: %w",
		attempts, lastErr,
	)
}

// isOllamaRunnerTokenizerEOF recognizes the narrow failure reported in
// #296: Ollama returns HTTP 400 even though its private loopback tokenizer
// transport died. Ordinary validation errors, remote URLs, other runner
// endpoints, and other status codes must not trigger retries.
func isOllamaRunnerTokenizerEOF(err error) bool {
	var apiErr *ollamaAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusBadRequest {
		return false
	}

	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.body), &payload) != nil {
		return false
	}

	const postMarker = `Post "`
	start := strings.Index(payload.Error, postMarker)
	if start < 0 {
		return false
	}
	rest := payload.Error[start+len(postMarker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 || strings.TrimSpace(rest[end+1:]) != ": EOF" {
		return false
	}

	runnerURL, err := url.Parse(rest[:end])
	if err != nil || runnerURL.Scheme != "http" || runnerURL.Path != "/tokenize" || runnerURL.Port() == "" {
		return false
	}
	host := runnerURL.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *APIProvider) embedOllamaOnce(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := ollamaRequest{
		Model:    p.model,
		Input:    truncateEmbedInputs(texts),
		Truncate: true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.url + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.doRequest(ctx, req, body)
	if err != nil {
		return nil, fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &ollamaAPIError{
			statusCode: resp.StatusCode,
			body:       string(respBody),
		}
	}

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Embeddings) > 0 && p.dims == 0 {
		p.dims = len(result.Embeddings[0])
	}

	return result.Embeddings, nil
}

// --- OpenAI API ---

type openAIRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIResponse struct {
	Data  []openAIEmbedding `json:"data"`
	Usage openAIUsage       `json:"usage"`
}

// openAIUsage carries the token accounting OpenAI returns alongside every
// embeddings response. total_tokens is what the request is billed on.
type openAIUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type openAIEmbedding struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

func (p *APIProvider) embedOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := openAIRequest{
		Model: p.model,
		Input: truncateEmbedInputs(texts),
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// OpenAI-compatible bases are conventionally given WITH the version
	// segment (OpenAI "https://api.openai.com/v1", OpenRouter
	// "https://openrouter.ai/api/v1", Requesty "https://router.requesty.ai/v1").
	// Append "/v1" only when it is absent,
	// so a "…/v1" base does not become "…/v1/v1/embeddings" (a 404 that
	// silently degrades the whole vector index to BM25).
	endpoint := "/v1/embeddings"
	if strings.HasSuffix(p.url, "/v1") {
		endpoint = "/embeddings"
	}
	url := p.url + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.doRequest(ctx, req, body)
	if err != nil {
		return nil, fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if result.Usage.TotalTokens > 0 {
		atomic.AddInt64(&p.tokensUsed, int64(result.Usage.TotalTokens))
	}

	vecs := make([][]float32, len(result.Data))
	for _, d := range result.Data {
		vecs[d.Index] = d.Embedding
	}

	if len(vecs) > 0 && p.dims == 0 && len(vecs[0]) > 0 {
		p.dims = len(vecs[0])
	}

	return vecs, nil
}
