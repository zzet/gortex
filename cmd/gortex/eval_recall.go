package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/eval/recall"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/tokens"
)

var (
	evalRecallFixture       string
	evalRecallIndex         string
	evalRecallFormat        string
	evalRecallOut           string
	evalRecallEmbeddings    bool
	evalRecallEmbedder      string
	evalRecallEmbeddingsURL string
	evalRecallModel         string
	evalRecallRankers       string
	evalRecallSkipRipgrep   bool
	evalRecallVerbose       bool
	evalRecallJudge         string
)

var evalRecallCmd = &cobra.Command{
	Use:   "recall",
	Short: "Recall@1/5/20 + latency + tokens-returned per ranker against a fixture",
	Long: `Runs each query in the fixture through every retrieval ranker and reports
any-hit recall@1/5/20, mean reciprocal rank, p50/p95 latency, and tokens-
returned. Cases are tiered (exact / concept / multi_hop) so per-tier recall
is broken out separately.

Rankers:
  bm25      — lexical-only: the store-native symbol FTS queried
              through Engine.SearchSymbols (the search_symbols path)
  semantic  — vector-only (requires --embeddings)
  rrf       — native FTS + vector with adaptive alpha (historical key;
              requires --embeddings)
  winnow    — graph-aware constraint chain (MCP winnow_symbols scorer)
  ripgrep   — rg --files-with-matches baseline ("retrieval floor")

By default every available ranker runs; narrow with --rankers bm25,rrf.

Without --embeddings, semantic and RRF both report SKIPPED. Use
--embeddings to enable the built-in static (GloVe) provider, or
--embeddings-url to
point at an OpenAI-compatible API (e.g. Ollama).`,
	RunE: runEvalRecall,
}

func init() {
	evalRecallCmd.Flags().StringVar(&evalRecallFixture, "fixture", "bench/fixtures/retrieval.yaml", "fixture YAML path")
	evalRecallCmd.Flags().StringVar(&evalRecallIndex, "index", ".", "repository path to index before running queries")
	evalRecallCmd.Flags().StringVar(&evalRecallFormat, "format", "markdown", "output format: markdown or json")
	evalRecallCmd.Flags().StringVar(&evalRecallOut, "out", "", "output file (default: stdout)")
	evalRecallCmd.Flags().BoolVar(&evalRecallEmbeddings, "embeddings", false, "enable local embedder for semantic/RRF rankers (auto-picks Hugot MiniLM-L6-v2 by default)")
	evalRecallCmd.Flags().StringVar(&evalRecallEmbedder, "embedder", "auto", "embedder to use when --embeddings is set: auto|hugot|static")
	evalRecallCmd.Flags().StringVar(&evalRecallEmbeddingsURL, "embeddings-url", "", "OpenAI-compatible embeddings API URL (overrides --embedder)")
	evalRecallCmd.Flags().StringVar(&evalRecallModel, "embeddings-model", "", "embeddings model name (for --embeddings-url)")
	evalRecallCmd.Flags().StringVar(&evalRecallRankers, "rankers", "", "comma-separated subset of rankers to run (default: all)")
	evalRecallCmd.Flags().BoolVar(&evalRecallSkipRipgrep, "skip-ripgrep", false, "skip the rg baseline row")
	evalRecallCmd.Flags().BoolVar(&evalRecallVerbose, "verbose", false, "print per-case miss diagnostics to stderr")
	evalRecallCmd.Flags().StringVar(&evalRecallJudge, "judge", "", "LLM judge model (e.g. claude-haiku-4-5) — rescues misses where a top-K entry plausibly answers the query. Requires ANTHROPIC_API_KEY.")
	evalCmd.AddCommand(evalRecallCmd)
}

