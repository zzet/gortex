package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

const prefixOwnershipTimeout = 2 * time.Second

var (
	errPrefixStatUnavailable = errors.New("repository identity stat allowance is occupied")
	errPrefixOwnerChanged    = errors.New("repository prefix owner changed during identity observation; retry the operation")
	errPrefixConfigChanged   = errors.New("repository configuration changed during prefix preparation; retry the operation")
	errPrefixIdentityUnknown = errors.New("repository root identity could not be established")
)

// ErrRepoIdentityPending is retryable uncertainty about a configured physical
// root. No track/config/catalog publication is authorized by this outcome.
var ErrRepoIdentityPending = errPrefixIdentityUnknown

// prefixStatAllowance is deliberately not a filesystem worker service. Native
// filesystem calls have no portable cancellation API. At most two observations per indexer
// may remain physically stuck; further callers abstain without starting work.
// Each worker captures only immutable path/function/channel inputs, never an
// indexer, store or publication callback. Late results fit in a private buffer
// and are discarded. Close therefore does NOT join a hung OS Stat, and makes no
// physical OS cancellation guarantee; these workers cannot access the store.
type prefixStatAllowance struct {
	once  sync.Once
	slots chan struct{}
	stat  func(string) (os.FileInfo, error) // nil in production; instance-local test seam
}

type prefixStatResult struct {
	info os.FileInfo
	err  error
}

func (a *prefixStatAllowance) observe(ctx context.Context, path string) prefixStatResult {
	if err := ctx.Err(); err != nil {
		return prefixStatResult{err: err}
	}
	a.once.Do(func() { a.slots = make(chan struct{}, 2) })
	select {
	case a.slots <- struct{}{}:
	default:
		return prefixStatResult{err: errPrefixStatUnavailable}
	}
	stat := a.stat
	if stat == nil {
		stat = os.Stat
	}
	slots := a.slots
	done := make(chan prefixStatResult, 1)
	go func() {
		defer func() { <-slots }()
		info, err := stat(path)
		done <- prefixStatResult{info: info, err: err}
	}()
	select {
	case <-ctx.Done():
		return prefixStatResult{err: ctx.Err()}
	case result := <-done:
		// Do not accept a late observation when deadline/result race.
		if err := ctx.Err(); err != nil {
			return prefixStatResult{err: err}
		}
		return result
	}
}

// A witness copies identity strings under mu. Its metadata pointer is used
// only for equality outside the lock, never dereferenced there. Replacing an
// entry at the same root (ABA by spelling) must invalidate the observation.
type prefixOwnerWitness struct {
	prefix string
	root   string
	meta   *RepoMetadata
}

type prefixOwnerSnapshot struct {
	requested string
	root      string
	owner     prefixOwnerWitness
	aliases   []prefixOwnerWitness
}

// prefixOwnerSnapshotLocked performs no filesystem operations. Only the
// requested owner and cheap lexical same-root candidates can require stats;
// it never stats every registered root on a track.
func (mi *MultiIndexer) prefixOwnerSnapshotLocked(prefix, root string) prefixOwnerSnapshot {
	out := prefixOwnerSnapshot{requested: prefix, root: root}
	if owner := mi.repos[prefix]; owner != nil {
		out.owner = prefixOwnerWitness{prefix: prefix, root: owner.RootPath, meta: owner}
	}
	for name, meta := range mi.repos {
		if meta != nil && pathkey.EqualPaths(meta.RootPath, root) {
			out.aliases = append(out.aliases, prefixOwnerWitness{prefix: name, root: meta.RootPath, meta: meta})
		}
	}
	sort.Slice(out.aliases, func(i, j int) bool { return out.aliases[i].prefix < out.aliases[j].prefix })
	return out
}

func (s prefixOwnerSnapshot) matchesLocked(mi *MultiIndexer) bool {
	next := mi.prefixOwnerSnapshotLocked(s.requested, s.root)
	if s.owner != next.owner || len(s.aliases) != len(next.aliases) {
		return false
	}
	for i := range s.aliases {
		if s.aliases[i] != next.aliases[i] {
			return false
		}
	}
	return true
}

type prefixOwnershipObservation struct {
	snapshot         prefixOwnerSnapshot
	registeredPrefix string
	candidate        prefixStatResult
	owner            prefixStatResult
	collision        *PrefixCollisionError
}

// observePrefixOwnership returns a witnessed decision, not a registration
// reservation. A caller that publishes under mu must still check matchesLocked
// there. The stable repository mutation lane retains its own admission recheck.
// Both stats and one owner-replacement retry share this ONE deadline.
func (mi *MultiIndexer) observePrefixOwnership(ctx context.Context, prefix, root string) (prefixOwnershipObservation, error) {
	return mi.observePrefixOwnershipBefore(ctx, prefix, root, time.Now().Add(prefixOwnershipTimeout), nil)
}

// before is shared with candidate validation during one preparation. A later
// coordinated re-admission receives its own budget, not an expired deadline
// from time spent waiting for the repository's stable mutation lane.
func (mi *MultiIndexer) observePrefixOwnershipBefore(ctx context.Context, prefix, root string, before time.Time, candidate *prefixStatResult) (prefixOwnershipObservation, error) {
	if err := ctx.Err(); err != nil {
		return prefixOwnershipObservation{}, err
	}
	budget, cancel := context.WithDeadline(ctx, before)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		mi.mu.RLock()
		snapshot := mi.prefixOwnerSnapshotLocked(prefix, root)
		mi.mu.RUnlock()
		observation, err := mi.samplePrefixOwnership(budget, snapshot, candidate)
		if ctx.Err() != nil {
			return prefixOwnershipObservation{}, ctx.Err()
		}
		mi.mu.RLock()
		current := snapshot.matchesLocked(mi)
		mi.mu.RUnlock()
		if current {
			return observation, err
		}
		// Do not reuse FileInfo across a replacement, even when the new owner
		// has the same spelling. Resample under the original remaining budget.
		candidate = nil
	}
	return prefixOwnershipObservation{}, errPrefixOwnerChanged
}

