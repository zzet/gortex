// Package embedding provides pluggable embedding providers for semantic search.
//
// The default build includes the Hugot provider (pure-Go ONNX runtime via
// hugot.NewGoSession) which auto-downloads MiniLM-L6-v2 on first use — no
// external runtime, no manual model placement. The legacy StaticProvider
// (GloVe word vectors) and APIProvider (Ollama/OpenAI) are also always
// available.
//
// Opt-in build tags enable faster transformer backends for users who are
// willing to manage native dependencies:
//   - embeddings_onnx  — yalue/onnxruntime_go with libonnxruntime on PATH
//   - embeddings_gomlx — hugot with XLA/PJRT plugin (~100MB auto-download)
package embedding

import (
	"context"
	"errors"
	"fmt"
)

// ErrBackendNotCompiled marks a local-backend factory that failed only because
// its build tag was not set (the onnx/gomlx stubs). Such a failure is benign
// noise in a default build — callers use errors.Is to log it at debug rather
// than warn even when the chain degrades to the static fallback.
var ErrBackendNotCompiled = errors.New("embedding backend not compiled in")

// Provider generates embedding vectors from text.
type Provider interface {
	// Embed returns the embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns embeddings for multiple texts, in input order.
	// The caller owns the returned slices: each vector must be freshly
	// allocated, must not share a backing array with another vector, and
	// must not be retained or modified by the provider after it returns.
	// The indexer stores them in its vector plan without copying.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the embedding vector size.
	Dimensions() int

	// Close releases resources.
	Close() error
}

// NewHugotProvider exposes the pure-Go Hugot backend (MiniLM-L6-v2)
// directly, without the NewLocalProvider fallback chain. Useful when a
// caller wants a hard error if Hugot can't start (e.g. eval harnesses
// that mustn't silently degrade to static GloVe).
func NewHugotProvider() (Provider, error) { return newHugotProvider() }

// NewHugotProviderWithVariant loads a specific embedder variant from
// any registered HuggingFace repo (MiniLM variants, code-tuned models,
// …). Pass a name returned by KnownHugotVariants (e.g. "fp32",
// "qint8_arm64", "jina_code", "bge_code"). Returns an error if the
// variant name is unknown or the download/load fails.
func NewHugotProviderWithVariant(variant string) (Provider, error) {
	v, ok := LookupHugotVariant(variant)
	if !ok {
		return nil, fmt.Errorf("unknown hugot variant %q (known: %v)", variant, KnownHugotVariants())
	}
	return newHugotProviderWithSpec(v)
}

// ProviderConfig is the subset of an embedding configuration that
// NewProviderFromConfig needs. It is a local struct — not the
// config.EmbeddingConfig type — so the embedding package stays free of
// an import dependency on internal/config. Callers translate their
// config block into this shape.
type ProviderConfig struct {
	// Provider selects the backend: "static" (baked GloVe, the
	// default), "local" (best available transformer), or "api" (an
	// external embedding endpoint). Empty is treated as "static".
	Provider string
	// APIURL / APIModel parameterise the "api" provider.
	APIURL   string
	APIModel string
	// Variant names a specific local transformer model to load (a key
	// from KnownHugotVariants, e.g. "fp32", "bge_small", "jina_code").
	// Honoured only when Provider is "local": a non-empty Variant pins
	// that exact model via NewHugotProviderWithVariant instead of the
	// auto-selected NewLocalProvider backend. Empty preserves the
	// existing default-selection behaviour. Ignored for other providers.
	Variant string
}

