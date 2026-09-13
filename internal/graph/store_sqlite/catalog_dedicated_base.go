package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
)

// dedicatedBasePublicationsSchemaSQL is shared by fresh-store initialization
// and the versioned additive migration. Runtime authority is installed lazily.
const dedicatedBasePublicationsSchemaSQL = `CREATE TABLE IF NOT EXISTS dedicated_base_publications (
	graph_id TEXT PRIMARY KEY REFERENCES dedicated_graphs(graph_id) ON DELETE CASCADE,
	owner_checkout_id TEXT NOT NULL,
	owner_incarnation TEXT NOT NULL,
	owner_generation_floor INTEGER NOT NULL,
	repo_prefix TEXT NOT NULL,
	family_id TEXT NOT NULL,
	authority_epoch INTEGER NOT NULL,
	authority_token TEXT NOT NULL,
	desired_epoch INTEGER NOT NULL DEFAULT 0,
	tree_oid TEXT NOT NULL DEFAULT '',
	config_hash TEXT NOT NULL DEFAULT '',
	extractor_versions TEXT NOT NULL DEFAULT '',
	resolver_version TEXT NOT NULL DEFAULT '',
	dependency_revision TEXT NOT NULL DEFAULT '',
	attempt_token TEXT NOT NULL DEFAULT '',
	attempt_state TEXT NOT NULL DEFAULT 'idle',
	generation_id INTEGER NOT NULL DEFAULT 0,
	base_generation_id INTEGER NOT NULL DEFAULT 0,
	layer_id TEXT NOT NULL DEFAULT '',
	lower_view_fingerprint TEXT NOT NULL DEFAULT '',
	expected_active_generation_id INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`

// DedicatedBaseIdentity is the semantic identity of a complete committed view.
// Commit provenance is deliberately not part of tree-equivalent payload reuse.
type DedicatedBaseIdentity struct {
	TreeOID, ConfigHash, ExtractorVersions, ResolverVersion string
	// DependencyRevision identifies derived dependency inputs, not parent parse
	// policy. Empty preserves legacy/unproven output identity only.
	DependencyRevision string
}

type DedicatedBaseOwner struct {
	CheckoutID, Incarnation string
}

type DedicatedBaseAuthority struct {
	GraphID              string
	Owner                DedicatedBaseOwner
	RepoPrefix, FamilyID string
	// GenerationFloor is an exclusive creation high-water mark. Immutable
	// generations do not otherwise persist checkout incarnation provenance.
	GenerationFloor int64
	Epoch           int64
	Token           string
}

type DedicatedBaseDesire struct {
	Authority DedicatedBaseAuthority
	Epoch     int64
	Identity  DedicatedBaseIdentity
}

type AcquireDedicatedBaseAuthorityRequest struct {
	GraphID       string
	Owner         DedicatedBaseOwner
	ExpectedEpoch int64
	ExpectedToken string
	Token         string
}

type RecordDedicatedBaseDesireRequest struct {
	Authority            DedicatedBaseAuthority
	ExpectedDesiredEpoch int64
	Identity             DedicatedBaseIdentity
}

type ClaimDedicatedBaseBuildRequest struct {
	// ExistingGenerationID selects validation-only mode when positive: the
	// same current generation and attempt must exist in a live attempt state.
	// This mode never binds, allocates or writes; zero preserves normal claims.
	ExistingGenerationID                               int64
	Desire                                             DedicatedBaseDesire
	ExpectedActiveGenerationID                         int64
	AttemptToken                                       string
	BaseGenerationID                                   int64
	LayerID, LowerViewFingerprint, ProvenanceCommitOID string
	CreatedAt                                          int64
}

type DedicatedBaseBuildClaim struct {
	Desire                                                     DedicatedBaseDesire
	AttemptToken                                               string
	GenerationID, BaseGenerationID, ExpectedActiveGenerationID int64
	LayerID, LowerViewFingerprint                              string
	// Status is allocated, building, or ready. An adopted claim returns ready
	// with AlreadyAdopted=true; its original attempt is preserved without writes.
	Status         string
	AlreadyAdopted bool
}

type AdoptDedicatedBaseGenerationRequest struct{ Claim DedicatedBaseBuildClaim }

// DedicatedBaseAdoption is what one adoption installed.
//
// PreviousGenerationID is the active pointer the adoption replaced, so a caller
// can tell an advance (previous != GenerationID) from a replay of the pointer
// that was already installed. TreeOID and CommitOID are the committed point the
// adopted generation represents, read from the generation row rather than from
// the request: a claim that resolved to an already-ready candidate names the
// tree that candidate was built for, which is the tree the base actually holds.
// HeadAdvanced reports whether the owner checkout's head columns moved with it;
// false means a later observation had already moved them past this base, which
// is not a failure (see AdoptDedicatedBaseGeneration).
//
// PreviousSuperseded reports that the head this adoption replaced was labelled
// superseded in the same transaction. It is false whenever there was nothing to
// label — a first adoption, a replay, a previous head this adoption still
// descends from, or one already discarded by an earlier pass.
//
// AdoptedRestored reports the mirror write: the generation this adoption
// installed was itself carrying a superseded label from an earlier replacement
// and was cleared back to ready, because a graph's live head must not describe
// itself as replaced. It is false for the ordinary adoption of a generation
// that was already ready.
//
// A replay — AlreadyAdopted, the pointer already installed — reports the
// pointers alone. It moved nothing, so it reads nothing: the idle
// observe-claim-adopt cycle a warm daemon runs stays a cycle that performs no
// catalog work beyond the guards it has to check.
type DedicatedBaseAdoption struct {
	GenerationID         int64
	PreviousGenerationID int64
	TreeOID              string
	CommitOID            string
	HeadAdvanced         bool
	AlreadyAdopted       bool
	PreviousSuperseded   bool
	AdoptedRestored      bool
}
type FailDedicatedBaseBuildRequest struct {
	Claim DedicatedBaseBuildClaim
	Error string
}

type DedicatedBasePublication struct {
	Desire              DedicatedBaseDesire
	Claim               DedicatedBaseBuildClaim
	AttemptState, Error string
}