func (mi *MultiIndexer) samplePrefixOwnership(ctx context.Context, snapshot prefixOwnerSnapshot, candidate *prefixStatResult) (prefixOwnershipObservation, error) {
	out := prefixOwnershipObservation{snapshot: snapshot}
	for _, alias := range snapshot.aliases {
		if alias.root == snapshot.root {
			out.registeredPrefix = alias.prefix
			return out, nil // byte-identical registered identity needs no Stat
		}
	}
	if snapshot.owner.meta == nil && len(snapshot.aliases) == 0 {
		return out, nil
	}
	if candidate != nil {
		out.candidate = *candidate
	} else {
		out.candidate = mi.prefixStats.observe(ctx, snapshot.root)
	}
	owners := make(map[string]prefixStatResult, len(snapshot.aliases)+1)
	ownerStat := func(root string) prefixStatResult {
		if result, ok := owners[root]; ok {
			return result
		}
		result := mi.prefixStats.observe(ctx, root)
		owners[root] = result
		return result
	}
	for _, alias := range snapshot.aliases {
		owner := ownerStat(alias.root)
		if sameObservedDirectory(out.candidate, owner) {
			out.registeredPrefix = alias.prefix
			return out, nil
		}
	}
	if snapshot.owner.meta == nil {
		if out.candidate.err != nil {
			return out, fmt.Errorf("%w: %w", errPrefixIdentityUnknown, out.candidate.err)
		}
		if out.candidate.info == nil {
			return out, errPrefixIdentityUnknown
		}
		// A folded candidate with unavailable filesystem identity cannot be
		// certified distinct merely because the requested prefix is unused.
		for _, alias := range snapshot.aliases {
			owner := ownerStat(alias.root)
			if owner.err != nil {
				return out, fmt.Errorf("%w: %w", errPrefixIdentityUnknown, owner.err)
			}
			if owner.info == nil {
				return out, errPrefixIdentityUnknown
			}
		}
		return out, nil
	}
	out.owner = ownerStat(snapshot.owner.root)
	if sameObservedDirectory(out.candidate, out.owner) {
		out.registeredPrefix = snapshot.owner.prefix
		return out, nil
	}
	out.collision = newObservedPrefixCollision(snapshot.requested, snapshot.owner.root, snapshot.root, out.owner)
	return out, out.collision
}

func newObservedPrefixCollision(prefix, owner, root string, observed prefixStatResult) *PrefixCollisionError {
	state := "unknown"
	if errors.Is(observed.err, os.ErrNotExist) {
		state = "missing"
	} else if observed.err == nil && observed.info != nil {
		state = "present"
	}
	return &PrefixCollisionError{Prefix: prefix, ExistingPath: owner, Path: root, OwnerMissing: state == "missing", OwnerState: state}
}

// registeredPrefixForExactRoot deliberately does not use ResolvePrefix: a
// registered ancestor is useful for reads, but cannot authorize recording a
// new checkout or graph binding for a descendant that was never indexed.
func (mi *MultiIndexer) registeredPrefixForExactRoot(ctx context.Context, requested, root string) (string, error) {
	observation, err := mi.observePrefixOwnership(ctx, requested, root)
	if err != nil {
		return "", err
	}
	if observation.registeredPrefix != "" {
		return observation.registeredPrefix, nil
	}
	return "", newObservedPrefixCollision(requested, observation.snapshot.owner.root, root,
		prefixStatResult{err: errPrefixIdentityUnknown})
}

// prefixOwnerForRoot is stricter than a same-root registration under ANY name:
// promotion and rollback need this specific prefix's corpus to be the root's.
func (mi *MultiIndexer) prefixOwnerForRoot(ctx context.Context, prefix, root string) (prefixOwnerWitness, error) {
	observation, err := mi.observePrefixOwnership(ctx, prefix, root)
	if err != nil {
		return prefixOwnerWitness{}, err
	}
	owner := observation.snapshot.owner
	if owner.meta != nil && (owner.root == root || observation.registeredPrefix == prefix) {
		return owner, nil
	}
	return prefixOwnerWitness{}, newObservedPrefixCollision(prefix, owner.root, root, observation.owner)
}

// Callers hold mi.mu. The comparison is memory-only, including the original
// immutable root string; no availability probe runs while closing admission.
func (mi *MultiIndexer) validatePrefixCleanupOwnerLocked(prefix string, expected *prefixOwnerWitness) error {
	if expected == nil {
		return nil // ordinary authoritative untrack keeps its existing contract
	}
	current := mi.repos[prefix]
	if current != nil && current != expected.meta {
		if current.RootPath != expected.root {
			return newObservedPrefixCollision(prefix, current.RootPath, expected.root, prefixStatResult{err: errPrefixOwnerChanged})
		}
		return errPrefixOwnerChanged
	}
	owned := current
	if state := mi.pendingRepositoryUntracks[prefix]; state != nil {
		owned = state.metadata // retained cleanup owns this immutable snapshot
	}
	if prefix != expected.prefix || owned == nil || owned != expected.meta || owned.RootPath != expected.root {
		return errPrefixOwnerChanged
	}
	return nil
}

func sameObservedDirectory(a, b prefixStatResult) bool {
	return a.err == nil && b.err == nil && a.info != nil && b.info != nil &&
		a.info.IsDir() && b.info.IsDir() && os.SameFile(a.info, b.info)
}