func runEvalRecall(_ *cobra.Command, _ []string) error {
	fixtureBytes, err := os.ReadFile(evalRecallFixture)
	if err != nil {
		return fmt.Errorf("reading fixture: %w", err)
	}
	var fixture recall.Fixture
	if err := yaml.Unmarshal(fixtureBytes, &fixture); err != nil {
		return fmt.Errorf("parsing fixture: %w", err)
	}
	if len(fixture.Cases) == 0 {
		return fmt.Errorf("fixture %s has no cases", evalRecallFixture)
	}
	if fixture.Name == "" {
		fixture.Name = filepath.Base(evalRecallFixture)
	}

	absIndex, err := filepath.Abs(evalRecallIndex)
	if err != nil {
		return fmt.Errorf("resolving index path: %w", err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build indexer, optionally with an embedder so vector search has data.
	// A real stderr logger surfaces embedding failures —
	// silently swallowing them via zap.NewNop() let the recall harness
	// fall back to BM25 with no visible signal that the hybrid backend
	// failed to build.
	g, closeStore, err := newEvalStore("recall")
	if err != nil {
		return fmt.Errorf("opening eval store: %w", err)
	}
	defer closeStore()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, newRecallLogger())

	embedder := chooseEmbedder()
	if embedder != nil {
		idx.SetEmbedder(embedder)
	}

	fmt.Fprintf(os.Stderr, "[gortex eval recall] indexing %s...\n", absIndex)
	res, err := idx.Index(absIndex)
	if err != nil {
		return fmt.Errorf("indexing: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[gortex eval recall] indexed %d files, %d nodes in %dms\n",
		res.FileCount, res.NodeCount, res.DurationMs)

	// Validate fixture: every expected ID must exist in the graph,
	// otherwise the case is unreachable and skews recall downwards
	// regardless of ranker quality. Warnings go to stderr so they
	// surface during curation but don't pollute the report.
	if missing := validateFixture(fixture, g); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "[gortex eval recall] WARNING: %d expected IDs missing from graph — fixture bug:\n", len(missing))
		for _, m := range missing {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", m.caseID, m.id)
		}
	}

	selected := parseSelectedRankers(evalRecallRankers)

	// Peel the Swappable wrapper so we can see the real backend. When
	// embeddings are on, the indexer builds a HybridBackend internally;
	// its TextBackend() is the pure lexical side — an adapter over the
	// store's own symbol FTS, not a harness-built index — and the whole
	// HybridBackend is what RRF queries.
	inner := idx.Search()
	if sw, ok := inner.(*search.Swappable); ok {
		var release func()
		inner, release = sw.AcquireBackend()
		defer release()
	}
	hybrid, _ := inner.(*search.HybridBackend)
	var textBackend search.Backend
	switch {
	case hybrid != nil:
		textBackend = hybrid.TextBackend()
	default:
		textBackend = inner
	}

	fmt.Fprintf(os.Stderr, "[gortex eval recall] inner backend: %T\n", inner)
	fmt.Fprintf(os.Stderr, "[gortex eval recall] text backend: %T count=%d\n", textBackend, textBackend.Count())
	if hybrid != nil {
		vec := hybrid.VectorIndex()
		vecCount := 0
		if vec != nil {
			vecCount = vec.Count()
		}
		fmt.Fprintf(os.Stderr, "[gortex eval recall] vector index: count=%d\n", vecCount)
	}

	// The engine-backed lexical row mirrors the MCP search_symbols call
	// path: the store-native FTS ranking plus the substring fallback for
	// camelCase-only queries, run through Engine.SearchSymbols against
	// the store's own full-text index — the same backend the daemon
	// serves, so the row reports what real callers hit. The "bm25" row
	// key is kept only so historical bench artifacts stay joinable; no
	// in-process BM25 index is involved. Critically: the engine is pointed
	// at the PURE text backend even when --embeddings is on — otherwise
	// the "bm25" row would silently run through HybridBackend.Search (RRF
	// fusion with vector results) and the measurement would no longer
	// isolate the lexical contribution.
	engForBM25 := query.NewEngine(g)
	engForBM25.SetSearch(textBackend)
	engineSearch := func(q string, limit int) []string {
		nodes := engForBM25.SearchSymbols(q, limit)
		out := make([]string, len(nodes))
		for i, n := range nodes {
			out[i] = n.ID
		}
		return out
	}

	rankers := []recall.Ranker{}
	if selected["bm25"] {
		rankers = append(rankers, recall.EngineRanker("bm25", engineSearch))
	}
	if selected["semantic"] {
		if hybrid != nil && embedder != nil && hybrid.VectorIndex() != nil && hybrid.VectorIndex().Count() > 0 {
			rankers = append(rankers, recall.SemanticRanker("semantic", hybrid.VectorIndex(), embedder))
		} else {
			rankers = append(rankers, skippedRanker("semantic", "no embedder (pass --embeddings)"))
		}
	}
	if selected["rrf"] {
		if hybrid != nil {
			rankers = append(rankers, recall.RRFRanker("rrf", hybrid))
		} else {
			rankers = append(rankers, skippedRanker("rrf", "no hybrid backend (pass --embeddings)"))
		}
	}

	// Graph-traversal ranker for DI-dependent / call-chain queries. Uses
	// the same engine pointed at the pure text backend as Winnow above —
	// text side doesn't matter for traversal, but keeping the engine
	// identical makes it trivial to add text-plus-graph hybrid rankers
	// later without re-plumbing.
	if selected["graph"] {
		graphEng := query.NewEngine(g)
		graphEng.SetSearch(textBackend)
		rankers = append(rankers, recall.GraphRanker("graph", &graphTraverser{eng: graphEng}))
	}

	// Winnow: needs a Server to run the graph-aware scorer. The Server's
	// own engine is pointed at the PURE text backend so winnow's
	// internal text-match step measures graph+lexical behaviour, not
	// graph+hybrid — otherwise weak static-embedding semantics would
	// drag winnow's exact-tier recall down artificially, hiding the
	// real graph-axis contribution.
	if selected["winnow"] {
		eng := query.NewEngine(g)
		eng.SetSearch(textBackend)
		srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), cfg.Guards.Rules, gortexmcp.MultiRepoOptions{EditAckMode: cfg.Edit.Ack})
		srv.SetArchitecture(cfg.Architecture)
		srv.SetEventRules(cfg.Events.Rules)
		srv.SetArtifacts(cfg.Artifacts)
		srv.SetNamedQueries(cfg.Queries)
		srv.RunAnalysis()
		provider := func(q string, extras map[string]any, limit int) []string {
			return srv.WinnowForEval(q, extras, limit)
		}
		// Pass through per-case constraints via a query→extras lookup
		// built from the fixture.
		extrasByQuery := make(map[string]map[string]any, len(fixture.Cases))
		for _, c := range fixture.Cases {
			if c.WinnowConstraints != nil {
				extrasByQuery[c.Query] = c.WinnowConstraints
			}
		}
		caseExtras := func(q string) map[string]any { return extrasByQuery[q] }
		rankers = append(rankers, recall.WinnowRanker("winnow", provider, caseExtras))
	}

	// Ripgrep baseline over file paths. Uses a filename-adapted copy of
	// the fixture so any-hit recall compares file paths, not symbol IDs.
	var rgReport *recall.Report
	if selected["ripgrep"] && !evalRecallSkipRipgrep {
		if _, err := exec.LookPath("rg"); err == nil {
			rgRanker := recall.RipgrepRanker("ripgrep", absIndex)
			rgFixture := recall.Fixture{
				Name:  fixture.Name + " (file-level)",
				Cases: recall.AdaptCasesForFileRanker(fixture.Cases),
			}
			r := recall.Run(rgFixture, []recall.Ranker{rgRanker}, tokenCounter())
			rgReport = &r
		} else {
			rankers = append(rankers, skippedRanker("ripgrep", "rg not on PATH"))
		}
	}

	report := recall.Run(fixture, rankers, tokenCounter())
	report.GortexRev = readGitRev(absIndex)
	stampSkipReasons(&report)

	if evalRecallVerbose {
		printMissDiagnostics(&report)
	}

	if evalRecallJudge != "" {
		judge := recall.NewJudge(evalRecallJudge)
		if judge == nil {
			fmt.Fprintln(os.Stderr, "[gortex eval recall] --judge set but ANTHROPIC_API_KEY is empty — skipping")
		} else {
			rescued, errs := recall.ApplyJudge(&report, judge)
			fmt.Fprintf(os.Stderr, "[gortex eval recall] judge: rescued %d misses across all rankers\n", rescued)
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "[gortex eval recall] judge error: %v\n", e)
			}
		}
	}

	// Merge rg result into the main report so the markdown table has
	// a single coherent row-set. File-level recall is still honest
	// per-query — methodology note flags the difference.
	if rgReport != nil && len(rgReport.Rankers) > 0 {
		report.Rankers = append(report.Rankers, rgReport.Rankers[0])
	}

	var out []byte
	switch evalRecallFormat {
	case "markdown", "md":
		out = []byte(recall.Markdown(report))
	case "json":
		out, err = json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		out = append(out, '\n')
	default:
		return fmt.Errorf("unknown format: %s (want markdown or json)", evalRecallFormat)
	}

	if evalRecallOut == "" {
		_, _ = os.Stdout.Write(out)
		return nil
	}
	if err := os.WriteFile(evalRecallOut, out, 0o644); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[gortex eval recall] wrote %s\n", evalRecallOut)
	return nil
}

