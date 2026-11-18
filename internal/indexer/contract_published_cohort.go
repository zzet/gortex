package indexer

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ErrContractPublishedCohortAbsent permits restart recovery only when no owned
// metadata exists. Invalid/retired retained proof never becomes this sentinel.
var ErrContractPublishedCohortAbsent = fmt.Errorf("no retained contract publication: %w", graph.ErrContractProjectionIncomplete)

// ContractPublishedTarget identifies the actual immutable winner, including a
// competing publication that won before this job's atomic transaction.
type ContractPublishedTarget struct {
	Key               graph.ContractAttachmentKey
	PayloadGeneration int64
}

// ContractPublishedCohort is owned historical proof, with no reader, store,
// source lifetime or cache pointer. Seq belongs to this proof's publication;
// an actor's later no-op or mixed-target job does not fabricate newer proof.
// Consumers must acquire its leases and open these exact keys, then apply
// their own freshness policy. This record does not authorize serving a view.
type ContractPublishedCohort struct {
	Seq             uint64
	Witnesses       []graph.ContractInputWitness
	Targets         []ContractPublishedTarget
	ExpectedTargets []graph.ContractAttachmentKey
	Stamps          map[string]ContractReceiptStamp
	Census          []string
}

func cloneContractPublishedCohort(in *ContractPublishedCohort) *ContractPublishedCohort {
	if in == nil {
		return nil
	}
	out := *in
	out.Witnesses = slices.Clone(in.Witnesses)
	for i := range out.Witnesses {
		state := &out.Witnesses[i].State
		state.RepoPrefix, state.CheckoutID = strings.Clone(state.RepoPrefix), strings.Clone(state.CheckoutID)
		state.InputVersion, state.InputFingerprint = strings.Clone(state.InputVersion), strings.Clone(state.InputFingerprint)
		state.PreviousInputVersion, state.PreviousInputFingerprint = strings.Clone(state.PreviousInputVersion), strings.Clone(state.PreviousInputFingerprint)
	}
	out.ExpectedTargets = slices.Clone(in.ExpectedTargets)
	for i := range out.ExpectedTargets {
		out.ExpectedTargets[i] = cloneContractPublishedKey(out.ExpectedTargets[i])
	}
	out.Targets = slices.Clone(in.Targets)
	for i := range out.Targets {
		key := &out.Targets[i].Key
		key.RepoPrefix, key.CheckoutID = strings.Clone(key.RepoPrefix), strings.Clone(key.CheckoutID)
		key.InputVersion, key.InputFingerprint = strings.Clone(key.InputVersion), strings.Clone(key.InputFingerprint)
	}
	out.Stamps = make(map[string]ContractReceiptStamp, len(in.Stamps))
	for path, stamp := range in.Stamps {
		out.Stamps[strings.Clone(path)] = cloneContractDeltaStamp(stamp)
	}
	out.Census = cloneContractDeltaStrings(in.Census)
	return &out
}

func contractPublishedProof(inputs []graph.ContractInputWitness, clock contractChangeClock, checkout string) *ContractPublishedCohort {
	expected := contractPublishedExpectedTargets(inputs, checkout)
	return cloneContractPublishedCohort(&ContractPublishedCohort{Witnesses: inputs, ExpectedTargets: expected, Stamps: clock.last, Census: slices.Sorted(maps.Keys(clock.lastCensus))})
}

func cloneContractPublishedKey(key graph.ContractAttachmentKey) graph.ContractAttachmentKey {
	key.RepoPrefix, key.CheckoutID = strings.Clone(key.RepoPrefix), strings.Clone(key.CheckoutID)
	key.InputVersion, key.InputFingerprint = strings.Clone(key.InputVersion), strings.Clone(key.InputFingerprint)
	return key
}

// Capture considers every repository in W before filtering already-attached
// targets. Derive that exact full frontier independently of the filtered job.
func contractPublishedExpectedTargets(inputs []graph.ContractInputWitness, checkout string) []graph.ContractAttachmentKey {
	repos := map[string]bool{}
	for _, witness := range inputs {
		repos[witness.State.RepoPrefix] = true
	}
	var keys []graph.ContractAttachmentKey
	for _, repo := range slices.Sorted(maps.Keys(repos)) {
		state, err := graph.ComposeContractInputState(repo, checkout, inputs)
		if err != nil || !state.Accepted {
			return nil
		}
		keys = append(keys, graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint})
	}
	return keys
}

func contractPublishedCohortComplete(cohort *ContractPublishedCohort) bool {
	if cohort == nil || len(cohort.Targets) == 0 || len(cohort.ExpectedTargets) == 0 {
		return false
	}
	expected := contractPublishedExpectedTargets(cohort.Witnesses, cohort.ExpectedTargets[0].CheckoutID)
	if !slices.Equal(expected, cohort.ExpectedTargets) || len(cohort.Targets) != len(expected) {
		return false
	}
	remaining := map[graph.ContractAttachmentKey]bool{}
	for _, key := range expected {
		remaining[key] = true
	}
	for _, target := range cohort.Targets {
		if !remaining[target.Key] || target.PayloadGeneration <= 0 {
			return false
		}
		delete(remaining, target.Key)
	}
	return len(remaining) == 0
}

