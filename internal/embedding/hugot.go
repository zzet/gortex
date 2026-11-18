package embedding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/zzet/gortex/internal/platform"
)

// miniLMRepo is the sentence-transformers MiniLM-L6-v2 repo used by
// the default bundled variants. Other variants (code-tuned, multilingual,
// …) carry their own RepoID on the variant spec.
const miniLMRepo = "sentence-transformers/all-MiniLM-L6-v2"

// HugotProvider uses Hugot with the pure Go backend for offline transformer embeddings.
// Model auto-downloads from Hugging Face on first use.
type HugotProvider struct {
	session   *hugot.Session
	pipeline  *pipelines.FeatureExtractionPipeline
	dims      int
	truncator *tokenTruncator
	mu        sync.Mutex
}

// DefaultHugotVariant is the short name of the variant newHugotProvider
// loads when no explicit choice is made. MiniLM-L6-v2 fp32 — baseline
// quality, slowest inference, small footprint.
const DefaultHugotVariant = "fp32"

// HugotVariant describes one embeddable model: which HuggingFace repo
// to pull, which ONNX variant file inside it to load, the embedding
// dimension, and a human-readable label. Exposed so `gortex eval
// embedders` can enumerate + compare arbitrary models.
type HugotVariant struct {
	RepoID     string // HuggingFace repo path, e.g. "BAAI/bge-code-v1"
	OnnxFile   string // path inside the repo, e.g. "onnx/model.onnx"
	Dimensions int    // embedding dim (must match model output)
	Label      string // short human label shown in the report
}

var hugotVariants = map[string]HugotVariant{
	// MiniLM-L6-v2 variants — general-English baseline.
	"fp32":         {RepoID: miniLMRepo, OnnxFile: "onnx/model.onnx", Dimensions: 384, Label: "MiniLM-L6 fp32"},
	"o2":           {RepoID: miniLMRepo, OnnxFile: "onnx/model_O2.onnx", Dimensions: 384, Label: "MiniLM-L6 fp32-O2"},
	"o3":           {RepoID: miniLMRepo, OnnxFile: "onnx/model_O3.onnx", Dimensions: 384, Label: "MiniLM-L6 fp32-O3"},
	"o4":           {RepoID: miniLMRepo, OnnxFile: "onnx/model_O4.onnx", Dimensions: 384, Label: "MiniLM-L6 fp32-O4"},
	"qint8_arm64":  {RepoID: miniLMRepo, OnnxFile: "onnx/model_qint8_arm64.onnx", Dimensions: 384, Label: "MiniLM-L6 qint8-arm64"},
	"qint8_avx512": {RepoID: miniLMRepo, OnnxFile: "onnx/model_qint8_avx512.onnx", Dimensions: 384, Label: "MiniLM-L6 qint8-avx512"},
	"quint8_avx2":  {RepoID: miniLMRepo, OnnxFile: "onnx/model_quint8_avx2.onnx", Dimensions: 384, Label: "MiniLM-L6 quint8-avx2"},

	// General retrieval-tuned models — trained for search (not just
	// sentence similarity), published with ONNX exports, drop-in under
	// Hugot's pure-Go runtime.
	"bge_small": {RepoID: "BAAI/bge-small-en-v1.5", OnnxFile: "onnx/model.onnx", Dimensions: 384, Label: "BGE small-en-v1.5"},

	// Code-tuned models — hypothesis: trained on code, should beat
	// MiniLM on concept / multi-hop code queries.
	"jina_code": {RepoID: "jinaai/jina-embeddings-v2-base-code", OnnxFile: "onnx/model.onnx", Dimensions: 768, Label: "Jina v2 base-code"},

	// Not currently loadable under the pure-Go runtime — ships as
	// safetensors only on HuggingFace. Kept here so `gortex eval
	// embedders` surfaces a clear error rather than silently falling
	// back. To use: export locally via optimum-cli + `--local-model`
	// (follow-up), or route via APIProvider against an OpenAI-compat
	// embeddings endpoint.
	"bge_code": {RepoID: "BAAI/bge-code-v1", OnnxFile: "onnx/model.onnx", Dimensions: 1024, Label: "BAAI bge-code-v1 (no ONNX)"},
}

// LookupHugotVariant returns the variant spec for a short name, or false.
func LookupHugotVariant(name string) (HugotVariant, bool) {
	v, ok := hugotVariants[name]
	return v, ok
}