const maxDedicatedBaseAncestry = 64
const maxDedicatedBaseReuseCandidates = 16

var ErrDedicatedBaseCandidate = errors.New("invalid dedicated base candidate")

const dedicatedBasePublicationColumns = `graph_id, owner_checkout_id, owner_incarnation, owner_generation_floor, repo_prefix, family_id,
	authority_epoch, authority_token, desired_epoch, tree_oid, config_hash,
	extractor_versions, resolver_version, dependency_revision, attempt_token, attempt_state,
	generation_id, base_generation_id, layer_id, lower_view_fingerprint,
	expected_active_generation_id, error`

type dedicatedBaseQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func dedicatedBasePublicationRow(ctx context.Context, q dedicatedBaseQuerier, graphID string) (DedicatedBasePublication, bool, error) {
	var p DedicatedBasePublication
	a := &p.Desire.Authority
	i := &p.Desire.Identity
	err := q.QueryRowContext(ctx, `SELECT `+dedicatedBasePublicationColumns+` FROM dedicated_base_publications WHERE graph_id=?`, graphID).Scan(
		&a.GraphID, &a.Owner.CheckoutID, &a.Owner.Incarnation, &a.GenerationFloor, &a.RepoPrefix, &a.FamilyID, &a.Epoch, &a.Token,
		&p.Desire.Epoch, &i.TreeOID, &i.ConfigHash, &i.ExtractorVersions, &i.ResolverVersion, &i.DependencyRevision,
		&p.Claim.AttemptToken, &p.AttemptState, &p.Claim.GenerationID, &p.Claim.BaseGenerationID,
		&p.Claim.LayerID, &p.Claim.LowerViewFingerprint, &p.Claim.ExpectedActiveGenerationID, &p.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return DedicatedBasePublication{}, false, nil
	}
	if err != nil {
		return DedicatedBasePublication{}, false, err
	}
	p.Claim.Desire = p.Desire
	p.Claim.Status = p.AttemptState
	if p.AttemptState == "adopted" {
		p.Claim.Status, p.Claim.AlreadyAdopted = "ready", true
	}
	return p, true, nil
}

func (c *Catalog) DedicatedBasePublication(ctx context.Context, graphID string) (DedicatedBasePublication, bool, error) {
	return dedicatedBasePublicationRow(ctx, c.store.db, graphID)
}

func dedicatedBaseStale(reason string) error {
	return fmt.Errorf("%w: dedicated base %s", ErrCatalogStaleGuard, reason)
}

// Only the initial, verified authorization scope is implemented: an available
// steady dedicated owner. Promotion must gain journal-backed authorization in
// a separate reviewed extension; caller-supplied mode tuples cannot authorize it.
func dedicatedBaseOwnerTx(ctx context.Context, tx *sql.Tx, graphID string, owner DedicatedBaseOwner, expected ...DedicatedBaseAuthority) (int64, error) {
	var checkoutID, incarnation, graphState, checkoutState, desired, effective, transition string
	var repoPrefix, familyID string
	var active int64
	err := tx.QueryRowContext(ctx, `SELECT d.owner_checkout_id, c.incarnation,
		d.state, c.state, c.desired_mode, c.effective_mode,
		COALESCE(c.active_intent_transition_id,''), COALESCE(d.active_generation_id,0), d.repo_prefix, d.family_id
		FROM dedicated_graphs d JOIN checkouts c ON c.checkout_id=d.owner_checkout_id AND c.family_id=d.family_id
		WHERE d.graph_id=?`, graphID).Scan(&checkoutID, &incarnation, &graphState,
		&checkoutState, &desired, &effective, &transition, &active, &repoPrefix, &familyID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, dedicatedBaseStale("owner missing")
	}
	if err != nil {
		return 0, err
	}
	if checkoutID != owner.CheckoutID || incarnation != owner.Incarnation || graphState != DedicatedGraphReady ||
		checkoutState != string(CheckoutStateReady) || desired != string(CheckoutModeDedicated) ||
		effective != string(CheckoutModeDedicated) || transition != "" {
		return 0, dedicatedBaseStale("owner unavailable or not steady dedicated")
	}
	if len(expected) == 1 && (expected[0].RepoPrefix != repoPrefix || expected[0].FamilyID != familyID) {
		return 0, dedicatedBaseStale("graph namespace or family changed")
	}
	return active, nil
}

