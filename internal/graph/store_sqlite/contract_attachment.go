package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
	"slices"
)

var _ graph.ContractAttachmentReader = (*Store)(nil)

// GetContractAttachmentContext is an indexed exact historical identity lookup.
// It never scans actor history or falls back to its latest payload.
func (s *Store) GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key.InputVersion == "" || key.InputFingerprint == "" {
		return nil, fmt.Errorf("%w: empty attachment key", ErrCatalogInvalidValue)
	}
	if s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	a := &graph.ContractAttachment{RepoPrefix: key.RepoPrefix, CheckoutID: key.CheckoutID, InputVersion: key.InputVersion, InputFingerprint: key.InputFingerprint}
	err := s.db.QueryRowContext(ctx, `SELECT a.payload_generation FROM contract_attachments a JOIN view_generations v ON v.generation_id=a.payload_generation WHERE a.repo_prefix=? AND a.checkout_id=? AND a.input_version=? AND a.input_fingerprint=? AND v.state IN ('ready','superseded')`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint).Scan(&a.PayloadGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token FROM contract_attachment_work WHERE repo_prefix=? AND checkout_id=? AND attachment_version=? AND attachment_fingerprint=? ORDER BY token LIMIT 32769`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		a.CompletedTokens = append(a.CompletedTokens, token)
		if len(a.CompletedTokens) > contractWorkReadLimit {
			return nil, ErrContractWorkLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

// PublishContractAttachmentContext seals the isolated managed payload and binds
// it to the selected accepted input together with immutable debt acknowledgments.
// The receiver selects the core generation whose ancestry the worker captured;
// no mutable corpus clock or latest-actor attachment is consulted. Actual
// inherited0 inclusion is a trusted leased RepoView capture boundary: catalog
// ancestry alone cannot recover historical routed commitIndex/fullroot selection.
func (s *Store) PublishContractAttachmentContext(ctx context.Context, expected graph.ContractInputState, attachment graph.ContractAttachment, work []graph.ContractWork, publishedAt int64) error {
	return s.publishContractAttachment(ctx, expected, nil, attachment, work, publishedAt)
}

// PublishContractAttachmentWithInputsContext compares every exact selected
// physical witness while binding the deduplicated logical attachment identity.
func (s *Store) PublishContractAttachmentWithInputsContext(ctx context.Context, expected graph.ContractInputState, witnesses []graph.ContractInputWitness, attachment graph.ContractAttachment, work []graph.ContractWork, publishedAt int64) error {
	if len(witnesses) == 0 {
		return graph.ErrContractInputVector
	}
	return s.publishContractAttachment(ctx, expected, witnesses, attachment, work, publishedAt)
}

// ContractAttachmentPublishTarget binds a captured input to its selected core
// handle. Every handle in a batch must share the receiver's store core.
type ContractAttachmentPublishTarget struct {
	Store      *Store
	Expected   graph.ContractInputState
	Witnesses  []graph.ContractInputWitness
	Attachment graph.ContractAttachment
	Work       []graph.ContractWork
}

// ContractAttachmentPublication reports the immutable winning payload and the
// losing reservation, if any. The caller owns retirement of unused reservations.
type ContractAttachmentPublication struct {
	PayloadGeneration       int64
	UnusedPayloadGeneration int64
}

// PublishContractAttachmentsWithInputsContext publishes the entire cohort under
// one write gate and transaction. Results are returned only after commit and in
// target order. An empty cohort is a no-op; any refusal rolls back every target.
func (s *Store) PublishContractAttachmentsWithInputsContext(ctx context.Context, targets []ContractAttachmentPublishTarget, publishedAt int64) ([]ContractAttachmentPublication, error) {
	for _, target := range targets {
		if len(target.Witnesses) == 0 {
			return nil, graph.ErrContractInputVector
		}
	}
	return s.publishContractAttachments(ctx, targets, publishedAt, true)
}

func (s *Store) publishContractAttachment(ctx context.Context, expected graph.ContractInputState, witnesses []graph.ContractInputWitness, attachment graph.ContractAttachment, work []graph.ContractWork, publishedAt int64) error {
	_, err := s.publishContractAttachments(ctx, []ContractAttachmentPublishTarget{{Store: s, Expected: expected, Witnesses: witnesses, Attachment: attachment, Work: work}}, publishedAt, false)
	return err
}

func (s *Store) publishContractAttachments(ctx context.Context, targets []ContractAttachmentPublishTarget, publishedAt int64, allowDuplicate bool) ([]ContractAttachmentPublication, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	reservations := make(map[int64]bool, len(targets))
	for _, target := range targets {
		if reservations[target.Attachment.PayloadGeneration] {
			return nil, fmt.Errorf("%w: shared attachment reservation", ErrCatalogInvalidValue)
		}
		reservations[target.Attachment.PayloadGeneration] = true
		if target.Store == nil || target.Store.coreless() || target.Store.storeCore != s.storeCore {
			return nil, fmt.Errorf("%w: attachment stores differ", ErrCatalogInvalidValue)
		}
	}
	if len(targets) == 0 {
		return []ContractAttachmentPublication{}, nil
	}
	release, err := s.HoldWriteGate(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := s.AtGeneration(0).beginWriteContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	results := make([]ContractAttachmentPublication, len(targets))
	for i, target := range targets {
		if err := target.Store.validateContractAttachmentPublicationTx(ctx, tx, target.Expected, target.Witnesses, target.Attachment, target.Work, &results[i]); err != nil {
			return nil, err
		}
	}
	for i, target := range targets {
		if results[i].PayloadGeneration != 0 {
			if !allowDuplicate {
				return nil, fmt.Errorf("%w: attachment already published", ErrCatalogStaleGuard)
			}
			continue
		}
		if err := target.Store.writeContractAttachmentTx(ctx, tx, target.Expected, target.Witnesses, target.Attachment, target.Work, publishedAt); err != nil {
			return nil, err
		}
		results[i].PayloadGeneration = target.Attachment.PayloadGeneration
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Store) validateContractAttachmentPublicationTx(ctx context.Context, tx *sql.Tx, expected graph.ContractInputState, witnesses []graph.ContractInputWitness, attachment graph.ContractAttachment, work []graph.ContractWork, result *ContractAttachmentPublication) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if !expected.Accepted {
		return fmt.Errorf("%w: worker captured pending input", ErrCatalogStaleGuard)
	}
	if err := validateContractInput(expected); err != nil {
		return err
	}
	if attachment.PayloadGeneration <= 0 || attachment.PayloadGeneration == s.viewGen || attachment.RepoPrefix != expected.RepoPrefix || attachment.CheckoutID != expected.CheckoutID || attachment.InputVersion != expected.InputVersion || attachment.InputFingerprint != expected.InputFingerprint {
		return fmt.Errorf("%w: attachment identity", ErrCatalogInvalidValue)
	}
	if len(work) > contractWorkReadLimit {
		return ErrContractWorkLimit
	}
	encoded, err := encodeContractWork(work)
	if err != nil {
		return err
	}
	tokens := make([]string, len(work))
	for i, row := range work {
		if row.State != graph.ContractWorkPending || row.RepoPrefix != expected.RepoPrefix || (!contractWorkActorSelected(expected, witnesses, row.CheckoutID)) {
			return fmt.Errorf("%w: attachment work namespace/state", ErrCatalogInvalidValue)
		}
		tokens[i] = row.Token
	}
	slices.Sort(tokens)
	declared := slices.Clone(attachment.CompletedTokens)
	slices.Sort(declared)
	if !slices.Equal(tokens, declared) {
		return fmt.Errorf("%w: attachment acknowledgment batch", ErrCatalogInvalidValue)
	}
	// The attachment row is a retirement reference to its payload generation
	// (contractAttachmentReferenceSQL). The guarded building -> ready UPDATE
	// below already rolls back an attachment to a generation that is no longer
	// building; this refuses a retiring one up front with the typed admission
	// error the other reference writers return, before the ancestry work.
	if err := validateViewGenerationAdmissionTx(ctx, tx, attachment.PayloadGeneration); err != nil {
		return err
	}
	var chain []int64
	if len(witnesses) == 0 {
		chain, err = contractInputAncestryTx(ctx, tx, s.viewGen, expected)
	} else {
		var logical graph.ContractInputState
		logical, err = graph.ComposeContractInputState(expected.RepoPrefix, expected.CheckoutID, witnesses)
		if err == nil && (!logical.Accepted || !sameContractInput(logical, expected)) {
			err = ErrCatalogStaleGuard
		}
		if err == nil {
			if source := s.contractPublicationSource; source != nil {
				if source.expected != expected || !slices.Equal(source.witnesses, witnesses) {
					return ErrCatalogStaleGuard
				}
				chain = source.chain
			} else {
				chain, err = contractGenerationAncestryTx(ctx, tx, s.viewGen)
			}
		}
		if err == nil {
			err = validateContractInputWitnessesTx(ctx, tx, witnesses)
		}
	}
	if err != nil {
		return err
	}
	if len(witnesses) > 0 {
		if err := validateSelectedContractWitnessAncestry(chain, expected.RepoPrefix, witnesses); err != nil {
			return err
		}
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT payload_generation FROM contract_attachments WHERE repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=?`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if existing <= 0 || existing == s.viewGen {
			return ErrCatalogInvalidValue
		}
		if err := validateViewGenerationAdmissionTx(ctx, tx, existing); err != nil {
			return err
		}
		var existingState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, existing).Scan(&existingState); err != nil {
			return err
		}
		if existingState != string(ViewGenerationReady) && existingState != string(ViewGenerationSuperseded) {
			return ErrCatalogStaleGuard
		}
	}
	payloadToCheck := attachment.PayloadGeneration
	if existing > 0 {
		payloadToCheck = existing
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM generation_producer_completeness WHERE view_gen=? AND producer='graph.contracts'`, payloadToCheck).Scan(&state); err != nil {
		return err
	}
	if state != string(ProducerStateComplete) {
		return fmt.Errorf("%w: contract payload incomplete", ErrCatalogInvalidValue)
	}
	var masks int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM generation_file_masks WHERE view_gen=?)+(SELECT count(*) FROM generation_node_tombstones WHERE view_gen=?)+(SELECT count(*) FROM generation_edge_sources WHERE view_gen=?)`, payloadToCheck, payloadToCheck, payloadToCheck).Scan(&masks); err != nil {
		return err
	}
	if masks != 0 {
		return fmt.Errorf("%w: isolated attachment cannot mask core", ErrCatalogInvalidValue)
	}
	if existing > 0 {
		if err := validatePublishedContractAttachmentTx(ctx, tx, expected, witnesses, work, encoded); err != nil {
			return err
		}
		result.PayloadGeneration = existing
		if existing != attachment.PayloadGeneration {
			result.UnusedPayloadGeneration = attachment.PayloadGeneration
		}
		return nil
	}
	// SQL NULL catalog bases do not encode live primary inheritance. A checked
	// own-repo found primary witness is the explicit trusted capture authority.
	workChain := slices.Clone(chain)
	for _, witness := range witnesses {
		if witness.GenerationID == 0 && witness.Found && witness.State.Accepted && witness.State.RepoPrefix == expected.RepoPrefix && !slices.Contains(workChain, int64(0)) {
			workChain = append(workChain, 0)
		}
	}
	for i, row := range work {
		if err := validateCapturedContractWorkTx(ctx, tx, workChain, row, string(encoded[i])); err != nil {
			return err
		}
	}
	var payloadState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, attachment.PayloadGeneration).Scan(&payloadState); err != nil {
		return err
	}
	if payloadState != string(ViewGenerationBuilding) {
		return ErrCatalogStaleGuard
	}
	return nil
}

