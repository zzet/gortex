package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

const batchTransactionVersion = 1

type plannedBatchEdit struct {
	edit            batchEditItem
	op              string
	order           int
	file            string
	absPath         string
	destination     string
	destinationPath string
	idx             int
	node            *graph.Node
	err             string
}

type batchFileBuffer struct {
	absPath      string
	relPath      string
	mode         os.FileMode // permission bits preserved when writing replacement files
	fileMode     os.FileMode // complete mode retained for symlink and regular-file checks
	original     []byte
	content      []byte
	existsBefore bool
	existsAfter  bool
	existenceSet bool
}

type batchTransactionFile struct {
	Path              string      `json:"path"`
	RelativePath      string      `json:"relative_path,omitempty"`
	Mode              os.FileMode `json:"mode"`
	BeforeSHA256      string      `json:"before_sha256,omitempty"`
	AfterSHA256       string      `json:"after_sha256,omitempty"`
	BeforeAbsent      bool        `json:"before_absent,omitempty"`
	AfterAbsent       bool        `json:"after_absent,omitempty"`
	Backup            string      `json:"backup,omitempty"`
	ReindexReceipt    string      `json:"reindex_receipt,omitempty"`
	ReindexGeneration uint64      `json:"reindex_generation,omitempty"`
}

type batchTransactionReceipt struct {
	Version       int                    `json:"version"`
	TransactionID string                 `json:"transaction_id"`
	Fingerprint   string                 `json:"fingerprint"`
	Status        string                 `json:"status"`
	DiskStatus    string                 `json:"disk_status"`
	GraphStatus   string                 `json:"graph_status"`
	Error         string                 `json:"error,omitempty"`
	Results       []batchEditResult      `json:"results,omitempty"`
	Summary       map[string]int         `json:"summary,omitempty"`
	Files         []batchTransactionFile `json:"files,omitempty"`
	StartedAt     time.Time              `json:"started_at"`
	CompletedAt   *time.Time             `json:"completed_at,omitempty"`
	Recovered     bool                   `json:"recovered,omitempty"`
}

type batchTransactionState struct {
	fingerprint string
	done        chan struct{}
	doneOnce    sync.Once
	graphMu     sync.Mutex
	mu          sync.RWMutex
	receipt     batchTransactionReceipt
}

// cloneBatchReceipt returns a receipt that shares no slice or map storage with
// the original. Both state boundaries copy through it, so a published receipt is
// owned solely by the state: a publisher stays free to keep mutating its own
// copy — runBatchTransaction stamps Results[i].Status = "applied" in place after
// publishing "prepared" — without writing into memory a concurrent reader holds.
func cloneBatchReceipt(receipt batchTransactionReceipt) batchTransactionReceipt {
	clone := receipt
	clone.Results = append([]batchEditResult(nil), receipt.Results...)
	clone.Files = append([]batchTransactionFile(nil), receipt.Files...)
	if receipt.Summary != nil {
		clone.Summary = make(map[string]int, len(receipt.Summary))
		for key, value := range receipt.Summary {
			clone.Summary[key] = value
		}
	}
	if receipt.CompletedAt != nil {
		completedAt := *receipt.CompletedAt
		clone.CompletedAt = &completedAt
	}
	return clone
}

func (state *batchTransactionState) snapshot() batchTransactionReceipt {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return cloneBatchReceipt(state.receipt)
}

func (state *batchTransactionState) publish(receipt batchTransactionReceipt, terminal bool) {
	// Copy before taking the lock: the clone reads only the publisher's own
	// storage, and the state must not retain a slice the publisher still holds.
	stored := cloneBatchReceipt(receipt)
	state.mu.Lock()
	state.receipt = stored
	state.mu.Unlock()
	if terminal {
		state.doneOnce.Do(func() { close(state.done) })
	}
}

func batchEditFingerprint(edits []batchEditItem) string {
	payload, _ := json.Marshal(edits)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func normalizeBatchTransactionID(requested, fingerprint string) (string, error) {
	id := strings.TrimSpace(requested)
	if id == "" {
		// Payload-derived IDs collide across repositories, worktrees, and later
		// intentional repetitions of the same edit. Idempotency therefore uses
		// an explicit caller key; ordinary calls receive a unique receipt ID.
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", fmt.Errorf("generate transaction_id: %w", err)
		}
		return "batch-" + fingerprint[:12] + "-" + hex.EncodeToString(nonce[:]), nil
	}
	if len(id) > 200 {
		return "", fmt.Errorf("transaction_id exceeds 200 characters")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("transaction_id contains control characters")
		}
	}
	return id, nil
}

