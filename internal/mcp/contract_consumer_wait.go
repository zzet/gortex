package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

// ContractAnalysisRuntime is installed together with the contract worker.
// Schedule records a request's interest in an actor's inputs and returns at
// once; it owns the prepared source on every return, and a full scheduler
// refuses a new actor with a retryable *indexer.ContractQueueFullError.
// WaitActor waits on the actor's promise for the seq Schedule answered.
// WaitCapacity waits for capacity to free past CapacityEpoch, and WaitInputs
// for the input epoch (InputEpoch) to move. None of them waits for ordinary
// core publication.
type ContractAnalysisRuntime struct {
	AcquireLastPublishedCohort func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error)
	Schedule                   func(context.Context, indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error)
	WaitActor                  func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error)
	WaitCapacity               func(context.Context, uint64) error
	CapacityEpoch              func() uint64
	InputEpoch                 func() uint64
	WaitInputs                 func(context.Context, uint64) error
}

// SetContractAnalysisRuntime is a startup-only installation seam. Leaving it
// nil preserves disabled behavior; installing incomplete callbacks is rejected.
func (s *Server) SetContractAnalysisRuntime(runtime *ContractAnalysisRuntime) {
	if s == nil {
		return
	}
	if runtime != nil && (runtime.Schedule == nil || runtime.WaitActor == nil || runtime.WaitCapacity == nil ||
		runtime.CapacityEpoch == nil || runtime.InputEpoch == nil || runtime.WaitInputs == nil) {
		return
	}
	s.contractAnalysisRuntime = runtime
}