// writeContractAttachmentTx runs only after every target has been validated.
func (s *Store) writeContractAttachmentTx(ctx context.Context, tx *sql.Tx, expected graph.ContractInputState, witnesses []graph.ContractInputWitness, attachment graph.ContractAttachment, work []graph.ContractWork, publishedAt int64) error {
	encoded, err := encodeContractWork(work)
	if err != nil {
		return err
	}
	// Immutable key: a competing publication cannot replace historical bytes.
	if _, err := tx.ExecContext(ctx, `INSERT INTO contract_attachments(repo_prefix,checkout_id,input_version,input_fingerprint,payload_generation) VALUES(?,?,?,?,?)`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint, attachment.PayloadGeneration); err != nil {
		return err
	}
	effective := effectiveContractWitnesses(witnesses)
	for i, w := range witnesses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO contract_attachment_inputs(repo_prefix,checkout_id,input_version,input_fingerprint,source_generation,source_repo,source_checkout,source_version,source_fingerprint,source_accepted,source_found,source_effective) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint, w.GenerationID, w.State.RepoPrefix, w.State.CheckoutID, w.State.InputVersion, w.State.InputFingerprint, w.State.Accepted, w.Found, effective[i]); err != nil {
			return err
		}
	}
	for i, row := range work {
		if _, err := tx.ExecContext(ctx, `INSERT INTO contract_attachment_work(repo_prefix,checkout_id,work_checkout_id,attachment_version,attachment_fingerprint,token,origin_generation,file_path,input_version,input_fingerprint,scope) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, row.RepoPrefix, expected.CheckoutID, row.CheckoutID, expected.InputVersion, expected.InputFingerprint, row.Token, row.OriginGeneration, row.FilePath, row.InputVersion, row.InputFingerprint, string(encoded[i])); err != nil {
			return err
		}
	}
	// Primary completion reclaims only this exact captured batch; no foreground
	// mutation or other actor pays a lifetime acknowledgment scan.
	if s.viewGen == 0 {
		for i, row := range work {
			if _, err := tx.ExecContext(ctx, `DELETE FROM generation_contract_work WHERE view_gen=0 AND token=? AND repo_prefix=? AND checkout_id=? AND origin_generation=? AND file_path=? AND input_version=? AND input_fingerprint=? AND scope=?`, row.Token, row.RepoPrefix, row.CheckoutID, row.OriginGeneration, row.FilePath, row.InputVersion, row.InputFingerprint, string(encoded[i])); err != nil {
				return err
			}
		}
	}
	if err := execGuardedTx(ctx, tx, "contract attachment payload is not building", `UPDATE view_generations SET state=?,published_at=? WHERE generation_id=? AND state=?`, string(ViewGenerationReady), publishedAt, attachment.PayloadGeneration, string(ViewGenerationBuilding)); err != nil {
		return err
	}
	return nil
}

// Duplicate validation uses immutable acknowledgments because successful primary
// publication may already have deleted the live captured debt.
func validatePublishedContractAttachmentTx(ctx context.Context, tx *sql.Tx, expected graph.ContractInputState, witnesses []graph.ContractInputWitness, work []graph.ContractWork, encoded [][]byte) error {
	args := []any{expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contract_attachment_work WHERE repo_prefix=? AND checkout_id=? AND attachment_version=? AND attachment_fingerprint=?`, args...).Scan(&count); err != nil {
		return err
	}
	if count != len(work) {
		return ErrCatalogStaleGuard
	}
	for i, row := range work {
		var matches int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM contract_attachment_work WHERE repo_prefix=? AND checkout_id=? AND attachment_version=? AND attachment_fingerprint=? AND token=? AND work_checkout_id=? AND origin_generation=? AND file_path=? AND input_version=? AND input_fingerprint=? AND scope=?`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint, row.Token, row.CheckoutID, row.OriginGeneration, row.FilePath, row.InputVersion, row.InputFingerprint, string(encoded[i])).Scan(&matches); err != nil {
			return err
		}
		if matches != 1 {
			return ErrCatalogStaleGuard
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_generation,source_repo,source_checkout,source_version,source_fingerprint,source_accepted,source_found,source_effective FROM contract_attachment_inputs WHERE repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=? ORDER BY source_generation`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var stored []graph.ContractInputWitness
	var effective []bool
	for rows.Next() {
		var w graph.ContractInputWitness
		var selected bool
		if err := rows.Scan(&w.GenerationID, &w.State.RepoPrefix, &w.State.CheckoutID, &w.State.InputVersion, &w.State.InputFingerprint, &w.State.Accepted, &w.Found, &selected); err != nil {
			return err
		}
		stored = append(stored, w)
		effective = append(effective, selected)
		if len(stored) > contractWorkReadLimit {
			return ErrContractWorkLimit
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(witnesses) == 0 {
		if len(stored) != 0 {
			return ErrCatalogStaleGuard
		}
		return nil
	}
	logical, err := graph.ComposeContractInputState(expected.RepoPrefix, expected.CheckoutID, stored)
	if err != nil || !sameContractInput(logical, expected) || !logical.Accepted {
		return ErrCatalogStaleGuard
	}
	selected := effectiveContractWitnesses(stored)
	for i, flag := range effective {
		if flag != selected[i] {
			return ErrCatalogStaleGuard
		}
	}
	// Copies into later selected generations change physical provenance without
	// changing the logical proof. Compare cohort membership and effective inputs,
	// ignoring physical generation and actor provenance.
	type proof struct {
		Repo, Version, Fingerprint string
		Found                      bool
	}
	normalize := func(ws []graph.ContractInputWitness) map[proof]bool {
		out := make(map[proof]bool)
		eff := effectiveContractWitnesses(ws)
		for i, w := range ws {
			out[proof{Repo: w.State.RepoPrefix}] = true
			if eff[i] {
				out[proof{Repo: w.State.RepoPrefix, Version: w.State.InputVersion, Fingerprint: w.State.InputFingerprint, Found: w.Found}] = true
			}
		}
		return out
	}
	a, b := normalize(stored), normalize(witnesses)
	if len(a) != len(b) {
		return ErrCatalogStaleGuard
	}
	for key := range a {
		if !b[key] {
			return ErrCatalogStaleGuard
		}
	}
	return nil
}

func contractInputAncestryTx(ctx context.Context, tx *sql.Tx, generation int64, expected graph.ContractInputState) ([]int64, error) {
	var chain []int64
	matched := false
	for steps := 0; steps < 1024; steps++ {
		chain = append(chain, generation)
		if !matched {
			var v, f string
			var accepted bool
			err := tx.QueryRowContext(ctx, `SELECT input_version,input_fingerprint,accepted FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, generation, expected.RepoPrefix, expected.CheckoutID).Scan(&v, &f, &accepted)
			if err == nil {
				if !accepted || v != expected.InputVersion || f != expected.InputFingerprint {
					return nil, ErrCatalogStaleGuard
				}
				matched = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		if generation == 0 {
			if !matched {
				return nil, ErrCatalogStaleGuard
			}
			return chain, nil
		}
		var base sql.NullInt64
		var coreState string
		if err := tx.QueryRowContext(ctx, `SELECT base_generation_id,state FROM view_generations WHERE generation_id=?`, generation).Scan(&base, &coreState); err != nil {
			return nil, err
		}
		if coreState != string(ViewGenerationReady) && coreState != string(ViewGenerationSuperseded) {
			return nil, ErrCatalogStaleGuard
		}
		if !base.Valid {
			if !matched {
				return nil, ErrCatalogStaleGuard
			}
			return chain, nil
		}
		if base.Int64 >= generation || base.Int64 < 0 {
			return nil, ErrCatalogStaleGuard
		}
		generation = base.Int64
	}
	return nil, fmt.Errorf("%w: contract ancestry limit", ErrCatalogInvalidValue)
}

func validateCapturedContractWorkTx(ctx context.Context, tx *sql.Tx, chain []int64, w graph.ContractWork, scope string) error {
	for _, generation := range chain {
		var origin int64
		var checkout, repo, file, version, fingerprint, state, storedScope string
		err := tx.QueryRowContext(ctx, `SELECT origin_generation,checkout_id,repo_prefix,file_path,input_version,input_fingerprint,state,scope FROM generation_contract_work WHERE view_gen=? AND token=?`, generation, w.Token).Scan(&origin, &checkout, &repo, &file, &version, &fingerprint, &state, &storedScope)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if origin != w.OriginGeneration || checkout != w.CheckoutID || repo != w.RepoPrefix || file != w.FilePath || version != w.InputVersion || fingerprint != w.InputFingerprint || state != string(graph.ContractWorkPending) || storedScope != scope {
			return ErrCatalogStaleGuard
		}
		return nil
	}
	return ErrCatalogStaleGuard
}

func contractGenerationAncestryTx(ctx context.Context, tx *sql.Tx, generation int64) ([]int64, error) {
	var chain []int64
	for steps := 0; steps < 1024; steps++ {
		chain = append(chain, generation)
		if generation == 0 {
			return chain, nil
		}
		var base sql.NullInt64
		var coreState string
		if err := tx.QueryRowContext(ctx, `SELECT base_generation_id,state FROM view_generations WHERE generation_id=?`, generation).Scan(&base, &coreState); err != nil {
			return nil, err
		}
		if coreState != string(ViewGenerationReady) && coreState != string(ViewGenerationSuperseded) {
			return nil, ErrCatalogStaleGuard
		}
		if !base.Valid {
			return chain, nil
		}
		if base.Int64 >= generation || base.Int64 < 0 {
			return nil, ErrCatalogStaleGuard
		}
		generation = base.Int64
	}
	return nil, graph.ErrContractInputVector
}

func validateContractInputWitnessesTx(ctx context.Context, tx *sql.Tx, witnesses []graph.ContractInputWitness) error {
	type witnessKey struct {
		generation     int64
		repo, checkout string
	}
	seen := make(map[witnessKey]bool)
	for _, w := range witnesses {
		key := witnessKey{w.GenerationID, w.State.RepoPrefix, w.State.CheckoutID}
		if seen[key] {
			return graph.ErrContractInputVector
		}
		seen[key] = true
		if w.GenerationID != 0 {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, w.GenerationID).Scan(&state); err != nil {
				return err
			}
			if state != string(ViewGenerationReady) && state != string(ViewGenerationSuperseded) {
				return ErrCatalogStaleGuard
			}
		}
		actual := graph.ContractInputState{RepoPrefix: w.State.RepoPrefix, CheckoutID: w.State.CheckoutID}
		err := tx.QueryRowContext(ctx, `SELECT input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, w.GenerationID, w.State.RepoPrefix, w.State.CheckoutID).Scan(&actual.InputVersion, &actual.InputFingerprint, &actual.Accepted, &actual.PreviousInputVersion, &actual.PreviousInputFingerprint)
		if errors.Is(err, sql.ErrNoRows) {
			if w.Found {
				return ErrCatalogStaleGuard
			}
			continue
		}
		if err != nil {
			return err
		}
		if !w.Found || actual != w.State {
			return ErrCatalogStaleGuard
		}
	}
	return nil
}

// Cumulative positive authority means GC depends on top logical components,
// while every historical/absence row remains part of publication's exact CAS.
func effectiveContractWitnesses(w []graph.ContractInputWitness) map[int]bool {
	type pair struct{ base, positive int }
	selected := make(map[string]pair)
	for i, row := range w {
		if !row.Found {
			continue
		}
		p, ok := selected[row.State.RepoPrefix]
		if !ok {
			p = pair{-1, -1}
		}
		if row.GenerationID == 0 {
			p.base = i
		} else {
			p.positive = i
		}
		selected[row.State.RepoPrefix] = p
	}
	out := make(map[int]bool)
	for _, p := range selected {
		if p.base >= 0 {
			out[p.base] = true
		}
		if p.positive >= 0 {
			out[p.positive] = true
		}
	}
	return out
}

func contractWorkActorSelected(expected graph.ContractInputState, witnesses []graph.ContractInputWitness, actor string) bool {
	if len(witnesses) == 0 {
		return actor == expected.CheckoutID
	}
	for _, w := range witnesses {
		if w.Found && w.State.RepoPrefix == expected.RepoPrefix && w.State.CheckoutID == actor {
			return true
		}
	}
	return false
}

func validateSelectedContractWitnessAncestry(chain []int64, repo string, witnesses []graph.ContractInputWitness) error {
	ranks := make(map[int64]int)
	for i, id := range chain {
		ranks[id] = i
	}
	covered := make(map[int64]bool)
	lastByRepo := make(map[string]int)
	for _, w := range witnesses {
		if w.GenerationID == 0 {
			continue
		} // actual inherited0 remains trusted selected-view capture
		rank, ok := ranks[w.GenerationID]
		if !ok {
			return ErrCatalogStaleGuard
		}
		last, seen := lastByRepo[w.State.RepoPrefix]
		if !seen {
			last = len(chain)
		}
		if rank > last {
			return ErrCatalogStaleGuard
		}
		lastByRepo[w.State.RepoPrefix] = rank
		if w.State.RepoPrefix == repo {
			covered[w.GenerationID] = true
		}
	}
	// A companion may share the selected receiver's positive ancestry, but may
	// never substitute a sibling source. Own-repo absence coverage is mandatory.
	for _, id := range chain {
		if id != 0 && !covered[id] {
			return ErrCatalogStaleGuard
		}
	}
	return nil
}
