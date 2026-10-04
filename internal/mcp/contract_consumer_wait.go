package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

// ContractAnalysisRuntime is installed together with the contract worker.
// Request must take independent selected-view/source ownership synchronously;
// it may not retain the RPC's borrowed view after returning. Nil inputs request
// only explicitly supported baseline reconciliation. WaitChange observes exact
// attachment publication or input supersession, including a publication racing
// its registration. Neither callback waits for ordinary core publication.
type ContractAnalysisRuntime struct {
	Request    func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error)
	WaitChange func(context.Context, graph.ContractAttachmentKey) error
}

// SetContractAnalysisRuntime is a startup-only installation seam. Leaving it
// nil preserves disabled behavior; installing incomplete callbacks is rejected.
func (s *Server) SetContractAnalysisRuntime(runtime *ContractAnalysisRuntime) {
	if s == nil {
		return
	}
	if runtime != nil && (runtime.Request == nil || runtime.WaitChange == nil) {
		return
	}
	s.contractAnalysisRuntime = runtime
}

// ContractAnalysisShouldYield excludes RPCs suspended for this producer from
// foreground demand. Ordinary active tools and the edit lane retain priority.
func (s *Server) ContractAnalysisShouldYield() bool {
	if s == nil {
		return false
	}
	if s.analysisYield != nil && s.analysisYield() {
		return true
	}
	if s.editLaneBusy() {
		return true
	}
	return runtimeactivity.Current().ByKind["mcp"] > s.contractAnalysisWaiters.Load()
}

type contractConsumerContextKey struct{}

type contractConsumerStatus struct {
	mode     contractConsumerMode
	state    graphview.CapabilityState
	reason   string
	navigate bool
}

func contractConsumerStatusFromContext(ctx context.Context) *contractConsumerStatus {
	status, _ := ctx.Value(contractConsumerContextKey{}).(*contractConsumerStatus)
	return status
}