// newRecallLogger returns a stderr-only zap logger gated at warn level
// so the recall harness stays quiet on the success path but surfaces
// embedding failures and other indexer warnings — silently swallowing
// them via zap.NewNop() lets a missed embedding chunk demote the
// backend to BM25 with no visible signal.
func newRecallLogger() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.WarnLevel)
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	logger, err := cfg.Build()
	if err != nil {
		return zap.NewNop()
	}
	return logger
}

// chooseEmbedder honours --embeddings-url > --embedder > --embeddings > off.
// Default with --embeddings is the best local provider (Hugot MiniLM-L6-v2
// auto-downloads to ~/.gortex/models/ on first use). Users can force
// static GloVe with --embedder static.
func chooseEmbedder() embedding.Provider {
	if evalRecallEmbeddingsURL != "" {
		fmt.Fprintf(os.Stderr, "[gortex eval recall] embedder: API (%s)\n", evalRecallEmbeddingsURL)
		return embedding.NewAPIProvider(evalRecallEmbeddingsURL, evalRecallModel)
	}
	if !evalRecallEmbeddings {
		return nil
	}
	switch evalRecallEmbedder {
	case "static":
		p, err := embedding.NewStaticProvider()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex eval recall] static embedder failed: %v\n", err)
			return nil
		}
		fmt.Fprintln(os.Stderr, "[gortex eval recall] embedder: static (GloVe)")
		return p
	case "hugot":
		p, err := embedding.NewHugotProvider()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex eval recall] hugot failed: %v (semantic/RRF will be skipped)\n", err)
			return nil
		}
		fmt.Fprintf(os.Stderr, "[gortex eval recall] embedder: hugot (dim=%d)\n", p.Dimensions())
		return p
	case "auto", "":
		p, err := embedding.NewLocalProvider()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex eval recall] local embedder failed: %v (semantic/RRF will be skipped)\n", err)
			return nil
		}
		fmt.Fprintf(os.Stderr, "[gortex eval recall] embedder: local (%T, dim=%d)\n", p, p.Dimensions())
		return p
	default:
		fmt.Fprintf(os.Stderr, "[gortex eval recall] unknown --embedder %q\n", evalRecallEmbedder)
		return nil
	}
}