func (c *Catalog) AcquireDedicatedBaseAuthority(ctx context.Context, req AcquireDedicatedBaseAuthorityRequest) (DedicatedBaseAuthority, error) {
	if req.GraphID == "" || req.Owner.CheckoutID == "" || req.Owner.Incarnation == "" || req.Token == "" || req.ExpectedEpoch < 0 {
		return DedicatedBaseAuthority{}, fmt.Errorf("dedicated base authority requires graph, owner, incarnation and token")
	}
	var out DedicatedBaseAuthority
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := dedicatedBaseOwnerTx(ctx, tx, req.GraphID, req.Owner); err != nil {
			return err
		}
		p, found, err := dedicatedBasePublicationRow(ctx, tx, req.GraphID)
		if err != nil {
			return err
		}
		var repoPrefix, familyID string
		if err := tx.QueryRowContext(ctx, `SELECT repo_prefix,family_id FROM dedicated_graphs WHERE graph_id=?`, req.GraphID).Scan(&repoPrefix, &familyID); err != nil {
			return err
		}
		if !found {
			if req.ExpectedEpoch != 0 || req.ExpectedToken != "" {
				return dedicatedBaseStale("authority absent")
			}
			out = DedicatedBaseAuthority{GraphID: req.GraphID, Owner: req.Owner, RepoPrefix: repoPrefix, FamilyID: familyID, Epoch: 1, Token: req.Token}
			// generation_id is INTEGER PRIMARY KEY AUTOINCREMENT. Capture once,
			// without a payload scan, in the same transaction as graph ownership.
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation_id),0) FROM view_generations`).Scan(&out.GenerationFloor); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO dedicated_base_publications
				(graph_id,owner_checkout_id,owner_incarnation,owner_generation_floor,repo_prefix,family_id,authority_epoch,authority_token) VALUES (?,?,?,?,?,?,?,?)`,
				req.GraphID, req.Owner.CheckoutID, req.Owner.Incarnation, out.GenerationFloor, repoPrefix, familyID, out.Epoch, req.Token)
			return err
		}
		a := p.Desire.Authority
		if a.Owner != req.Owner || a.RepoPrefix != repoPrefix || a.FamilyID != familyID {
			return dedicatedBaseStale("authority owner, namespace or family changed")
		}
		if a.Token == req.Token {
			out = a
			return nil
		} // lost-response retry
		if a.Epoch != req.ExpectedEpoch || a.Token != req.ExpectedToken || a.Epoch == math.MaxInt64 {
			return dedicatedBaseStale("authority changed")
		}
		out = a
		out.Epoch++
		out.Token = req.Token
		_, err = tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET authority_epoch=?, authority_token=?,
			attempt_token='', attempt_state='idle', generation_id=0, base_generation_id=0,
			layer_id='', lower_view_fingerprint='', expected_active_generation_id=0, error='' WHERE graph_id=?`,
			out.Epoch, out.Token, out.GraphID)
		return err
	})
	if err != nil {
		return DedicatedBaseAuthority{}, err
	}
	return out, nil
}

// RecordDedicatedBaseDesire requires the caller's shared per-graph
// observe-through-record lock. This API does not order filesystem observations.
func (c *Catalog) RecordDedicatedBaseDesire(ctx context.Context, req RecordDedicatedBaseDesireRequest) (DedicatedBaseDesire, error) {
	if req.Identity.TreeOID == "" || req.Identity.ConfigHash == "" || req.Identity.ExtractorVersions == "" || req.Identity.ResolverVersion == "" {
		return DedicatedBaseDesire{}, fmt.Errorf("dedicated base desire requires a committed tree and complete policy identity")
	}
	var out DedicatedBaseDesire
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := dedicatedBaseOwnerTx(ctx, tx, req.Authority.GraphID, req.Authority.Owner, req.Authority); err != nil {
			return err
		}
		p, found, err := dedicatedBasePublicationRow(ctx, tx, req.Authority.GraphID)
		if err != nil {
			return err
		}
		if !found || p.Desire.Authority != req.Authority || p.Desire.Epoch != req.ExpectedDesiredEpoch {
			return dedicatedBaseStale("desire fence changed")
		}
		out = p.Desire
		if out.Epoch > 0 && out.Identity == req.Identity {
			return nil
		}
		if out.Epoch == math.MaxInt64 {
			return dedicatedBaseStale("desire epoch exhausted")
		}
		out.Epoch++
		out.Identity = req.Identity
		_, err = tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET desired_epoch=?,tree_oid=?,config_hash=?,
			extractor_versions=?,resolver_version=?,dependency_revision=?,attempt_token='',attempt_state='idle',generation_id=0,
			base_generation_id=0,layer_id='',lower_view_fingerprint='',expected_active_generation_id=0,error='' WHERE graph_id=?`,
			out.Epoch, out.Identity.TreeOID, out.Identity.ConfigHash, out.Identity.ExtractorVersions, out.Identity.ResolverVersion, out.Identity.DependencyRevision, out.Authority.GraphID)
		return err
	})
	if err != nil {
		return DedicatedBaseDesire{}, err
	}
	return out, nil
}

func dedicatedBaseGenerationTx(ctx context.Context, tx *sql.Tx, id int64) (ViewGeneration, error) {
	g := ViewGeneration{GenerationID: id}
	err := scanViewGeneration(tx.QueryRowContext(ctx, `SELECT `+viewGenerationColumns+` FROM view_generations WHERE generation_id=?`, id).Scan, &g)
	if errors.Is(err, sql.ErrNoRows) {
		return ViewGeneration{}, fmt.Errorf("%w: generation %d missing", ErrDedicatedBaseCandidate, id)
	}
	return g, err
}

func validateDedicatedBaseAncestryTx(ctx context.Context, tx *sql.Tx, desire DedicatedBaseDesire, id int64, exactTree bool) (ViewGeneration, error) {
	limit := maxDedicatedBaseAncestry
	if !exactTree {
		// A proposed parent must leave room for the new child. Reject an
		// over-deep build before allocating payload that could never adopt.
		limit--
	}
	return validateDedicatedBaseChainTx(ctx, tx, desire, id, exactTree, false, limit)
}

func validateDedicatedBaseChainTx(ctx context.Context, tx *sql.Tx, desire DedicatedBaseDesire, id int64, exactTree, allowBuildingCandidate bool, limit int) (ViewGeneration, error) {
	var first ViewGeneration
	seen := make(map[int64]bool)
	for depth := 0; id > 0; depth++ {
		if id <= desire.Authority.GenerationFloor {
			return ViewGeneration{}, fmt.Errorf("%w: generation %d predates owner publication authority", ErrDedicatedBaseCandidate, id)
		}
		if depth >= limit || seen[id] {
			return ViewGeneration{}, fmt.Errorf("%w: cyclic or excessive ancestry", ErrDedicatedBaseCandidate)
		}
		seen[id] = true
		g, err := dedicatedBaseGenerationTx(ctx, tx, id)
		if err != nil {
			return ViewGeneration{}, err
		}
		servableState := g.State == ViewGenerationReady || g.State == ViewGenerationSuperseded || (depth == 0 && allowBuildingCandidate && g.State == ViewGenerationBuilding)
		if g.OwnerKind != "dedicated_graph" || g.GenerationKind != "dedicated" || g.GraphID != desire.Authority.GraphID ||
			g.CheckoutID != desire.Authority.Owner.CheckoutID || !servableState || g.TreeOID == "" ||
			g.ConfigHash != desire.Identity.ConfigHash || g.ExtractorVersions != desire.Identity.ExtractorVersions || g.ResolverVersion != desire.Identity.ResolverVersion || g.BaseGenerationID < 0 {
			return ViewGeneration{}, fmt.Errorf("%w: generation %d has incompatible ownership, policy or state", ErrDedicatedBaseCandidate, id)
		}
		// The output candidate must always match current dependency inputs, and
		// a COMPLETE output must compose only layers frozen under them. A head
		// that matches over an older lower is a mixed composition: handing one
		// back as ready — or certifying one at adoption — gives the publisher a
		// delta parent its builder must refuse, which wedges publication for as
		// long as the tree is unchanged. Two deliberate exceptions keep their
		// latitude below depth 0: a merely PROPOSED parent (exactTree=false),
		// which may still supply the lower for a derived-only refresh, and a
		// still-BUILDING candidate, whose parent composition belongs to the
		// builder's own typed refusal before any payload is written.
		if exactTree && g.DependencyRevision != desire.Identity.DependencyRevision && (depth == 0 || !allowBuildingCandidate) {
			return ViewGeneration{}, fmt.Errorf("%w: generation %d dependency revision mismatch", ErrDedicatedBaseCandidate, id)
		}
		if depth == 0 {
			first = g
			if exactTree && g.TreeOID != desire.Identity.TreeOID {
				return ViewGeneration{}, fmt.Errorf("%w: tree mismatch", ErrDedicatedBaseCandidate)
			}
		}
		id = g.BaseGenerationID
	}
	if first.GenerationID <= 0 {
		return ViewGeneration{}, fmt.Errorf("%w: candidate must be positive", ErrDedicatedBaseCandidate)
	}
	return first, nil
}

