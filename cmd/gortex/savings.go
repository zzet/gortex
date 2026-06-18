package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/tokens"
	"github.com/zzet/gortex/internal/tui"
)

var (
	savingsModel    string
	savingsJSON     bool
	savingsReset    bool
	savingsCacheDir string
	savingsVerbose  bool
	savingsBarCells int
	savingsUTC      bool
)

// defaultHeadlineModel is the model the headline "Cost avoided" figure is
// priced against when the caller doesn't pass --model. Set to the model
// gortex itself runs on so the dollar figure reflects real usage.
const defaultHeadlineModel = "claude-opus-4-8"

var savingsCmd = &cobra.Command{
	Use:   "savings",
	Short: "Show token-savings dashboard (Today / Last 7 days / All time) + cost avoided",
	Long: `Renders a bar-chart dashboard of token-savings buckets persisted across
sessions: Today, Last 7 days, and All time. Each bucket shows a 16-cell
saved/total bar, percentage saved, raw token counts, and the USD value of
the tokens avoided (priced against popular models).

Savings accumulate every time a source-reading MCP tool — read_file,
get_file_summary, get_editing_context, get_symbol_source, batch_symbols,
smart_context — returns a summary, symbol, or compressed view that stands
in for a full-file read. The ledger lives in the machine-global sidecar
database (~/.gortex/sidecar.sqlite); flat-file ledgers from older
releases (savings.json / savings.jsonl under the cache dir) are imported
once and renamed *.bak.

Percentages are computed over ALL recorded source fetches — including
uncompressed read_file calls that returned the whole file and saved
nothing — so the bars reflect how the agent actually reads, not just
the best cases; per-tool rates live in --verbose.

Override the ledger location with --cache-dir (reads that directory's
sidecar.sqlite; the one-shot legacy import runs only against the
default location), override pricing by exporting
GORTEX_MODEL_PRICING_JSON, and pass --verbose for a per-tool breakdown
inside each bucket.`,
	RunE: runSavings,
}

func init() {
	savingsCmd.Flags().StringVar(&savingsModel, "model", "", "highlight one model in USD output (default: show all)")
	savingsCmd.Flags().BoolVar(&savingsJSON, "json", false, "emit machine-readable JSON instead of the dashboard")
	savingsCmd.Flags().BoolVar(&savingsReset, "reset", false, "wipe cumulative totals + event history and exit")
	savingsCmd.Flags().StringVar(&savingsCacheDir, "cache-dir", "", "override the ledger directory (its sidecar.sqlite holds the savings ledger)")
	savingsCmd.Flags().BoolVarP(&savingsVerbose, "verbose", "v", false, "include per-tool breakdown for each bucket")
	savingsCmd.Flags().IntVar(&savingsBarCells, "bar-width", 16, "number of cells in each bar (default 16, matching semble)")
	savingsCmd.Flags().BoolVar(&savingsUTC, "utc", false, "bucket Today by UTC calendar (default: local time)")
	rootCmd.AddCommand(savingsCmd)
}