// contractConsumerInputsPoll bounds one wait for the input epoch: inputs can
// move without an announced core acceptance (a linked worktree's
// generation), so a stale capture is taken again at least once a second.
const contractConsumerInputsPoll = time.Second

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
	mode            contractConsumerMode
	state           graphview.CapabilityState
	reason          string
	navigate        bool
	allowServed     bool
	fresh           bool
	consulted       bool
	retryAfter      time.Duration
	receiptsUnknown bool
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
	status := &contractConsumerStatus{mode: mode, state: graphview.StateIncomplete, reason: "no_attachment", navigate: contractNavigationRequest(lowered)}
	if mode == contractConsumerRequired {
		switch lowered.Params.Name {
		case "contracts", "api_impact", "analyze":
			// These handlers select their analysis sections explicitly.
		default:
			status.navigate = true
		}
	}
	// Only explicitly declared required capabilities forbid predecessor answers;
	// tool-inferred strictness and require_complete defaults are distinct.
	status.allowServed = !freshness.requireExact && len(want.required) == 0
	ctx = context.WithValue(ctx, contractConsumerContextKey{}, status)
	if mode == contractConsumerOptional {
		binding := &contractAnalysisContext{}
		optionalRequest := lowered
		optionalArgs := make(map[string]any, len(lowered.GetArguments()))
		for key, value := range lowered.GetArguments() {
			optionalArgs[key] = value
		}
		optionalRequest.Params.Arguments = optionalArgs
		binding.lazy = func(readCtx context.Context) error {
			return s.bindOptionalContractConsumer(readCtx, optionalRequest, status, binding)
		}
		ctx = withContractAnalysisContext(ctx, binding)
		return ctx, nil
	}
	status.consulted = true
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
	runtime := s.contractAnalysisRuntime
	actor := indexer.NewContractActor(checkout, repos)
	var arrivalSeq uint64
	fallback := func(cause error, retry time.Duration) (context.Context, *mcp.CallToolResult) {
		if ctx.Err() != nil {
			return ctx, contractWaitRefusal(ctx.Err(), retry)
		}
		if status.allowServed && runtime.AcquireLastPublishedCohort != nil {
			binding, proof, openErr := s.openContractPredecessorBinding(ctx, selected, checkout, repos, actor)
			if openErr == nil {
				status.state, status.reason, status.fresh = graphview.StateBuilding, "contract_analysis_pending", false
				status.receiptsUnknown = binding.recovered
				if arrivalSeq > 0 && proof != nil && proof.Seq >= arrivalSeq {
					status.state, status.reason, status.fresh = graphview.StateComplete, "", true
				}
				if view != nil {
					// Completeness of the served immutable payload is separate from
					// whether the current logical key is still building.
					complete := graphview.StateComplete
					view.contractCompleteness = &complete
				}
				return withContractAnalysisContext(ctx, binding), nil
			}
		}
		return ctx, contractWaitRefusal(cause, retry)
	}
	// scheduled holds the input epoch each key was last scheduled at: a key
	// is asked for at most once per epoch, so a promise that resolved
	// without its attachment opening waits for the inputs to move instead
	// of asking again.
	scheduled := make(map[graph.ContractAttachmentKey]uint64)
	for {
		if err := waitCtx.Err(); err != nil {
			return fallback(err, 0)
		}
		// Sampled before the capture: an accept landing after it moves the
		// epoch past this sample.
		epoch := runtime.InputEpoch()
		binding, pending, err := s.openContractConsumerBinding(waitCtx, selected, checkout, repos)
		if err == nil {
			status.state, status.reason, status.fresh = graphview.StateComplete, "", true
			binding.served = status.allowServed
			if view != nil {
				view.contractCompleteness = &status.state
			}
			return withContractAnalysisContext(ctx, binding), nil
		}
		if waitCtx.Err() != nil {
			return fallback(waitCtx.Err(), 0)
		}
		if pending == nil {
			return ctx, mcp.NewToolResultError(err.Error())
		}
		// A missing entry differs from a scheduled entry at epoch zero: cold
		// startup still schedules once before waiting for an input notification.
		if at, seen := scheduled[pending.key]; pending.stale || (seen && at >= epoch) {
			// Superseded inputs are captured again once they move; never
			// schedule the stale vector, nor one vector twice per epoch.
			if err := s.waitContractInputs(waitCtx, epoch); err != nil {
				return fallback(err, 0)
			}
			continue
		}
		scheduled[pending.key] = epoch
		result, retry, err := s.scheduleAndWaitContractAnalysis(waitCtx, actor, selected, pending, epoch)
		if arrivalSeq == 0 {
			arrivalSeq = pending.arrivalSeq
		}
		switch {
		case errors.Is(err, indexer.ErrContractUnsupported):
			return ctx, mcp.NewToolResultError(graphview.NewViewError(graphview.CodeCapabilityUnavailable, "contract analysis cannot build the selected historical inputs").Error())
		case errors.Is(err, graph.ErrContractProjectionStale):
			// The selected view's handoff could not be taken any more.
			delete(scheduled, pending.key)
			if err := s.waitContractInputs(waitCtx, epoch); err != nil {
				return fallback(err, 0)
			}
			continue
		case err != nil:
			return fallback(err, retry)
		}
		if status.allowServed && arrivalSeq > 0 && runtime.AcquireLastPublishedCohort != nil {
			binding, proof, openErr := s.openContractPredecessorBinding(ctx, selected, checkout, repos, actor)
			if openErr == nil {
				if proof != nil && proof.Seq >= arrivalSeq {
					status.state, status.reason, status.fresh = graphview.StateComplete, "", true
					if view != nil {
						view.contractCompleteness = &status.state
					}
					return withContractAnalysisContext(ctx, binding), nil
				}
				binding.close()
			}
		}
		switch result.Result {
		case indexer.ContractActorFailed, indexer.ContractActorStalled, indexer.ContractActorPanic:
			return fallback(fmt.Errorf("contract analysis of these inputs %s", result.Result), result.RetryAfter)
		case indexer.ContractActorInputsStale:
			// The request's capture can no longer be taken: capture the
			// inputs again once they moved, and ask for them again.
			delete(scheduled, pending.key)
			if err := s.waitContractInputs(waitCtx, epoch); err != nil {
				return fallback(err, 0)
			}
		}
		// Published, found published, or replaced by newer inputs: open the
		// inputs again.
	}
}