func batchSummary(results []batchEditResult) map[string]int {
	summary := map[string]int{"applied": 0, "failed": 0, "skipped": 0, "total": len(results)}
	for _, result := range results {
		switch result.Status {
		case "applied":
			summary["applied"]++
		case "failed":
			summary["failed"]++
		case "skipped":
			summary["skipped"]++
		}
	}
	return summary
}

func batchFailureResults(plans []plannedBatchEdit, failedAt int, message string) []batchEditResult {
	results := make([]batchEditResult, len(plans))
	for i, plan := range plans {
		result := batchEditResult{Op: plan.op, SymbolID: plan.edit.SymbolID, FilePath: plan.file, DestinationPath: plan.destination, Status: "skipped"}
		if i == failedAt {
			result.Status = "failed"
			result.Error = message
		}
		results[i] = result
	}
	return results
}

func markBatchCommitFailure(results []batchEditResult, failedPath, message string) []batchEditResult {
	marked := append([]batchEditResult(nil), results...)
	for i := range marked {
		marked[i].Status = "skipped"
		marked[i].Error = ""
		if marked[i].FilePath == failedPath || marked[i].DestinationPath == failedPath {
			marked[i].Status = "failed"
			marked[i].Error = message
		}
	}
	return marked
}

func (s *Server) planBatchTransaction(ctx context.Context, edits []batchEditItem, resolvePaths bool) []plannedBatchEdit {
	plans := make([]plannedBatchEdit, 0, len(edits))
	for i, edit := range edits {
		plan := plannedBatchEdit{edit: edit, op: edit.kind(), idx: i, order: 50}
		switch plan.op {
		case "edit_file":
			plan.order = 1000
			plan.file = edit.Path
			switch {
			case edit.Path == "":
				plan.err = "edit_file op requires path"
			case edit.OldString == edit.NewString:
				plan.err = "old_string and new_string are identical"
			default:
				absPath, relPath, err := s.resolveFilePath(ctx, edit.Path)
				if err != nil {
					plan.err = err.Error()
				} else {
					plan.absPath, plan.file = absPath, relPath
				}
			}
		case "move_file":
			plan.order = 2000
			plan.file, plan.destination = edit.SourcePath, edit.DestinationPath
			switch {
			case edit.SourcePath == "":
				plan.err = "move_file op requires source"
			case edit.DestinationPath == "":
				plan.err = "move_file op requires destination"
			case !validBatchExpectedSHA256(edit.ExpectedSHA256):
				plan.err = "expected_sha256 must be exactly 64 hexadecimal characters"
			default:
				sourcePath, sourceRel, err := s.resolveFilePath(ctx, edit.SourcePath)
				if err != nil {
					plan.err = err.Error()
					break
				}
				destinationPath, destinationRel, err := s.resolveFilePath(ctx, edit.DestinationPath)
				if err != nil {
					plan.err = err.Error()
					break
				}
				if sourcePath == destinationPath {
					plan.err = "move_file source and destination resolve to the same path"
					break
				}
				plan.absPath, plan.file = sourcePath, sourceRel
				plan.destinationPath, plan.destination = destinationPath, destinationRel
			}
		case "delete_file":
			plan.order = 2000
			plan.file = edit.Path
			switch {
			case edit.Path == "":
				plan.err = "delete_file op requires path"
			case !validBatchExpectedSHA256(edit.ExpectedSHA256):
				plan.err = "expected_sha256 must be exactly 64 hexadecimal characters"
			default:
				absPath, relPath, err := s.resolveFilePath(ctx, edit.Path)
				if err != nil {
					plan.err = err.Error()
				} else {
					plan.absPath, plan.file = absPath, relPath
				}
			}
		case "edit_symbol":
			switch {
			case edit.SymbolID == "":
				plan.err = "edit_symbol op requires id"
			case edit.OldSource == edit.NewSource:
				plan.err = "old_source and new_source are identical"
			default:
				plan.node = s.engineFor(ctx).GetSymbol(edit.SymbolID)
				if plan.node == nil {
					plan.err = "symbol not found: " + edit.SymbolID
					break
				}
				plan.file = plan.node.FilePath
				if plan.node.StartLine == 0 || plan.node.EndLine == 0 {
					plan.err = "symbol has no line range"
					break
				}
				if resolvePaths {
					absPath, err := s.resolveNodePath(ctx, plan.node)
					if err != nil {
						plan.err = err.Error()
						break
					}
					plan.absPath = absPath
				}
				switch plan.node.Kind {
				case graph.KindInterface, graph.KindType:
					plan.order = 0
				case graph.KindFunction, graph.KindMethod:
					plan.order = 20
				}
			}
		default:
			plan.err = fmt.Sprintf("unsupported batch edit op %q", plan.op)
		}
		plans = append(plans, plan)
	}

	// A lifecycle operation owns the complete path state. Reject overlap with
	// any other operation rather than assigning surprising sequential semantics
	// to move/delete chains. Multiple content edits to one file remain supported.
	type pathOwner struct {
		index     int
		lifecycle bool
	}
	owners := make(map[string]pathOwner)
	for i := range plans {
		lifecycle := plans[i].op == "move_file" || plans[i].op == "delete_file"
		for _, path := range []string{plans[i].absPath, plans[i].destinationPath} {
			if path == "" {
				continue
			}
			if owner, exists := owners[path]; exists && (owner.lifecycle || lifecycle) {
				plans[i].err = fmt.Sprintf("file lifecycle operation overlaps batch item %d", plans[owner.index].idx+1)
				break
			}
			owners[path] = pathOwner{index: i, lifecycle: lifecycle}
		}
	}

	// Preserve the established definitions-before-callers behavior without
	// performing graph work while disk locks are held.
	for i := range plans {
		if plans[i].node == nil || (plans[i].node.Kind != graph.KindFunction && plans[i].node.Kind != graph.KindMethod) {
			continue
		}
		callers := s.engineFor(ctx).GetCallers(plans[i].edit.SymbolID, query.QueryOptions{Depth: 1, Limit: 100, Detail: "brief"})
		for _, caller := range callers.Nodes {
			for j := range plans {
				if caller.ID == plans[j].edit.SymbolID && plans[j].edit.SymbolID != plans[i].edit.SymbolID {
					plans[i].order = 10
					break
				}
			}
		}
	}

	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].order != plans[j].order {
			return plans[i].order < plans[j].order
		}
		if plans[i].file != plans[j].file {
			return plans[i].file < plans[j].file
		}
		return plans[i].idx < plans[j].idx
	})
	return plans
}