// A duplicate's immutable payload can precede this job's receipt reads even
// under the same boundary key. Its validated input/ack proof does not supply
// historical source stamps. Preserve an already-retained predecessor instead
// of relabeling the winner with newer preparation stamps.
func contractPublishedResults(proof *ContractPublishedCohort, submitted []store_sqlite.ContractAttachmentPublishTarget, results []store_sqlite.ContractAttachmentPublication) *ContractPublishedCohort {
	if proof == nil || len(submitted) != len(results) || len(submitted) != len(proof.ExpectedTargets) {
		return nil
	}
	for i, result := range results {
		if result.PayloadGeneration != submitted[i].Attachment.PayloadGeneration {
			return nil
		}
	}
	cohort := cloneContractPublishedCohort(proof)
	for i, result := range results {
		cohort.Targets = append(cohort.Targets, ContractPublishedTarget{Key: graph.ContractAttachmentKey{RepoPrefix: submitted[i].Attachment.RepoPrefix, CheckoutID: submitted[i].Attachment.CheckoutID, InputVersion: submitted[i].Attachment.InputVersion, InputFingerprint: submitted[i].Attachment.InputFingerprint}, PayloadGeneration: result.PayloadGeneration})
	}
	if !contractPublishedCohortComplete(cohort) {
		return nil
	}
	return cohort
}

// AcquireLastPublishedCohort returns owned metadata with all positive source
// and winning payload generations pinned. A missing or retiring predecessor,
// changed exact binding, or incomplete winning payload refuses. The caller
// still owns exact cohort/view opening and the decision whether to serve it.
func (c *ContractAnalysisCoordinator) AcquireLastPublishedCohort(ctx context.Context, actor ContractActor) (*ContractPublishedCohort, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	var cohort *ContractPublishedCohort
	if a := c.actors[actor]; a != nil {
		cohort = a.lastCohort
	}
	c.mu.Unlock()
	cohort = cloneContractPublishedCohort(cohort)
	if cohort == nil {
		return nil, nil, ErrContractPublishedCohortAbsent
	}
	if !contractPublishedCohortComplete(cohort) || cohort == nil || len(cohort.Targets) == 0 || len(cohort.Witnesses) == 0 {
		return nil, nil, graph.ErrContractProjectionIncomplete
	}
	generations := map[int64]bool{}
	for _, witness := range cohort.Witnesses {
		if witness.GenerationID > 0 {
			generations[witness.GenerationID] = true
		}
	}
	for _, stamp := range cohort.Stamps {
		if stamp.Generation > 0 {
			generations[stamp.Generation] = true
		}
	}
	for _, target := range cohort.Targets {
		if target.PayloadGeneration <= 0 {
			return nil, nil, graph.ErrContractProjectionStale
		}
		generations[target.PayloadGeneration] = true
		logical, err := graph.ComposeContractInputState(target.Key.RepoPrefix, target.Key.CheckoutID, cohort.Witnesses)
		if err != nil || !logical.Accepted || logical.InputVersion != target.Key.InputVersion || logical.InputFingerprint != target.Key.InputFingerprint {
			return nil, nil, graph.ErrContractProjectionStale
		}
	}
	var releases []func()
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, close := range releases {
				close()
			}
		})
	}
	for generation := range generations {
		lease := c.options.Leases.Acquire(generation)
		releases = append(releases, lease.Release)
	}
	fail := func(err error) (*ContractPublishedCohort, func(), error) { release(); return nil, nil, err }
	for generation := range generations {
		state, found, err := c.options.Store.Catalog().GetViewGeneration(ctx, generation)
		if err != nil {
			return fail(err)
		}
		if !found || state.State == store_sqlite.ViewGenerationRetiring {
			return fail(graph.ErrContractPredecessorRetired)
		}
		if state.State != store_sqlite.ViewGenerationReady && state.State != store_sqlite.ViewGenerationSuperseded {
			return fail(graph.ErrContractProjectionStale)
		}
	}
	for _, target := range cohort.Targets {
		current, err := c.options.Store.GetContractAttachmentContext(ctx, target.Key)
		if err != nil {
			return fail(err)
		}
		if current == nil {
			return fail(graph.ErrContractPredecessorRetired)
		}
		if current.PayloadGeneration != target.PayloadGeneration {
			return fail(graph.ErrContractProjectionStale)
		}
		states, err := c.options.Store.AtGeneration(target.PayloadGeneration).ProducerStates()
		if err != nil {
			return fail(err)
		}
		complete := false
		for _, state := range states {
			complete = complete || state.Producer == "graph.contracts" && state.State == store_sqlite.ProducerStateComplete
		}
		if !complete {
			return fail(graph.ErrContractProjectionIncomplete)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return cohort, release, nil
}
