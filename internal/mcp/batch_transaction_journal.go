package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/gitstate"
)

const batchTransactionDirEnv = "GORTEX_BATCH_TRANSACTION_DIR"

func batchTransactionRoot() string {
	if root := os.Getenv(batchTransactionDirEnv); root != "" {
		return filepath.Clean(root)
	}
	return filepath.Join(daemon.StateDir(), "batch-transactions")
}

func batchTransactionDir(transactionID string) string {
	sum := sha256.Sum256([]byte(transactionID))
	return filepath.Join(batchTransactionRoot(), hex.EncodeToString(sum[:16]))
}

func batchManifestPath(transactionID string) string {
	return filepath.Join(batchTransactionDir(transactionID), "manifest.json")
}

func digestBatchBytes(content []byte) string {
	return sha256Hex(content)
}

func (s *Server) persistBatchManifest(receipt batchTransactionReceipt) error {
	if receipt.TransactionID == "" {
		return nil
	}
	path := batchManifestPath(receipt.TransactionID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := s.batchDurability().writeFile(path, payload, 0o600); err != nil {
		return err
	}
	// The transaction directory sync persists backup and manifest renames;
	// syncing its parent persists creation of a new transaction directory.
	return s.syncBatchDirectories(dir, filepath.Dir(dir))
}

func readBatchManifest(transactionID string) (batchTransactionReceipt, bool, error) {
	payload, err := os.ReadFile(batchManifestPath(transactionID))
	if errors.Is(err, os.ErrNotExist) {
		return batchTransactionReceipt{}, false, nil
	}
	if err != nil {
		return batchTransactionReceipt{}, false, err
	}
	var receipt batchTransactionReceipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return batchTransactionReceipt{}, false, fmt.Errorf("decode transaction manifest: %w", err)
	}
	if receipt.Version != batchTransactionVersion {
		return batchTransactionReceipt{}, false, fmt.Errorf("unsupported transaction manifest version %d", receipt.Version)
	}
	if receipt.TransactionID != transactionID {
		return batchTransactionReceipt{}, false, fmt.Errorf("transaction manifest identity mismatch")
	}
	return receipt, true, nil
}

func existingBatchTransactionAction(state *batchTransactionState) string {
	receipt := state.snapshot()
	if receipt.Status == "prepared" {
		select {
		case <-state.done:
			return "recover"
		default:
		}
	}
	if receipt.Status == "committed" && receipt.GraphStatus != "fresh" {
		return "refresh_graph"
	}
	return "existing"
}