func validBatchExpectedSHA256(expected string) bool {
	if expected == "" {
		return true
	}
	decoded, err := hex.DecodeString(expected)
	return err == nil && len(decoded) == sha256.Size
}

// guardBatchLifecycleDestination rejects every symlink component below the
// lexical repository root. General file resolution permits in-repo symlinks,
// which is correct for reads, but a move destination must not redirect a
// transaction write through either a symlink leaf or a symlinked parent.
// The repository root itself is deliberately not inspected so checkouts under
// symlinked system prefixes remain valid.
func (s *Server) guardBatchLifecycleDestination(absPath string) error {
	cleanPath := filepath.Clean(absPath)
	root := ""
	considerRoot := func(candidate string) {
		candidate = filepath.Clean(candidate)
		if pathContainedIn(cleanPath, candidate) && len(candidate) > len(root) {
			root = candidate
		}
	}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			if candidate, ok := s.multiIndexer.RepoRoot(prefix); ok {
				considerRoot(candidate)
			}
		}
	}
	if s.indexer != nil && s.indexer.RootPath() != "" {
		considerRoot(s.indexer.RootPath())
	}
	if root == "" {
		return nil
	}

	rel, err := filepath.Rel(root, cleanPath)
	if err != nil {
		return fmt.Errorf("could not inspect move destination %s: %w", cleanPath, err)
	}
	current := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			return nil
		case statErr != nil:
			return fmt.Errorf("could not inspect move destination %s: %w", current, statErr)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("move destination contains symlink component %s", current)
		}
	}
	return nil
}

func (s *Server) validateBatchCreateTarget(absPath, relPath string) error {
	if err := s.guardBatchLifecycleDestination(absPath); err != nil {
		return err
	}
	_, err := os.Lstat(absPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("could not stat %s: %w", relPath, err)
	default:
		return fmt.Errorf("destination already exists")
	}
}