func runSavings(_ *cobra.Command, _ []string) error {
	dbPath := savings.DefaultDBPath()
	if savingsCacheDir != "" {
		dbPath = persistence.DefaultSidecarPath(savingsCacheDir)
	}

	store, err := savings.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open savings ledger: %w", err)
	}
	// Legacy flat-file import runs only against the default locations:
	// pointing the dashboard at a directory with --cache-dir must never
	// rename files there as a side effect of looking.
	if savingsCacheDir == "" {
		if ierr := store.ImportLegacy(savings.DefaultPath()); ierr != nil {
			fmt.Fprintf(os.Stderr, "[gortex savings] legacy import failed: %v\n", ierr)
		}
	}

	if savingsReset {
		if err := store.Reset(); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[gortex savings] reset cumulative totals + event history at %s\n", dbPath)
		return nil
	}

	snap, serr := store.Snapshot()
	if serr != nil {
		// Surface it: an unreadable ledger must not masquerade as a
		// fresh install's "nothing recorded yet" empty state.
		fmt.Fprintf(os.Stderr, "[gortex savings] totals read failed: %v\n", serr)
	}

	loc := time.Local
	if savingsUTC {
		loc = time.UTC
	}
	now := time.Now()
	events, err := store.EventsSince(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		// Don't fail the whole command on event read errors — fall back
		// to a dashboard with empty Today/7d buckets.
		fmt.Fprintf(os.Stderr, "[gortex savings] event history read failed: %v\n", err)
		events = nil
	}
	allPerTool, err := store.ToolTotals(time.Time{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex savings] per-tool aggregate failed: %v\n", err)
	}
	buckets := savings.BuildDashboard(events, snap.Totals, allPerTool, now, loc)

	// Per-model / per-client breakdowns (all time). Only populated for
	// calls whose model the host surfaced (model) or whose client the
	// initialize handshake captured (client).
	modelTotals, err := store.ModelTotals(time.Time{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex savings] per-model aggregate failed: %v\n", err)
	}
	clientTotals, err := store.ClientTotals(time.Time{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex savings] per-client aggregate failed: %v\n", err)
	}

	if savingsJSON {
		return emitSavingsJSON(snap, buckets, modelTotals, clientTotals, dbPath)
	}
	emitSavingsDashboard(snap, buckets, modelTotals, clientTotals, dbPath)
	return nil
}

