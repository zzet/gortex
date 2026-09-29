package indexer

import (
	"context"
	"crypto/sha1" //nolint:gosec // git's object format, not a security primitive
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Primary-to-worktree propagation under the per-file delta model.
//
// A worktree's view composes over the family's base. When the base advances
// (the primary commits, or a release asks a pinned dependent to leave the base
// it was built against) the question for each worktree is decided by CONTENT,
// path by path, never by policy:
//
//	path f the base changed | worktree W's bytes at f     | W's action
//	f covered by W          | equal to the base's new     | drop coverage (its own copy is now redundant)
//	f covered by W          | differ from the base's new  | keep (nothing at f)
//	f not covered by W      | equal to the base's new     | untouched
//	f not covered by W      | differ (main moved, W did not) | re-assert W's own f: one parse
//
// "Covered" is what W's routed layers claim (its commit layer's paths plus its
// working-tree chain's). An empty scope leaves W's layers alone: its view
// re-composes over the new base with no build.
//
// # Why the working-tree chain is carried over by copy
//
// A checkout's commit layer is the committed-tree delta diff(base, HEAD), so
// base + commit layer composes to the checkout's committed tree over EITHER
// base. The working-tree chain is a delta over exactly that composed committed
// tree, derived from the checkout's own bytes, so its rows do not depend on
// which base the committed tree was composed from: carried over to the
// recomposed commit layer row for row, it describes the same working tree. The
// recomposition therefore rebuilds only the commit layer (bounded by what the
// two committed trees differ by — the re-asserted paths plus the checkout's own
// committed changes) and copies the chain; no working-tree file is parsed.
// The copy is refused, and the chain rebuilt the old way, whenever the chain's
// identity differs from what a build would mint now in anything but its base
// (a configuration, cohort, extractor or resolver change), or the working tree
// moved since the chain was published.
//
// # Laziness
//
// A base advance is NOTED on every dependent coordinator without a cycle: no
// sample, no build, no build-lane admission and no ticket. It is applied the
// next time the checkout is used — selected by a query, edited (a refresh
// ticket), or asked to release its pinned base by the retirement sweep. Until
// then the checkout keeps serving the stack it is routed to; in the committed
// regime that stack is exact (it composes over the immutable base it names),
// which is what makes deferring it free.

// basePropagation is one coordinator's pending base advance.
type basePropagation struct {
	mu sync.Mutex
	// pendingGeneration / pendingTree name the newest base advance noted
	// without a cycle; zero/empty when none is pending.
	pendingGeneration int64
	pendingTree       string
	pendingSince      time.Time
	// wanted records that a use asked for the pending rebase to be applied.
	wanted       bool
	wantedReason string
	// noted counts advances noted without a cycle; deferred counts cycles
	// that found a base-only advance and left the stack as it was; applied
	// counts rebases the coordinator carried out.
	noted, deferred, applied int
}

// PropagationStats is what a coordinator did with base advances.
type PropagationStats struct {
	Noted    int
	Deferred int
	Applied  int
	Pending  bool
	// Wanted is true when a use asked for the pending advance to be applied.
	Wanted bool
}

// PropagationStats reports the coordinator's propagation counters.
func (c *CheckoutCoordinator) PropagationStats() PropagationStats {
	if c == nil {
		return PropagationStats{}
	}
	c.propagation.mu.Lock()
	defer c.propagation.mu.Unlock()
	return PropagationStats{
		Noted:    c.propagation.noted,
		Deferred: c.propagation.deferred,
		Applied:  c.propagation.applied,
		Pending:  c.propagation.pendingGeneration > 0 || c.propagation.pendingTree != "",
		Wanted:   c.propagation.wanted,
	}
}

// NoteBaseAdvance records that the family's base advanced, without running a
// cycle. It is what the lifecycle calls on every dependent instead of Signal.
// Nothing is read and nothing is written here; a
// selection, an edit or a base-release request applies it.
func (c *CheckoutCoordinator) NoteBaseAdvance(generationID int64, treeOID, reason string) {
	if c == nil {
		return
	}
	c.propagation.mu.Lock()
	if c.propagation.pendingGeneration == 0 && c.propagation.pendingTree == "" {
		c.propagation.pendingSince = time.Now()
	}
	c.propagation.pendingGeneration = generationID
	c.propagation.pendingTree = treeOID
	c.propagation.noted++
	c.propagation.mu.Unlock()
	c.logger.Debug("checkout coordinator: base advance noted; rebase deferred to the next use",
		zap.String("checkout", c.checkoutID),
		zap.Int64("generation", generationID),
		zap.String("tree", treeOID),
		zap.String("reason", reason))
}

// wantRebase records that the checkout is being used. When a base advance is
// pending, signal asks for the cycle that applies it; a caller that already
// runs a cycle (a refresh ticket) passes false.
func (c *CheckoutCoordinator) wantRebase(reason string, signal bool) {
	if c == nil {
		return
	}
	c.propagation.mu.Lock()
	pending := c.propagation.pendingGeneration > 0 || c.propagation.pendingTree != ""
	already := c.propagation.wanted
	if pending {
		// A use with nothing pending marks nothing: it would otherwise make
		// the NEXT advance eager for a checkout that was used once.
		c.propagation.wanted = true
		c.propagation.wantedReason = reason
	}
	c.propagation.mu.Unlock()
	if pending && signal && !already {
		c.Signal("checkout used with a base advance pending: " + reason)
	}
}

// rebaseWantedNow reports whether a base-only advance found by this cycle may
// be applied now: something is using the checkout. A refresh ticket waiting on
// this checkout (an edit, or a request for a fresh answer), a selection since
// the last applied rebase, and a sweep asking for the pinned base back are
// uses; a poll and a watcher event of another checkout are not.
func (c *CheckoutCoordinator) rebaseWantedNow() bool {
	if c.checkoutRefreshHighWater() > 0 || c.releaseRequestedBasePin() > 0 {
		return true
	}
	c.propagation.mu.Lock()
	defer c.propagation.mu.Unlock()
	return c.propagation.wanted
}

// deferRebase leaves a base-only advance for the checkout's next use and
// reports it on the cycle.
func (c *CheckoutCoordinator) deferRebase(base primaryBase, out *CheckoutCycle) {
	c.propagation.mu.Lock()
	if c.propagation.pendingGeneration == 0 && c.propagation.pendingTree == "" {
		c.propagation.pendingSince = time.Now()
	}
	c.propagation.pendingGeneration = base.generationID
	c.propagation.pendingTree = base.treeOID
	c.propagation.deferred++
	c.propagation.mu.Unlock()
	out.RebaseDeferred = true
	c.logger.Debug("checkout coordinator: base advanced under an unused checkout; rebase deferred",
		zap.String("checkout", c.checkoutID),
		zap.Int64("base_generation", base.generationID),
		zap.String("base_tree", base.treeOID))
}

// rebaseApplied clears the pending advance once a recomposition installed a
// stack over the base.
func (c *CheckoutCoordinator) rebaseApplied() {
	c.propagation.mu.Lock()
	c.propagation.pendingGeneration = 0
	c.propagation.pendingTree = ""
	c.propagation.pendingSince = time.Time{}
	c.propagation.wanted = false
	c.propagation.wantedReason = ""
	c.propagation.applied++
	c.propagation.mu.Unlock()
}

// --- scope by content --------------------------------------------------

// PropagationAction is what one worktree does about one path a base delta
// changed.
type PropagationAction string

const (
	// PropagationUntouched: the worktree does not cover the path and already
	// holds the base's new bytes there.
	PropagationUntouched PropagationAction = "untouched"
	// PropagationDropCoverage: the worktree covers the path, but its bytes now
	// equal the base's new bytes, so its own copy is redundant.
	PropagationDropCoverage PropagationAction = "drop_coverage"
	// PropagationKeep: the worktree covers the path with bytes of its own that
	// still differ from the base's; nothing happens at the path.
	PropagationKeep PropagationAction = "keep"
	// PropagationReassert: the worktree does not cover the path and holds
	// other bytes than the base's new ones (the base moved, the worktree did
	// not): its own file is re-asserted, one parse.
	PropagationReassert PropagationAction = "reassert"
)

// BaseDeltaPath is one path a base advance changed, with the git blob object
// ids on either side ("" where the path is absent).
type BaseDeltaPath struct {
	Path    string
	OldBlob string
	NewBlob string
}

// BaseDelta is what a base advance changed, by content.
type BaseDelta struct {
	FromTree string
	ToTree   string
	Paths    []BaseDeltaPath
	// ChangedDeclarations are the node ids whose identity or shape the
	// advance changed, when the producer of the delta knows them (the delta
	// writer's changed-symbol set). nil means unknown: every declaration at a
	// re-asserted path is then treated as changed.
	ChangedDeclarations []string
	// ServedNewBytes is true when the worktree's view served the base's NEW
	// bytes at paths it does not cover before the rebase — a base rewritten
	// in place, the legacy regime's per-save. Only then does re-asserting a
	// path change a declaration the worktree's own files are bound to. A
	// committed-base advance leaves it false: the routed stack composed over
	// the old immutable base, so the worktree already served its own bytes.
	ServedNewBytes bool
}

// PropagationScope is one worktree's scope for one base delta.
type PropagationScope struct {
	FromTree string
	ToTree   string
	// Actions maps each changed path (repository-relative) to its action.
	Actions      map[string]PropagationAction
	Untouched    []string
	DropCoverage []string
	Keep         []string
	Reassert     []string
	// Rebind lists the worktree's covered paths whose rows bind to a
	// declaration whose view changes for this worktree (only possible when
	// BaseDelta.ServedNewBytes).
	Rebind []string
}

// Empty reports whether the delta leaves the worktree's layers alone: nothing
// to re-assert, no coverage to drop, nothing to re-bind.
func (s *PropagationScope) Empty() bool {
	return s == nil || len(s.Reassert) == 0 && len(s.DropCoverage) == 0 && len(s.Rebind) == 0
}

// Parses is how many files applying the scope parses: one per re-asserted path
// and per re-bound path.
func (s *PropagationScope) Parses() int {
	if s == nil {
		return 0
	}
	return len(s.Reassert) + len(s.Rebind)
}

// propagationWorktree is what the scope needs to know about one worktree.
type propagationWorktree struct {
	// Covered is the set of repository-relative paths the worktree's routed
	// layers claim.
	Covered map[string]struct{}
	// Content is the worktree's own content identity at a path, as the git
	// blob id in the object format of like (a blob id of the base, which
	// fixes the hash); present is false for a path the worktree does not
	// hold.
	Content func(path, like string) (blob string, present bool, err error)
	// Referrers lists the worktree's covered paths whose rows reference any
	// of the given declaration ids (or, for a nil id list, any declaration at
	// the given paths). nil disables re-binding.
	Referrers func(declarationIDs []string, paths []string) ([]string, error)
}

// scopeBaseDelta classifies every path of a base delta for one worktree by
// content. It reads the worktree's bytes only at the changed paths.
func scopeBaseDelta(delta BaseDelta, wt propagationWorktree) (*PropagationScope, error) {
	scope := &PropagationScope{
		FromTree: delta.FromTree,
		ToTree:   delta.ToTree,
		Actions:  make(map[string]PropagationAction, len(delta.Paths)),
	}
	for _, change := range delta.Paths {
		like := change.NewBlob
		if like == "" {
			like = change.OldBlob
		}
		blob, present, err := wt.Content(change.Path, like)
		if err != nil {
			return nil, fmt.Errorf("indexer: content identity of %s: %w", change.Path, err)
		}
		equal := present == (change.NewBlob != "") && (!present || blob == change.NewBlob)
		_, covered := wt.Covered[change.Path]
		var action PropagationAction
		switch {
		case covered && equal:
			action = PropagationDropCoverage
			scope.DropCoverage = append(scope.DropCoverage, change.Path)
		case covered:
			action = PropagationKeep
			scope.Keep = append(scope.Keep, change.Path)
		case equal:
			action = PropagationUntouched
			scope.Untouched = append(scope.Untouched, change.Path)
		default:
			action = PropagationReassert
			scope.Reassert = append(scope.Reassert, change.Path)
		}
		scope.Actions[change.Path] = action
	}
	if delta.ServedNewBytes && wt.Referrers != nil && len(scope.Reassert) > 0 {
		declarations := delta.ChangedDeclarations
		if declarations != nil {
			declarations = declarationsAtPaths(declarations, scope.Reassert)
		}
		if declarations == nil || len(declarations) > 0 {
			referrers, err := wt.Referrers(declarations, scope.Reassert)
			if err != nil {
				return nil, fmt.Errorf("indexer: referrers of the re-asserted declarations: %w", err)
			}
			seen := map[string]struct{}{}
			for _, p := range scope.Reassert {
				seen[p] = struct{}{}
			}
			for _, p := range referrers {
				if _, covered := wt.Covered[p]; !covered {
					continue
				}
				if _, dup := seen[p]; dup {
					continue
				}
				seen[p] = struct{}{}
				scope.Rebind = append(scope.Rebind, p)
			}
		}
	}
	for _, list := range [][]string{scope.Untouched, scope.DropCoverage, scope.Keep, scope.Reassert, scope.Rebind} {
		sort.Strings(list)
	}
	return scope, nil
}

// declarationsAtPaths keeps the ids whose file is one of paths. A node id
// names its repository-prefixed file before "::"; the comparison is on the
// repository-relative tail, so it holds whatever the prefix.
func declarationsAtPaths(ids []string, paths []string) []string {
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	out := []string{}
	for _, id := range ids {
		file, _, _ := strings.Cut(id, "::")
		for p := range want {
			if file == p || strings.HasSuffix(file, "/"+p) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// baseTreeDelta lists the paths two committed trees differ by, with their blob
// ids, from one `git diff-tree`. A rename decomposes into a delete and an add,
// as the layers' masks speak about paths.
func baseTreeDelta(ctx context.Context, repoDir, fromTree, toTree string) (BaseDelta, error) {
	delta := BaseDelta{FromTree: fromTree, ToTree: toTree}
	if fromTree == "" || toTree == "" || fromTree == toTree {
		return delta, nil
	}
	out, err := gitcmd.RunNoLazy(ctx, repoDir,
		"diff-tree", "-r", "--no-renames", "--no-commit-id", "-z", fromTree, toTree)
	if err != nil {
		return delta, fmt.Errorf("indexer: diff-tree %s..%s in %s: %w", fromTree, toTree, repoDir, err)
	}
	paths, err := parseRawDiffTree(string(out))
	if err != nil {
		return delta, err
	}
	delta.Paths = paths
	return delta, nil
}

// parseRawDiffTree parses `git diff-tree -r -z` raw output:
// ":<old mode> <new mode> <old oid> <new oid> <status>\0<path>\0" per record.
func parseRawDiffTree(out string) ([]BaseDeltaPath, error) {
	fields := strings.Split(out, "\x00")
	var paths []BaseDeltaPath
	for i := 0; i < len(fields); i++ {
		header := fields[i]
		if header == "" {
			continue
		}
		if !strings.HasPrefix(header, ":") {
			return nil, fmt.Errorf("indexer: unexpected diff-tree record %q", header)
		}
		parts := strings.Fields(strings.TrimPrefix(header, ":"))
		if len(parts) < 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("indexer: truncated diff-tree record %q", header)
		}
		i++
		change := BaseDeltaPath{Path: path.Clean(fields[i]), OldBlob: parts[2], NewBlob: parts[3]}
		if zeroOID(change.OldBlob) {
			change.OldBlob = ""
		}
		if zeroOID(change.NewBlob) {
			change.NewBlob = ""
		}
		// A submodule (gitlink, mode 160000) is not content a layer indexes.
		if parts[0] == "160000" || parts[1] == "160000" {
			continue
		}
		paths = append(paths, change)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	return paths, nil
}

func zeroOID(oid string) bool { return strings.Trim(oid, "0") == "" }

// workingBlobID is the git blob id of a checkout's file at rel, in the object
// format like names (a 40-hex id is SHA-1, a 64-hex one SHA-256). present is
// false when the checkout holds no regular file there.
func workingBlobID(root, rel, like string) (string, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	var content []byte
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", false, err
		}
		content = []byte(target)
	case info.Mode().IsRegular():
		content, err = os.ReadFile(full)
		if err != nil {
			return "", false, err
		}
	default:
		return "", false, nil
	}
	return gitBlobID(content, len(like) == 64), true, nil
}

// gitBlobID hashes content the way git names a blob.
func gitBlobID(content []byte, sha256Format bool) string {
	header := []byte(fmt.Sprintf("blob %d\x00", len(content)))
	if sha256Format {
		h := sha256.New()
		h.Write(header)
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New() //nolint:gosec // git's object format
	h.Write(header)
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// coveredPaths is the set of repository-relative paths a routed stack's
// layers claim: the commit layer's and every working-tree chain member's.
func (c *CheckoutCoordinator) coveredPaths(ctx context.Context, generations ...int64) (map[string]struct{}, error) {
	covered := map[string]struct{}{}
	for _, id := range generations {
		if id <= 0 {
			continue
		}
		// The file masks alone: one indexed read per generation, cheap
		// enough for the per-edit fold check.
		masks, err := c.store.AtGeneration(id).FileMasksContext(ctx)
		if err != nil {
			return nil, err
		}
		for _, mask := range masks {
			if mask.Mode != store_sqlite.OwnershipReplace && mask.Mode != store_sqlite.OwnershipDelete {
				continue
			}
			if rel, ok := builderRelPath(c.repoPrefix, mask.FilePath); ok {
				covered[rel] = struct{}{}
			}
		}
	}
	return covered, nil
}

// propagationScopeFor computes this checkout's scope for a committed base
// advance from oldTree to newTree, over the routed commit generation and the
// working-tree chain topped by dirtyTop.
func (c *CheckoutCoordinator) propagationScopeFor(
	ctx context.Context, oldTree, newTree string, commitGeneration, dirtyTop int64,
) (*PropagationScope, error) {
	delta, err := baseTreeDelta(ctx, c.root, oldTree, newTree)
	if err != nil {
		return nil, err
	}
	layers := append([]int64{commitGeneration}, c.dirtyChainMembers(ctx, dirtyTop)...)
	covered, err := c.coveredPaths(ctx, layers...)
	if err != nil {
		return nil, err
	}
	root := c.root
	return scopeBaseDelta(delta, propagationWorktree{
		Covered: covered,
		Content: func(rel, like string) (string, bool, error) { return workingBlobID(root, rel, like) },
	})
}

// --- re-parenting the working-tree chain by copy ------------------------

// errReparentRefused is a copy the coordinator declines; the caller rebuilds
// the chain instead. It is never a cycle failure.
var errReparentRefused = errors.New("indexer: working-tree chain not carried over")

// reparentDirtyChain carries the routed working-tree chain topped by top from
// oldCommit over to newCommit by copying each member's rows into a new
// generation whose physical parent is the previous copy (the bottom one sits on
// newCommit). Nothing is parsed. It returns the new top and its logical reuse
// key.
//
// It refuses (errReparentRefused, nothing left behind) when the chain is not a
// well-formed chain rooted at oldCommit, when its identity differs from the one
// a build would mint over newCommit in anything but the base, or when the
// working tree no longer carries the fingerprint the chain was published for.
func (c *CheckoutCoordinator) reparentDirtyChain(
	ctx context.Context,
	graphID string,
	top int64,
	oldCommit store_sqlite.ViewGeneration,
	newCommit int64,
	fingerprint string,
) (int64, string, error) {
	chain, ok, reason, err := c.dirtyChainRoot(ctx, top, oldCommit, maxDirtyChainDepth)
	if err != nil {
		return 0, "", err
	}
	if !ok || len(chain) == 0 {
		return 0, "", fmt.Errorf("%w: %s", errReparentRefused, reason)
	}
	head := chain[0]
	if fingerprint == "" || head.LowerViewFingerprint != fingerprint {
		return 0, "", fmt.Errorf("%w: the working tree moved since the chain was published", errReparentRefused)
	}
	want := c.dirtyIdentity(graphID, newCommit)
	if head.ConfigHash != want.ConfigHash || head.ExtractorVersions != want.ExtractorVersions ||
		head.ResolverVersion != want.ResolverVersion || head.DependencyRevision != want.DependencyRevision ||
		head.GraphID != want.GraphID || head.LayerID != want.LayerID || head.CheckoutID != want.CheckoutID {
		return 0, "", fmt.Errorf("%w: the chain was built under another identity", errReparentRefused)
	}
	var built []int64
	abandon := func() {
		for _, id := range built {
			c.abandonCopiedGeneration(ctx, id)
		}
	}
	parent := newCommit
	now := time.Now().Unix()
	// Bottom first: every copy's parent must already exist.
	for i := len(chain) - 1; i >= 0; i-- {
		member := chain[i]
		generationID, _, adopted, err := c.store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
			OwnerKind: member.OwnerKind, GraphID: member.GraphID, LayerID: member.LayerID,
			CheckoutID: member.CheckoutID, GenerationKind: member.GenerationKind,
			BaseGenerationID:     parent,
			LowerViewFingerprint: member.LowerViewFingerprint, TreeOID: member.TreeOID,
			ProvenanceCommitOID: member.ProvenanceCommitOID, ConfigHash: member.ConfigHash,
			ExtractorVersions: member.ExtractorVersions, ResolverVersion: member.ResolverVersion,
			DependencyRevision: member.DependencyRevision, CreatedAt: now,
		})
		if err != nil {
			abandon()
			return 0, "", fmt.Errorf("indexer: begin the carried-over working-tree generation: %w", err)
		}
		if adopted {
			// Another writer is building this very identity; it owns it.
			abandon()
			return 0, "", fmt.Errorf("%w: generation %d is being built by another writer", errReparentRefused, generationID)
		}
		built = append(built, generationID)
		if _, err := c.store.CopyGenerationPayloadWhole(ctx, member.GenerationID, generationID); err != nil {
			abandon()
			return 0, "", fmt.Errorf("indexer: copy working-tree generation %d into %d: %w", member.GenerationID, generationID, err)
		}
		if err := c.store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix()); err != nil {
			abandon()
			return 0, "", fmt.Errorf("indexer: publish carried-over working-tree generation %d: %w", generationID, err)
		}
		parent = generationID
	}
	newTop := built[len(built)-1]
	row, found, err := c.catalog.GetViewGeneration(ctx, newTop)
	if err != nil || !found {
		abandon()
		if err == nil {
			err = fmt.Errorf("indexer: carried-over generation %d vanished", newTop)
		}
		return 0, "", err
	}
	return newTop, logicalDirtyKey(row, newCommit), nil
}

// abandonCopiedGeneration gives up one generation a refused or failed
// re-parent created: a building one is marked failed, a published one
// superseded, and either is owed to the background sweep.
func (c *CheckoutCoordinator) abandonCopiedGeneration(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	err := c.catalog.SetViewGenerationState(ctx, generationID,
		store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding)
	if err != nil {
		// Not building any more: it was published.
		c.supersede(ctx, generationID)
	}
	c.deferRetire(generationID, "carried-over working-tree generation not routed")
}
