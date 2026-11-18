package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// PotionProvider embeds text with a Model2Vec static embedding model —
// a WordPiece tokenizer plus a single [vocab × dims] embedding matrix.
// Inference is: tokenize → gather the token rows → mean-pool →
// L2-normalise (the model config's `normalize: true` convention, matching
// the reference implementations). No transformer, no positional limit —
// a 50-candidate rerank batch embeds in well under a millisecond.
//
// The bundled model is minishlab/potion-code-16M-v2 (MIT): 256-dim
// vectors over a ~63.5k-token code-mined vocabulary, distilled from a
// code-retrieval teacher. It replaces the older averaged word-vector
// channel for the rerank's semantic-cosine signal.
type PotionProvider struct {
	tok *wordPieceTokenizer
	// mat is the row-major [vocab][dims] embedding matrix kept in the
	// half-precision form it ships in. Expanding it to float32 at load
	// doubled the resident cost — a measured 61.98 MiB against the 32 MB
	// file — for a matrix that is read at most maxPotionTokens rows per
	// call. Converting per element inside the gather loop trades a few
	// thousand conversions per embed for half the daemon's steady-state
	// footprint.
	mat  []uint16
	dims int
}

// Potion model pin. The revision and checksums identify the exact
// artifacts the loader accepts; a mismatched download is discarded.
const (
	potionModelName = "potion-code-16M-v2"
	potionRevision  = "d3daf3e31f36d78f75913030b8bdf4a505d5b833"
	potionBaseURL   = "https://huggingface.co/minishlab/" + potionModelName + "/resolve/" + potionRevision

	potionWeightsFile   = "model.safetensors"
	potionTokenizerFile = "tokenizer.json"

	potionWeightsSHA256   = "75cf7a6c2171b230ad19b1e7d8e0b1aee86da5a02af8e7cacedd9921d227623c"
	potionTokenizerSHA256 = "107bbdcbad4bff1d299b7a4c3a2fb17c52890688b7dd0e4c9deab79d3c4f3d45"
)

// maxPotionTokens caps how many tokens of one text feed the mean-pool.
// The model is static (no sequence limit), but the rerank only ever
// embeds short name+signature+doc fragments; the cap bounds the cost of
// a pathological input.
const maxPotionTokens = 512

// NewPotionProviderFromDir loads the model from a directory holding
// model.safetensors and tokenizer.json.
func NewPotionProviderFromDir(dir string) (*PotionProvider, error) {
	tok, err := loadWordPieceTokenizer(filepath.Join(dir, potionTokenizerFile))
	if err != nil {
		return nil, err
	}
	mat, dims, err := loadSafetensorsF16(filepath.Join(dir, potionWeightsFile))
	if err != nil {
		return nil, err
	}
	return &PotionProvider{tok: tok, mat: mat, dims: dims}, nil
}

func (p *PotionProvider) Dimensions() int { return p.dims }
func (p *PotionProvider) Close() error    { return nil }

func (p *PotionProvider) Embed(_ context.Context, text string) ([]float32, error) {
	return p.embed(text), nil
}

func (p *PotionProvider) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = p.embed(t)
	}
	return out, nil
}

func (p *PotionProvider) embed(text string) []float32 {
	ids := p.tok.Encode(text)
	if len(ids) > maxPotionTokens {
		ids = ids[:maxPotionTokens]
	}
	vec := make([]float32, p.dims)
	if len(ids) == 0 {
		return vec
	}
	rows := len(p.mat) / p.dims
	n := 0
	for _, id := range ids {
		if id < 0 || id >= rows {
			continue
		}
		row := p.mat[id*p.dims : (id+1)*p.dims]
		for i, v := range row {
			vec[i] += f16ToF32(v)
		}
		n++
	}
	if n == 0 {
		return vec
	}
	inv := 1.0 / float32(n)
	var norm float64
	for i := range vec {
		vec[i] *= inv
		norm += float64(vec[i]) * float64(vec[i])
	}
	// L2-normalise (model config `normalize: true`).
	if norm > 0 {
		s := float32(1.0 / math.Sqrt(norm))
		for i := range vec {
			vec[i] *= s
		}
	}
	return vec
}

