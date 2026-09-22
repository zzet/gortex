package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIProvider_Concurrent asserts the API provider declares itself
// safe to call concurrently — the signal the embedding pool gates on.
func TestAPIProvider_Concurrent(t *testing.T) {
	p := NewAPIProvider("http://localhost:11434", "")
	assert.True(t, p.Concurrent(), "the API provider must opt into concurrent embedding")
}

func TestAPIProvider_OllamaBoundsBatchSize(t *testing.T) {
	texts := make([]string, 500)
	ordinals := make(map[string]int, len(texts))
	for i := range texts {
		texts[i] = "input-" + strconv.Itoa(i)
		ordinals[texts[i]] = i
	}

	var (
		mu         sync.Mutex
		batchSizes []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !req.Truncate {
			http.Error(w, "truncate must be explicit", http.StatusBadRequest)
			return
		}

		mu.Lock()
		batchSizes = append(batchSizes, len(req.Input))
		mu.Unlock()

		embeddings := make([][]float32, len(req.Input))
		for i, text := range req.Input {
			ordinal, ok := ordinals[text]
			if !ok {
				http.Error(w, "unexpected input", http.StatusBadRequest)
				return
			}
			embeddings[i] = []float32{float32(ordinal)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaResponse{Embeddings: embeddings})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	vecs, err := p.EmbedBatch(context.Background(), texts)
	require.NoError(t, err)
	require.Len(t, vecs, len(texts))
	for i := range vecs {
		assert.Equal(t, []float32{float32(i)}, vecs[i], "output order must match input order")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, batchSizes, 8, "500 inputs should be sent as seven full batches and one remainder")
	for _, size := range batchSizes {
		assert.LessOrEqual(t, size, maxOllamaBatchSize)
	}
}

func TestAPIProvider_OllamaSplitsAfterTokenizerEOF(t *testing.T) {
	texts := make([]string, 17)
	ordinals := make(map[string]int, len(texts))
	for i := range texts {
		texts[i] = "input-" + strconv.Itoa(i)
		ordinals[texts[i]] = i
	}

	var (
		mu         sync.Mutex
		batchSizes []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		inputs, ok := req.Input.([]any)
		if !ok {
			http.Error(w, "expected input array", http.StatusBadRequest)
			return
		}

		mu.Lock()
		batchSizes = append(batchSizes, len(inputs))
		mu.Unlock()

		if len(inputs) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`))
			return
		}

		embeddings := make([][]float32, len(inputs))
		for i, value := range inputs {
			text, ok := value.(string)
			if !ok {
				http.Error(w, "expected string input", http.StatusBadRequest)
				return
			}
			ordinal, ok := ordinals[text]
			if !ok {
				http.Error(w, "unexpected input", http.StatusBadRequest)
				return
			}
			embeddings[i] = []float32{float32(ordinal)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaResponse{Embeddings: embeddings})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	vecs, err := p.EmbedBatch(context.Background(), texts)
	require.NoError(t, err)
	require.Len(t, vecs, len(texts))
	for i := range vecs {
		assert.Equal(t, []float32{float32(i)}, vecs[i], "split responses must retain input order")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, batchSizes, 2*len(texts)-1,
		"a backend accepting only singletons should exercise the complete bounded split tree")
	assert.Equal(t, len(texts), countEqual(batchSizes, 1))
}

func TestAPIProvider_OllamaTokenizerEOFRetriesAreBounded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	texts := make([]string, maxOllamaBatchSize)
	for i := range texts {
		texts[i] = strings.Repeat("x", 16)
	}
	_, err := p.EmbedBatch(context.Background(), texts)
	require.Error(t, err)
	assert.ErrorContains(t, err, "after 3 recovery attempts")
	assert.Equal(t, int32(10), atomic.LoadInt32(&calls),
		"a persistent failure should follow one branch to a singleton, then stop after bounded retries")
}

func TestAPIProvider_OllamaRetriesSingletonBeforeShortening(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Input) != 1 {
			http.Error(w, "invalid input", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, req.Input[0])
		call := len(received)
		mu.Unlock()

		if call == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaResponse{Embeddings: [][]float32{{1, 2}}})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	vecs, err := p.EmbedBatch(context.Background(), []string{"unchanged retry"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 2}}, vecs)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"unchanged retry", "unchanged retry"}, received)
}

func TestAPIProvider_OllamaShortensCrashingSingleton(t *testing.T) {
	text := strings.Repeat("x", 64)
	var (
		mu        sync.Mutex
		byteSizes []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Input) != 1 {
			http.Error(w, "invalid input", http.StatusBadRequest)
			return
		}
		mu.Lock()
		byteSizes = append(byteSizes, len(req.Input[0]))
		mu.Unlock()

		if len(req.Input[0]) > 16 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaResponse{Embeddings: [][]float32{{3, 4}}})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	vecs, err := p.EmbedBatch(context.Background(), []string{text})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{3, 4}}, vecs)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{64, 64, 32, 16}, byteSizes)
}

func TestAPIProvider_OllamaDoesNotRetryUnrelated400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"input validation failed with EOF"}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	_, err := p.EmbedBatch(context.Background(), []string{"one", "two"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "input validation failed")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestAPIProvider_OllamaTokenizerEOFHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		cancel()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "nomic-embed-text")
	p.format = formatOllama
	_, err := p.EmbedBatch(ctx, []string{"one", "two"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestIsOllamaRunnerTokenizerEOF(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "reported IPv4 runner failure",
			err: &ollamaAPIError{
				statusCode: http.StatusBadRequest,
				body:       `{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`,
			},
			want: true,
		},
		{
			name: "localhost runner failure",
			err: &ollamaAPIError{
				statusCode: http.StatusBadRequest,
				body:       `{"error":"Post \"http://localhost:51824/tokenize\": EOF"}`,
			},
			want: true,
		},
		{
			name: "remote URL",
			err: &ollamaAPIError{
				statusCode: http.StatusBadRequest,
				body:       `{"error":"Post \"http://example.com:51824/tokenize\": EOF"}`,
			},
		},
		{
			name: "other endpoint",
			err: &ollamaAPIError{
				statusCode: http.StatusBadRequest,
				body:       `{"error":"Post \"http://127.0.0.1:51824/v1/embeddings\": EOF"}`,
			},
		},
		{
			name: "other status",
			err: &ollamaAPIError{
				statusCode: http.StatusInternalServerError,
				body:       `{"error":"Post \"http://127.0.0.1:51824/tokenize\": EOF"}`,
			},
		},
		{
			name: "malformed body",
			err:  &ollamaAPIError{statusCode: http.StatusBadRequest, body: "EOF"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isOllamaRunnerTokenizerEOF(tt.err))
		})
	}
}

func countEqual(values []int, want int) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

// TestParseRetryAfter covers the delta-seconds Retry-After parser.
func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, 12*time.Second, parseRetryAfter("12"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("  "))
	assert.Equal(t, time.Duration(0), parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"),
		"the HTTP-date form is not parsed — returns 0 so the caller uses a fixed backoff")
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5"), "a negative hint is rejected")
}

// TestAPIProvider_HonorsRetryAfterOn429 asserts the provider retries
// once after an HTTP 429, honouring the Retry-After header, and
// succeeds when the retry returns 200.
func TestAPIProvider_HonorsRetryAfterOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First call: rate-limited with a 1-second hint.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Retry: succeed with an OpenAI-shaped embedding response.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Data: []openAIEmbedding{{Embedding: []float32{1, 2, 3}, Index: 0}},
		})
	}))
	defer srv.Close()

	// srv.URL has no Ollama markers, so the provider uses OpenAI format.
	p := NewAPIProvider(srv.URL, "text-embedding-3-small")

	start := time.Now()
	vecs, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err, "the embed must succeed after the post-429 retry")
	require.Len(t, vecs, 1)
	assert.Equal(t, []float32{1, 2, 3}, vecs[0])
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "exactly one retry after the 429")
	assert.GreaterOrEqual(t, time.Since(start), time.Second,
		"the provider must wait out the Retry-After hint before retrying")
}

// TestAPIProvider_429WithoutHintStillRetries asserts that a 429 with no
// Retry-After header still triggers exactly one retry (on a short fixed
// backoff) rather than failing immediately.
func TestAPIProvider_429WithoutHintStillRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Data: []openAIEmbedding{{Embedding: []float32{4, 5, 6}, Index: 0}},
		})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "")
	vecs, err := p.EmbedBatch(context.Background(), []string{"x"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

// TestAPIProvider_PersistentRateLimitFails asserts a server that keeps
// returning 429 eventually surfaces an error — the retry is bounded to
// one attempt, it is not an infinite loop.
func TestAPIProvider_PersistentRateLimitFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "")
	_, err := p.EmbedBatch(context.Background(), []string{"x"})
	require.Error(t, err, "a persistent 429 must eventually fail")
	assert.LessOrEqual(t, atomic.LoadInt32(&calls), int32(2),
		"the 429 retry is bounded to a single re-attempt")
}

// TestAPIProvider_SendsAuthorizationHeader asserts that an embeddings API
// key (GORTEX_EMBEDDINGS_API_KEY) is forwarded as an Authorization: Bearer
// header — the fix that lets gortex use authenticated backends like OpenAI.
func TestAPIProvider_SendsAuthorizationHeader(t *testing.T) {
	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "test-secret")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`))
	}))
	defer srv.Close()

	// A non-Ollama URL selects the OpenAI format (the /v1/embeddings path).
	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	_, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-secret", gotAuth)
}