func emitSavingsJSON(snap savings.File, buckets []savings.Bucket, modelTotals, clientTotals []savings.DimTotal, path string) error {
	bucketJSON := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		entry := map[string]any{
			"label":            b.Label,
			"tokens_saved":     b.Totals.TokensSaved,
			"tokens_returned":  b.Totals.TokensReturned,
			"calls_counted":    b.Totals.CallsCounted,
			"percent_saved":    savings.SavingsPercent(b.Totals),
			"cost_avoided_usd": savings.CostAvoidedAll(b.Totals.TokensSaved),
		}
		if len(b.PerTool) > 0 {
			tools := make([]map[string]any, 0, len(b.PerTool))
			for _, t := range b.PerTool {
				tools = append(tools, map[string]any{
					"tool":            t.Tool,
					"tokens_saved":    t.TokensSaved,
					"tokens_returned": t.TokensReturned,
					"calls_counted":   t.CallsCounted,
				})
			}
			entry["per_tool"] = tools
		}
		bucketJSON = append(bucketJSON, entry)
	}

	out := map[string]any{
		"path":             path,
		"tokens_saved":     snap.Totals.TokensSaved,
		"tokens_returned":  snap.Totals.TokensReturned,
		"calls_counted":    snap.Totals.CallsCounted,
		"cost_avoided_usd": savings.CostAvoidedAll(snap.Totals.TokensSaved),
		"buckets":          bucketJSON,
	}
	if !snap.FirstSeen.IsZero() {
		out["first_seen"] = snap.FirstSeen.Format(time.RFC3339)
	}
	if !snap.LastUpdated.IsZero() {
		out["last_updated"] = snap.LastUpdated.Format(time.RFC3339)
	}
	if len(snap.PerRepo) > 0 {
		out["per_repo"] = snap.PerRepo
	}
	if len(snap.PerLanguage) > 0 {
		out["per_language"] = snap.PerLanguage
	}
	// Per-model actual attribution: tokens_saved is rescaled into the
	// model's own tokenizer (cl100k retained as tokens_saved_cl100k) and
	// the cost is that model's real rate — not the all-models
	// counterfactual that cost_avoided_usd carries.
	if len(modelTotals) > 0 {
		models := make([]map[string]any, 0, len(modelTotals))
		for _, m := range modelTotals {
			adj := tokens.ScaleFromCL100K(m.Name, m.TokensSaved)
			models = append(models, map[string]any{
				"model":               m.Name,
				"calls_counted":       m.CallsCounted,
				"tokens_saved_cl100k": m.TokensSaved,
				"tokens_saved":        adj,
				"cost_avoided_usd":    savings.CostAvoided(adj, m.Name),
			})
		}
		out["per_model"] = models
	}
	if len(clientTotals) > 0 {
		clients := make([]map[string]any, 0, len(clientTotals))
		for _, c := range clientTotals {
			clients = append(clients, map[string]any{
				"client":        c.Name,
				"calls_counted": c.CallsCounted,
				"tokens_saved":  c.TokensSaved,
			})
		}
		out["per_client"] = clients
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// emitSavingsDashboard renders the bar-chart dashboard. Layout mirrors
// semble's format_savings_report(): a header card with cumulative USD on
// top, then one row per bucket with bar / percentage / token counts / dollar
// amount, optionally followed by a per-tool breakdown in --verbose mode.
//
// On a TTY we wrap the header in a styled banner + stat-strip card; on a
// non-TTY (output piped into grep / a file) we preserve the bare text
// header so script parsers keep matching.
func emitSavingsDashboard(snap savings.File, buckets []savings.Bucket, modelTotals, clientTotals []savings.DimTotal, path string) {
	tty := progress.IsTTY(os.Stdout) && !noProgress

	if tty {
		banner := tui.Banner{
			Title:    "gortex savings",
			Subtitle: "Token-savings dashboard — Today / Last 7 days / All time.",
		}.Render()
		fmt.Println()
		fmt.Println(banner)
		fmt.Println()
		fmt.Println("  " + progress.Row("store", path, 14))
		if !snap.FirstSeen.IsZero() {
			fmt.Println("  " + progress.Row("tracking since", snap.FirstSeen.Format("2006-01-02 15:04"), 14))
		}
		if !snap.LastUpdated.IsZero() {
			fmt.Println("  " + progress.Row("last updated", snap.LastUpdated.Format("2006-01-02 15:04"), 14))
		}
	} else {
		fmt.Println("Gortex Token Savings")
		fmt.Println("====================")
		fmt.Printf("Store:          %s\n", path)
		if !snap.FirstSeen.IsZero() {
			fmt.Printf("Tracking since: %s\n", snap.FirstSeen.Format("2006-01-02 15:04"))
		}
		if !snap.LastUpdated.IsZero() {
			fmt.Printf("Last updated:   %s\n", snap.LastUpdated.Format("2006-01-02 15:04"))
		}
	}

	costs := savings.CostAvoidedAll(snap.Totals.TokensSaved)
	_, headlineModel := pickHeadlineCost(costs, savingsModel)
	fmt.Println()
	if snap.Totals.CallsCounted == 0 {
		hint := "savings record when the agent reads code through gortex (read_file, get_file_summary, get_editing_context, get_symbol_source, smart_context, …)"
		if tty {
			fmt.Println("  " + progress.StyleHint.Render("◌  no source-reading tool calls recorded yet"))
			fmt.Println("     " + progress.Caption(hint))
			fmt.Println()
		} else {
			fmt.Println("No source-reading tool calls recorded yet.")
			fmt.Println("Savings record when the agent reads code through gortex (read_file, get_file_summary, get_editing_context, get_symbol_source, smart_context, ...).")
		}
		return
	}

	// Per-bucket bar rows.
	fmt.Println()
	labelWidth := 0
	for _, b := range buckets {
		if l := len(b.Label); l > labelWidth {
			labelWidth = l
		}
	}
	for _, b := range buckets {
		renderBucketRow(b, labelWidth, headlineModel)
	}

	// Per-model attribution — only models the host actually surfaced via the
	// hook model hint. Tokens are rescaled into each model's own tokenizer
	// and priced at that model's real rate.
	fmt.Println()
	if tty {
		fmt.Println("  " + progress.Heading("cost avoided per model (all time)"))
	} else {
		fmt.Println("Cost avoided per model (all time):")
	}
	if len(modelTotals) == 0 {
		if tty {
			fmt.Println("     " + progress.Caption("(none — no model hint captured yet)"))
		} else {
			fmt.Println("  (none — no model hint captured yet)")
		}
	} else {
		nameWidth := 0
		for _, m := range modelTotals {
			if l := len(m.Name); l > nameWidth {
				nameWidth = l
			}
		}
		for _, m := range modelTotals {
			adj := tokens.ScaleFromCL100K(m.Name, m.TokensSaved)
			if tty {
				cost := savings.CostAvoided(adj, m.Name)
				stats := []string{
					progress.Stat(formatUSD(cost), "", progress.StatGood),
					progress.Stat(humanInt(m.CallsCounted), "calls", progress.StatNeutral),
					progress.Stat(humanInt(adj), "tokens saved", progress.StatNeutral),
				}
				fmt.Printf("     %-*s  %s\n", nameWidth, m.Name, progress.StatStrip(stats...))
			} else {
				fmt.Printf("  %-*s %s   (%s calls · %s tokens saved)\n",
					nameWidth, m.Name, formatUSD(savings.CostAvoided(adj, m.Name)),
					humanInt(m.CallsCounted), humanInt(adj))
			}
		}
	}

	// Per-client breakdown — which agent app drove the savings (claude-code
	// / cursor / …), captured from the MCP initialize handshake.
	if len(clientTotals) > 0 {
		fmt.Println()
		if tty {
			fmt.Println("  " + progress.Heading("savings by client (all time)"))
		} else {
			fmt.Println("Savings by client (all time):")
		}
		nameWidth := 0
		for _, c := range clientTotals {
			if l := len(c.Name); l > nameWidth {
				nameWidth = l
			}
		}
		for _, c := range clientTotals {
			if tty {
				cost := savings.CostAvoided(c.TokensSaved, headlineModel)
				stats := []string{
					progress.Stat(formatUSD(cost), "", progress.StatGood),
					progress.Stat(humanInt(c.CallsCounted), "calls", progress.StatNeutral),
					progress.Stat(humanInt(c.TokensSaved), "tokens saved", progress.StatNeutral),
				}
				fmt.Printf("     %-*s  %s\n", nameWidth, c.Name, progress.StatStrip(stats...))
			} else {
				fmt.Printf("  %-*s  %s calls · %s tokens saved\n",
					nameWidth, c.Name, humanInt(c.CallsCounted), humanInt(c.TokensSaved))
			}
		}
	}

	// --verbose: per-tool breakdown inside each bucket. Hidden by
	// default because the headline 3-row dashboard is what the user
	// asked for; this is the deep-dive.
	if savingsVerbose {
		for _, b := range buckets {
			if len(b.PerTool) == 0 {
				continue
			}
			fmt.Println()
			fmt.Printf("%s by tool:\n", b.Label)
			toolWidth := 0
			for _, t := range b.PerTool {
				if l := len(t.Tool); l > toolWidth {
					toolWidth = l
				}
			}
			for _, t := range b.PerTool {
				pct := savings.SavingsPercent(t.Totals)
				bar := savings.BarString(pct, savingsBarCells)
				fmt.Printf("  %-*s %s  %5.1f%%  saved %s (%d calls)\n",
					toolWidth, t.Tool, bar, pct,
					humanInt(t.TokensSaved), t.CallsCounted)
			}
		}
	}

	// Per-repo and per-language rollups (carried over from the original
	// dashboard — agents still find these useful at the bottom).
	printBucket("Per-repo totals (all time)", snap.PerRepo, tty, headlineModel)
	printBucket("Per-language totals (all time)", snap.PerLanguage, tty, headlineModel)
}

func renderBucketRow(b savings.Bucket, labelWidth int, headlineModel string) {
	pct := savings.SavingsPercent(b.Totals)
	bar := savings.BarString(pct, savingsBarCells)
	cost := savings.CostAvoided(b.Totals.TokensSaved, headlineModel)
	costStr := formatUSD(cost)
	if cost == 0 && b.Totals.TokensSaved == 0 {
		costStr = "  $0.00"
	}
	fmt.Printf("%-*s %s  %5.1f%%  saved %s / %s tokens  %s\n",
		labelWidth, b.Label, bar, pct,
		humanInt(b.Totals.TokensSaved),
		humanInt(b.Totals.TokensSaved+b.Totals.TokensReturned),
		costStr,
	)
}

// pickHeadlineCost selects which model's cost is featured at the top of
// the dashboard. Honors --model when set, otherwise picks the highest
// dollar amount so the headline is the most-impressive credible figure.
// Returns the cost and the model name actually used.
func pickHeadlineCost(costs map[string]float64, preferred string) (float64, string) {
	if preferred != "" {
		// Use the user's pick even when the model isn't in our table —
		// CostAvoided handles fuzzy matching and returns 0 for misses.
		return costs[preferred], preferred
	}
	if len(costs) == 0 {
		return 0, "n/a"
	}
	// Default the headline to the model gortex itself runs on, so the figure
	// reflects real usage rather than the priciest entry in the table. Falls
	// through to the highest-value pick when that model isn't priced.
	if v, ok := costs[defaultHeadlineModel]; ok {
		return v, defaultHeadlineModel
	}
	var bestName string
	var bestVal float64
	// Stable selection: highest value, then lexicographic name.
	names := make([]string, 0, len(costs))
	for n := range costs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if costs[n] > bestVal {
			bestVal = costs[n]
			bestName = n
		}
	}
	if bestName == "" {
		// All zeros — return any deterministic name so the header is
		// still meaningful ("$0.00 (claude-opus-4)").
		bestName = names[0]
	}
	return bestVal, bestName
}

