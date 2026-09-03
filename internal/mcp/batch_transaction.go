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
	"github.com/zzet/gortex/internal/pathkey"
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
	followedInfo os.FileInfo // identity behind a symlink leaf, so a link and its target are one file
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

// isBatchLifecycleOp reports whether an op owns a whole file rather than a
// fragment of one. The plan, the transaction, and the dry-run preflight all
// branch on it, so it is spelled once.
func isBatchLifecycleOp(op string) bool { return op == "move_file" || op == "delete_file" }

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
		lifecycle := isBatchLifecycleOp(plans[i].op)
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

// batchCheckoutCandidate is one spelling of a checkout a request may write
// through, with the identity behind it when the spelling exists on disk.
type batchCheckoutCandidate struct {
	spelling string
	identity os.FileInfo
}

// batchLifecycleRoot returns the innermost checkout containing a lifecycle
// path, in the caller's own spelling, plus whether any checkout is known at
// all. A control client with no known roots gets ("", false) and keeps its
// deliberate no-op posture; ("", true) means checkouts are known and none of
// them accounts for this spelling, which the callers refuse rather than skip.
//
// The candidates are every checkout a request can legitimately write through:
// each tracked repository root — linked worktrees included, since they are
// registered under their own prefix like any other repo — the lone indexer's
// root, and the root of a view the request was routed to. Each is registered
// in its own spelling and in its symlink-resolved one, and the match runs in
// two passes.
//
// The first pass is lexical: the longest candidate spelling that contains the
// path wins. It decides every ordinary request, and it is what makes a symlink
// inside a checkout a component of the path rather than a root of its own —
// `repoRoot/self -> repoRoot` is a candidate spelling only after resolution,
// while `repoRoot` itself lexically contains the destination and is the deeper
// answer the guard needs.
//
// The second pass exists for spellings no candidate prefixes: a path that
// resolves some of its symlinks and not others (macOS hands out /var/folders/…
// for a checkout registered as /private/var/…), a case variant of the root on a
// case-insensitive filesystem, or an alias nobody registered. It climbs the
// caller's own ancestors. A real directory is the root when it IS a checkout
// root — os.SameFile with a candidate — and it is taken at once, being the
// deepest such spelling. A symlink ancestor is an alias into a checkout when
// its resolved location lies inside one; it is remembered rather than taken,
// and a later (outer) alias replaces it, so that when only aliases identify the
// checkout the outermost one becomes the root and every link between it and
// the destination stays in the range the callers inspect. A real directory is
// never matched by containment: a subdirectory inside a checkout resolves
// inside it too, and taking one as the root would lift a symlinked parent
// above the inspected range.
func (s *Server) batchLifecycleRoot(ctx context.Context, absPath string) (string, bool) {
	var candidates []batchCheckoutCandidate
	seen := make(map[string]struct{}, 4)
	addSpelling := func(spelling string) {
		spelling = filepath.Clean(spelling)
		if _, dup := seen[spelling]; dup {
			return
		}
		seen[spelling] = struct{}{}
		candidate := batchCheckoutCandidate{spelling: spelling}
		if info, err := os.Stat(spelling); err == nil {
			candidate.identity = info
		}
		candidates = append(candidates, candidate)
	}
	considerRoot := func(candidate string) {
		if candidate == "" {
			return
		}
		addSpelling(candidate)
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			addSpelling(resolved)
		}
	}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			if candidate, ok := s.multiIndexer.RepoRoot(prefix); ok {
				considerRoot(candidate)
			}
		}
	}
	if s.indexer != nil {
		considerRoot(s.indexer.RootPath())
	}
	considerRoot(requestViewPathRoot(ctx).root)
	if len(candidates) == 0 {
		return "", false
	}

	cleanPath := filepath.Clean(absPath)
	lexical := ""
	for _, candidate := range candidates {
		if pathContainedIn(cleanPath, candidate.spelling) && len(candidate.spelling) > len(lexical) {
			lexical = candidate.spelling
		}
	}
	if lexical != "" {
		return lexical, true
	}

	isCheckoutRoot := func(ancestor string) bool {
		info, err := os.Stat(ancestor)
		if err != nil {
			return false
		}
		for _, candidate := range candidates {
			if candidate.identity != nil && os.SameFile(info, candidate.identity) {
				return true
			}
		}
		return false
	}
	aliasesCheckout := func(ancestor string) bool {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			return false
		}
		resolved = filepath.Clean(resolved)
		for _, candidate := range candidates {
			if pathContainedIn(resolved, candidate.spelling) {
				return true
			}
		}
		return false
	}
	// Start at the parent: a lifecycle destination is allowed not to exist, so
	// the path itself has no spelling to resolve.
	symlinked := ""
	for ancestor := filepath.Dir(cleanPath); ; {
		info, err := os.Lstat(ancestor)
		switch {
		case err != nil:
		case info.Mode()&os.ModeSymlink != 0:
			if aliasesCheckout(ancestor) {
				symlinked = ancestor
			}
		case isCheckoutRoot(ancestor):
			return ancestor, true
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return symlinked, true
		}
		ancestor = parent
	}
}