func (s *Server) readBatchBuffers(plans []plannedBatchEdit) (map[string]*batchFileBuffer, []string, error) {
	buffers := make(map[string]*batchFileBuffer)
	paths := make([]string, 0)
	add := func(path, relPath string) error {
		if _, exists := buffers[path]; exists {
			return nil
		}
		buffer := &batchFileBuffer{absPath: path, relPath: relPath, mode: 0o644, existenceSet: true}
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Missing paths are retained in the transaction snapshot so a move
			// destination can be created and rollback can prove it was absent.
		case err != nil:
			return fmt.Errorf("could not stat %s: %w", relPath, err)
		default:
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("could not read %s: %w", relPath, readErr)
			}
			buffer.fileMode = info.Mode()
			// fileMode intentionally describes the path itself for lifecycle
			// type checks, while mode follows symlinks so edit_file preserves
			// the permissions of the content source it is replacing.
			if followed, statErr := os.Stat(path); statErr == nil {
				buffer.mode = followed.Mode().Perm()
			}
			buffer.original = append([]byte(nil), content...)
			buffer.content = append([]byte(nil), content...)
			buffer.existsBefore = true
			buffer.existsAfter = true
		}
		buffers[path] = buffer
		paths = append(paths, path)
		return nil
	}
	for _, plan := range plans {
		if err := add(plan.absPath, plan.file); err != nil {
			return nil, nil, err
		}
		if plan.destinationPath != "" {
			if err := s.guardBatchLifecycleDestination(plan.destinationPath); err != nil {
				return nil, nil, err
			}
			if err := add(plan.destinationPath, plan.destination); err != nil {
				return nil, nil, err
			}
		}
	}
	sort.Strings(paths)
	return buffers, paths, nil
}

func applyBatchFileToContent(edit batchEditItem, content []byte) ([]byte, bool, error) {
	fileStr := string(content)
	matches := findEOLMatches(fileStr, edit.OldString)
	if matches.count == 0 {
		return nil, false, fmt.Errorf("old_string not found in file")
	}
	if matches.count > 1 && !edit.ReplaceAll {
		return nil, false, fmt.Errorf("old_string matches %d locations%s. Provide a larger fragment for uniqueness or set replace_all=true", matches.count, matchSpansHint(fileStr, matches.spans))
	}
	var newContent string
	normalized := false
	switch {
	case matches.normalized:
		limit := 1
		if edit.ReplaceAll {
			limit = -1
		}
		newContent = spliceSpansEOL(fileStr, matches.spans, edit.NewString, limit)
		normalized = true
	case edit.ReplaceAll:
		newContent = strings.ReplaceAll(fileStr, edit.OldString, edit.NewString)
	default:
		newContent = strings.Replace(fileStr, edit.OldString, edit.NewString, 1)
	}
	if newContent == fileStr {
		return nil, normalized, fmt.Errorf("old_string and new_string are identical after line-ending normalization")
	}
	return []byte(newContent), normalized, nil
}

func applyBatchSymbolToContent(edit batchEditItem, node *graph.Node, content []byte) ([]byte, bool, error) {
	fileStr := string(content)
	lines := strings.Split(fileStr, "\n")
	regionMatches := findEOLMatches(fileStr, edit.OldSource)
	symbolStart := 0
	rangeMatched := false
	if node.StartLine <= len(lines) && node.EndLine <= len(lines) {
		symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")
		effectiveStart := node.StartLine
		if findEOLMatches(symbolSource, edit.OldSource).count == 0 {
			expandedStart := node.StartLine - 1
			for expandedStart > 0 {
				trimmed := strings.TrimSpace(lines[expandedStart-1])
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") || trimmed == "" {
					expandedStart--
				} else {
					break
				}
			}
			if expandedStart < node.StartLine-1 {
				expanded := strings.Join(lines[expandedStart:node.EndLine], "\n")
				if findEOLMatches(expanded, edit.OldSource).count > 0 {
					symbolSource = expanded
					effectiveStart = expandedStart + 1
				}
			}
		}
		for i := 0; i < effectiveStart-1 && i < len(lines); i++ {
			symbolStart += len(lines[i]) + 1
		}
		symbolEnd := min(symbolStart+len(symbolSource), len(fileStr))
		candidate := findEOLMatches(fileStr[symbolStart:symbolEnd], edit.OldSource)
		if candidate.count == 1 {
			regionMatches = candidate
			rangeMatched = true
		}
	}
	if !rangeMatched {
		symbolStart = 0
		switch regionMatches.count {
		case 0:
			return nil, false, fmt.Errorf("old_source not found within symbol or current file")
		case 1:
		default:
			return nil, false, fmt.Errorf("symbol range is stale and old_source is not unique in the current file")
		}
	}
	span := regionMatches.spans[0]
	editStart, editEnd := symbolStart+span.start, symbolStart+span.end
	effectiveNew := edit.NewSource
	if regionMatches.normalized {
		effectiveNew = adaptToDominantEOL(edit.NewSource, fileStr[editStart:editEnd])
	}
	newContent := fileStr[:editStart] + effectiveNew + fileStr[editEnd:]
	if newContent == fileStr {
		return nil, regionMatches.normalized, fmt.Errorf("old_source and new_source are identical after line-ending normalization")
	}
	return []byte(newContent), regionMatches.normalized, nil
}

