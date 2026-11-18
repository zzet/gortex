package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/indexer"
)

// These slots reserve names for the receipt-recovery follow-ups. Empty slots
// are deliberately not advertised: none of these recovery mechanisms exists.
type freshnessRecoveryHintSlots struct {
	operatorReconcile  string
	automaticReconcile string // Include an ETA only when one is known.
	retryExhausted     string
}

func mutationFreshnessRecoveryHint() string {
	reserved := freshnessRecoveryHintSlots{}
	hints := []string{
		"a later successful edit of the same path",
		"workspace_admin operation=reindex options.paths=[P] (reindex_repository) naming exactly that single file",
		reserved.operatorReconcile,
		reserved.automaticReconcile,
		reserved.retryExhausted,
	}
	available := hints[:0]
	for _, hint := range hints {
		if hint != "" {
			available = append(available, hint)
		}
	}
	return "terminally failed generations do not recover by waiting — recovery: " + strings.Join(available, " or ")
}

func (s *Server) mutationFreshnessReceiptHint(ctx context.Context, id string) string {
	hint := fmt.Sprintf(" (query: change.receipt options.receipt=%s)", id)
	if record, ok := s.mutationCommits.byFreshnessReceipt(id); ok && mutationCommitInScope(ctx, record) {
		// Commit ids and caller keys never change after registration. Bound the
		// quoted key in this diagnostic; the receipt query returns the full key.
		hint += " commit=" + record.id
		if record.key != "" {
			hint += fmt.Sprintf(" mutation_id=%.128q", record.key)
			if utf8.RuneCountInString(record.key) > 128 {
				hint += "(truncated)"
			}
		}
	}
	if record, ok := indexer.DefaultPublicationPhases().Lookup(id); ok {
		hint += " " + mutationFreshnessPhaseSummary(record.Snapshot())
	}
	return hint
}

// Show the latest three phases, not an unbounded timeline. The full record
// remains available through the receipt query. Phase names are fixed by the
// recorder vocabulary; cap them as well to keep this annotation bounded even
// if a future producer supplies an unexpected name.
func mutationFreshnessPhaseSummary(snapshot indexer.PublicationPhaseSnapshot) string {
	const maxPhases = 3
	start := max(0, len(snapshot.Phases)-maxPhases)
	parts := make([]string, 0, maxPhases+1)
	if start > 0 {
		parts = append(parts, fmt.Sprintf("...+%d", start))
	}
	for _, phase := range snapshot.Phases[start:] {
		parts = append(parts, fmt.Sprintf("%.40q@%.1fms", phase.Phase, phase.OffsetMS))
	}
	return "publication_phases=[" + strings.Join(parts, ",") + "]"
}