// guardBatchLifecycleDestination inspects every component of a move destination
// strictly below the checkout root batchLifecycleRoot selects, Lstat-ing each
// and refusing the destination when one is a symlink. The root itself is not
// inspected, which is what keeps a checkout under a symlinked system prefix
// valid. A symlink can be the root only when it is a registered spelling of a
// checkout or an alias that resolves into one; the write then still lands
// inside that checkout, and every component below the alias is inspected.
// General file resolution permits in-repo symlinks, which is correct for
// reads, but a move destination must not redirect a transaction write through
// either a symlink leaf or a symlinked parent.
func (s *Server) guardBatchLifecycleDestination(ctx context.Context, absPath string) error {
	cleanPath := filepath.Clean(absPath)
	root, known := s.batchLifecycleRoot(ctx, cleanPath)
	if root == "" {
		if !known {
			return nil
		}
		// Path resolution admitted the destination, so its real location is
		// inside some checkout — yet no spelling of one accounts for it.
		// Skipping the walk here would leave whatever redirected the path
		// uninspected, so the destination is refused instead.
		return fmt.Errorf("move destination %s is not spelled under any indexed checkout", cleanPath)
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

// errBatchDestinationExists is the single wording for a lifecycle destination
// that is already taken. Three sites raise it — the dry-run preflight, the
// pre-write create guard, and the plan application — and a dry run only
// predicts the abort it names if all three spell it the same way.
var errBatchDestinationExists = errors.New("destination already exists")

func (s *Server) validateBatchCreateTarget(ctx context.Context, absPath, relPath string) error {
	if err := s.guardBatchLifecycleDestination(ctx, absPath); err != nil {
		return err
	}
	_, err := os.Lstat(absPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("could not stat %s: %w", relPath, err)
	default:
		return errBatchDestinationExists
	}
}

// batchPathBytes is the stat-then-read the transaction performs for one batch
// path: the Lstat result (nil when the path is absent), the bytes behind it,
// and the refusal that aborts the batch when either step fails. The dry-run
// preflight runs the same function over the same fixture, which is what keeps
// a reported conflict spelled exactly like the abort it predicts.
func batchPathBytes(path, relPath string) (os.FileInfo, []byte, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("could not stat %s: %w", relPath, err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return info, nil, fmt.Errorf("could not read %s: %w", relPath, readErr)
	}
	return info, content, nil
}

// batchLifecycleSourceRefusal reports why a whole-file lifecycle operation
// cannot take this source, once its bytes have been collected. It is the
// per-operation half of the contract batchPathBytes opens: shared for the same
// reason, so the dry run and the commit refuse in identical words.
func batchLifecycleSourceRefusal(exists bool, fileMode os.FileMode, content []byte, expectedSHA256 string) error {
	switch {
	case !exists:
		return errors.New("source file does not exist")
	case fileMode&os.ModeSymlink != 0:
		return errors.New("source path is a symlink; whole-file lifecycle operations require a regular file")
	case !fileMode.IsRegular():
		return errors.New("source path is not a regular file")
	case expectedSHA256 != "" && !strings.EqualFold(expectedSHA256, digestBatchBytes(content)):
		return errors.New("expected_sha256 does not match complete source bytes")
	}
	return nil
}

func (s *Server) readBatchBuffers(ctx context.Context, plans []plannedBatchEdit) (map[string]*batchFileBuffer, []string, error) {
	buffers := make(map[string]*batchFileBuffer)
	paths := make([]string, 0)
	add := func(path, relPath string) error {
		if _, exists := buffers[path]; exists {
			return nil
		}
		buffer := &batchFileBuffer{absPath: path, relPath: relPath, mode: 0o644, existenceSet: true}
		info, content, err := batchPathBytes(path, relPath)
		if err != nil {
			return err
		}
		// A missing path is retained in the transaction snapshot so a move
		// destination can be created and rollback can prove it was absent.
		if info != nil {
			buffer.fileMode = info.Mode()
			buffer.followedInfo = batchFollowedIdentity(path, info)
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
			if err := s.guardBatchLifecycleDestination(ctx, plan.destinationPath); err != nil {
				return nil, nil, err
			}
			if err := add(plan.destinationPath, plan.destination); err != nil {
				return nil, nil, err
			}
		}
	}
	sort.Strings(paths)
	if err := rejectAliasedBatchPaths(buffers, paths); err != nil {
		return nil, nil, err
	}
	return buffers, paths, nil
}

// rejectAliasedBatchPaths refuses a batch whose distinct paths name one file.
// A hard link, or a case alias on a case-insensitive filesystem, gives one
// inode two buffers, and the transaction then plans an independent future for
// each. On a case alias the two spellings are one directory entry, so
// `edit_file a.txt` plus `delete_file A.txt` deletes what the edit just wrote
// and reports both as applied. A hard link survives that exact sequence — the
// writer renames a replacement into place, which breaks the link — and is
// refused all the same: rollback and the before-image journal are recorded per
// path and assume independent inodes, so one inode under two entries would be
// restored through a history that was never written for it.
//
// A symlink leaf and the file it points at are one file too, and that is the
// single rule the comparison runs: what the path ultimately names.
// batchFollowedIdentity falls back to the Lstat result for everything that is
// not a resolvable symlink, so one comparison covers the hard link and the case
// alias as well as the link beside its target. A content edit through a symlink
// replaces the link with a regular file, so `edit_file link.txt` beside
// `edit_file a.txt` would otherwise commit two divergent copies of what the
// batch addressed as one. A lifecycle operation on a symlink source stays
// refused on its own terms when its target is not also in the batch; when it
// is, this pairing is what refuses first.
//
// Two batch items spelling the path identically stay supported — they share one
// buffer, and lifecycle overlap is already governed by the plan's path owners.
func rejectAliasedBatchPaths(buffers map[string]*batchFileBuffer, paths []string) error {
	pairs := batchAliasedPathPairs(paths, func(path string) os.FileInfo {
		if buffer := buffers[path]; buffer != nil {
			return buffer.followedInfo
		}
		return nil
	})
	if len(pairs) == 0 {
		return nil
	}
	return errors.New(pairs[0].message(buffers[pairs[0].left].relPath, buffers[pairs[0].right].relPath))
}

// batchFollowedIdentity reports what a batch path ultimately names: the Lstat
// result for anything but a symlink, and the target's identity for a symlink
// that resolves. A broken link keeps its own identity, which names nothing but
// itself.
func batchFollowedIdentity(path string, info os.FileInfo) os.FileInfo {
	if info == nil || info.Mode()&os.ModeSymlink == 0 {
		return info
	}
	if followed, err := os.Stat(path); err == nil {
		return followed
	}
	return info
}

// batchAliasKind says why two distinct batch paths cannot be planned
// independently, which is also how the refusal is worded.
type batchAliasKind int

const (
	// batchAliasSameFile: two existing paths that ultimately name one file.
	batchAliasSameFile batchAliasKind = iota
	// batchAliasCaseVariant: two absent destinations one directory would
	// create under names that differ only by case.
	batchAliasCaseVariant
	// batchAliasSameEntry: two absent destinations one directory entry would
	// serve — byte-identical names under two spellings of the directory, or
	// names that differ only by Unicode normalisation.
	batchAliasSameEntry
)

// batchAliasedPair is one pair of distinct batch paths the transaction cannot
// plan independent futures for. Nothing on disk distinguishes an absent pair,
// so it is worded for what it is rather than as two spellings of one file.
type batchAliasedPair struct {
	left, right string
	kind        batchAliasKind
}

// message renders this pair's refusal from the relative spellings the batch
// quoted, so the dry-run preflight and the transaction word it identically.
func (pair batchAliasedPair) message(left, right string) string {
	switch pair.kind {
	case batchAliasCaseVariant:
		return fmt.Sprintf("batch destinations %s and %s differ only by case", left, right)
	case batchAliasSameEntry:
		return fmt.Sprintf("batch destinations %s and %s would be created as one directory entry", left, right)
	}
	return fmt.Sprintf("batch paths %s and %s name the same file", left, right)
}

// batchAliasedPathIdentity is one path reduced to what the pairwise comparison
// needs, resolved once per path so the pass costs no map lookup per pair.
type batchAliasedPathIdentity struct {
	path     string
	identity os.FileInfo // what the path ultimately names; nil when it is absent
	anchor   os.FileInfo // for an absent path, its nearest existing ancestor
	suffix   string      // for an absent path, the components below that ancestor
}

// batchAbsentPathAnchor splits an absent path into its nearest existing
// ancestor and the components the commit would create below it. The ancestor
// is identified through symlinks, so two spellings of one directory anchor
// alike, and a parent that does not exist yet is simply part of the suffix —
// which is what lets two destinations under a directory the batch would
// create be compared at all.
func batchAbsentPathAnchor(path string) (os.FileInfo, string) {
	current := filepath.Clean(path)
	suffix := ""
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return nil, ""
		}
		suffix = filepath.Join(filepath.Base(current), suffix)
		if info, err := os.Stat(parent); err == nil {
			return info, suffix
		}
		current = parent
	}
}