func applyBatchPlans(plans []plannedBatchEdit, buffers map[string]*batchFileBuffer) ([]batchEditResult, int, error) {
	results := make([]batchEditResult, 0, len(plans))
	for i, plan := range plans {
		buffer := buffers[plan.absPath]
		result := batchEditResult{
			Op: plan.op, SymbolID: plan.edit.SymbolID, FilePath: plan.file,
			DestinationPath: plan.destination, Status: "validated",
		}
		var (
			content    []byte
			normalized bool
			err        error
		)
		switch plan.op {
		case "edit_file":
			if !buffer.existsAfter {
				err = fmt.Errorf("file does not exist")
				break
			}
			content, normalized, err = applyBatchFileToContent(plan.edit, buffer.content)
			if err == nil {
				buffer.content = content
			}
		case "move_file", "delete_file":
			switch {
			case !buffer.existsAfter:
				err = fmt.Errorf("source file does not exist")
			case buffer.fileMode&os.ModeSymlink != 0:
				err = fmt.Errorf("source path is a symlink; whole-file lifecycle operations require a regular file")
			case !buffer.fileMode.IsRegular():
				err = fmt.Errorf("source path is not a regular file")
			case plan.edit.ExpectedSHA256 != "" && !strings.EqualFold(plan.edit.ExpectedSHA256, digestBatchBytes(buffer.content)):
				err = fmt.Errorf("expected_sha256 does not match complete source bytes")
			}
			if err == nil && plan.op == "move_file" {
				destination := buffers[plan.destinationPath]
				if destination.existsAfter {
					err = fmt.Errorf("destination already exists")
				} else {
					destination.mode = buffer.mode
					destination.fileMode = buffer.fileMode
					destination.content = append([]byte(nil), buffer.content...)
					destination.existsAfter = true
				}
			}
			if err == nil {
				buffer.existsAfter = false
			}
		default:
			if !buffer.existsAfter {
				err = fmt.Errorf("symbol file does not exist")
				break
			}
			content, normalized, err = applyBatchSymbolToContent(plan.edit, plan.node, buffer.content)
			if err == nil {
				buffer.content = content
			}
		}
		if err != nil {
			return batchFailureResults(plans, i, err.Error()), i, err
		}
		result.EOLNormalized = normalized
		results = append(results, result)
	}
	return results, -1, nil
}