// scheduleAndWaitContractAnalysis asks the scheduler for the pending inputs
// and waits on the actor's promise. A runtime with publication custody returns
// queue-full immediately for predecessor selection; legacy runtimes wait for capacity. A waiting contract RPC cannot be foreground demand that
// parks its own producer: the counter balances cancellation, error and
// callback panics.
func (s *Server) scheduleAndWaitContractAnalysis(ctx context.Context, actor indexer.ContractActor, selected *graphview.RepoView, pending *pendingContractBinding, epoch uint64) (indexer.ContractActorStatus, time.Duration, error) {
	s.contractAnalysisWaiters.Add(1)
	defer s.contractAnalysisWaiters.Add(-1)
	runtime := s.contractAnalysisRuntime
	for {
		capacity := runtime.CapacityEpoch()
		source, err := indexer.PrepareContractCaptureSource(selected)
		if err != nil {
			return indexer.ContractActorStatus{}, 0, err
		}
		scheduled, err := runtime.Schedule(ctx, indexer.ContractScheduleRequest{Actor: actor, Key: pending.key, Inputs: pending.inputs, Source: source, Epoch: epoch})
		var full *indexer.ContractQueueFullError
		if errors.As(err, &full) {
			if runtime.AcquireLastPublishedCohort != nil {
				return indexer.ContractActorStatus{}, full.RetryAfter, err
			}
			if waitErr := runtime.WaitCapacity(ctx, capacity); waitErr != nil {
				return indexer.ContractActorStatus{}, full.RetryAfter, fmt.Errorf("%w: %w", err, waitErr)
			}
			continue
		}
		if err != nil {
			return indexer.ContractActorStatus{}, 0, err
		}
		pending.arrivalSeq = scheduled.ArrivalSeq
		status, err := runtime.WaitActor(ctx, scheduled.Actor, scheduled.ArrivalSeq)
		return status, status.RetryAfter, err
	}
}

// waitContractInputs waits, as a suspended producer waiter, for the input
// epoch to pass since, at most contractConsumerInputsPoll.
func (s *Server) waitContractInputs(ctx context.Context, since uint64) error {
	s.contractAnalysisWaiters.Add(1)
	defer s.contractAnalysisWaiters.Add(-1)
	wait, cancel := context.WithTimeout(ctx, contractConsumerInputsPoll)
	defer cancel()
	_ = s.contractAnalysisRuntime.WaitInputs(wait, since)
	return ctx.Err()
}

type pendingContractBinding struct {
	key        graph.ContractAttachmentKey
	inputs     *graphview.SelectedContractInputs
	stale      bool
	arrivalSeq uint64
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

// contractWaitRefusal refuses a strict call whose contract analysis did not
// complete in time, with a retry hint when the scheduler gave one.
func contractWaitRefusal(err error, retryAfter time.Duration) *mcp.CallToolResult {
	message := "graph.contracts did not become complete within this request: " + err.Error()
	if retryAfter > 0 {
		message += fmt.Sprintf("; retry after %s", retryAfter.Round(time.Second))
	}
	return mcp.NewToolResultError(graphview.NewViewError(graphview.CodeRequiredCapabilityIncomplete, message).Error())
}

func decorateContractConsumerResult(ctx context.Context, result *mcp.CallToolResult) *mcp.CallToolResult {
	status := contractConsumerStatusFromContext(ctx)
	if status == nil || (status.mode == contractConsumerOptional && !status.consulted) || result == nil || result.IsError {
		return result
	}
	fresh := status.fresh
	if result.Meta != nil {
		if freshness, ok := result.Meta.AdditionalFields["freshness"].(map[string]any); ok {
			if value, ok := freshness["fresh"].(bool); ok && !value {
				fresh = false
			}
		}
	}
	text, isText := singleTextContent(result)
	var object map[string]any
	if isText && json.Unmarshal([]byte(text), &object) == nil && object != nil {
		if freshness, ok := object["freshness"].(map[string]any); ok {
			if value, ok := freshness["fresh"].(bool); ok && !value {
				fresh = false
			}
		}
	}
	fields := map[string]any{"capability": graphview.CapContracts, "state": status.state, "fresh": fresh}
	if status.reason != "" {
		fields["reason"] = status.reason
	}
	if status.receiptsUnknown {
		fields["changed_files_known"] = false
	}
	if status.retryAfter > 0 {
		fields["retry_after"] = int64((status.retryAfter + time.Second - 1) / time.Second)
	}
	result = mergeResultMeta(result, map[string]any{"contract_analysis": fields})
	text, ok := singleTextContent(result)
	if !ok {
		return result
	}
	object = nil
	if json.Unmarshal([]byte(text), &object) == nil && object != nil {
		object["contract_analysis"] = fields
		if body, err := json.Marshal(object); err == nil {
			return rebuildTextResult(result, string(body))
		}
	}
	if status.state != graphview.StateComplete {
		return rebuildTextResult(result, text+"\n\nContract analysis is rebuilding; the served attachment is not certified current.")
	}
	return result
}
