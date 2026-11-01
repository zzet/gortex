package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
)

// All filesystem inputs to advisory naming are captured without retaining a
// lifecycle, config manager, store or indexer. The same two-slot allowance as
// root Stat bounds physically stuck discovery. Late results cannot publish.
type trackNamingFacts struct {
	canonical string
	workspace *config.Config
	parseErr  error
	inventory *gitstate.FamilyInventory
	record    *gitstate.WorktreeRecord
	err       error
}

func trackNamingFailure(ctx context.Context, err error) error {
	if ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errPrefixStatUnavailable)) {
		return fmt.Errorf("%w: %w", errPrefixIdentityUnknown, err)
	}
	return err
}

func readTrackCanonical(_ context.Context, root string) trackNamingFacts {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return trackNamingFacts{err: err}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return trackNamingFacts{err: err}
	}
	if !info.IsDir() {
		return trackNamingFacts{err: fmt.Errorf("invalid path: %s (not a directory)", root)}
	}
	return trackNamingFacts{canonical: canonical}
}

func readTrackNaming(ctx context.Context, root string) trackNamingFacts {
	facts := readTrackCanonical(ctx, root)
	if facts.err != nil {
		return facts
	}
	facts.workspace, facts.parseErr = config.ParseWorkspaceFile(filepath.Join(root, ".gortex.yaml"))
	// Non-git directories retain basename naming, as ordinary track does.
	facts.inventory, _ = gitstate.Inventory(ctx, root)
	if facts.inventory != nil {
		facts.record = recordForRoot(facts.inventory, root)
	}
	return facts
}

func (a *prefixStatAllowance) naming(ctx context.Context, root string) (trackNamingFacts, error) {
	return a.observeNaming(ctx, root, readTrackNaming)
}

func (a *prefixStatAllowance) canonical(ctx context.Context, root string) (trackNamingFacts, error) {
	return a.observeNaming(ctx, root, readTrackCanonical)
}

// The two fixed discovery operations share the existing identity allowance.
// Canonical membership lookup must not parse workspace configuration or run git.
func (a *prefixStatAllowance) observeNaming(ctx context.Context, root string, read func(context.Context, string) trackNamingFacts) (trackNamingFacts, error) {
	if err := ctx.Err(); err != nil {
		return trackNamingFacts{}, err
	}
	a.once.Do(func() { a.slots = make(chan struct{}, 2) })
	select {
	case a.slots <- struct{}{}:
	default:
		return trackNamingFacts{}, errPrefixStatUnavailable
	}
	slots := a.slots
	done := make(chan trackNamingFacts, 1)
	deadline, _ := ctx.Deadline()
	workerCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	go func() {
		defer func() { <-slots }()
		done <- read(workerCtx, root)
	}()
	select {
	case <-ctx.Done():
		return trackNamingFacts{}, ctx.Err()
	case facts := <-done:
		if err := ctx.Err(); err != nil {
			return trackNamingFacts{}, err
		}
		return facts, facts.err
	}
}

func (mi *MultiIndexer) effectiveTrackPrefix(ctx context.Context, entry config.RepoEntry, before time.Time) (string, error) {
	base := config.ResolvePrefix(entry)
	if entry.Name != "" {
		return base, nil
	}
	budget, cancel := context.WithDeadline(ctx, before)
	defer cancel()
	facts, err := mi.prefixStats.naming(budget, entry.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return base, nil // absent configured owners retain their basename
		}
		return "", trackNamingFailure(ctx, err)
	}
	return mi.trackPrefixFromFacts(entry, facts), nil
}

func (mi *MultiIndexer) trackPrefixFromFacts(entry config.RepoEntry, facts trackNamingFacts) string {
	base := config.ResolvePrefix(entry)
	if entry.Name != "" {
		return base
	}
	workspace := entry.Workspace
	if workspace == "" {
		cfg := facts.workspace
		if facts.parseErr != nil && !os.IsNotExist(facts.parseErr) && mi.configMgr != nil {
			cfg = mi.configMgr.LastGoodWorkspace(base, entry.Path)
		}
		if cfg != nil {
			workspace = cfg.Workspace
		}
	}
	linked := facts.record != nil && facts.record.AdminName != gitstate.MainAdminName
	if !entry.AsWorktree && (!linked || workspace == "" || workspace == base) {
		return base
	}
	tag := ""
	if workspace != "" && workspace != base {
		tag = workspace
	} else if facts.record != nil {
		tag = facts.record.Branch
	}
	if tag = sanitizeInstanceTag(tag); tag != "" {
		return base + "@" + tag
	}
	return base + "@" + shortPathHash(entry.Path)
}

// Catalog naming consumes completed discovery; no unjoinable worker can retain
// this lifecycle. Reads respect the remaining admission budget.
func (l *CheckoutLifecycle) dedicatedPrefixFromFacts(ctx context.Context, root string, facts trackNamingFacts, before time.Time) (string, error) {
	if l.catalog == nil || facts.inventory == nil || facts.record == nil || facts.record.AdminName == gitstate.MainAdminName {
		return "", nil
	}
	familyID := FamilyIDFor(facts.inventory.CommonDir)
	if prefix := l.boundPrefixForAdminName(ctx, familyID, facts.record.AdminName); prefix != "" {
		return prefix, nil
	}
	base := l.familyBasePrefix(ctx, familyID)
	if base == "" {
		return "", nil
	}
	prefix := DedicatedWorktreePrefix(base, facts.record.AdminName, root, nil)
	holder, taken := l.prefixHolder(ctx)(prefix)
	if !taken || holder == root || holder == facts.canonical {
		return prefix, nil
	}
	budget, cancel := context.WithDeadline(ctx, before)
	defer cancel()
	owner := l.mi.prefixStats.observe(budget, holder)
	candidate := l.mi.prefixStats.observe(budget, root)
	if sameObservedDirectory(owner, candidate) {
		return prefix, nil
	}
	if owner.err != nil || candidate.err != nil {
		return "", errPrefixIdentityUnknown
	}
	return WorktreeInstanceCollisionName(prefix, root), nil
}
