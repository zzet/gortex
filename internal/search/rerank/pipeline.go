// Package rerank implements the I13 11-signal scoring refresh — a
// pluggable rerank pipeline that combines BM25, semantic similarity,
// graph-topology, version-history, similarity-clustering, signature-
// match, recency, and feedback signals into one ranked output.
//
// Signals each return a normalised contribution in [0, 1]; the Pipeline
// multiplies by a per-signal weight and sums the contributions to
// produce the final score. Per-signal contributions ride on the
// Candidate so callers can surface them in debug / winnow output.
package rerank

import (
	"maps"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Candidate is one symbol under consideration plus the rank data and
// score breakdown the rerank pass attaches to it.
type Candidate struct {
	Node *graph.Node

	// TextRank is the 0-based text-search rank. -1 means the candidate
	// did not appear in the text-search result list (e.g. a substring
	// fallback hit, or a candidate added by another retrieval channel).
	TextRank int
	// VectorRank is the 0-based vector-search rank. -1 means absent.
	VectorRank int

	// Score is the final weighted sum produced by Pipeline.Rerank.
	Score float64
	// Signals carries the per-signal raw contribution in [0,1]
	// (unweighted) keyed by Signal.Name(). Populated by Rerank.
	Signals map[string]float64
}

// Signal is one named scoring function. Contribute returns a raw
// value in [0, 1]. The pipeline scales it by the signal's weight.
// Signals must be pure functions over the Candidate and Context —
// no hidden state. They may call back into Context for shared data.
type Signal interface {
	Name() string
	Contribute(query string, c *Candidate, ctx *Context) float64
}

// Pipeline runs a fixed set of signals against a batch of candidates,
// applies per-signal weights, and returns the batch sorted by score
// descending.
type Pipeline struct {
	signals []Signal
	weights map[string]float64
}

// New constructs a Pipeline. weights is keyed by Signal.Name(); any
// signal missing from the map gets weight 0 (effectively disabled).
// Pass DefaultWeights() to start from the tuned baseline.
func New(signals []Signal, weights map[string]float64) *Pipeline {
	w := make(map[string]float64, len(weights))
	maps.Copy(w, weights)
	return &Pipeline{signals: signals, weights: w}
}

// NewDefault is shorthand for New(DefaultSignals(), DefaultWeights()).
func NewDefault() *Pipeline { return New(DefaultSignals(), DefaultWeights()) }

// Weights returns a copy of the current weight map.
func (p *Pipeline) Weights() map[string]float64 {
	out := make(map[string]float64, len(p.weights))
	maps.Copy(out, p.weights)
	return out
}

// SetWeight overrides one signal's weight at runtime.
func (p *Pipeline) SetWeight(name string, w float64) { p.weights[name] = w }

// Rerank scores each candidate against every signal, sorts the
// batch by descending weighted score, and returns it. The candidate
// slice is mutated in place (Score + Signals populated). When ctx is
// nil a zero Context is used and any signal that depends on session
// data contributes 0.
func (p *Pipeline) Rerank(query string, cands []*Candidate, ctx *Context) []*Candidate {
	if len(cands) == 0 {
		return cands
	}
	if ctx == nil {
		ctx = &Context{}
	}
	// Auto-detect the query class once per call when the caller has
	// not pinned one. The class scales the bm25 / semantic weights so
	// identifier and path queries lean on exact-token evidence while
	// natural-language queries give the semantic channel its full say.
	if ctx.QueryClass == QueryClassUnknown {
		ctx.QueryClass = ClassifyQuery(query)
	}
	// Skip prepare when the caller already invoked Context.Prepare
	// for per-phase timing on this exact slice — avoids paying the
	// batched edge fetch twice on the search hot path. Identity check
	// is intentional: any mutation that reallocates resets it.
	if !sameSliceHeader(ctx.preparedCands, cands) {
		ctx.prepare(cands)
	}

	observer := ctx.ObserveTiming
	var scoringStart time.Time
	if observer != nil {
		scoringStart = time.Now()
	}
	for _, c := range cands {
		if c.Signals == nil {
			c.Signals = make(map[string]float64, len(p.signals))
		}
		var total float64
		for _, sig := range p.signals {
			w, ok := p.weights[sig.Name()]
			if !ok || w == 0 {
				continue
			}
			raw := sig.Contribute(query, c, ctx)
			if raw < 0 {
				raw = 0
			}
			if raw > 1 {
				raw = 1
			}
			c.Signals[sig.Name()] = raw
			// The bm25↔semantic balance uses the continuous α lever
			// when the caller set Context.Alpha, else the discrete
			// per-class table. classMult is 1.0 for every other signal.
			var classMult float64
			if ctx.Alpha > 0 {
				classMult = continuousClassMultiplier(ctx.Alpha, sig.Name())
			} else {
				classMult = ClassWeightMultiplier(ctx.QueryClass, sig.Name())
			}
			// Prose profile rides on its own lever, composed
			// multiplicatively with the class / α multiplier so a docs
			// query keeps its query-shape blend while the structural
			// code-only signals are suppressed for the prose corpus.
			// 1.0 (no-op) for every code query.
			if ctx.ProseMode {
				classMult *= proseWeightMultiplier(sig.Name())
			}
			total += w * classMult * raw
		}
		// Multiplicative supporting-cast demotion, applied after the
		// additive signal sum: a test file that out-scores the real
		// implementation on shared vocabulary is pushed below it rather
		// than merely nudged. 1.0 (no-op) for every non-test candidate.
		c.Score = total * supportFileDemotion(c, ctx)
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		// Stable secondary keys keep the ordering deterministic
		// when two candidates tie on score.
		if cands[i].TextRank != cands[j].TextRank {
			if cands[i].TextRank < 0 {
				return false
			}
			if cands[j].TextRank < 0 {
				return true
			}
			return cands[i].TextRank < cands[j].TextRank
		}
		return cands[i].Node.ID < cands[j].Node.ID
	})
	// Bounded file diversification, concept queries only: an intent
	// query answered by one big file's symbols crowds every other file
	// out of the head. Each file keeps its two best rows untouched;
	// from the third on, a row is pushed down by a bounded positional
	// penalty so a fresh file's best candidate can enter the head.
	// Identifier / path queries are exempt — an exact-token query that
	// legitimately resolves to many rows of one file (overloads in one
	// implementation file) must keep its literal order.
	if ctx.QueryClass == QueryClassConcept {
		applyFileDiversity(cands)
	}
	if observer != nil {
		observer(Timing{Scoring: time.Since(scoringStart), ScoringCalls: 1})
	}
	return cands
}