func dedicatedBaseClaimMatches(a, b DedicatedBaseBuildClaim) bool {
	return a.Desire == b.Desire && a.AttemptToken == b.AttemptToken && a.GenerationID == b.GenerationID &&
		a.BaseGenerationID == b.BaseGenerationID && a.LayerID == b.LayerID && a.LowerViewFingerprint == b.LowerViewFingerprint &&
		a.ExpectedActiveGenerationID == b.ExpectedActiveGenerationID
}

func validateDedicatedBaseClaimTx(ctx context.Context, tx *sql.Tx, claim DedicatedBaseBuildClaim, allowBuilding ...bool) error {
	allow := len(allowBuilding) == 1 && allowBuilding[0]
	g, err := validateDedicatedBaseChainTx(ctx, tx, claim.Desire, claim.GenerationID, true, allow, maxDedicatedBaseAncestry)
	if err != nil {
		return err
	}
	if g.BaseGenerationID != claim.BaseGenerationID || g.LayerID != claim.LayerID || g.LowerViewFingerprint != claim.LowerViewFingerprint {
		return fmt.Errorf("%w: physical identity mismatch", ErrDedicatedBaseCandidate)
	}
	return nil
}

func bindDedicatedBaseClaimTx(ctx context.Context, tx *sql.Tx, claim DedicatedBaseBuildClaim, state string) error {
	_, err := tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET attempt_token=?,attempt_state=?,generation_id=?,
		base_generation_id=?,layer_id=?,lower_view_fingerprint=?,expected_active_generation_id=?,error='' WHERE graph_id=?`,
		claim.AttemptToken, state, claim.GenerationID, claim.BaseGenerationID, claim.LayerID, claim.LowerViewFingerprint,
		claim.ExpectedActiveGenerationID, claim.Desire.Authority.GraphID)
	return err
}

func (c *Catalog) ClaimDedicatedBaseBuild(ctx context.Context, req ClaimDedicatedBaseBuildRequest) (DedicatedBaseBuildClaim, error) {
	if req.AttemptToken == "" || req.BaseGenerationID < 0 || req.ExpectedActiveGenerationID < 0 || req.ExistingGenerationID < 0 {
		return DedicatedBaseBuildClaim{}, fmt.Errorf("dedicated base claim requires token and nonnegative generations")
	}
	var out DedicatedBaseBuildClaim
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		active, err := dedicatedBaseOwnerTx(ctx, tx, req.Desire.Authority.GraphID, req.Desire.Authority.Owner, req.Desire.Authority)
		if err != nil {
			return err
		}
		p, found, err := dedicatedBasePublicationRow(ctx, tx, req.Desire.Authority.GraphID)
		if err != nil {
			return err
		}
		if !found || p.Desire != req.Desire || p.Desire.Epoch <= 0 {
			return dedicatedBaseStale("claim desire changed")
		}
		if req.ExistingGenerationID > 0 && (p.Claim.GenerationID != req.ExistingGenerationID ||
			p.Claim.AttemptToken != req.AttemptToken ||
			(p.AttemptState != "building" && p.AttemptState != "ready" && p.AttemptState != "adopted")) {
			return dedicatedBaseStale("existing claim changed or is no longer live")
		}
		// Preserve the original adopted attempt before comparing its old expected
		// pointer with the caller's now-current active pointer. This is the idle
		// observe→claim→adopt zero-write path, not a new publication attempt.
		if p.AttemptState == "adopted" && active == p.Claim.GenerationID {
			if err := validateDedicatedBaseClaimTx(ctx, tx, p.Claim); err != nil {
				return err
			}
			out = p.Claim
			return nil
		}
		if p.AttemptState == "adopted" {
			return dedicatedBaseStale("adopted active pointer was changed outside publication")
		}
		if active != req.ExpectedActiveGenerationID {
			return dedicatedBaseStale("claim active pointer changed")
		}
		var buildingGeneration ViewGeneration
		// A physical leader normally marks both records failed. If its bounded
		// notification was lost (including process exit), recover only a fully
		// verified current Failed payload. Validation-only callers must never
		// enter this allocating path, and a healthy Building row still coalesces.
		if p.AttemptState == "building" && req.ExistingGenerationID == 0 {
			if p.Claim.ExpectedActiveGenerationID != active {
				return dedicatedBaseStale("existing claim active pointer changed")
			}
			var failed bool
			buildingGeneration, failed, err = failedDedicatedBaseClaimTx(ctx, tx, p.Claim)
			if err != nil {
				return err
			}
			if failed {
				// Only local state changes here. The replacement binding below is
				// the sole publication write, in the same guarded transaction.
				p.AttemptState = "failed"
			}
		}
		if p.AttemptState == "building" || p.AttemptState == "ready" {
			if p.Claim.ExpectedActiveGenerationID != active {
				return dedicatedBaseStale("existing claim active pointer changed")
			}
			if p.AttemptState == "ready" {
				if err := validateDedicatedBaseClaimTx(ctx, tx, p.Claim); err != nil {
					return err
				}
			} else {
				if err := validateDedicatedBaseClaimTx(ctx, tx, p.Claim, true); err != nil {
					return err
				}
				// Ordinary claims already read this row while checking failure
				// recovery. Reuse it within the same transaction rather than
				// adding a third metadata query to healthy build coalescing.
				g := buildingGeneration
				if req.ExistingGenerationID > 0 {
					g, err = dedicatedBaseGenerationTx(ctx, tx, p.Claim.GenerationID)
					if err != nil {
						return err
					}
				}
				if g.State == ViewGenerationReady || g.State == ViewGenerationSuperseded {
					if err := validateDedicatedBaseClaimTx(ctx, tx, p.Claim); err != nil {
						return err
					}
					p.Claim.Status = "ready"
				} else if g.State != ViewGenerationBuilding {
					return fmt.Errorf("%w: claimed build is no longer building", ErrDedicatedBaseCandidate)
				}
			}
			out = p.Claim
			return nil
		}
		// Keep validation-only requests out of every binding/allocation path,
		// including if future attempt-state handling adds another fallthrough.
		if req.ExistingGenerationID > 0 {
			return dedicatedBaseStale("existing claim cannot be validated")
		}
		if p.Claim.AttemptToken != "" && req.AttemptToken == p.Claim.AttemptToken {
			return dedicatedBaseStale("retired attempt token reused")
		}
		out = DedicatedBaseBuildClaim{Desire: req.Desire, AttemptToken: req.AttemptToken, ExpectedActiveGenerationID: active}
		// Metadata-only bounded lookup; no payload scan. Close rows before ancestry
		// queries so the same transaction never depends on concurrent row cursors.
		rows, err := tx.QueryContext(ctx, `SELECT generation_id FROM view_generations WHERE generation_id>? AND owner_kind='dedicated_graph'
			AND generation_kind='dedicated' AND graph_id=? AND checkout_id=? AND tree_oid=? AND config_hash=?
			AND extractor_versions=? AND resolver_version=? AND dependency_revision=? AND state IN ('ready','superseded') ORDER BY generation_id DESC LIMIT ?`,
			req.Desire.Authority.GenerationFloor, req.Desire.Authority.GraphID, req.Desire.Authority.Owner.CheckoutID, req.Desire.Identity.TreeOID,
			req.Desire.Identity.ConfigHash, req.Desire.Identity.ExtractorVersions, req.Desire.Identity.ResolverVersion, req.Desire.Identity.DependencyRevision, maxDedicatedBaseReuseCandidates)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for _, id := range ids {
			g, e := validateDedicatedBaseAncestryTx(ctx, tx, req.Desire, id, true)
			if errors.Is(e, ErrDedicatedBaseCandidate) {
				continue
			}
			if e != nil {
				return e
			}
			out.GenerationID, out.BaseGenerationID, out.LayerID, out.LowerViewFingerprint, out.Status = g.GenerationID, g.BaseGenerationID, g.LayerID, g.LowerViewFingerprint, "ready"
			return bindDedicatedBaseClaimTx(ctx, tx, out, "ready")
		}
		if req.BaseGenerationID > 0 {
			if _, err := validateDedicatedBaseAncestryTx(ctx, tx, req.Desire, req.BaseGenerationID, false); err != nil {
				return err
			}
		}
		g := ViewGeneration{OwnerKind: "dedicated_graph", GraphID: req.Desire.Authority.GraphID, CheckoutID: req.Desire.Authority.Owner.CheckoutID,
			GenerationKind: "dedicated", LayerID: req.LayerID, BaseGenerationID: req.BaseGenerationID, LowerViewFingerprint: req.LowerViewFingerprint,
			TreeOID: req.Desire.Identity.TreeOID, ConfigHash: req.Desire.Identity.ConfigHash, ExtractorVersions: req.Desire.Identity.ExtractorVersions,
			ResolverVersion: req.Desire.Identity.ResolverVersion, DependencyRevision: req.Desire.Identity.DependencyRevision,
			ProvenanceCommitOID: req.ProvenanceCommitOID, CreatedAt: req.CreatedAt, State: ViewGenerationBuilding}
		if err := g.validate(); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, insertViewGenerationSQL, viewGenerationInsertArgs(g)...)
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if id <= 0 {
			return fmt.Errorf("%w: allocation returned nonpositive ID", ErrDedicatedBaseCandidate)
		}
		out.GenerationID, out.BaseGenerationID, out.LayerID, out.LowerViewFingerprint, out.Status = id, g.BaseGenerationID, g.LayerID, g.LowerViewFingerprint, "allocated"
		return bindDedicatedBaseClaimTx(ctx, tx, out, "building")
	})
	if err != nil {
		return DedicatedBaseBuildClaim{}, err
	}
	return out, nil
}

// AdoptDedicatedBaseGeneration installs a built generation as the graph's
// committed base.
//
// It advances two identities for the same base in one transaction. The first is
// dedicated_graphs.active_generation_id, the pointer every dependent's
// graphBase reads. The second is the owner checkout's head_tree / head_commit:
// until this write existed, checkouts.head_tree moved only when a reconciliation
// pass sampled the working copy (internal/reconcile/reconcile.go applyPresent,
// observeNew), so between a commit and the next pass — an hour by default — the
// two identities for one base disagreed, and the fallback that reads the
// checkout row (checkout_coordinator.go graphBase, for a graph with no published
// generation) named a tree the family had already moved past. Both pointers now
// stand or fall together: an adoption that cannot commit leaves the checkout row
// exactly as it found it.
//
// The head advance is fenced three ways. Owner and incarnation come from the
// claim's authority, which dedicatedBaseOwnerTx has already matched against the
// live row in this transaction. The previous active pointer is the CAS below.
// And the observation clock keeps the advance monotonic against the
// reconciliation passes that write the same columns: an adopted generation
// carries the clock of the observation it was claimed for, so a base built from
// an older sample cannot rewind a head a later pass already moved. That last
// guard is not an error — the checkout row is then AHEAD of this base, which is
// a true statement about a family that kept committing, and the generation is
// still adopted.
func (c *Catalog) AdoptDedicatedBaseGeneration(ctx context.Context, req AdoptDedicatedBaseGenerationRequest) (DedicatedBaseAdoption, error) {
	claim := req.Claim
	var out DedicatedBaseAdoption
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		out = DedicatedBaseAdoption{}
		active, err := dedicatedBaseOwnerTx(ctx, tx, claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner, claim.Desire.Authority)
		if err != nil {
			return err
		}
		p, found, err := dedicatedBasePublicationRow(ctx, tx, claim.Desire.Authority.GraphID)
		if err != nil {
			return err
		}
		if !found || !dedicatedBaseClaimMatches(p.Claim, claim) {
			return dedicatedBaseStale("adoption attempt changed")
		}
		if p.AttemptState != "building" && p.AttemptState != "ready" && p.AttemptState != "adopted" {
			return dedicatedBaseStale("attempt cannot adopt")
		}
		if err := validateDedicatedBaseClaimTx(ctx, tx, claim); err != nil {
			return err
		}
		out.GenerationID = claim.GenerationID
		out.PreviousGenerationID = active
		if p.AttemptState == "adopted" && active == claim.GenerationID {
			out.AlreadyAdopted = true
			return nil
		}
		if p.AttemptState == "adopted" {
			return dedicatedBaseStale("adopted active pointer was changed outside publication")
		}
		if active != claim.ExpectedActiveGenerationID {
			return dedicatedBaseStale("adoption active pointer changed")
		}
		if err := execGuardedTx(ctx, tx, "dedicated base active pointer", `UPDATE dedicated_graphs SET active_generation_id=? WHERE graph_id=? AND COALESCE(active_generation_id,0)=?`, claim.GenerationID, claim.Desire.Authority.GraphID, claim.ExpectedActiveGenerationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET attempt_state='adopted',error='' WHERE graph_id=?`, claim.Desire.Authority.GraphID); err != nil {
			return err
		}
		adopted, err := dedicatedBaseGenerationTx(ctx, tx, claim.GenerationID)
		if err != nil {
			return err
		}
		out.TreeOID, out.CommitOID = adopted.TreeOID, adopted.ProvenanceCommitOID
		out.PreviousSuperseded, err = supersedeReplacedDedicatedHeadTx(
			ctx, tx, claim.Desire.Authority, claim.BaseGenerationID, active, claim.GenerationID)
		if err != nil {
			return err
		}
		out.AdoptedRestored, err = restoreAdoptedDedicatedHeadTx(
			ctx, tx, claim.Desire.Authority, claim.GenerationID)
		if err != nil {
			return err
		}
		out.HeadAdvanced, err = advanceDedicatedBaseOwnerHeadTx(ctx, tx, claim.Desire.Authority.Owner, adopted)
		return err
	})
	if err != nil {
		return DedicatedBaseAdoption{}, err
	}
	announceDedicatedBaseAdoption(c.store, DedicatedBaseAdoptionEvent{
		GraphID:    claim.Desire.Authority.GraphID,
		FamilyID:   claim.Desire.Authority.FamilyID,
		RepoPrefix: claim.Desire.Authority.RepoPrefix,
		Owner:      claim.Desire.Authority.Owner,
		Adoption:   out,
	})
	return out, nil
}