// NewProviderFromConfig constructs an embedding provider from a
// configuration block. The selection logic:
//
//   - "static" (or empty)  → NewStaticProvider — baked GloVe word
//     vectors, zero download, CPU-only. This is the default because
//     it makes semantic search work with no setup.
//   - "local"              → NewLocalProvider — the best available
//     transformer backend (Hugot MiniLM auto-downloads on first use).
//     When cfg.Variant names a specific model, that exact variant is
//     loaded via NewHugotProviderWithVariant instead.
//   - "api"                → NewAPIProvider against cfg.APIURL.
//
// An unknown provider name is an error so a typo in `.gortex.yaml`
// fails loudly instead of silently degrading.
func NewProviderFromConfig(cfg ProviderConfig) (Provider, error) {
	switch cfg.Provider {
	case "", "static":
		return NewStaticProvider()
	case "local":
		// A pinned variant loads that exact model; an empty variant
		// keeps the existing auto-selection (ONNX → GoMLX → Hugot →
		// static) so every prior config behaves identically.
		if cfg.Variant != "" {
			return NewHugotProviderWithVariant(cfg.Variant)
		}
		return NewLocalProvider()
	case "api":
		if cfg.APIURL == "" {
			return nil, fmt.Errorf("embedding provider %q requires an api_url", cfg.Provider)
		}
		if err := ValidateEmbeddingAPIURL(cfg.APIURL); err != nil {
			return nil, err
		}
		return NewAPIProvider(cfg.APIURL, cfg.APIModel), nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q (want static, local, or api)", cfg.Provider)
	}
}

// NewProviderFromConfigWithReport is NewProviderFromConfig plus a SelectionReport
// for the auto-selected local backend (Provider "local" with no pinned Variant),
// so the caller can log which backend was constructed and which were skipped.
// For every other provider the report is empty.
func NewProviderFromConfigWithReport(cfg ProviderConfig) (Provider, SelectionReport, error) {
	if cfg.Provider == "local" && cfg.Variant == "" {
		p, report := NewLocalProviderWithReport()
		if p == nil {
			err := fmt.Errorf("no embedding provider available")
			if n := len(report.Attempts); n > 0 {
				err = report.Attempts[n-1].Err
			}
			return nil, report, err
		}
		return p, report, nil
	}
	p, err := NewProviderFromConfig(cfg)
	return p, SelectionReport{}, err
}

// SelectionAttempt records one backend the local-provider chain tried and the
// error that made it fall through. It exists so a silent degradation to the
// static GloVe fallback becomes observable to the caller.
type SelectionAttempt struct {
	Backend string
	Err     error
}

// SelectionReport describes how NewLocalProviderWithReport chose a backend: the
// backend actually constructed, its dimension, and every rejected attempt.
// Chosen is the backend name (e.g. "hugot", "static"); it is "static" when the
// chain fell all the way through to the GloVe fallback.
type SelectionReport struct {
	Chosen   string
	Dims     int
	Attempts []SelectionAttempt
}

// NewLocalProviderWithReport returns the best available local embedding provider
// along with a report of every backend it tried. Preference order: ONNX
// (fastest, requires libonnxruntime) → GoMLX (XLA) → Hugot (pure Go, always
// compiled in) → Static (GloVe word vectors fallback). A nil provider means even
// the static fallback failed to construct (its error is the last attempt).
func NewLocalProviderWithReport() (Provider, SelectionReport) {
	factories := []struct {
		name    string
		factory func() (Provider, error)
	}{
		{"onnx", newONNXProvider},
		{"gomlx", newGoMLXProvider},
		{"hugot", newHugotProvider},
	}
	var report SelectionReport
	for _, nf := range factories {
		p, err := nf.factory()
		if err == nil {
			report.Chosen = nf.name
			report.Dims = p.Dimensions()
			return p, report
		}
		report.Attempts = append(report.Attempts, SelectionAttempt{Backend: nf.name, Err: err})
	}
	// Fallback: static word vectors (always available, no network).
	p, err := NewStaticProvider()
	if err != nil {
		report.Attempts = append(report.Attempts, SelectionAttempt{Backend: "static", Err: err})
		return nil, report
	}
	report.Chosen = "static"
	report.Dims = p.Dimensions()
	return p, report
}

// NewLocalProvider returns the best available local embedding provider,
// discarding the selection report. See NewLocalProviderWithReport.
func NewLocalProvider() (Provider, error) {
	p, report := NewLocalProviderWithReport()
	if p == nil {
		if n := len(report.Attempts); n > 0 {
			return nil, report.Attempts[n-1].Err
		}
		return nil, fmt.Errorf("no embedding provider available")
	}
	return p, nil
}
