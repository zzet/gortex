package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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
type DedicatedBaseAdoption struct {
	GenerationID   int64
	AlreadyAdopted bool
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
		if depth == 0 {
			first = g
			if exactTree && g.TreeOID != desire.Identity.TreeOID {
				return ViewGeneration{}, fmt.Errorf("%w: tree mismatch", ErrDedicatedBaseCandidate)
			}
			// Only the output candidate must match current dependency inputs.
			// A same-policy parent may supply the lower for a derived-only refresh.
			if exactTree && g.DependencyRevision != desire.Identity.DependencyRevision {
				return ViewGeneration{}, fmt.Errorf("%w: dependency revision mismatch", ErrDedicatedBaseCandidate)
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

func (c *Catalog) AdoptDedicatedBaseGeneration(ctx context.Context, req AdoptDedicatedBaseGenerationRequest) (DedicatedBaseAdoption, error) {
	claim := req.Claim
	var out DedicatedBaseAdoption
	err := c.withTx(ctx, func(tx *sql.Tx) error {
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
		_, err = tx.ExecContext(ctx, `UPDATE dedicated_base_publications SET attempt_state='adopted',error='' WHERE graph_id=?`, claim.Desire.Authority.GraphID)
		return err
	})
	if err != nil {
		return DedicatedBaseAdoption{}, err
	}
	return out, nil
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