// loadSafetensorsF16 reads a single-tensor safetensors file holding an
// F16 "embeddings" matrix and returns it row-major, still half-precision.
// The caller converts the handful of rows it touches; keeping the matrix
// packed halves what the process holds for the rest of its life.
func loadSafetensorsF16(path string) ([]uint16, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read weights: %w", err)
	}
	if len(raw) < 8 {
		return nil, 0, fmt.Errorf("weights file too short")
	}
	hlen := binary.LittleEndian.Uint64(raw[:8])
	if hlen == 0 || 8+hlen > uint64(len(raw)) {
		return nil, 0, fmt.Errorf("weights header out of range")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+hlen], &header); err != nil {
		return nil, 0, fmt.Errorf("parse weights header: %w", err)
	}
	var info struct {
		Dtype       string `json:"dtype"`
		Shape       []int  `json:"shape"`
		DataOffsets []int  `json:"data_offsets"`
	}
	entry, ok := header["embeddings"]
	if !ok {
		return nil, 0, fmt.Errorf("weights file has no 'embeddings' tensor")
	}
	if err := json.Unmarshal(entry, &info); err != nil {
		return nil, 0, fmt.Errorf("parse tensor info: %w", err)
	}
	if info.Dtype != "F16" {
		return nil, 0, fmt.Errorf("unsupported tensor dtype %q (want F16)", info.Dtype)
	}
	if len(info.Shape) != 2 || len(info.DataOffsets) != 2 {
		return nil, 0, fmt.Errorf("unexpected tensor shape/offsets")
	}
	rows, dims := info.Shape[0], info.Shape[1]
	start := 8 + int(hlen) + info.DataOffsets[0]
	end := 8 + int(hlen) + info.DataOffsets[1]
	if start < 0 || end > len(raw) || end-start != rows*dims*2 {
		return nil, 0, fmt.Errorf("tensor data out of range")
	}
	data := raw[start:end]
	mat := make([]uint16, rows*dims)
	for i := range mat {
		mat[i] = binary.LittleEndian.Uint16(data[i*2 : i*2+2])
	}
	return mat, dims, nil
}

// f16ToF32 converts an IEEE-754 half-precision value to float32.
func f16ToF32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	man := uint32(h) & 0x3ff
	switch exp {
	case 0:
		if man == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal half: renormalise into a normal float32.
		e := uint32(113) // 127 - 14
		for man&0x400 == 0 {
			man <<= 1
			e--
		}
		man &= 0x3ff
		return math.Float32frombits(sign | e<<23 | man<<13)
	case 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | man<<13)
	default:
		return math.Float32frombits(sign | (exp+112)<<23 | man<<13)
	}
}

// resolvePotionDir returns the first directory that holds both model
// files, probing in order:
//
//  1. $GORTEX_POTION_DIR — explicit override.
//  2. <executable dir>/models/<model> — the release/bench sidecar. A
//     packaged install ships the model next to the binary, so it is
//     fully offline after install.
//  3. <gortex home>/models/<model> — the per-user cache the first-use
//     download fills.
//
// Returns "" when none has the files.
func resolvePotionDir() string {
	var candidates []string
	if d := os.Getenv("GORTEX_POTION_DIR"); d != "" {
		candidates = append(candidates, d)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "models", potionModelName))
	}
	candidates = append(candidates, filepath.Join(platform.ModelsDir(), potionModelName))
	for _, dir := range candidates {
		if hasPotionFiles(dir) {
			return dir
		}
	}
	return ""
}

func hasPotionFiles(dir string) bool {
	for _, f := range []string{potionWeightsFile, potionTokenizerFile} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.IsDir() {
			return false
		}
	}
	return true
}

// downloadPotion fetches the pinned model revision into the per-user
// models dir, verifying each file's SHA-256 before moving it into
// place. Returns the directory on success. Disabled entirely when
// GORTEX_POTION_DOWNLOAD=0 (offline installs ship the sidecar instead).
func downloadPotion() (string, error) {
	if v := strings.TrimSpace(os.Getenv("GORTEX_POTION_DOWNLOAD")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		return "", fmt.Errorf("model download disabled by GORTEX_POTION_DOWNLOAD")
	}
	// Test binaries never reach the network: a `go test` run that
	// exercises a search path must stay hermetic and fall back to the
	// baked vectors instead of pulling 32MB mid-suite.
	if strings.HasSuffix(os.Args[0], ".test") || strings.HasSuffix(os.Args[0], ".test.exe") {
		return "", fmt.Errorf("model download disabled in test binaries")
	}
	dir := filepath.Join(platform.ModelsDir(), potionModelName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 120 * time.Second}
	files := []struct{ name, sum string }{
		{potionWeightsFile, potionWeightsSHA256},
		{potionTokenizerFile, potionTokenizerSHA256},
	}
	for _, f := range files {
		dst := filepath.Join(dir, f.name)
		if st, err := os.Stat(dst); err == nil && !st.IsDir() {
			if ok, _ := fileSHA256Matches(dst, f.sum); ok {
				continue
			}
		}
		if err := downloadVerified(client, potionBaseURL+"/"+f.name, dst, f.sum); err != nil {
			return "", fmt.Errorf("fetch %s: %w", f.name, err)
		}
	}
	return dir, nil
}

// downloadVerified streams url into dst.partial, verifies the SHA-256,
// and renames into place atomically.
func downloadVerified(client *http.Client, url, dst, wantSHA string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, cpErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	closeErr := out.Close()
	if cpErr != nil {
		os.Remove(tmp)
		return cpErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch: got %s want %s", got, wantSHA)
	}
	return os.Rename(tmp, dst)
}

func fileSHA256Matches(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}