// formatUSD picks a precision that's useful at the bucket scale: full
// cents for amounts >= $1, four decimals for the long-tail small ones so
// fresh installs don't show "$0.00" everywhere.
func formatUSD(usd float64) string {
	if usd >= 1.0 {
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.4f", usd)
}

// printBucket renders a sorted breakdown of name → Totals. Skipped when
// the bucket is empty so older savings files (with no per_language data)
// don't produce a noisy "Per-language totals: (none)" line.
func printBucket(title string, bucket map[string]*savings.Totals, tty bool, headlineModel string) {
	if len(bucket) == 0 {
		return
	}
	keys := make([]string, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		// Heaviest first — agents care about where the savings come from.
		if a, b := bucket[keys[i]].TokensSaved, bucket[keys[j]].TokensSaved; a != b {
			return a > b
		}
		return keys[i] < keys[j]
	})
	nameWidth := 0
	for _, k := range keys {
		if l := len(k); l > nameWidth {
			nameWidth = l
		}
	}
	fmt.Println()
	if tty {
		fmt.Println("  " + progress.Heading(title))
	} else {
		fmt.Println(title + ":")
	}
	for _, k := range keys {
		t := bucket[k]
		if tty {
			cost := savings.CostAvoided(t.TokensSaved, headlineModel)
			stats := []string{
				progress.Stat(formatUSD(cost), "", progress.StatGood),
				progress.Stat(humanInt(t.TokensSaved), "tokens saved", progress.StatNeutral),
				progress.Stat(fmt.Sprintf("%d", t.CallsCounted), "calls", progress.StatNeutral),
			}
			fmt.Printf("     %-*s  %s\n", nameWidth, k, progress.StatStrip(stats...))
		} else {
			fmt.Printf("  %-24s tokens_saved=%-12s calls=%d\n",
				k, humanInt(t.TokensSaved), t.CallsCounted)
		}
	}
}

// humanInt renders a number with thousands separators so big totals are readable.
func humanInt(n int64) string {
	if n < 0 {
		return "-" + humanInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	// Insert commas every 3 digits from the right.
	var sb strings.Builder
	sb.Grow(len(s) + len(s)/3)
	prefix := len(s) % 3
	if prefix > 0 {
		sb.WriteString(s[:prefix])
		if len(s) > prefix {
			sb.WriteByte(',')
		}
	}
	for i := prefix; i < len(s); i += 3 {
		sb.WriteString(s[i : i+3])
		if i+3 < len(s) {
			sb.WriteByte(',')
		}
	}
	return sb.String()
}