func (s *Server) runBatchTransaction(ctx context.Context, edits []batchEditItem, requestedID string) (batchTransactionReceipt, error) {
	fingerprint := batchEditFingerprint(edits)
	transactionID, err := normalizeBatchTransactionID(requestedID, fingerprint)
	if err != nil {
		return batchTransactionReceipt{}, err
	}
	state, action, err := s.loadOrCreateBatchTransaction(transactionID, fingerprint)
	if err != nil {
		return batchTransactionReceipt{}, err
	}
	switch action {
	case "existing":
		return waitBatchTransaction(ctx, state), nil
	case "recover":
		s.recoverBatchTransaction(ctx, state)
		return state.snapshot(), nil
	case "refresh_graph":
		s.refreshBatchGraph(state)
		return state.snapshot(), nil
	}

	receipt := state.snapshot()
	plans := s.planBatchTransaction(ctx, edits, true)
	for i, plan := range plans {
		if plan.err != "" {
			receipt.Results = batchFailureResults(plans, i, plan.err)
			receipt.Summary = batchSummary(receipt.Results)
			return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", plan.err), nil
		}
	}
	paths := make([]string, 0, len(plans)*2)
	for _, plan := range plans {
		paths = append(paths, plan.absPath)
		if plan.destinationPath != "" {
			paths = append(paths, plan.destinationPath)
		}
	}
	release, lockErr := acquireMutationPaths(ctx, paths)
	if lockErr != nil {
		receipt.Results = batchFailureResults(plans, 0, "edit cancelled while waiting for exclusive file access: "+lockErr.Error())
		receipt.Summary = batchSummary(receipt.Results)
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", receipt.Results[0].Error), nil
	}
	defer release()
	if err := ctx.Err(); err != nil {
		receipt.Results = batchFailureResults(plans, 0, "edit cancelled before commit: "+err.Error())
		receipt.Summary = batchSummary(receipt.Results)
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", receipt.Results[0].Error), nil
	}

	buffers, orderedPaths, readErr := s.readBatchBuffers(plans)
	if readErr != nil {
		receipt.Results = batchFailureResults(plans, 0, readErr.Error())
		receipt.Summary = batchSummary(receipt.Results)
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", readErr.Error()), nil
	}
	results, _, applyErr := applyBatchPlans(plans, buffers)
	receipt.Results = results
	receipt.Summary = batchSummary(results)
	if applyErr != nil {
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", applyErr.Error()), nil
	}
	if err := s.prepareBatchJournal(&receipt, buffers, orderedPaths); err != nil {
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", "could not persist transaction journal: "+err.Error()), nil
	}
	receipt.Status, receipt.DiskStatus, receipt.GraphStatus = "prepared", "unchanged", "not_started"
	state.publish(receipt, false)
	if err := ctx.Err(); err != nil {
		return s.finishBatchTransaction(state, receipt, "aborted", "unchanged", "not_started", "edit cancelled before commit: "+err.Error()), nil
	}

	// Commit is deliberately non-cancellable. Once the first rename succeeds,
	// every remaining write or rollback must run to a terminal disk state.
	writer := s.batchDurability().writeFile
	remover := s.batchDurability().removeFile
	if s.batchWriteOverride != nil {
		// Preserve the target-only fault-injection seam used by commit tests;
		// journal and rollback writes always retain the durability discipline.
		writer = s.batchWriteOverride
	}
	if s.batchRemoveOverride != nil {
		remover = s.batchRemoveOverride
	}
	finishCommitFailure := func(failedPath, message string) batchTransactionReceipt {
		status, rollbackErr := s.rollbackBatchReceipt(receipt)
		if rollbackErr != nil {
			message += "; " + rollbackErr.Error()
		}
		diskStatus := "rolled_back"
		if status == "recovery_conflict" {
			diskStatus = "conflict"
		}
		receipt.Results = markBatchCommitFailure(receipt.Results, failedPath, message)
		receipt.Summary = batchSummary(receipt.Results)
		return s.finishBatchTransaction(state, receipt, status, diskStatus, "not_started", message)
	}
	// Publish every after-image before removing any before-image. For moves this
	// keeps the source intact until the destination has passed its final
	// collision guard and has been durably written.
	for _, path := range orderedPaths {
		buffer := buffers[path]
		if !buffer.existsAfter {
			continue
		}
		var commitErr error
		if !buffer.existsBefore {
			if targetErr := s.validateBatchCreateTarget(path, buffer.relPath); targetErr != nil {
				commitErr = targetErr
			} else {
				commitErr = writer(path, buffer.content, buffer.mode)
			}
		} else {
			commitErr = writer(path, buffer.content, buffer.mode)
		}
		if commitErr != nil {
			message := fmt.Sprintf("could not commit %s: %v", buffer.relPath, commitErr)
			return finishCommitFailure(buffer.relPath, message), nil
		}
	}
	for _, path := range orderedPaths {
		buffer := buffers[path]
		if buffer.existsAfter {
			continue
		}
		if commitErr := remover(path); commitErr != nil {
			message := fmt.Sprintf("could not commit %s: %v", buffer.relPath, commitErr)
			return finishCommitFailure(buffer.relPath, message), nil
		}
	}
	if syncErr := s.syncBatchDirectories(batchPathDirectories(orderedPaths)...); syncErr != nil {
		failedPath := buffers[orderedPaths[len(orderedPaths)-1]].relPath
		message := "could not persist committed files: " + syncErr.Error()
		return finishCommitFailure(failedPath, message), nil
	}

	for i := range receipt.Results {
		receipt.Results[i].Status = "applied"
	}
	receipt.Summary = batchSummary(receipt.Results)
	receipt.Status, receipt.DiskStatus, receipt.GraphStatus = "committed", "committed", "pending"
	receipt.Error = ""
	if persistErr := s.persistBatchManifest(receipt); persistErr != nil {
		receipt.Error = "disk committed; terminal journal update failed: " + persistErr.Error()
	}
	state.publish(receipt, false)

	for _, plan := range plans {
		session := s.sessionFor(ctx)
		session.recordModified(plan.file)
		if plan.destination != "" {
			session.recordModified(plan.destination)
		}
		if plan.edit.SymbolID != "" {
			session.recordSymbol(plan.edit.SymbolID)
		}
	}
	s.refreshBatchGraph(state)
	return state.snapshot(), nil
}

