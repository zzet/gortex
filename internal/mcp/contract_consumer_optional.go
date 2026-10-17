package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/review"
)

// bindOptionalContractConsumer runs only at the first actual registry, graph or
// shape read. It never waits for producer progress or scheduler capacity.
func (s *Server) bindOptionalContractConsumer(ctx context.Context, req mcp.CallToolRequest, status *contractConsumerStatus, binding *contractAnalysisContext) error {
	status.consulted = true
	if err := ctx.Err(); err != nil {
		return err
	}
	status.state, status.reason, status.fresh = graphview.StateIncomplete, "no_attachment", false
	status.retryAfter = time.Second
	missing := func(cause error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if status.allowServed {
			return nil
		}
		message := "exact contract analysis has no current attachment"
		if status.retryAfter > 0 {
			message += fmt.Sprintf("; retry after %s", status.retryAfter)
		}
		if cause != nil {
			message += ": " + cause.Error()
		}
		return graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, message)
	}
	if s.materializer == nil {
		return missing(nil)
	}
	// Repo is a response selector, not a narrower dependency authority.
	cohortReq := req
	args := make(map[string]any, len(req.GetArguments()))
	for key, value := range req.GetArguments() {
		if key != "repo" {
			args[key] = value
		}
	}
	cohortReq.Params.Arguments = args
	repos, err := s.contractConsumerRepos(ctx, cohortReq)
	if err != nil {
		return missing(err)
	}
	view := requestViewFromContext(ctx)
	var selected *graphview.RepoView
	checkout := ""
	if view != nil {
		selected = view.materialized
		if selected != nil && view.rider != nil {
			checkout = view.rider.CheckoutID
		}
	}
	epoch := s.contractAnalysisRuntime.InputEpoch()
	opened, pending, openErr := s.openContractConsumerBinding(ctx, selected, checkout, repos)
	adopt := func(opened *contractAnalysisContext) {
		// Transfer only resource ownership into the existing deferred placeholder;
		// no mutex/Once is copied, and no local-only context replacement is used.
		binding.views, binding.releaseCohort = opened.views, opened.releaseCohort
		binding.served = status.allowServed
		binding.recovered = opened.recovered
		status.receiptsUnknown = opened.recovered
		opened.views, opened.releaseCohort = nil, nil
	}
	if openErr == nil {
		adopt(opened)
		status.state, status.reason, status.fresh = graphview.StateComplete, "", true
		status.retryAfter = 0
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	actor := indexer.NewContractActor(checkout, repos)
	// Schedule owns the prepared source on every return, including queue-full.
	// Only one lazy initialization exists, so nested consumers cannot reschedule.
	if pending != nil {
		source, sourceErr := indexer.PrepareContractCaptureSource(selected)
		if sourceErr == nil {
			_, scheduleErr := s.contractAnalysisRuntime.Schedule(ctx, indexer.ContractScheduleRequest{Actor: actor, Key: pending.key, Inputs: pending.inputs, Source: source, Epoch: epoch})
			var full *indexer.ContractQueueFullError
			if errors.As(scheduleErr, &full) {
				status.retryAfter = full.RetryAfter
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if status.allowServed && s.contractAnalysisRuntime.AcquireLastPublishedCohort != nil {
		previous, _, previousErr := s.openContractPredecessorBinding(ctx, selected, checkout, repos, actor)
		if previousErr == nil {
			adopt(previous)
			status.state, status.reason, status.fresh = graphview.StateBuilding, "contract_analysis_pending", false
			return nil
		}
	}
	return missing(openErr)
}

func optionalContractWarning(status *contractConsumerStatus) string {
	if status == nil || !status.consulted {
		return ""
	}
	if status.state == graphview.StateComplete && status.fresh {
		return ""
	}
	if status.state == graphview.StateBuilding {
		return "contract analysis is from an earlier input"
	}
	return "contract analysis has no attachment yet"
}

// The existing minimal status seam is shared with optional impact sections;
// full format/status rider work is a separate increment.
func optionalContractFacts(status *contractConsumerStatus) map[string]any {
	fields := map[string]any{"capability": graphview.CapContracts, "state": status.state, "fresh": status.fresh}
	if status.reason != "" {
		fields["reason"] = status.reason
	}
	if status.receiptsUnknown {
		fields["changed_files_known"] = false
	}
	if status.retryAfter > 0 {
		fields["retry_after"] = int64((status.retryAfter + time.Second - 1) / time.Second)
	}
	return fields
}

func optionalContractReviewGate(status *contractConsumerStatus, impact *contractImpact) reviewGate {
	gate := prReviewContractGate(impact)
	if detail := optionalContractWarning(status); detail != "" && gate.Status == prReviewPass {
		gate.Status, gate.Detail = prReviewWarn, detail
	}
	return gate
}

func optionalContractReviewVerdict(verdict review.Verdict, status *contractConsumerStatus) review.Verdict {
	if optionalContractWarning(status) != "" && verdict == review.VerdictApprove {
		return review.VerdictReview
	}
	return verdict
}