func (s *Server) loadOrCreateBatchTransaction(ctx context.Context, transactionID, fingerprint string) (*batchTransactionState, string, error) {
	if value, ok := s.batchTransactions.Load(transactionID); ok {
		state, valid := value.(*batchTransactionState)
		if !valid {
			return nil, "", fmt.Errorf("invalid transaction state for %q", transactionID)
		}
		if fingerprint != "" && state.fingerprint != fingerprint {
			return nil, "", fmt.Errorf("transaction_id %q is already bound to a different edit payload", transactionID)
		}
		if err := s.validateBatchCheckout(ctx, state.snapshot()); err != nil {
			return nil, "", err
		}
		return state, existingBatchTransactionAction(state), nil
	}

	persisted, found, err := readBatchManifest(transactionID)
	if err != nil {
		return nil, "", err
	}
	if found {
		if err := s.validateBatchCheckout(ctx, persisted); err != nil {
			return nil, "", err
		}
		if fingerprint != "" && persisted.Fingerprint != fingerprint {
			return nil, "", fmt.Errorf("transaction_id %q is already bound to a different edit payload", transactionID)
		}
		terminal := persisted.Status != "prepared" && (persisted.Status != "committed" || persisted.GraphStatus != "pending")
		state := &batchTransactionState{
			fingerprint: persisted.Fingerprint,
			done:        make(chan struct{}),
			receipt:     persisted,
		}
		if terminal {
			state.doneOnce.Do(func() { close(state.done) })
		}
		actual, loaded := s.batchTransactions.LoadOrStore(transactionID, state)
		if loaded {
			existing := actual.(*batchTransactionState)
			if fingerprint != "" && existing.fingerprint != fingerprint {
				return nil, "", fmt.Errorf("transaction_id %q is already bound to a different edit payload", transactionID)
			}
			if err := s.validateBatchCheckout(ctx, existing.snapshot()); err != nil {
				return nil, "", err
			}
			return existing, existingBatchTransactionAction(existing), nil
		}
		if persisted.Status == "prepared" {
			return state, "recover", nil
		}
		if persisted.Status == "committed" && persisted.GraphStatus == "pending" {
			return state, "refresh_graph", nil
		}
		return state, existingBatchTransactionAction(state), nil
	}
	if fingerprint == "" {
		return nil, "", fmt.Errorf("transaction %q not found", transactionID)
	}
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: transactionID, Fingerprint: fingerprint,
		Status: "preparing", DiskStatus: "unchanged", GraphStatus: "not_started", StartedAt: time.Now().UTC(),
	}
	if mutation := checkoutMutationFromContext(ctx); mutation != nil {
		receipt.CheckoutID, receipt.Incarnation, receipt.CheckoutRoot = mutation.checkoutID, mutation.incarnation, mutation.root
		receipt.RootIdentity = gitstate.SamplePathEvidence(mutation.root).RootIdentity
		if authority, ok := mutation.mutation.(checkoutBatchAuthority); ok {
			receipt.CheckoutGeneration, receipt.HeadRef, receipt.HeadCommit, receipt.HeadTree = authority.BatchAuthority()
		}
	}
	state := &batchTransactionState{fingerprint: fingerprint, done: make(chan struct{}), receipt: receipt}
	actual, loaded := s.batchTransactions.LoadOrStore(transactionID, state)
	if loaded {
		existing := actual.(*batchTransactionState)
		if existing.fingerprint != fingerprint {
			return nil, "", fmt.Errorf("transaction_id %q is already bound to a different edit payload", transactionID)
		}
		if err := s.validateBatchCheckout(ctx, existing.snapshot()); err != nil {
			return nil, "", err
		}
		return existing, existingBatchTransactionAction(existing), nil
	}
	return state, "execute", nil
}

