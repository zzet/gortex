package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Only stable accepted facts use the batched path. Pending/deleted rows and
// policy conversions keep the original point admission, including write-window,
// generated-file and stuck-repair checks. Facts retain no receipt payload.
type contractCaptureReceiptFact struct {
	source   string
	excluded bool
	opaque   bool
}

func contractCaptureReceiptFacts(ctx context.Context, options ContractFollowupCaptureOptions, handles map[string][]*store_sqlite.Store, files map[string]ContractFollowupFile) (map[string]contractCaptureReceiptFact, error) {
	facts := make(map[string]contractCaptureReceiptFact)
	if options.selectedInputs == nil {
		return facts, nil
	}
	blocked := make(map[string]bool)
	byRepo := make(map[string][]string)
	for path, file := range files {
		byRepo[file.RepoPrefix] = append(byRepo[file.RepoPrefix], path)
	}
	for repo, paths := range byRepo {
		repoHandles := handles[repo]
		for i := len(repoHandles) - 1; i >= 0; i-- {
			handle := repoHandles[i]
			for _, witness := range options.selectedInputs.Witnesses {
				if witness.GenerationID != handle.ViewGeneration() || witness.State.RepoPrefix != repo {
					continue
				}
				var unresolved []string
				for _, path := range paths {
					if _, resolved := facts[path]; !resolved && !blocked[path] {
						unresolved = append(unresolved, path)
					}
				}
				if len(unresolved) == 0 {
					continue
				}
				actor := witness.State.CheckoutID
				found := 0
				err := handle.VisitContractBoundaryReceiptsContext(ctx, repo, actor, unresolved, func(row graph.ContractBoundaryReceipt) error {
					found++
					if row.Version != contractBoundaryReceiptVersion {
						return nil
					}
					if row.Accepted && contractReceiptIsFailureEnvelope(&row) {
						facts[row.FilePath] = contractCaptureReceiptFact{excluded: true}
						return nil
					}
					// Do not resolve below a row whose original admission must run.
					blocked[row.FilePath] = true
					if !row.Accepted || row.Deleted {
						return nil
					}
					prior, known, err := contractCorePriorReceipt(&row, true)
					if err != nil {
						return nil // original point path retains its error semantics
					}
					policy, opaque := "", false
					if known && prior != nil {
						policy = prior.Policy
					} else if markerPolicy, overflow := contractReceiptOverflowPolicy(&row); overflow {
						policy, opaque = markerPolicy, true
					}
					if policy != "" && policy == files[row.FilePath].Policy {
						facts[row.FilePath] = contractCaptureReceiptFact{source: row.SourceFingerprint, opaque: opaque}
					}
					return nil
				})
				if err != nil {
					return nil, err
				}
				if found < len(unresolved) {
					// Point misses validate this header even though capture does not
					// consume known-empty. Preserve corrupt/header read errors, once
					// per chunk instead of up to twice for every missing path.
					if _, err := handle.ContractBoundaryReceiptBaselineContext(ctx, repo, actor); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return facts, ctx.Err()
}