// KnownHugotVariants returns sorted short names of every known variant.
func KnownHugotVariants() []string {
	names := make([]string, 0, len(hugotVariants))
	for k := range hugotVariants {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func newHugotProvider() (Provider, error) {
	spec, _ := LookupHugotVariant(DefaultHugotVariant)
	return newHugotProviderWithSpec(spec)
}

func newHugotProviderWithSpec(spec HugotVariant) (Provider, error) {
	if spec.RepoID == "" || spec.OnnxFile == "" {
		return nil, fmt.Errorf("invalid variant spec: RepoID=%q OnnxFile=%q", spec.RepoID, spec.OnnxFile)
	}
	session, err := hugot.NewGoSession(context.Background())
	if err != nil {
		return nil, fmt.Errorf("hugot session: %w", err)
	}

	modelPath, err := ensureHugotModel(spec)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("hugot model: %w", err)
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath:    modelPath,
		Name:         "gortex-embeddings",
		OnnxFilename: filepath.Base(spec.OnnxFile),
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}

	pipeline, err := session.NewPipeline(config)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("hugot pipeline: %w", err)
	}

	dims := spec.Dimensions
	if dims == 0 {
		dims = 384 // conservative fallback
	}

	// Cap inputs at the model's positional budget before they reach the
	// pipeline. The pure-Go tokenizer path does not honour
	// max_position_embeddings, so an over-long text would otherwise abort the
	// whole vector index with a tensor shape mismatch. A missing config.json can
	// use the conservative fallback budget, but an exact tokenizer is mandatory:
	// without it there is no safe sequence-length check.
	truncator, terr := newTokenTruncator(modelPath)
	if truncator == nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("hugot token truncator: %w", terr)
	}
	if terr != nil {
		fmt.Fprintf(os.Stderr, "[gortex embedding] %v\n", terr)
	}

	return &HugotProvider{
		session:   session,
		pipeline:  pipeline,
		dims:      dims,
		truncator: truncator,
	}, nil
}

func (p *HugotProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("hugot returned no embeddings")
	}
	return vecs[0], nil
}

func (p *HugotProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	texts = p.truncator.TruncateAll(texts)

	output, err := p.pipeline.RunPipeline(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("hugot run: %w", err)
	}
	if err := validateBatch("hugot", texts, output.Embeddings, p.dims); err != nil {
		return nil, err
	}
	return output.Embeddings, nil
}

func (p *HugotProvider) Dimensions() int { return p.dims }

func (p *HugotProvider) Close() error {
	if p.session != nil {
		return p.session.Destroy()
	}
	return nil
}

// embeddingOfflineEnv, when set to a truthy value (1/true), disables
// network downloads of embedding models. ensureHugotModel then returns
// an error for any not-yet-cached model so NewLocalProvider falls back
// to the static provider. Useful for air-gapped hosts, sandboxes, and
// tests that must not touch the network.
const embeddingOfflineEnv = "GORTEX_EMBEDDING_OFFLINE"

// embeddingDownloadsDisabled reports whether model downloads are turned
// off via embeddingOfflineEnv. An unset or falsey value keeps the
// default behaviour (download on first use).
func embeddingDownloadsDisabled() bool {
	on, _ := strconv.ParseBool(os.Getenv(embeddingOfflineEnv))
	return on
}

// ensureHugotModel downloads the variant's HuggingFace repo if needed
// and returns the on-disk path Hugot will load from. The ONNX file is
// specified via DownloadOptions because most repos ship multiple
// variants and the downloader refuses to guess. The cache layout
// mirrors Hugot's own convention: `<cache>/<org>_<model-name>/…`.
func ensureHugotModel(spec HugotVariant) (string, error) {
	dest := platform.ModelsDir()
	modelDir := filepath.Join(dest, hfCacheDirName(spec.RepoID))

	tokenizerReady := false
	if _, err := os.Stat(filepath.Join(modelDir, "tokenizer.json")); err == nil {
		tokenizerReady = true
	}
	// The downloader flattens `<subdir>/<file>.onnx` to `<file>.onnx`
	// in modelDir, so check both the nested path and the basename.
	variantReady := false
	if _, err := os.Stat(filepath.Join(modelDir, spec.OnnxFile)); err == nil {
		variantReady = true
	} else if _, err := os.Stat(filepath.Join(modelDir, filepath.Base(spec.OnnxFile))); err == nil {
		variantReady = true
	}
	if tokenizerReady && variantReady {
		return modelDir, nil
	}

	// Offline guard: when downloads are disabled, refuse to fetch a
	// missing model instead of blocking on (or racing inside) the
	// network downloader. NewLocalProvider treats this error as "backend
	// unavailable" and falls through to the static provider.
	if embeddingDownloadsDisabled() {
		return "", fmt.Errorf("embedding model %s (%s) not cached and downloads are disabled via %s", spec.RepoID, spec.OnnxFile, embeddingOfflineEnv)
	}

	opts := hugot.NewDownloadOptions()
	opts.OnnxFilePath = spec.OnnxFile
	path, err := downloadHugotModel(spec, dest, opts)
	if err != nil {
		return "", fmt.Errorf("download %s (%s): %w", spec.RepoID, spec.OnnxFile, err)
	}
	return path, nil
}