func (s *Server) prepareBatchJournal(receipt *batchTransactionReceipt, buffers map[string]*batchFileBuffer, orderedPaths []string) error {
	dir := batchTransactionDir(receipt.TransactionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := make([]batchTransactionFile, 0, len(orderedPaths))
	for i, path := range orderedPaths {
		buffer := buffers[path]
		if !buffer.existenceSet {
			return fmt.Errorf("batch buffer existence state is unset for %s", buffer.relPath)
		}
		existsBefore, existsAfter := buffer.existsBefore, buffer.existsAfter
		file := batchTransactionFile{
			Path: path, RelativePath: buffer.relPath, Mode: buffer.mode,
			BeforeAbsent: !existsBefore, AfterAbsent: !existsAfter,
		}
		if existsBefore {
			backupName := fmt.Sprintf("before-%04d.bin", i)
			backupPath := filepath.Join(dir, backupName)
			if err := s.batchDurability().writeFile(backupPath, buffer.original, 0o600); err != nil {
				return fmt.Errorf("write backup for %s: %w", buffer.relPath, err)
			}
			file.BeforeSHA256 = digestBatchBytes(buffer.original)
			file.Backup = backupName
		}
		if existsAfter {
			file.AfterSHA256 = digestBatchBytes(buffer.content)
		}
		files = append(files, file)
		// Keep the receipt aware of every completed backup so an error on a
		// later file still cleans the already-durable partial journal.
		receipt.Files = append([]batchTransactionFile(nil), files...)
	}
	receipt.Files = files
	receipt.Status, receipt.DiskStatus, receipt.GraphStatus = "prepared", "unchanged", "not_started"
	if err := s.persistBatchManifest(*receipt); err != nil {
		return err
	}
	return nil
}

func readBatchBackup(receipt batchTransactionReceipt, file batchTransactionFile) ([]byte, error) {
	if file.Backup == "" || filepath.Base(file.Backup) != file.Backup {
		return nil, fmt.Errorf("invalid backup name for %s", file.RelativePath)
	}
	payload, err := os.ReadFile(filepath.Join(batchTransactionDir(receipt.TransactionID), file.Backup))
	if err != nil {
		return nil, err
	}
	if digestBatchBytes(payload) != file.BeforeSHA256 {
		return nil, fmt.Errorf("backup hash mismatch for %s", file.RelativePath)
	}
	return payload, nil
}

func classifyBatchFiles(files []batchTransactionFile) (before, after, unknown []batchTransactionFile, err error) {
	for _, file := range files {
		content, readErr := os.ReadFile(file.Path)
		if errors.Is(readErr, os.ErrNotExist) {
			switch {
			case file.BeforeAbsent:
				before = append(before, file)
			case file.AfterAbsent:
				after = append(after, file)
			default:
				unknown = append(unknown, file)
				if err == nil {
					err = fmt.Errorf("%s is unexpectedly absent", file.RelativePath)
				}
			}
			continue
		}
		if readErr != nil {
			unknown = append(unknown, file)
			if err == nil {
				err = fmt.Errorf("read %s: %w", file.RelativePath, readErr)
			}
			continue
		}
		digest := digestBatchBytes(content)
		switch {
		case !file.BeforeAbsent && digest == file.BeforeSHA256:
			before = append(before, file)
		case !file.AfterAbsent && digest == file.AfterSHA256:
			after = append(after, file)
		default:
			unknown = append(unknown, file)
			if err == nil {
				err = fmt.Errorf("%s has neither the before nor after transaction state", file.RelativePath)
			}
		}
	}
	return before, after, unknown, err
}

func (s *Server) rollbackBatchReceipt(receipt batchTransactionReceipt, contexts ...context.Context) (string, error) {
	for _, ctx := range contexts {
		for _, file := range receipt.Files {
			if err := guardCheckoutMutationPath(ctx, file.Path); err != nil {
				return "recovery_conflict", err
			}
		}
	}
	_, after, unknown, classifyErr := classifyBatchFiles(receipt.Files)
	if len(unknown) > 0 {
		return "recovery_conflict", fmt.Errorf("rollback refused unknown disk state: %w", classifyErr)
	}
	for _, file := range after {
		if file.BeforeAbsent {
			if err := s.batchDurability().removeFile(file.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "recovery_conflict", fmt.Errorf("remove rollback-created %s: %w", file.RelativePath, err)
			}
			continue
		}
		backup, err := readBatchBackup(receipt, file)
		if err != nil {
			return "recovery_conflict", fmt.Errorf("load rollback backup for %s: %w", file.RelativePath, err)
		}
		if err := s.batchDurability().writeFile(file.Path, backup, file.Mode); err != nil {
			return "recovery_conflict", fmt.Errorf("restore %s: %w", file.RelativePath, err)
		}
	}
	if len(after) > 0 {
		dirs := batchFileDirectories(after)
		if err := s.syncBatchDirectories(dirs...); err != nil {
			return "recovery_conflict", fmt.Errorf("persist rollback: %w", err)
		}
	}
	before, _, unknown, verifyErr := classifyBatchFiles(receipt.Files)
	if len(unknown) > 0 || len(before) != len(receipt.Files) {
		if verifyErr == nil {
			verifyErr = fmt.Errorf("not every file matches its before hash")
		}
		return "recovery_conflict", fmt.Errorf("rollback verification failed: %w", verifyErr)
	}
	return "aborted", nil
}

func (s *Server) recoverBatchTransaction(ctx context.Context, state *batchTransactionState) {
	state.recoveryMu.Lock()
	defer state.recoveryMu.Unlock()
	receipt := state.snapshot()
	if receipt.Status != "prepared" {
		return
	}
	if receipt.CheckoutID != "" {
		var release func()
		var err error
		ctx, release, err = s.acquireBatchRecovery(ctx, receipt)
		if err != nil {
			receipt.Error = err.Error()
			state.publish(receipt, true)
			return
		}
		defer release()
	}
	paths := make([]string, 0, len(receipt.Files))
	for _, file := range receipt.Files {
		paths = append(paths, file.Path)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	release, err := acquireMutationPaths(recoveryCtx, paths)
	if err != nil {
		s.finishBatchTransaction(state, receipt, "recovery_conflict", "conflict", "not_started", "could not acquire recovery locks: "+err.Error())
		return
	}
	defer release()

	before, after, unknown, classifyErr := classifyBatchFiles(receipt.Files)
	if receipt.CheckoutID != "" && receipt.HeadCommit == "" && len(before) != len(receipt.Files) {
		s.finishBatchTransaction(state, receipt, "recovery_conflict", "conflict", "not_started", "prepared checkout journal has no pre-write HEAD authority and disk is not unchanged")
		return
	}
	switch {
	case len(unknown) > 0:
		receipt.Recovered = true
		s.finishBatchTransaction(state, receipt, "recovery_conflict", "conflict", "not_started", "recovery refused unknown disk state: "+classifyErr.Error())
	case len(after) == len(receipt.Files):
		receipt.Recovered = true
		if err := s.syncBatchDirectories(batchFileDirectories(receipt.Files)...); err != nil {
			s.finishBatchTransaction(state, receipt, "recovery_conflict", "conflict", "not_started", "could not persist recovered commit: "+err.Error())
			return
		}
		for i := range receipt.Results {
			receipt.Results[i].Status = "applied"
		}
		receipt.Summary = batchSummary(receipt.Results)
		receipt.Status, receipt.DiskStatus, receipt.GraphStatus = "committed", "committed", "pending"
		receipt.Error = ""
		state.publish(receipt, false)
		s.refreshBatchGraph(ctx, state)
	case len(before) == len(receipt.Files):
		receipt.Recovered = true
		s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", "recovered prepared transaction before commit")
	default:
		if err := prepareBatchRecovery(ctx); err != nil {
			receipt.Error = err.Error()
			state.publish(receipt, true)
			return
		}
		status, rollbackErr := s.rollbackBatchReceipt(receipt, ctx)
		receipt.Recovered = true
		message := "recovered interrupted mixed commit by restoring original bytes"
		if rollbackErr != nil {
			message = rollbackErr.Error()
		}
		diskStatus := "rolled_back"
		if status == "recovery_conflict" {
			diskStatus = "conflict"
		}
		s.finishBatchTransaction(state, receipt, status, diskStatus, "not_started", message)
	}
}

func batchReceiptCleanupSafe(receipt batchTransactionReceipt) bool {
	switch receipt.DiskStatus {
	case "committed", "unchanged", "rolled_back":
		return true
	default:
		return false
	}
}

func (s *Server) cleanupBatchBackups(receipt batchTransactionReceipt) error {
	dir := batchTransactionDir(receipt.TransactionID)
	ops := s.batchDurability()
	removed := false
	var firstErr error
	for _, file := range receipt.Files {
		if file.Backup == "" || filepath.Base(file.Backup) != file.Backup {
			continue
		}
		err := ops.removeFile(filepath.Join(dir, file.Backup))
		switch {
		case err == nil:
			removed = true
		case errors.Is(err, os.ErrNotExist):
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if removed {
		if err := s.syncBatchDirectories(dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