// TestAPIProvider_NoKeyOmitsAuthorizationHeader asserts that with no key
// configured, no Authorization header is sent (keyless Ollama stays keyless
// and a stray OPENAI_API_KEY does not leak to a non-OpenAI endpoint).
func TestAPIProvider_NoKeyOmitsAuthorizationHeader(t *testing.T) {
	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1],"index":0}]}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	_, err := p.EmbedBatch(context.Background(), []string{"hi"})
	require.NoError(t, err)
	assert.False(t, hadAuth, "no Authorization header without a configured key")
}

// TestAPIProvider_RequestyKeyFallback asserts REQUESTY_API_KEY is picked
// up only for a requesty.ai endpoint, mirroring the OPENAI_API_KEY rule
// for openai.com: the vendor key must never leak to another host, and an
// explicit GORTEX_EMBEDDINGS_API_KEY still wins.
func TestAPIProvider_RequestyKeyFallback(t *testing.T) {
	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("REQUESTY_API_KEY", "rq-secret")

	p := NewAPIProvider("https://router.requesty.ai/v1", "openai/text-embedding-3-small")
	assert.Equal(t, "rq-secret", p.apiKey, "requesty host falls back to REQUESTY_API_KEY")

	p = NewAPIProvider("https://router.eu.requesty.ai/v1", "openai/text-embedding-3-small")
	assert.Equal(t, "rq-secret", p.apiKey, "regional requesty host falls back too")

	p = NewAPIProvider("https://api.openai.com/v1", "text-embedding-3-small")
	assert.Equal(t, "", p.apiKey, "REQUESTY_API_KEY must not leak to a non-requesty host")

	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "explicit")
	p = NewAPIProvider("https://router.requesty.ai/v1", "openai/text-embedding-3-small")
	assert.Equal(t, "explicit", p.apiKey, "an explicit embeddings key wins over the fallback")
}