// embeddingDownloadBudgetEnv overrides how long a first-use model
// download may take before it is abandoned. Accepts any Go duration
// ("90s", "5m"); an unset or unparseable value keeps the default.
const embeddingDownloadBudgetEnv = "GORTEX_EMBEDDING_DOWNLOAD_TIMEOUT"

// defaultDownloadBudget bounds a cold model fetch. MiniLM is ~90MB and
// lands in well under a minute on a working link, so ten minutes is
// generous for a slow one and still short enough that a wedged
// connection degrades to the static provider instead of stalling the
// process that asked for an embedder.
const defaultDownloadBudget = 10 * time.Minute

// downloadBudget resolves the wall-clock budget for one model download.
func downloadBudget() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(embeddingDownloadBudgetEnv)); err == nil && d > 0 {
		return d
	}
	return defaultDownloadBudget
}

// hugotDownloadModel is the upstream fetch, indirected so the watchdog
// below can be tested against a stalled download without a network.
var hugotDownloadModel = hugot.DownloadModel

// downloadHugotModel fetches a model repo under a wall-clock budget and
// gives up rather than blocking forever on a stalled transfer. The
// returned error wraps context.DeadlineExceeded when the budget ran out,
// so a caller can tell "the network stalled" from "the model is broken".
//
// The budget has to be enforced out here because nothing inside the
// download honours one. hugot.DownloadModel takes a context but only
// passes it to the post-download file copy: the fetch itself goes
// through hub.Repo.DownloadFiles, which starts from
// context.Background(). Under that, go-huggingface builds its own
// http.Client per request with no Timeout and exposes no hook to supply
// one, and its worker pool waits on a plain sync.Cond that no context
// can interrupt — which is exactly how a stalled transfer parks the
// caller in Semaphore.Acquire indefinitely. So the fetch runs on its own
// goroutine and this call abandons it when the budget is spent; the
// orphan finishes into a buffered channel and exits whenever the
// connection finally breaks, leaving only a discarded ".part" file the
// next download overwrites.
func downloadHugotModel(spec HugotVariant, dest string, opts hugot.DownloadOptions) (string, error) {
	budget := downloadBudget()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	type outcome struct {
		path string
		err  error
	}
	// Read the indirection here, not inside the goroutine: the goroutine
	// can outlive this call by design, and a test that restores the
	// original fetch would otherwise be writing the variable while the
	// abandoned goroutine still reads it.
	fetch := hugotDownloadModel
	done := make(chan outcome, 1)
	go func() {
		path, err := fetch(ctx, spec.RepoID, dest, opts)
		done <- outcome{path: path, err: err}
	}()

	select {
	case got := <-done:
		return got.path, got.err
	case <-ctx.Done():
		return "", fmt.Errorf("no progress within %s: %w", budget, ctx.Err())
	}
}

// hfCacheDirName turns a HuggingFace repo path ("org/name") into the
// directory name Hugot writes to ("org_name"). Hugot already does this
// internally when downloading; we mirror the convention so the
// tokenizer/variant existence checks find the cached files.
func hfCacheDirName(repoID string) string {
	// Use path separator normalisation rather than a raw replace so
	// nested subdirs in custom repos don't corrupt the cache layout.
	return filepath.Clean(filepath.FromSlash(
		replaceAllSlashes(repoID, "_"),
	))
}

// replaceAllSlashes is a tiny helper to avoid pulling in strings just
// for one call site.
func replaceAllSlashes(s, repl string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '/' {
			out = append(out, repl...)
		} else {
			out = append(out, string(r)...)
		}
	}
	return string(out)
}
