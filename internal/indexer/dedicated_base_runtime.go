package indexer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

var errDedicatedBaseRuntimeInput = errors.New("invalid dedicated base runtime input")
var errDedicatedBaseAdvanceRequired = errors.New("dedicated base requires incremental advancement")

// dedicatedBaseAdvanceRequiredError leaves desire and the active pointer intact.
// The initial seed primitive must never become a full rebuild on every commit.
type dedicatedBaseAdvanceRequiredError struct {
	GraphID            string
	ActiveGenerationID int64
	Active, Observed   store_sqlite.DedicatedBaseIdentity
}

func (e *dedicatedBaseAdvanceRequiredError) Error() string {
	return fmt.Sprintf("%v: graph %s active generation %d", errDedicatedBaseAdvanceRequired, e.GraphID, e.ActiveGenerationID)
}

func (e *dedicatedBaseAdvanceRequiredError) Unwrap() error { return errDedicatedBaseAdvanceRequired }

// dedicatedBaseRuntime must live with the daemon/MultiIndexer, not a trigger or
// replaceable publisher. Its graph gates span authority replacement and all
// observation callers. It retains no observed config, builder or content source.
// Startup must call it directly, not through a gate that startup itself opens.
type dedicatedBaseRuntime struct {
	store                *store_sqlite.Store
	mu                   sync.Mutex
	gates                map[string]*dedicatedBaseObservationGate
	ownerAdmissions      map[string]*dedicatedBaseOwnerAdmission
	admittedActors       int
	admissionClosed      bool
	admissionDrain       chan struct{}
	admissionDrainClosed bool
}

type dedicatedBaseObservationGate struct {
	token chan struct{}
	refs  int
}

type dedicatedBasePublisher struct {
	runtime   *dedicatedBaseRuntime
	authority store_sqlite.DedicatedBaseAuthority
	admission *dedicatedBaseOwnerAdmission
}

// All fields must come from one fresh observation made inside ensureInitial's
// callback. Builder.Config (including referenced slices/maps) must remain an
// immutable snapshot for this call's build; copying its struct is not a freeze.
// Identity must include all output-affecting workspace/project configuration.
type dedicatedBaseObservation struct {
	Identity                   store_sqlite.DedicatedBaseIdentity
	ExpectedActiveGenerationID int64
	RootPath                   string
	WorkspaceID                string
	ProjectID                  string
	ProvenanceCommitOID        string
	CreatedAt                  int64
	Builder                    SparseGenerationBuilder
	PrePublish                 func(context.Context, int64) error
}

type dedicatedBaseResult struct {
	Claim    store_sqlite.DedicatedBaseBuildClaim
	Report   BuildReport
	Adoption store_sqlite.DedicatedBaseAdoption
}