// parseSelectedRankers turns a comma-separated flag into a set. Default
// (empty) = all known rankers on.
func parseSelectedRankers(csv string) map[string]bool {
	all := []string{"bm25", "semantic", "rrf", "winnow", "graph", "ripgrep"}
	if csv == "" {
		out := make(map[string]bool, len(all))
		for _, r := range all {
			out[r] = true
		}
		return out
	}
	out := make(map[string]bool)
	for _, r := range strings.Split(csv, ",") {
		out[strings.TrimSpace(r)] = true
	}
	return out
}

// skippedRanker registers a placeholder so the report shows the row
// with a skip reason instead of silently dropping it.
func skippedRanker(name, reason string) recall.Ranker {
	r := recall.Ranker{
		Name: name,
		Search: func(_ string, _ int) []string {
			return nil
		},
	}
	// The Run loop itself doesn't know about Skipped — we set it
	// post-hoc on the Report.Rankers entries that had Search returning
	// nothing, but only when the reason is non-empty. Easier path: the
	// caller marks the RankerResult. Since recall.Run can't see inside,
	// we record skip reason on a side channel applied after Run.
	evalSkipReasons[name] = reason
	return r
}

// evalSkipReasons carries the skip reasons keyed by ranker name so we
// can stamp them onto the report after Run. Shared across the single
// goroutine call site; not reached concurrently.
var evalSkipReasons = map[string]string{}