// prepareContractConsumer is deliberately cheap for optional consumers. It
// does not discover companion repos, read input vectors, load attachments, or
// queue work merely because an ordinary search may include contract symbols.
func (s *Server) prepareContractConsumer(ctx context.Context, req mcp.CallToolRequest, freshness requestFreshness, want capabilityRequest) (context.Context, *mcp.CallToolResult) {
	if s.contractAnalysisRuntime == nil {
		return ctx, nil
	}
	lowered := s.contractConsumerRequest(req)
	mode := contractConsumerForRequest(lowered, want)
	if id := lowered.GetString("id", ""); canonicalContractTarget(id) {
		// Known core definitions win over a virtual namespace spelling. A
		// missing canonical ID still dispatches to current analysis, where an
		// actual removal can be answered without consulting legacy core rows.
		nodes, err := graph.ContractSourceNodesContext(ctx, s.requestBaseReader(ctx), []string{id})
		if err != nil {
			return ctx, mcp.NewToolResultError(err.Error())
		}
		if node := nodes[id]; node != nil && node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge && node.Kind != graph.KindConfigKey {
			coreReq := lowered
			coreArgs := make(map[string]any, len(lowered.GetArguments()))
			for key, value := range lowered.GetArguments() {
				if key != "id" {
					coreArgs[key] = value
				}
			}
			coreReq.Params.Arguments = coreArgs
			mode = contractConsumerForRequest(coreReq, want)
		}
	}
	if mode == contractConsumerNone {
		return ctx, nil
	}
	status := &contractConsumerStatus{mode: mode, state: graphview.StateIncomplete, reason: "contract_component_not_loaded", navigate: contractNavigationRequest(lowered)}
	if mode == contractConsumerRequired {
		switch lowered.Params.Name {
		case "contracts", "api_impact", "analyze":
			// These handlers select their analysis sections explicitly.
		default:
			status.navigate = true
		}
	}
	ctx = context.WithValue(ctx, contractConsumerContextKey{}, status)
	if mode == contractConsumerOptional {
		ctx = withContractAnalysisContext(ctx, &contractAnalysisContext{})
		return ctx, nil
	}
	if s.materializer == nil {
		return ctx, mcp.NewToolResultError(graphview.NewViewError(graphview.CodeCapabilityUnavailable, "selected contract analysis is unsupported").Error())
	}
	started := toolReceivedAt(ctx)
	if started.IsZero() {
		started = time.Now()
	}
	deadline := freshness.effectiveDeadline(started, ctx)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Repo is a response selector. Matcher/name dependencies still use the
	// admitted workspace/project/ref/saved-scope cohort, as the worker does.
	cohortRequest := lowered
	cohortArgs := make(map[string]any, len(lowered.GetArguments()))
	for key, value := range lowered.GetArguments() {
		if key != "repo" {
			cohortArgs[key] = value
		}
	}
	cohortRequest.Params.Arguments = cohortArgs
	repos, err := s.contractConsumerRepos(ctx, cohortRequest)
	if err != nil {
		return ctx, mcp.NewToolResultError(err.Error())
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
	for {
		if err := waitCtx.Err(); err != nil {
			return ctx, contractWaitRefusal(err)
		}
		binding, pending, err := s.openContractConsumerBinding(waitCtx, selected, checkout, repos)
		if err == nil {
			status.state, status.reason = graphview.StateComplete, ""
			if view != nil {
				view.contractCompleteness = &status.state
			}
			return withContractAnalysisContext(ctx, binding), nil
		}
		if waitCtx.Err() != nil {
			return ctx, contractWaitRefusal(waitCtx.Err())
		}
		if pending == nil {
			return ctx, mcp.NewToolResultError(err.Error())
		}
		if pending.stale {
			if err := waitFreshnessRetry(waitCtx, deadline); err != nil {
				return ctx, contractWaitRefusal(err)
			}
			continue // Recapture superseded input; never queue the stale vector.
		}
		supported, requestErr, waitErr := s.requestAndWaitContractAnalysis(waitCtx, selected, pending)

		if requestErr != nil {
			if errors.Is(requestErr, graph.ErrContractProjectionStale) {
				if err := waitFreshnessRetry(waitCtx, deadline); err != nil {
					return ctx, contractWaitRefusal(err)
				}
				continue
			}
			return ctx, contractWaitRefusal(requestErr)
		}
		if !supported {
			return ctx, mcp.NewToolResultError(graphview.NewViewError(graphview.CodeCapabilityUnavailable, "contract analysis cannot build the selected historical inputs").Error())
		}
		if waitErr != nil {
			return ctx, contractWaitRefusal(waitErr)
		}
		// Superseded notifications may be coalesced. Bound read/queue rate even
		// if the runtime reports an already observed change immediately.
		if err := waitFreshnessRetry(waitCtx, deadline); err != nil {
			return ctx, contractWaitRefusal(err)
		}
	}
}

// A waiting contract RPC cannot be foreground demand that parks its own
// producer. The counter balances cancellation, error and callback panics.
func (s *Server) requestAndWaitContractAnalysis(ctx context.Context, selected *graphview.RepoView, pending *pendingContractBinding) (supported bool, requestErr, waitErr error) {
	s.contractAnalysisWaiters.Add(1)
	defer s.contractAnalysisWaiters.Add(-1)
	supported, requestErr = s.contractAnalysisRuntime.Request(ctx, selected, pending.inputs, pending.key.RepoPrefix, pending.key.CheckoutID)
	if requestErr != nil || !supported {
		return supported, requestErr, nil
	}
	return supported, nil, s.contractAnalysisRuntime.WaitChange(ctx, pending.key)
}

type pendingContractBinding struct {
	key    graph.ContractAttachmentKey
	inputs *graphview.SelectedContractInputs
	stale  bool
}

// contractConsumerRepos follows the same workspace/project/repo admission as
// the handlers. Only explicit contract consumers enumerate this scope.
func (s *Server) contractConsumerRepos(ctx context.Context, req mcp.CallToolRequest) ([]string, error) {
	resolved, err := s.resolveScopeForRequest(ctx, req, IntentReach)
	if err != nil {
		return nil, err
	}
	var repos []string
	if resolved.RepoAllow != nil {
		for repo, allowed := range resolved.RepoAllow {
			if allowed {
				repos = append(repos, repo)
			}
		}
	} else {
		repos = s.trackedRepoPrefixes()
		if len(repos) == 0 && s.indexer != nil {
			repos = []string{s.indexer.RepoPrefix()}
		}
	}
	if len(repos) == 0 {
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable, "no admitted contract repository is available")
	}
	// Reach scope often expresses the hard workspace boundary as a slug,
	// leaving RepoAllow nil. That does not authorize reading every tracked
	// workspace merely to construct a dependency vector.
	if resolved.WorkspaceID != "" {
		admitted := make(map[string]bool)
		if s.multiIndexer != nil {
			admitted = s.multiIndexer.ReposInWorkspace(resolved.WorkspaceID)
		} else if s.indexer != nil && s.indexer.WorkspaceID() == resolved.WorkspaceID {
			admitted[s.indexer.RepoPrefix()] = true
		}
		kept := repos[:0]
		for _, repo := range repos {
			if admitted[repo] {
				kept = append(kept, repo)
			}
		}
		repos = kept
		if len(repos) == 0 {
			return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable, "contract workspace membership is unavailable")
		}
	}
	sort.Strings(repos)
	return repos, nil
}