func (r *dedicatedBaseRuntime) acquire(ctx context.Context, graphID string) (func(), error) {
	if r == nil || ctx == nil || graphID == "" {
		return nil, fmt.Errorf("%w: graph gate requires runtime, context and graph", errDedicatedBaseRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.gates == nil {
		r.gates = make(map[string]*dedicatedBaseObservationGate)
	}
	g := r.gates[graphID]
	if g == nil {
		g = &dedicatedBaseObservationGate{token: make(chan struct{}, 1)}
		g.token <- struct{}{}
		r.gates[graphID] = g
	}
	g.refs++ // Includes queued waiters, preventing idle cleanup from splitting a gate.
	r.mu.Unlock()
	drop := func() {
		r.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(r.gates, graphID)
		}
		r.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-g.token:
		if err := ctx.Err(); err != nil {
			g.token <- struct{}{}
			drop()
			return nil, err
		}
	}
	var once sync.Once
	return func() { once.Do(func() { g.token <- struct{}{}; drop() }) }, nil
}

// install is an owner-installation operation, NEVER a per-poll operation. The
// canonical owner and explicit expected authority fence come from its caller.
func (r *dedicatedBaseRuntime) install(ctx context.Context, req store_sqlite.AcquireDedicatedBaseAuthorityRequest) (*dedicatedBasePublisher, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("%w: installation requires a store", errDedicatedBaseRuntimeInput)
	}
	actorRelease, admission, err := r.admitOwnerState(ctx, req.GraphID, req.Owner, nil)
	if err != nil {
		return nil, err
	}
	defer actorRelease()
	release, err := r.acquire(ctx, req.GraphID)
	if err != nil {
		return nil, err
	}
	defer release()
	authority, err := r.store.Catalog().AcquireDedicatedBaseAuthority(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := r.confirmOwner(req.GraphID, authority.Owner); err != nil {
		return nil, err
	}
	return &dedicatedBasePublisher{runtime: r, authority: authority, admission: admission}, nil
}

func (p *dedicatedBasePublisher) ensureObserved(ctx context.Context, observe func(context.Context) (dedicatedBaseObservation, error), leases *graphview.LeaseManager) (dedicatedBaseResult, error) {
	var out dedicatedBaseResult
	if p == nil || p.runtime == nil || p.runtime.store == nil || observe == nil {
		return out, fmt.Errorf("%w: ensure requires publisher, store and observer", errDedicatedBaseRuntimeInput)
	}
	r := p.runtime
	actorRelease, _, err := r.admitOwnerState(ctx, p.authority.GraphID, p.authority.Owner, p.admission)
	if err != nil {
		return out, err
	}
	defer actorRelease()
	release, err := r.acquire(ctx, p.authority.GraphID)
	if err != nil {
		return out, err
	}
	// Idempotent release also handles errors and panics in caller observation.
	defer release()
	catalog := r.store.Catalog()
	publication, found, err := catalog.DedicatedBasePublication(ctx, p.authority.GraphID)
	if err != nil {
		return out, err
	}
	if !found || publication.Desire.Authority != p.authority {
		return out, fmt.Errorf("%w: publisher authority changed", store_sqlite.ErrCatalogStaleGuard)
	}
	if err := r.confirmOwner(p.authority.GraphID, p.authority.Owner); err != nil {
		return out, err
	}
	observation, err := observe(ctx)
	if err != nil {
		return out, err
	}
	identity := observation.Identity
	if identity.TreeOID == "" || identity.ConfigHash == "" || identity.ExtractorVersions == "" || identity.ResolverVersion == "" ||
		observation.Builder.Store != r.store || observation.Builder.Registry == nil || observation.Builder.Logger == nil {
		return out, fmt.Errorf("%w: observation requires complete identity and a builder on this store", errDedicatedBaseRuntimeInput)
	}
	graph, found, err := catalog.GetDedicatedGraph(ctx, p.authority.GraphID)
	if err != nil {
		return out, err
	}
	if !found || graph.ActiveGenerationID != observation.ExpectedActiveGenerationID {
		return out, fmt.Errorf("%w: observed active generation changed", store_sqlite.ErrCatalogStaleGuard)
	}
	var parentID int64
	if graph.ActiveGenerationID > 0 {
		active, found, err := catalog.GetViewGeneration(ctx, graph.ActiveGenerationID)
		if err != nil {
			return out, err
		}
		if !found {
			return out, fmt.Errorf("%w: active generation missing", store_sqlite.ErrDedicatedBaseCandidate)
		}
		activeIdentity := store_sqlite.DedicatedBaseIdentity{TreeOID: active.TreeOID, ConfigHash: active.ConfigHash,
			ExtractorVersions: active.ExtractorVersions, ResolverVersion: active.ResolverVersion}
		if activeIdentity != identity && leases == nil {
			return out, &dedicatedBaseAdvanceRequiredError{GraphID: p.authority.GraphID,
				ActiveGenerationID: active.GenerationID, Active: activeIdentity, Observed: identity}
		}
		if leases == nil && (active.BaseGenerationID != 0 || active.LayerID != "" || active.LowerViewFingerprint != "") {
			return out, fmt.Errorf("%w: initial runtime cannot consume active ancestry", store_sqlite.ErrDedicatedBaseCandidate)
		}
		if activeIdentity != identity {
			parentID, err = dedicatedBaseParentForAdvance(ctx, catalog, p.authority, active, identity)
			if err != nil {
				return out, err
			}
		}
	}
	// The runtime gate covers its own adoption calls, but external publishers
	// still require the catalog's authority/desire/active-pointer CAS fences.
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: p.authority, ExpectedDesiredEpoch: publication.Desire.Epoch, Identity: identity,
	})
	if err != nil {
		return out, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return out, fmt.Errorf("indexer: generate dedicated base attempt token: %w", err)
	}
	claimRequest := store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, ExpectedActiveGenerationID: graph.ActiveGenerationID, AttemptToken: hex.EncodeToString(token[:]),
		ProvenanceCommitOID: observation.ProvenanceCommitOID, CreatedAt: observation.CreatedAt,
		BaseGenerationID: parentID,
	}
	if parentID > 0 {
		claimRequest.LayerID = fmt.Sprintf("dedicated-delta:%d", parentID)
		claimRequest.LowerViewFingerprint = fmt.Sprintf("dedicated:%s:%d", p.authority.GraphID, parentID)
	}
	out.Claim, err = catalog.ClaimDedicatedBaseBuild(ctx, claimRequest)
	if err != nil {
		return out, err
	}
	if leases == nil && (out.Claim.BaseGenerationID != 0 || out.Claim.LayerID != "" || out.Claim.LowerViewFingerprint != "") {
		return out, fmt.Errorf("%w: initial runtime cannot consume a claimed delta", store_sqlite.ErrDedicatedBaseCandidate)
	}
	release() // No graph observation gate across physical work or follower waits.
	id, report, err := p.buildObservedClaim(ctx, observation, out.Claim, leases)
	out.Report = report
	if err != nil {
		// A canceled flight follower does not own the physical attempt. Never
		// blanket-call FailDedicatedBaseBuild here; return the claim for diagnosis.
		return out, err
	}
	if id != out.Claim.GenerationID {
		return out, fmt.Errorf("%w: builder returned a different generation", store_sqlite.ErrDedicatedBaseCandidate)
	}
	adoptRelease, err := r.acquire(ctx, p.authority.GraphID)
	if err != nil {
		return out, err
	}
	defer adoptRelease()
	// Every successful path, including ready replay and followers, reaches this
	// same guard. A PrePublish callback cannot replace final adoption fencing.
	out.Adoption, err = catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: out.Claim})
	return out, err
}
