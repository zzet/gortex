package analyzer

// PURPOSE — pure computation core for the synthesizers analyzer: groups
// every synthesized edge by the framework-dispatch pass that produced it,
// returning a structured result the MCP layer and CLI can both consume
// without duplicating logic.
// RATIONALE — extracted from the MCP handler so the aggregation is
// independently testable and reusable across surfaces (MCP, CLI, etc.).
// KEYWORDS — synthesizers, framework-dispatch, pure, calculation

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// metaSynthesizedByKey is the Edge.Meta key stamped by the synthesizer engine.
	metaSynthesizedByKey = "synthesized_by"
	// metaProvenanceKey is the Edge.Meta key carrying provenance info.
	metaProvenanceKey = "provenance"
	// maxSamples is the maximum number of edge samples kept per synthesizer group.
	maxSamples = 5
)

// SynthesizerSample is one example edge from a synthesizer group.
type SynthesizerSample struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Via  string `json:"via,omitempty"`
}

// SynthesizerRow is one synthesizer group in the result.
// JSON field names are intentionally kept stable — callers rely on them.
type SynthesizerRow struct {
	Name       string              `json:"synthesizer"`
	Provenance string              `json:"provenance"`
	Edges      int                 `json:"edges"`
	ByKind     map[string]int      `json:"by_kind"`
	Samples    []SynthesizerSample `json:"samples,omitempty"`
}

// SynthesizersResult is the return type of AnalyzeSynthesizers.
// JSON field names mirror the MCP output shape exactly.
type SynthesizersResult struct {
	Synthesizers []*SynthesizerRow `json:"synthesizers"`
	TotalEdges   int               `json:"total_edges"`
}

// SynthesizersOption configures AnalyzeSynthesizers.
type SynthesizersOption func(*synthConfig)

type synthConfig struct {
	nameFilter string
	repoScope  map[string]bool
}

// WithSynthesizerNameFilter restricts the result to a single synthesizer name.
func WithSynthesizerNameFilter(name string) SynthesizersOption {
	return func(c *synthConfig) { c.nameFilter = name }
}

// WithSynthesizerRepoScope restricts the result to synthesized edges
// whose source node lives in one of the given repository prefixes — the
// repos of the caller's workspace. A nil/empty set disables the clamp
// (the whole-index default). Used to keep `analyze synthesizers` inside
// the session workspace boundary even though it is not repo-narrowed in
// v1.
func WithSynthesizerRepoScope(repos map[string]bool) SynthesizersOption {
	return func(c *synthConfig) { c.repoScope = repos }
}

// AnalyzeSynthesizers groups every synthesized edge in the graph by the
// synthesizer that produced it and returns a sorted, structured result.
// It is a pure Calculation: no side effects, no I/O.
func AnalyzeSynthesizers(g graph.Reader, opts ...SynthesizersOption) SynthesizersResult {
	cfg := &synthConfig{}
	for _, o := range opts {
		o(cfg)
	}

	rows := map[string]*SynthesizerRow{}
	for _, e := range g.AllEdges() {
		if e == nil || e.Meta == nil {
			continue
		}
		by, _ := e.Meta[metaSynthesizedByKey].(string)
		if by == "" {
			continue
		}
		if cfg.nameFilter != "" && by != cfg.nameFilter {
			continue
		}
		// Workspace clamp: drop edges whose source node is outside the
		// caller's workspace repos so the count, by-kind tally, and
		// samples never span sibling workspaces.
		if len(cfg.repoScope) > 0 && !cfg.repoScope[graph.RepoPrefixOfID(e.From)] {
			continue
		}
		row, ok := rows[by]
		if !ok {
			prov, _ := e.Meta[metaProvenanceKey].(string)
			row = &SynthesizerRow{Name: by, Provenance: prov, ByKind: map[string]int{}}
			rows[by] = row
		}
		row.Edges++
		row.ByKind[string(e.Kind)]++
		if len(row.Samples) < maxSamples {
			via, _ := e.Meta["via"].(string)
			row.Samples = append(row.Samples, SynthesizerSample{
				From: e.From,
				To:   e.To,
				Kind: string(e.Kind),
				Via:  via,
			})
		}
	}

	out := make([]*SynthesizerRow, 0, len(rows))
	total := 0
	for _, r := range rows {
		total += r.Edges
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Edges != out[j].Edges {
			return out[i].Edges > out[j].Edges
		}
		return out[i].Name < out[j].Name
	})

	return SynthesizersResult{Synthesizers: out, TotalEdges: total}
}