// fileDiversityAllowed is how many rows one file keeps un-demoted at
// the head of the ranking; fileDiversityStep is the positional penalty
// each further same-file row accrues. Both tuned on the discovery
// evaluation corpus with one tier per repo held out: allowed=2 with
// step=3 lifted the held-out intent tier's file-hit without moving any
// exact-identifier metric, while step>=4 began trading away symbol-level
// hits for file coverage.
const (
	fileDiversityAllowed = 2
	fileDiversityStep    = 3
)

// applyFileDiversity re-orders a score-sorted candidate slice by
// effective rank: a candidate whose file already holds
// fileDiversityAllowed higher-ranked candidates is demoted by
// fileDiversityStep positions per extra same-file row above it
// (xQuAD-lite — bounded, not a full round-robin, so the strongest
// same-file rows keep their positions and symbol-level hits are
// preserved). In-place, stable for untouched rows.
func applyFileDiversity(cands []*Candidate) {
	if len(cands) < 3 {
		return
	}
	seen := make(map[string]int, len(cands))
	eff := make([]int, len(cands))
	demoted := false
	for i, c := range cands {
		eff[i] = i
		if c == nil || c.Node == nil || c.Node.FilePath == "" {
			continue
		}
		above := seen[c.Node.FilePath]
		seen[c.Node.FilePath] = above + 1
		if above >= fileDiversityAllowed {
			eff[i] = i + fileDiversityStep*(above-fileDiversityAllowed+1)
			demoted = true
		}
	}
	if !demoted {
		return
	}
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if eff[idx[a]] != eff[idx[b]] {
			return eff[idx[a]] < eff[idx[b]]
		}
		return idx[a] < idx[b]
	})
	out := make([]*Candidate, len(cands))
	for i, j := range idx {
		out[i] = cands[j]
	}
	copy(cands, out)
}

// sameSliceHeader reports whether a and b alias the same underlying
// candidate slice (same backing array, same length). Used by Rerank to
// detect "the caller already invoked Prepare on this exact slice" and
// skip the duplicate prepare pass.
func sameSliceHeader(a, b []*Candidate) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	return &a[0] == &b[0]
}