func (s *Server) openContractConsumerBinding(ctx context.Context, selected *graphview.RepoView, checkout string, repos []string) (*contractAnalysisContext, *pendingContractBinding, error) {
	captures := make([]*graphview.SelectedContractInputs, 0, len(repos))
	for _, repo := range repos {
		var source *graphview.RepoView
		actor := ""
		if selected != nil && selected.ID.RepoPrefix == repo {
			source, actor = selected, checkout
		}
		var input *graphview.SelectedContractInputs
		var err error
		if selected != nil && source == nil {
			actor = checkout
			input, err = s.materializer.CaptureContractCompanionInputs(ctx, selected, repo, actor)
		} else {
			input, err = s.materializer.CaptureContractInputs(ctx, source, repo, actor)
		}
		if err != nil {
			if graphview.CodeOf(err) == graphview.CodeRequiredCapabilityIncomplete {
				return nil, &pendingContractBinding{key: graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: actor}}, err
			}
			return nil, nil, err
		}
		captures = append(captures, input)
	}
	binding := &contractAnalysisContext{views: make(map[string]*graphview.ContractAnalysisView, len(repos))}
	for index, repo := range repos {
		actor := captures[index].State.CheckoutID
		input, err := graphview.ComposeSelectedContractInputs(repo, actor, captures...)
		if err != nil {
			binding.close()
			return nil, nil, err
		}
		var source *graphview.RepoView
		if selected != nil && selected.ID.RepoPrefix == repo {
			source = selected
		}
		analysis, err := s.materializer.OpenContractAnalysisForInputs(ctx, source, input)
		if err != nil {
			binding.close()
			if graphview.CodeOf(err) == graphview.CodeRequiredCapabilityIncomplete || errors.Is(err, graph.ErrContractProjectionStale) {
				key := graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: actor, InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}
				return nil, &pendingContractBinding{key: key, inputs: input, stale: errors.Is(err, graph.ErrContractProjectionStale)}, err
			}
			return nil, nil, err
		}
		binding.views[repo] = analysis
	}
	if err := binding.checkCohort(); err != nil {
		binding.close()
		return nil, nil, err
	}
	return binding, nil, nil
}

func contractWaitRefusal(err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, "graph.contracts did not become complete within this request: "+err.Error()).Error())
}

func decorateContractConsumerResult(ctx context.Context, result *mcp.CallToolResult) *mcp.CallToolResult {
	status := contractConsumerStatusFromContext(ctx)
	if status == nil || result == nil || result.IsError {
		return result
	}
	fields := map[string]any{"capability": graphview.CapContracts, "state": status.state}
	if status.reason != "" {
		fields["reason"] = status.reason
	}
	result = mergeResultMeta(result, map[string]any{"contract_analysis": fields})
	text, ok := singleTextContent(result)
	if !ok {
		return result
	}
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) == nil && object != nil {
		object["contract_analysis"] = fields
		if body, err := json.Marshal(object); err == nil {
			return rebuildTextResult(result, string(body))
		}
	}
	if status.state != graphview.StateComplete {
		return rebuildTextResult(result, text+"\n\nContract analysis is incomplete; contract-derived rows are not certified current.")
	}
	return result
}