func waitBatchTransaction(ctx context.Context, state *batchTransactionState) batchTransactionReceipt {
	select {
	case <-state.done:
		return state.snapshot()
	case <-ctx.Done():
		receipt := state.snapshot()
		if receipt.Status == "preparing" || receipt.Status == "prepared" {
			receipt.Error = "transaction continues independently: " + ctx.Err().Error()
		}
		return receipt
	}
}

func (s *Server) batchTransactionStatus(ctx context.Context, transactionID string) (batchTransactionReceipt, error) {
	id := strings.TrimSpace(transactionID)
	if id == "" {
		return batchTransactionReceipt{}, fmt.Errorf("transaction_id is required for status")
	}
	if value, ok := s.batchTransactions.Load(id); ok {
		state, valid := value.(*batchTransactionState)
		if !valid {
			return batchTransactionReceipt{}, fmt.Errorf("invalid transaction state for %q", id)
		}
		if existingBatchTransactionAction(state) == "refresh_graph" {
			s.refreshBatchGraph(state)
		}
		return state.snapshot(), nil
	}
	state, action, err := s.loadOrCreateBatchTransaction(id, "")
	if err != nil {
		return batchTransactionReceipt{}, err
	}
	switch action {
	case "recover":
		s.recoverBatchTransaction(ctx, state)
	case "refresh_graph":
		s.refreshBatchGraph(state)
	}
	return state.snapshot(), nil
}

func (s *Server) beginBatchGraphRefresh(absPath string) mutationReindexOutcome {
	ctx := context.Background()
	if watcher := s.currentWatcher(); watcher != nil {
		if scheduler, ok := watcher.(mutationScheduler); ok {
			ticket, err := scheduler.EnqueueFileMutation(ctx, absPath)
			if err != nil {
				return mutationReindexOutcome{Err: err}
			}
			if ticket != nil {
				receipt := s.trackMutationTicket(ticket)
				return receipt.outcome(true)
			}
		}
	}
	return mutationReindexOutcome{Reindexed: s.reindexFile(absPath)}
}

func (s *Server) waitBatchGraphReceipts(files []batchTransactionFile) {
	deadline := time.Now().Add(s.mutationWaitDuration())
	for _, file := range files {
		if file.ReindexReceipt == "" {
			continue
		}
		outcome, ok := s.mutationReceiptState(file.ReindexReceipt)
		if !ok || !outcome.Pending {
			continue
		}
		value, ok := s.mutationReceipts.Load(file.ReindexReceipt)
		if !ok {
			continue
		}
		receipt, ok := value.(*mutationReceipt)
		if !ok {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-receipt.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			return
		}
	}
}

func (s *Server) refreshBatchGraph(state *batchTransactionState) {
	state.graphMu.Lock()
	defer state.graphMu.Unlock()

	receipt := state.snapshot()
	if receipt.Status != "committed" {
		state.publish(receipt, true)
		return
	}
	if receipt.GraphStatus == "fresh" {
		state.publish(receipt, true)
		return
	}

	outcomes := make(map[string]mutationReindexOutcome, len(receipt.Files))
	for i := range receipt.Files {
		file := &receipt.Files[i]
		if file.ReindexReceipt != "" {
			if outcome, ok := s.mutationReceiptState(file.ReindexReceipt); ok {
				outcomes[file.Path] = outcome
				continue
			}
			// Receipt state is daemon-local and can expire. A durable committed
			// transaction safely re-admits the file after restart or retention.
			file.ReindexReceipt = ""
			file.ReindexGeneration = 0
		}
		outcome := s.beginBatchGraphRefresh(file.Path)
		outcomes[file.Path] = outcome
		file.ReindexReceipt = outcome.Receipt
		file.ReindexGeneration = outcome.Generation
	}

	// Admit the entire file set before waiting. The bounded wait is shared by
	// the batch, rather than multiplied by the number of files.
	s.waitBatchGraphReceipts(receipt.Files)
	graphStatus := "fresh"
	for i := range receipt.Files {
		file := &receipt.Files[i]
		outcome := outcomes[file.Path]
		if file.ReindexReceipt != "" {
			if latest, ok := s.mutationReceiptState(file.ReindexReceipt); ok {
				outcome = latest
				outcomes[file.Path] = latest
			}
		}
		switch {
		case outcome.Err != nil:
			graphStatus = "failed"
			// A later status/retry call may re-admit a transient failure.
			file.ReindexReceipt = ""
		case outcome.Pending:
			if graphStatus != "failed" {
				graphStatus = "pending"
			}
		case !outcome.Reindexed:
			graphStatus = "failed"
			file.ReindexReceipt = ""
		}
	}
	for i := range receipt.Results {
		for _, file := range receipt.Files {
			if receipt.Results[i].FilePath != file.RelativePath {
				continue
			}
			outcome := outcomes[file.Path]
			receipt.Results[i].Reindexed = outcome.Reindexed
			receipt.Results[i].ReindexPending = outcome.Pending
			receipt.Results[i].ReindexReceipt = outcome.Receipt
			receipt.Results[i].ReindexGeneration = outcome.Generation
			receipt.Results[i].ReindexAppliedGeneration = outcome.AppliedGeneration
			receipt.Results[i].ReindexError = ""
			if outcome.Err != nil {
				receipt.Results[i].ReindexError = outcome.Err.Error()
			}
		}
	}
	receipt.GraphStatus = graphStatus
	if receipt.CompletedAt == nil {
		now := time.Now().UTC()
		receipt.CompletedAt = &now
	}
	persistErr := s.persistBatchManifest(receipt)
	if persistErr != nil {
		if receipt.Error == "" {
			receipt.Error = "terminal journal update failed: " + persistErr.Error()
		} else {
			receipt.Error += "; terminal journal update failed: " + persistErr.Error()
		}
	}
	state.publish(receipt, true)
	if persistErr == nil && batchReceiptCleanupSafe(receipt) {
		_ = s.cleanupBatchBackups(receipt)
	}
}