// tokenCounter returns a real tiktoken-based counter (internal/tokens
// already handles its own fallback if the encoder fails to init).
func tokenCounter() recall.TokenCounter {
	return func(s string) int { return tokens.Count(s) }
}

// stampSkipReasons copies skip reasons recorded on the eval-local side
// channel onto the matching report rows.
func stampSkipReasons(report *recall.Report) {
	for i, r := range report.Rankers {
		if reason, ok := evalSkipReasons[r.Name]; ok {
			report.Rankers[i].Skipped = reason
		}
	}
}

// printMissDiagnostics dumps per-ranker miss lists so fixture curators
// can see which cases are genuinely hard vs plainly mis-labelled.
func printMissDiagnostics(report *recall.Report) {
	for _, r := range report.Rankers {
		if r.Skipped != "" || len(r.Misses) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n[gortex eval recall] %s misses (%d):\n", r.Name, len(r.Misses))
		for _, m := range r.Misses {
			top3 := m.Top
			if len(top3) > 3 {
				top3 = top3[:3]
			}
			rank := "MISS"
			if m.Rank > 0 {
				rank = fmt.Sprintf("rank=%d", m.Rank)
			}
			fmt.Fprintf(os.Stderr, "  %-38s  %-6s  %q  want=%v  got=%v\n", m.CaseID, rank, m.Query, m.Expected, top3)
		}
	}
}

// fixtureMiss records an expected ID that doesn't exist in the graph.
type fixtureMiss struct{ caseID, id string }

// validateFixture reports expected IDs that aren't present in the graph
// so fixture curation errors surface before they depress recall numbers.
func validateFixture(f recall.Fixture, g graph.Store) []fixtureMiss {
	var out []fixtureMiss
	for _, c := range f.Cases {
		for _, id := range c.Expected {
			if g.GetNode(id) == nil {
				out = append(out, fixtureMiss{caseID: c.ID, id: id})
			}
		}
	}
	return out
}

// readGitRev returns the short commit SHA of the repo at root, empty
// string if git is missing or the path isn't a repo.
func readGitRev(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// gitcmd injects `-C root`; Output trims trailing whitespace, matching the
	// prior strings.TrimSpace on cmd.Output().
	out, err := gitcmd.Output(ctx, root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// graphTraverser adapts the query engine to recall.GraphProvider. Each
// "tool" prefix (callers, call_chain, usages, dependents) maps to a
// specific engine method with a sane default depth for the fixture's
// query shape.
type graphTraverser struct {
	eng *query.Engine
}

func (g *graphTraverser) Traverse(tool, id string, limit int) []string {
	if g == nil || g.eng == nil {
		return nil
	}
	opts := query.QueryOptions{Depth: 3, Limit: limit * 2}
	var sg *query.SubGraph
	switch tool {
	case "callers":
		sg = g.eng.GetCallers(id, opts)
	case "call_chain":
		sg = g.eng.GetCallChain(id, opts)
	case "usages":
		sg = g.eng.FindUsages(id)
	case "dependents":
		sg = g.eng.GetDependents(id, opts)
	default:
		return nil
	}
	if sg == nil {
		return nil
	}
	out := make([]string, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n.ID == id {
			continue // the subject itself is not a caller/callee/user
		}
		out = append(out, n.ID)
		if len(out) >= limit {
			break
		}
	}
	return out
}