// TestAPIProvider_AccumulatesTokenUsage asserts the provider reads the
// OpenAI `usage.total_tokens` field off each embedding response and
// accumulates it across calls — the signal the indexer logs so the paid
// embedding pass reports its actual token spend (it otherwise vanishes).
func TestAPIProvider_AccumulatesTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	assert.Equal(t, int64(0), p.TokensUsed(), "no tokens before any call")

	_, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err)
	_, err = p.EmbedBatch(context.Background(), []string{"world"})
	require.NoError(t, err)

	assert.Equal(t, int64(14), p.TokensUsed(), "usage accumulates across batches")
}

// TestAPIProvider_OpenAIBaseURLVariants asserts the OpenAI request path is
// "/v1/embeddings" whether the base URL is given with or without the "/v1"
// version segment. OpenAI-compatible bases conventionally include "/v1"
// (OpenAI, OpenRouter); without this normalization a "…/v1" base produced
// "…/v1/v1/embeddings" → 404 → silent fallback to BM25.
func TestAPIProvider_OpenAIBaseURLVariants(t *testing.T) {
	for _, suffix := range []string{"", "/v1"} {
		suffix := suffix
		t.Run("base"+suffix, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`))
			}))
			defer srv.Close()

			p := NewAPIProvider(srv.URL+suffix, "text-embedding-3-small")
			_, err := p.EmbedBatch(context.Background(), []string{"hi"})
			require.NoError(t, err)
			assert.Equal(t, "/v1/embeddings", gotPath,
				"both base forms must resolve to a single /v1/embeddings path")
		})
	}
}

// TestAPIProvider_ProbeDimensions asserts the probe discovers the vector
// width with exactly one embedding call, caches it (so Dimensions() then
// reports the true width up front), and is idempotent — a second probe
// issues no further HTTP. This is the fix for the daemon logging dim:0 and
// the snapshot-vector reload gate rejecting a correctly-sized cached index.
func TestAPIProvider_ProbeDimensions(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// A 4-dimensional vector stands in for OpenAI's 1536-d response.
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3,0.4],"index":0}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	assert.Equal(t, 0, p.Dimensions(), "width unknown before the first call")

	dim, err := p.ProbeDimensions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, dim, "probe reports the response vector width")
	assert.Equal(t, 4, p.Dimensions(), "width is cached after the probe")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "the probe is a single call")

	dim2, err := p.ProbeDimensions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, dim2)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "a second probe issues no further HTTP")
}

// TestAPIProvider_ProbeDimensionsError asserts that a probe against an
// unreachable / erroring backend surfaces the error and leaves the width at
// 0 (best-effort) — the caller only warns and indexing falls back to BM25.
func TestAPIProvider_ProbeDimensionsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	dim, err := p.ProbeDimensions(context.Background())
	require.Error(t, err, "an auth failure must surface as an error")
	assert.Equal(t, 0, dim)
	assert.Equal(t, 0, p.Dimensions(), "a failed probe leaves the width unset")
}

// TestAPIProvider_ProbeDimensionsLive hits the REAL OpenAI embeddings API to
// prove the fork's embedder is genuinely wired end-to-end (not a stub): the
// probe returns text-embedding-3-small's true 1536-d width, a batch embed
// returns 1536-d vectors, and token usage is accounted. Skipped unless a key
// is present so CI without credentials stays green.
//
//	OPENAI_API_KEY=sk-... go test ./internal/embedding -run ProbeDimensionsLive -v
func TestAPIProvider_ProbeDimensionsLive(t *testing.T) {
	if os.Getenv("GORTEX_EMBEDDINGS_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("no embeddings API key (set OPENAI_API_KEY) — skipping live OpenAI probe")
	}

	p := NewAPIProvider("https://api.openai.com/v1", "text-embedding-3-small")
	dim, err := p.ProbeDimensions(context.Background())
	require.NoError(t, err, "live probe against OpenAI must succeed")
	assert.Equal(t, 1536, dim, "text-embedding-3-small is 1536-dimensional")
	assert.Equal(t, 1536, p.Dimensions())

	vecs, err := p.EmbedBatch(context.Background(), []string{"rule engine evaluates facts", "knowledge base"})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	assert.Len(t, vecs[0], 1536, "each returned vector is 1536-d")
	assert.Len(t, vecs[1], 1536)
	assert.Greater(t, p.TokensUsed(), int64(0), "the paid embedding pass accounts token usage")
}

// TestTruncateEmbedInputs asserts oversized inputs are head-truncated to the
// byte cap (so OpenAI's 8192-token limit can't abort the whole vector index)
// while normal inputs pass through untouched.
func TestTruncateEmbedInputs(t *testing.T) {
	short := "small symbol"
	long := string(make([]byte, maxEmbedInputBytes+500))

	out := truncateEmbedInputs([]string{short, long})
	assert.Equal(t, short, out[0], "short input untouched")
	assert.LessOrEqual(t, len(out[1]), maxEmbedInputBytes, "long input capped")

	in := []string{"a", "b"}
	assert.Equal(t, in, truncateEmbedInputs(in), "no oversize → same slice values")

	multibyte := strings.Repeat("a", maxEmbedInputBytes-1) + "€"
	assert.Equal(t, strings.Repeat("a", maxEmbedInputBytes-1), truncateEmbedInputs([]string{multibyte})[0],
		"truncation must drop a partial UTF-8 rune")
}