// batchAliasedPathPairs reports every pair of distinct batch paths that cannot
// both be planned, ordered by the sorted paths so the first pair is the one the
// transaction aborts on. Two paths that exist pair when they ultimately name
// one file, which is the hard link, the case alias, and the symlink leaf beside
// its target alike.
//
// Two paths that do NOT exist have no identity to compare, and skipping them
// let a batch plan two destinations a case-insensitive filesystem creates as
// one: the first move made the file the second then found already taken, at
// commit time and past the point rollback can describe either. They pair when
// they would be created below one existing directory — os.SameFile on the
// nearest existing ancestors, so a directory reached two ways still counts
// once — under names that are one entry to a normalising or case-insensitive
// filesystem: byte-identical, equal after folding to NFC, or equal under case
// folding on top of that. The rule does not consult the filesystem's own
// sensitivity: where both destinations are legal the batch still cannot say
// which file the caller meant, and refusing is the fail-closed answer.
//
// The dry-run preflight reports every pair; the transaction refuses on the
// first.
func batchAliasedPathPairs(paths []string, followed func(string) os.FileInfo) []batchAliasedPair {
	identities := make([]batchAliasedPathIdentity, 0, len(paths))
	for _, path := range paths {
		identity := batchAliasedPathIdentity{path: path, identity: followed(path)}
		if identity.identity == nil {
			identity.anchor, identity.suffix = batchAbsentPathAnchor(path)
		}
		identities = append(identities, identity)
	}
	var pairs []batchAliasedPair
	for i, left := range identities {
		for _, right := range identities[i+1:] {
			switch {
			case left.identity != nil && right.identity != nil:
				if os.SameFile(left.identity, right.identity) {
					pairs = append(pairs, batchAliasedPair{left: left.path, right: right.path, kind: batchAliasSameFile})
				}
			case left.identity == nil && right.identity == nil:
				if left.anchor == nil || right.anchor == nil || !os.SameFile(left.anchor, right.anchor) {
					continue
				}
				// Composition is folded before case: a normalising filesystem
				// serves "é" and "é" from one entry, so the spellings
				// have to agree on composition before case can be compared.
				leftName, rightName := pathkey.Normalize(left.suffix), pathkey.Normalize(right.suffix)
				switch {
				case leftName == rightName:
					pairs = append(pairs, batchAliasedPair{left: left.path, right: right.path, kind: batchAliasSameEntry})
				case strings.EqualFold(leftName, rightName):
					pairs = append(pairs, batchAliasedPair{left: left.path, right: right.path, kind: batchAliasCaseVariant})
				}
			}
		}
	}
	return pairs
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
			err = batchLifecycleSourceRefusal(buffer.existsAfter, buffer.fileMode, buffer.content, plan.edit.ExpectedSHA256)
			if err == nil && plan.op == "move_file" {
				destination := buffers[plan.destinationPath]
				if destination.existsAfter {
					err = errBatchDestinationExists
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

	buffers, orderedPaths, readErr := s.readBatchBuffers(ctx, plans)
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
			if targetErr := s.validateBatchCreateTarget(ctx, path, buffer.relPath); targetErr != nil {
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
		// Paths are resolved exactly as the commit resolves them, symbol edits
		// included: without that a symbol edit has no path, and the rules that
		// compare paths — lifecycle overlap, then aliasing — see a shorter
		// batch than the one that would be committed.
		plans := s.planBatchTransaction(ctx, edits, true)
		// Lifecycle operations are re-validated against disk so the dry run
		// answers "would this commit?" rather than only "is this well-formed?".
		preflights := s.preflightBatchLifecycle(ctx, plans)
		plan := make([]map[string]any, 0, len(plans))
		// conflicts counts the lifecycle preconditions, the aliasing rule
		// across all items, and the argument failures: the first two report
		// "conflict: <err>" and the last "failed: <err>", and a caller deciding
		// whether to apply the batch needs one number covering both. It is not
		// a count of everything that would not commit — a content edit is never
		// evaluated against disk here, so an absent path, a directory, or an
		// old_string that is not there still counts as planned and refuses
		// under the lock instead.
		conflicts := 0
		for i, item := range plans {
			status := "planned"
			if item.err != "" {
				status = "failed: " + item.err
			}
			entry := map[string]any{
				"order": i + 1, "op": item.op, "id": item.edit.SymbolID,
				"path": item.file, "destination": item.destination, "status": status,
			}
			if preflight := preflights[i]; preflight != nil {
				preflight.annotate(entry, item)
			}
			if entry["status"] != "planned" {
				conflicts++
			}
			plan = append(plan, entry)
		}
		if isCompact(req) {
			var out strings.Builder
			for _, item := range plan {
				fmt.Fprintf(&out, "%s %s %s\n", item["op"], item["path"], item["status"])
			}
			return mcp.NewToolResultText(out.String()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"plan": plan, "dry_run": true, "total": len(plan), "conflicts": conflicts,
		})
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
