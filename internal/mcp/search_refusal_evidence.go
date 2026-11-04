package mcp

import (
	"context"
	"errors"
	"os"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

type searchRefusalStage uint8

const (
	searchRefusalBindInherited searchRefusalStage = iota + 1
	searchRefusalBindReentry
	searchRefusalBindSnapshot
	searchRefusalBindRefresh
	searchRefusalBindRetain
	searchRefusalBindFinal
	searchRefusalNameCandidates
	searchRefusalResponseValidation
	searchRefusalRefreshLiveNil
	searchRefusalRefreshLiveView
	searchRefusalRefreshDeadlineNil
	searchRefusalRefreshDeadlineView
	searchRefusalRefreshCancelledNil
	searchRefusalRefreshCancelledView
)

func (stage searchRefusalStage) label() string {
	switch stage {
	case searchRefusalBindInherited:
		return "bind.inherited_facts"
	case searchRefusalBindReentry:
		return "bind.reentry"
	case searchRefusalBindSnapshot:
		return "bind.snapshot_owner"
	case searchRefusalBindRefresh:
		return "bind.refresh"
	case searchRefusalBindRetain:
		return "bind.retain"
	case searchRefusalBindFinal:
		return "bind.final_validation"
	case searchRefusalNameCandidates:
		return "leaf.name_candidates"
	case searchRefusalResponseValidation:
		return "response.projection_validation"
	case searchRefusalRefreshLiveNil:
		return "bind.refresh/live/superseded_nil"
	case searchRefusalRefreshLiveView:
		return "bind.refresh/live/superseded_view"
	case searchRefusalRefreshDeadlineNil:
		return "bind.refresh/deadline/superseded_nil"
	case searchRefusalRefreshDeadlineView:
		return "bind.refresh/deadline/superseded_view"
	case searchRefusalRefreshCancelledNil:
		return "bind.refresh/cancelled/superseded_nil"
	case searchRefusalRefreshCancelledView:
		return "bind.refresh/cancelled/superseded_view"
	default:
		return ""
	}
}

type searchRefusalDiagnostic struct {
	cause        error
	stage        searchRefusalStage
	captureState string
}

func (e *searchRefusalDiagnostic) Error() string { return e.cause.Error() }
func (e *searchRefusalDiagnostic) Unwrap() error { return e.cause }

// GORTEX_SEARCH_REFUSAL_EVIDENCE=1 opts into two fixed scalar fields on an
// existing typed search refusal. The existing RPC/raw spool retains them;
// no query, path, payload, extra RPC or retained recorder state is added.
// Disabled annotation returns the original error without allocating diagnostic
// state. The platform's environment lookup is on this refusal path only, never
// successful search.
func annotateSearchRefusal(ctx context.Context, stage searchRefusalStage, err error) error {
	if !errors.Is(err, projectionSuperseded) || stage.label() == "" || os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != "1" {
		return err
	}
	if _, present := err.(*searchRefusalDiagnostic); present {
		return err // Preserve the first actual refusal site.
	}
	state := "none"
	if ctx != nil {
		facts, _ := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
		if capture, _ := ctx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture); capture != nil {
			capture.mu.RLock()
			switch {
			case capture.disabled:
				state = "disabled"
			case capture.facts == nil || len(capture.entries) == 0:
				state = "unsealed"
			case capture.facts != facts:
				state = "foreign"
			default:
				state = "sealed"
			}
			capture.mu.RUnlock()
		}
	}
	return &searchRefusalDiagnostic{cause: err, stage: stage, captureState: state}
}

// Refusal-only observations, not an inference about the underlying race:
// superseded means a nonnil typed refresh error; nil/view is its returned
// projection, and live/deadline/cancelled is the child context at this site.
// A nil view without an error is canonical handoff, never a refusal.
func annotateSearchRefreshRefusal(ctx, child context.Context, view *projectionEntry, err error) error {
	if !errors.Is(err, projectionSuperseded) || os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != "1" {
		return err
	}
	stage := searchRefusalBindRefresh
	if child != nil {
		switch child.Err() {
		case nil:
			stage = searchRefusalRefreshLiveNil
		case context.DeadlineExceeded:
			stage = searchRefusalRefreshDeadlineNil
		case context.Canceled:
			stage = searchRefusalRefreshCancelledNil
		}
		if view != nil && stage != searchRefusalBindRefresh {
			stage++ // Each closed context category has adjacent nil/view labels.
		}
	}
	return annotateSearchRefusal(ctx, stage, err)
}

func searchRefusalToolResult(err error) *mcplib.CallToolResult {
	result := mcplib.NewToolResultError(err.Error())
	if evidence, present := err.(*searchRefusalDiagnostic); present {
		// Both labels have closed vocabularies. This entire added metadata
		// member is under128 encoded bytes, independent of caller input.
		result.Meta = &mcplib.Meta{AdditionalFields: map[string]any{
			"gortex_search_refusal": map[string]any{"stage": evidence.stage.label(), "capture_state": evidence.captureState},
		}}
	}
	return result
}