// DefaultSignals returns the canonical signal lineup in stable order.
// Callers wanting a subset should construct New() directly.
func DefaultSignals() []Signal {
	return []Signal{
		BM25Signal{},
		SemanticSignal{},
		SemanticCosineSignal{},
		FanInSignal{},
		HITSSignal{},
		FanOutSignal{},
		ChurnSignal{},
		CoChangeSignal{},
		CommunitySignal{},
		MinHashSignal{},
		APISignatureSignal{},
		TypeSignatureSignal{},
		RecencySignal{},
		FeedbackSignal{},
		FileCoherenceSignal{},
		PathPenaltySignal{},
		DefinitionBiasSignal{},
		SourceBiasSignal{},
		ProvenanceSignal{},
		ProximitySignal{},
		OverloadProminenceSignal{},
	}
}

// DefaultWeights returns tuned per-signal weights. BM25 and semantic
// dominate (they answer "is this even relevant?"); fan-in is the
// load-bearing structural tie-breaker — when text signals are silent
// (no query or a query that misses every BM25 doc), fan-in should
// still discriminate. Community and feedback weight in below fan-in
// so a high-fan-in symbol can't be unseated by mere topic-cluster
// presence. File-coherence captures multi-chunk evidence one signal
// down from fan-in; the path-penalty multiplier sits at 0.4 so test /
// example demotion is noticeable on ties but never crushes a strong
// hit; definition-bias is gated by IsSymbolQuery so its weight only
// applies on identifier queries where the boost is desired. Weights
// sum to ~6.0 so the final score sits in a human-readable range
// when every signal saturates.
func DefaultWeights() map[string]float64 {
	return map[string]float64{
		SignalBM25:           1.00,
		SignalSemantic:       0.80,
		SignalSemanticCosine: 2.00,
		SignalFanIn:          0.60,
		SignalHITS:           0.40,
		SignalFanOut:         0.20,
		SignalChurn:          0.30,
		SignalCoChange:       0.25,
		SignalCommunity:      0.30,
		SignalMinHash:        0.30,
		SignalAPISignature:   0.45,
		SignalTypeSignature:  0.45,
		SignalRecency:        0.30,
		SignalFeedback:       0.50,
		SignalFileCoherence:  0.30,
		SignalPathPenalty:    0.40,
		SignalDefinitionBias: 0.60,
		SignalSourceBias:     0.25,
		SignalProvenance:     0.15,
		SignalProximity:      0.45,
		SignalOverload:       0.35,
	}
}

// Canonical signal names. Use these constants when reading or writing
// weights from config so a typo is a compile error.
const (
	SignalBM25           = "bm25"
	SignalSemantic       = "semantic"
	SignalSemanticCosine = "semantic_cosine"
	SignalFanIn          = "fan_in"
	SignalHITS           = "hits"
	SignalFanOut         = "fan_out"
	SignalChurn          = "churn"
	SignalCoChange       = "co_change"
	SignalCommunity      = "community"
	SignalMinHash        = "minhash"
	SignalAPISignature   = "api_signature"
	SignalTypeSignature  = "type_signature"
	SignalRecency        = "recency"
	SignalFeedback       = "feedback"
	SignalFileCoherence  = "file_coherence"
	SignalPathPenalty    = "path_penalty"
	SignalDefinitionBias = "definition_bias"
	SignalSourceBias     = "source_bias"
	SignalProvenance     = "provenance"
	SignalProximity      = "proximity"
	SignalOverload       = "overload"
)

// AllSignalNames lists every canonical signal name. Useful for config
// validation and debug-block emission.
func AllSignalNames() []string {
	return []string{
		SignalBM25,
		SignalSemantic,
		SignalSemanticCosine,
		SignalFanIn,
		SignalHITS,
		SignalFanOut,
		SignalChurn,
		SignalCoChange,
		SignalCommunity,
		SignalMinHash,
		SignalAPISignature,
		SignalTypeSignature,
		SignalRecency,
		SignalFeedback,
		SignalFileCoherence,
		SignalPathPenalty,
		SignalDefinitionBias,
		SignalSourceBias,
		SignalProvenance,
		SignalProximity,
		SignalOverload,
	}
}