func (s *Server) finishBatchTransaction(state *batchTransactionState, receipt batchTransactionReceipt, status, diskStatus, graphStatus, message string) batchTransactionReceipt {
	receipt.Status, receipt.DiskStatus, receipt.GraphStatus, receipt.Error = status, diskStatus, graphStatus, message
	now := time.Now().UTC()
	receipt.CompletedAt = &now
	persistErr := s.persistBatchManifest(receipt)
	if persistErr != nil {
		if receipt.Error == "" {
			receipt.Error = "terminal journal update failed: " + persistErr.Error()
		} else {
			receipt.Error += "; terminal journal update failed: " + persistErr.Error()
		}
	}
	state.publish(receipt, true)
	if persistErr == nil && batchReceiptCleanupSafe(receipt) {
		_ = s.cleanupBatchBackups(receipt)
	}
	return receipt
}

func (s *Server) handleAtomicBatchEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	transactionID, _ := args["transaction_id"].(string)
	statusOnly, _ := args["status_only"].(bool)
	rawEdits, hasEdits := args["edits"]
	if statusOnly || !hasEdits || rawEdits == nil {
		receipt, err := s.batchTransactionStatus(ctx, transactionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if isCompact(req) {
			return mcp.NewToolResultText(fmt.Sprintf("%s %s disk=%s graph=%s\n", receipt.TransactionID, receipt.Status, receipt.DiskStatus, receipt.GraphStatus)), nil
		}
		return s.respondJSONOrTOON(ctx, req, receipt)
	}

	edits, err := parseBatchEdits(rawEdits)
	if err != nil {
		return batchEditInvalidArgumentResult(err), nil
	}
	if len(edits) == 0 {
		return mcp.NewToolResultError("edits array is empty"), nil
	}
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		plans := s.planBatchTransaction(ctx, edits, false)
		plan := make([]map[string]any, 0, len(plans))
		for i, item := range plans {
			status := "planned"
			if item.err != "" {
				status = "failed: " + item.err
			}
			plan = append(plan, map[string]any{
				"order": i + 1, "op": item.op, "id": item.edit.SymbolID,
				"path": item.file, "destination": item.destination, "status": status,
			})
		}
		if isCompact(req) {
			var out strings.Builder
			for _, item := range plan {
				fmt.Fprintf(&out, "%s %s %s\n", item["op"], item["path"], item["status"])
			}
			return mcp.NewToolResultText(out.String()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{"plan": plan, "dry_run": true, "total": len(plan)})
	}

	receipt, err := s.runBatchTransaction(ctx, edits, transactionID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if isCompact(req) {
		var out strings.Builder
		fmt.Fprintf(&out, "%s %s disk=%s graph=%s\n", receipt.TransactionID, receipt.Status, receipt.DiskStatus, receipt.GraphStatus)
		for _, result := range receipt.Results {
			target := result.SymbolID
			if target == "" {
				target = result.FilePath
			}
			fmt.Fprintf(&out, "%s %s %s\n", result.Op, target, result.Status)
		}
		return mcp.NewToolResultText(out.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, receipt)
}