// supersedeReplacedDedicatedHeadTx labels the head an adoption replaced.
//
// Until this write existed nothing ever moved an adopted-then-replaced
// dedicated generation off ready: adoption repointed active_generation_id and
// stamped the publication adopted, and the generation it displaced kept the
// label of a live head. Every retirement enumeration reads state, so a chain
// replaced by a new full root — or by a revert onto an older tree-equal
// candidate — was unreachable by any sweep and stayed in the database for the
// life of the installation.
//
// It labels only a head this adoption genuinely replaced. A previous head the
// adopted generation still descends from is an ANCESTOR of the live chain: it
// is composed into every view the new head serves, so calling it superseded
// would be a false statement about a generation that is still being read, and
// would put the whole live ancestry in front of the sweep on every pass. The
// common advance — a delta whose base IS the previous head — is answered by
// that first comparison without reading anything.
//
// The label is not retirement permission and does not shorten anything's life.
// A superseded generation stays servable (validateDedicatedBaseChainTx and
// graphview's dedicated-root check both accept it, and the reuse lookup still
// offers it), and collection remains the retirement predicate's decision: an
// ancestor of a live route, a dependent's named base and a leased generation
// are each refused there until nothing references them.
//
// The UPDATE is a guarded CAS on the full dedicated identity, so a legacy or
// foreign row cannot be relabelled by a graph that does not own it, and a row
// already superseded, retiring or failed is left exactly as it is. A no-op is
// therefore an ordinary outcome and reports false rather than an error.
func supersedeReplacedDedicatedHeadTx(
	ctx context.Context,
	tx *sql.Tx,
	authority DedicatedBaseAuthority,
	adoptedBase, previous, adopted int64,
) (bool, error) {
	// A pointer at or below the owner's publication floor predates this
	// authority: it is a CAS fence, not a generation this protocol published,
	// and relabelling it would be a write outside the scope that authorized it.
	if previous <= 0 || previous == adopted || previous <= authority.GenerationFloor {
		return false, nil
	}
	if adoptedBase == previous {
		return false, nil
	}
	ancestor, err := dedicatedBaseAncestorTx(ctx, tx, adoptedBase, previous)
	if err != nil {
		return false, err
	}
	if ancestor {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE view_generations SET state=? WHERE generation_id=? AND state=?
		AND owner_kind='dedicated_graph' AND generation_kind='dedicated' AND graph_id=? AND checkout_id=?`,
		string(ViewGenerationSuperseded), previous, string(ViewGenerationReady),
		authority.GraphID, authority.Owner.CheckoutID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// restoreAdoptedDedicatedHeadTx clears the superseded label off the generation
// this adoption just installed as the graph's live head.
//
// A revert re-adopts a generation an earlier adoption labelled superseded — the
// reuse lookup deliberately keeps offering those, which is what makes a revert
// cheap instead of a rebuild. Without this write the row would keep saying
// "superseded" while dedicated_graphs.active_generation_id names it: a false
// statement in the catalog that every status and diagnostic surface renders,
// and one that also hides a live head from the retirement sweep's ready-only
// deleted-graph cohort, so a graph deleted while its head carried a stale label
// would leave that head behind.
//
// The write is the mirror of supersedeReplacedDedicatedHeadTx and carries the
// same fences: the publication floor (a pointer at or below it predates this
// authority and is not a generation this protocol published) and a guarded CAS
// on the full dedicated identity plus the superseded label itself. So nothing
// else is ever rewritten, a foreign or legacy row is left exactly as it is, and
// the ordinary adoption of an already-ready generation is a no-op that reports
// false rather than an error.
func restoreAdoptedDedicatedHeadTx(
	ctx context.Context,
	tx *sql.Tx,
	authority DedicatedBaseAuthority,
	adopted int64,
) (bool, error) {
	if adopted <= 0 || adopted <= authority.GenerationFloor {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE view_generations SET state=? WHERE generation_id=? AND state=?
		AND owner_kind='dedicated_graph' AND generation_kind='dedicated' AND graph_id=? AND checkout_id=?`,
		string(ViewGenerationReady), adopted, string(ViewGenerationSuperseded),
		authority.GraphID, authority.Owner.CheckoutID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// dedicatedBaseAncestorTx reports whether candidate is on the chain under head.
//
// It walks base_generation_id, which is the same edge validateDedicatedBaseChainTx
// has already validated for the adopted generation in this transaction, so the
// walk is bounded by construction. Every way of failing to finish the walk —
// a cycle, a chain deeper than the hard ancestry limit, a row that cannot be
// read — answers true, because the only thing the caller does with false is
// discard a generation, and a walk that could not prove the candidate is
// unreachable is not evidence that it is.
func dedicatedBaseAncestorTx(ctx context.Context, tx *sql.Tx, head, candidate int64) (bool, error) {
	if head <= 0 || candidate <= 0 {
		return false, nil
	}
	seen := make(map[int64]struct{}, 8)
	for id := head; id > 0; {
		if id == candidate {
			return true, nil
		}
		if _, looped := seen[id]; looped || len(seen) >= maxDedicatedBaseAncestry {
			return true, nil
		}
		seen[id] = struct{}{}
		// base_generation_id is nullable and a full root stores NULL, not 0, so
		// the walk has to read the column the way every other reader does.
		var base int64
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(base_generation_id, 0) FROM view_generations WHERE generation_id=?`, id).Scan(&base)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		id = base
	}
	return false, nil
}

// adoptedHeadEpochTx reports the clock at which an adoption published the head
// a checkout currently carries, and 0 when no adoption published it.
//
// It is the durable half of the head fence, and it exists because the fact it
// answers has to survive a restart. The head an adoption published is, by
// construction, the tree of the generation the owner's dedicated graph is
// active on: advanceDedicatedBaseOwnerHeadTx writes head_tree from the adopted
// generation in the same transaction that installs the active pointer. So a
// stored head that still equals the active generation's tree IS that
// publication, and the generation's created_at is the sequence it was published
// at — which a process with no memory of the adoption can read back exactly.
//
// A head that does not match the active generation's tree was not published by
// the standing adoption, and the fence has nothing to hold: 0 means "an
// ordinary observation put this head here", which any later observation may
// move. An owner with no dedicated graph, or a graph with no active generation,
// is the same answer for the same reason.
func adoptedHeadEpochTx(ctx context.Context, tx *sql.Tx, checkoutID, storedHeadTree string) (int64, error) {
	if storedHeadTree == "" {
		return 0, nil
	}
	var epoch int64
	err := tx.QueryRowContext(ctx, `
SELECT g.created_at
  FROM dedicated_graphs d
  JOIN view_generations g ON g.generation_id = d.active_generation_id
 WHERE d.owner_checkout_id = ? AND g.tree_oid = ?`, checkoutID, storedHeadTree).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// advanceDedicatedBaseOwnerHeadTx moves the owner checkout's committed head to
// the point the adopted generation represents, in the adoption's transaction.
//
// The clock comparison is the whole fence. created_at on a generation is the
// clock of the observation the claim was made for (the publisher stamps it when
// it observes; see internal/indexer), and last_seen on a checkout is the clock
// of the last observation written for it, which is what
// UpdateCheckoutObservation is fenced on too. Writing the later of the two back
// into last_seen is what makes the two writers one ordered sequence instead of
// two racing ones: a reconciliation pass sampled before this base was observed
// can no longer overwrite the head this adoption just published.
//
// That last sentence is a contract, not a hope, and last_seen alone does not
// keep it: the observation fence rebases its clock domain after
// observationRegressionQuorum refusals, so the third pass from behind is
// accepted. What keeps the contract is that the head axis is excluded from that
// rebase — adoptedHeadEpochTx above tells the fence that this tree was
// published here, at this created_at, and a sample behind that sequence is
// refused on the head axis alone however the clock axes are resolved. See
// UpdateCheckoutObservation.
//
// A generation with no tree, and a checkout whose clock has already passed this
// observation, both leave the row alone and report false. Neither is an error:
// the first cannot describe a committed point at all, and the second means a
// later observation already won, which the catalog has no reason to undo.
// head_commit is written only when the generation carries a provenance commit —
// a claim satisfied by a tree-equal candidate built for another commit names no
// commit this owner is at, and the tree is the identity that matters.
func advanceDedicatedBaseOwnerHeadTx(ctx context.Context, tx *sql.Tx, owner DedicatedBaseOwner, adopted ViewGeneration) (bool, error) {
	if adopted.TreeOID == "" {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE checkouts
   SET head_tree = ?,
       head_commit = CASE WHEN ? = '' THEN head_commit ELSE ? END,
       last_seen = CASE WHEN last_seen < ? THEN ? ELSE last_seen END
 WHERE checkout_id = ? AND incarnation = ? AND last_seen <= ?`,
		adopted.TreeOID, adopted.ProvenanceCommitOID, adopted.ProvenanceCommitOID,
		adopted.CreatedAt, adopted.CreatedAt,
		owner.CheckoutID, owner.Incarnation, adopted.CreatedAt)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// DedicatedBaseAdoptionEvent is one adopted committed base, announced to the
// observers registered on the store it was adopted in.
//
// It exists because the two halves of a base advance are owned by different
// layers and share nothing but the database. The publisher that adopts holds no
// coordinator registry; the checkout lifecycle that runs the dependents' build
// loops never sees a publication. Without an announcement a dependent learns
// that its base moved only when its own poll comes round, which is the latency
// this event removes — it carries no payload and no authority, only the fact
// that the pointer moved and what it moved to.
type DedicatedBaseAdoptionEvent struct {
	GraphID    string
	FamilyID   string
	RepoPrefix string
	Owner      DedicatedBaseOwner
	Adoption   DedicatedBaseAdoption
}

// Advanced reports an event that moved the active pointer, as opposed to a
// replay of one that was already installed. A replay changes nothing a
// dependent could observe, so it is not a reason to wake one.
func (e DedicatedBaseAdoptionEvent) Advanced() bool {
	return e.Adoption.GenerationID > 0 && !e.Adoption.AlreadyAdopted &&
		e.Adoption.PreviousGenerationID != e.Adoption.GenerationID
}

// dedicatedBaseAdoptionObservers maps one open database — the storeCore every
// handle over it shares — to the observers registered on it.
//
// The key is the core rather than a *Store or a *Catalog because neither of
// those is stable: Store.Catalog() mints a fresh handle on every call, so the
// publisher's catalog and the lifecycle's catalog are different values over the
// same database. Keying on the core is what makes an observer registered
// through one of them reachable from the other, while two stores in one process
// — two daemons, two fixtures — keep their announcements apart.
var dedicatedBaseAdoptionObservers sync.Map // *storeCore -> *dedicatedBaseAdoptionRegistry

type dedicatedBaseAdoptionRegistry struct {
	mu   sync.Mutex
	next uint64
	// detached marks a registry the last release took out of the map. A
	// registration that raced that release holds a value nothing will ever
	// announce through, so it retries rather than registering into it.
	detached  bool
	observers map[uint64]func(DedicatedBaseAdoptionEvent)
}

// ObserveDedicatedBaseAdoptions registers fn to receive every adoption made
// against this catalog's database, and returns the idempotent release that
// unregisters it. A nil fn registers nothing.
//
// Observers run synchronously on the adopting goroutine, AFTER the adoption
// transaction has committed, so an observer sees a state it can read back — and
// so an observer that panics or blocks cannot roll back a published base. They
// must therefore do no more than hand the fact on.
func (c *Catalog) ObserveDedicatedBaseAdoptions(fn func(DedicatedBaseAdoptionEvent)) func() {
	if c == nil || c.store == nil || fn == nil {
		return func() {}
	}
	key := c.store.storeCore
	var registry *dedicatedBaseAdoptionRegistry
	var id uint64
	for {
		value, _ := dedicatedBaseAdoptionObservers.LoadOrStore(key, &dedicatedBaseAdoptionRegistry{})
		candidate, ok := value.(*dedicatedBaseAdoptionRegistry)
		if !ok {
			return func() {}
		}
		candidate.mu.Lock()
		if candidate.detached {
			// The last release took this one out of the map between the load
			// and the lock. Nothing announces through it any more, so take the
			// replacement instead of registering into a value nobody reads.
			candidate.mu.Unlock()
			continue
		}
		candidate.next++
		id = candidate.next
		if candidate.observers == nil {
			candidate.observers = map[uint64]func(DedicatedBaseAdoptionEvent){}
		}
		candidate.observers[id] = fn
		candidate.mu.Unlock()
		registry = candidate
		break
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.mu.Lock()
			delete(registry.observers, id)
			if len(registry.observers) == 0 {
				// The last observer for this database is gone. Detach under the
				// lock so a concurrent registration sees the flag rather than a
				// value that has left the map, and delete only this registry —
				// a replacement another goroutine already stored stays.
				registry.detached = true
				dedicatedBaseAdoptionObservers.CompareAndDelete(key, registry)
			}
			registry.mu.Unlock()
		})
	}
}

func announceDedicatedBaseAdoption(store *Store, event DedicatedBaseAdoptionEvent) {
	if store == nil {
		return
	}
	value, found := dedicatedBaseAdoptionObservers.Load(store.storeCore)
	if !found {
		return
	}
	registry, ok := value.(*dedicatedBaseAdoptionRegistry)
	if !ok {
		return
	}
	registry.mu.Lock()
	observers := make([]func(DedicatedBaseAdoptionEvent), 0, len(registry.observers))
	for _, observer := range registry.observers {
		observers = append(observers, observer)
	}
	registry.mu.Unlock()
	for _, observer := range observers {
		observer(event)
	}
}

func (c *Catalog) FailDedicatedBaseBuild(ctx context.Context, req FailDedicatedBaseBuildRequest) error {
	return c.withTx(ctx, func(tx *sql.Tx) error {
		claim := req.Claim
		if _, err := dedicatedBaseOwnerTx(ctx, tx, claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner, claim.Desire.Authority); err != nil {
			return err
		}
		p, found, err := dedicatedBasePublicationRow(ctx, tx, claim.Desire.Authority.GraphID)
		if err != nil {
			return err
		}
		if !found || !dedicatedBaseClaimMatches(p.Claim, claim) || p.AttemptState == "adopted" {
			return dedicatedBaseStale("failed attempt changed or adopted")
		}
		if p.AttemptState == "failed" {
			return nil
		}
		detail := req.Error
		if len(detail) > 1024 {
			detail = detail[:1024]
		}
		_, err = tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET attempt_state='failed',error=? WHERE graph_id=?`, detail, claim.Desire.Authority.GraphID)
		return err
	})
}
