package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
)

// New compact records use tabs between fields and Go-quoted strings. The
// established return_usage line keeps its existing space-separated contract.
// Census headers describe emitted semantic rows, not graph freshness. Only an
// actual lagging scope gets the existing tier prefix and bounded path witness.
func compactConsumerPendingFiles(b *strings.Builder, label relationFreshness) {
	if label.state == "" {
		return
	}
	for _, path := range label.files[:min(maxLabelledPendingFiles, len(label.files))] {
		fmt.Fprintf(b, "pending_file %q\n", path)
	}
}

func compactVerifyConsumer(result *analysis.VerifyResult, label relationFreshness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "verify_v1\t%t\t%d\t%d\t%d\t%d\n", result.Clean, result.CheckedCallers, result.CheckedImpls, len(result.Violations), len(result.Errors))
	for _, row := range result.Violations {
		fmt.Fprintf(&b, "violation\t%q\t%q\t%q\t%q\t%d\t%q\n", row.Kind, row.SymbolID, row.Name, row.FilePath, row.Line, row.Description)
	}
	for _, message := range result.Errors {
		fmt.Fprintf(&b, "error\t%q\n", message)
	}
	// Keep the existing return-usage information, including its full label
	// distribution, without making it a source of caller/violation counts.
	for _, row := range result.ReturnUsage {
		fmt.Fprintf(&b, "return_usage %s call_sites:%d", row.SymbolID, row.CallSites)
		keys := make([]string, 0, len(row.Counts))
		for key := range row.Counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, " %s:%d", key, row.Counts[key])
		}
		if row.Unclassified > 0 {
			fmt.Fprintf(&b, " unclassified:%d", row.Unclassified)
		}
		b.WriteByte('\n')
	}
	compactConsumerPendingFiles(&b, label)
	return b.String()
}

func compactDeadCodeConsumer(dead, unknown []analysis.DeadCodeEntry, total, totalUnknown int, truncated bool, note string, label relationFreshness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dead_code_v1\t%d\t%d\t%t\n", total, totalUnknown, truncated)
	for _, group := range []struct {
		kind string
		rows []analysis.DeadCodeEntry
	}{{"dead", dead}, {"pending_unknown", unknown}} {
		for _, row := range group.rows {
			fmt.Fprintf(&b, "%s\t%q\t%q\t%q\t%q\t%d\n", group.kind, row.Kind, row.ID, row.Name, row.FilePath, row.Line)
		}
	}
	if note != "" {
		fmt.Fprintf(&b, "note\t%q\n", note)
	}
	compactConsumerPendingFiles(&b, label)
	return b.String()
}

// Only dry-run rename uses this wire. Mutating rename retains its existing
// receipt-rich JSON/TOON response, including per-file commit/publication data.
func compactRenameConsumer(payload map[string]any, edits []renameEdit, label relationFreshness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rename_v1\t%q\t%q\t%t\t%d\n", payload["old_name"], payload["new_name"], payload["dry_run"], len(edits))
	for _, row := range edits {
		fmt.Fprintf(&b, "edit\t%q\t%d\t%q\t%q\t%q\t%q\n", row.File, row.Line, row.OldText, row.NewText, row.Confidence, row.Reason)
	}
	fmt.Fprintf(&b, "rename_status\t%q\t%t\n", payload["status"], payload["written"])
	compactConsumerPendingFiles(&b, label)
	return b.String()
}

func compactDetectConsumer(payload map[string]any, files []string, symbols []analysis.ChangedSymbol, label relationFreshness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "detect_v1\t%q\t%d\t%d\n", payload["scope"], len(files), len(symbols))
	for _, file := range files {
		fmt.Fprintf(&b, "changed_file\t%q\n", file)
	}
	for _, row := range symbols {
		fmt.Fprintf(&b, "changed_symbol\t%q\t%q\t%q\t%q\t%d\n", row.Kind, row.ID, row.Name, row.FilePath, row.Line)
	}
	// Checkout-unavailable answers have no relation label. Preserve that
	// explicit incompleteness instead of treating their empty symbol set as fresh.
	if complete, exists := payload["complete"].(bool); exists {
		fmt.Fprintf(&b, "complete\t%t\n", complete)
	}
	for _, key := range []string{"risk", "summary", "note", "graph_status", "detail", "repo", "repo_root"} {
		if value, exists := payload[key].(string); exists {
			fmt.Fprintf(&b, "%s\t%q\n", key, value)
		}
	}
	compactConsumerPendingFiles(&b, label)
	return b.String()
}
