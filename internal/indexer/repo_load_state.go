package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

// RepoLoadState is where one configured repository stands on its way into the
// registry after a daemon start.
//
// The registry (MultiIndexer.repos) starts empty on every boot and a repository
// enters it only when its own warmup work finishes, which on a large store is
// minutes after the socket is serving. Without this state a configured
// repository read as untracked for that whole window: sessions were told to
// re-track it, the CLI printed "add it with gortex track", and agents stopped.
// Every configured repository now reads as loading until it reaches one of the
// three terminal states, so "configured, still warming" and "nobody tracks
// this" are told apart.
type RepoLoadState string

const (
	// RepoLoadIdentityPending is an unmatched request while configured root
	// identities are unresolved. It names no repository and grants no scope.
	RepoLoadIdentityPending RepoLoadState = "identity_pending"
	// RepoLoadQueued is a configured repository warmup has not started yet.
	RepoLoadQueued RepoLoadState = "queued"
	// RepoLoadRunning is a configured repository warmup is indexing or
	// reconciling right now.
	RepoLoadRunning RepoLoadState = "running"
	// RepoLoadReady is a registered repository: graph tools answer for it.
	RepoLoadReady RepoLoadState = "ready"
	// RepoLoadFailed is a repository whose warmup job ended without
	// registering it. Reason says why.
	RepoLoadFailed RepoLoadState = "failed"
	// RepoLoadMissing is a configured repository whose path does not exist.
	RepoLoadMissing RepoLoadState = "missing"
)

// Loading reports a state that has not reached a terminal answer yet.
func (s RepoLoadState) Loading() bool {
	return s == RepoLoadQueued || s == RepoLoadRunning || s == RepoLoadIdentityPending
}

// Ready reports a registered repository.
func (s RepoLoadState) Ready() bool { return s == RepoLoadReady }

// Unavailable reports a terminal state that will not become ready without
// intervention.
func (s RepoLoadState) Unavailable() bool {
	return s == RepoLoadFailed || s == RepoLoadMissing
}

// RepoLoad is one configured repository's progress into the registry.
type RepoLoad struct {
	// Root is the repository root as configured, made absolute.
	Root string `json:"root"`
	// Prefix is the repository's prefix once registered; before that it is
	// the prefix its config entry resolves to.
	Prefix string        `json:"prefix,omitempty"`
	State  RepoLoadState `json:"state"`
	// Reason explains a failed or missing repository.
	Reason string `json:"reason,omitempty"`
	// Since is when the repository entered State.
	Since time.Time `json:"since"`

	// key and canonicalKey are the lexical and symlink-resolved identity keys
	// of Root, computed once when the record is created so a snapshot rebuild
	// never touches the filesystem.
	key          string
	canonicalKey string
	// commonDir is the shared git directory resolved off the serving path.
	// A declaration uses only root/.git as a loading-only candidate until
	// warmup resolves identity; a request must independently resolve its own
	// linked-worktree marker before this can produce a pending refusal.
	commonDir string
	// identityPending distinguishes a lexical boot declaration from the
	// filesystem identity warmup fills in. It grants no graph admission.
	identityPending bool
	identityToken   *byte
}

// Label names the repository in user-facing text: its prefix, or its root
// when no prefix is known.
func (r RepoLoad) Label() string {
	if r.Prefix != "" {
		return r.Prefix
	}
	return r.Root
}

// repoLoadTable is copy-on-write: writers serialise on mu and publish a whole
// new snapshot; readers load the pointer and never block. The admission gate
// and the probe read it on every frame, so neither may wait behind a writer,
// and neither may take MultiIndexer.mu, which topology mutations hold.
type repoLoadTable struct {
	mu        sync.Mutex
	snap      atomic.Pointer[repoLoadSnapshot]
	listeners []func(RepoLoad) // guarded by mu
}

type repoLoadSnapshot struct {
	records []RepoLoad
	// index maps both identity keys of every record to its position.
	index map[string]int
	// loading counts records in a non-terminal state, so the worktree arm
	// can skip its filesystem walk once warmup has settled.
	loading         int
	identityPending bool
}

func newRepoLoadSnapshot(records []RepoLoad) *repoLoadSnapshot {
	snap := &repoLoadSnapshot{records: records, index: make(map[string]int, 2*len(records))}
	for i, rec := range records {
		snap.index[rec.key] = i
		if rec.identityPending {
			snap.identityPending = true
		}
		if rec.canonicalKey != "" {
			snap.index[rec.canonicalKey] = i
		}
		if rec.State.Loading() {
			snap.loading++
		}
	}
	return snap
}

func (t *repoLoadTable) load() *repoLoadSnapshot {
	if snap := t.snap.Load(); snap != nil {
		return snap
	}
	return &repoLoadSnapshot{}
}

// find returns the position of the record whose root is path.
func (s *repoLoadSnapshot) find(path string) (int, bool) {
	if len(s.records) == 0 || path == "" {
		return 0, false
	}
	if i, ok := s.index[pathkey.IdentityKey(path)]; ok {
		return i, true
	}
	return 0, false
}

// update applies mutate to a private copy of the records and publishes it.
// mutate reports whether anything changed; an unchanged table republishes
// nothing.
func (t *repoLoadTable) update(mutate func(records []RepoLoad) ([]RepoLoad, bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.load()
	next, changed := mutate(slices.Clone(current.records))
	if changed {
		t.snap.Store(newRepoLoadSnapshot(next))
	}
}

// newRepoLoad builds a record for root, reading the filesystem for its
// canonical spelling and git common directory. Called only off the hot path:
// when a start announces its repositories and when one registers.
func newRepoLoad(root, prefix string, state RepoLoadState, now time.Time) RepoLoad {
	rec := RepoLoad{Root: root, Prefix: prefix, State: state, Since: now,
		key: pathkey.IdentityKey(root), identityPending: true, identityToken: new(byte)}
	resolved, err := resolveRepoLoadIdentity(rec)
	if err != nil {
		rec.Reason = "identity resolution pending: " + err.Error()
		return rec
	}
	// Registration is authoritative about ready state even if its directory
	// disappeared since indexing. Identity absence is a separate observation.
	resolved.State = state
	return resolved
}

// absRepoRoot spells a configured path the way registration does.
func absRepoRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	return pathkey.NormalizeVolume(abs)
}

// DeclareRepos publishes configured roots before the daemon listens without
// touching those roots, resolving symlinks or reading git metadata. A hung
// configured mount must not delay the listener or hide the other declarations.
// ExpectRepos fills in filesystem identity and missing-path state during warmup.
func (mi *MultiIndexer) DeclareRepos(entries []config.RepoEntry) {
	mi.expectRepos(entries, false)
}

// ExpectRepos records the configured repositories a start is about to load.
//
// Called off the serving path before checkout seeding and again when warmup
// takes its own snapshot of the config. DeclareRepos publishes lexical roots
// before the socket serves; this enriches those declarations without resetting
// progress or a registration that raced with warmup. A repository already recorded keeps its record;
// one that is no longer configured and not registered is dropped, so a stale
// entry can never read as loading forever.
func (mi *MultiIndexer) ExpectRepos(entries []config.RepoEntry) {
	mi.expectRepos(entries, true)
}

func (mi *MultiIndexer) expectRepos(entries []config.RepoEntry, inspect bool) {
	if mi == nil {
		return
	}
	now := time.Now()
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		current := newRepoLoadSnapshot(records)
		wanted := make(map[int]bool, len(entries))
		addedKeys := make(map[string]bool)
		var added []RepoLoad
		for _, entry := range entries {
			root := absRepoRoot(entry.Path)
			key := pathkey.IdentityKey(root)
			if root == "" || addedKeys[key] {
				continue
			}
			if i, ok := current.find(root); ok {
				wanted[i] = true
				continue
			}
			added = append(added, RepoLoad{Root: root, Prefix: config.ResolvePrefix(entry), State: RepoLoadQueued,
				Since: now, key: key, commonDir: filepath.Join(root, ".git"), identityPending: true, identityToken: new(byte)})
			addedKeys[key] = true
		}
		next := make([]RepoLoad, 0, len(records)+len(added))
		for i, rec := range records {
			if wanted[i] || rec.State.Ready() {
				next = append(next, rec)
			}
		}
		next = append(next, added...)
		return next, len(added) > 0 || len(next) != len(records)
	})
	if !inspect {
		return
	}
	// Only the existing warmup owner resolves identities. No filesystem access
	// holds the publication lock; each completed root becomes visible before the
	// next root is inspected, even if that next mount never answers.
	for _, rec := range mi.repoLoads.load().records {
		if !rec.identityPending {
			continue
		}
		resolved, err := resolveRepoLoadIdentity(rec)
		if err != nil {
			// Retain the owner's diagnostic without turning uncertainty into a
			// terminal absent-alias result. No request performs this operation again.
			mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
				i, ok := newRepoLoadSnapshot(records).find(rec.Root)
				if !ok || !records[i].identityPending || records[i].identityToken != rec.identityToken {
					return records, false
				}
				reason := "identity resolution pending: " + err.Error()
				if records[i].Reason == reason {
					return records, false
				}
				records[i].Reason = reason
				return records, true
			})
			continue
		}
		mi.publishRepoLoadIdentity(rec, resolved)
	}
}

func resolveRepoLoadIdentity(rec RepoLoad) (RepoLoad, error) {
	return resolveRepoLoadIdentityWith(rec, filepath.EvalSymlinks)
}

// The resolver argument lets source-level I/O failures be exercised without
// depending on host privileges or an unavailable device. Production uses only
// the existing warmup owner's synchronous filesystem resolver.
func resolveRepoLoadIdentityWith(rec RepoLoad, canonicalize func(string) (string, error)) (RepoLoad, error) {
	canonical, err := canonicalize(rec.Root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return rec, err
		}
		rec.State, rec.Reason = RepoLoadMissing, "the configured path does not exist"
	} else {
		st, statErr := os.Stat(canonical)
		if statErr != nil {
			return rec, statErr
		}
		if !st.IsDir() {
			rec.State, rec.Reason = RepoLoadMissing, "the configured path is not a directory"
		} else {
			rec.canonicalKey = pathkey.IdentityKey(pathkey.NormalizeVolume(canonical))
			info, resolveErr := resolveWorktreeChecked(canonical)
			if resolveErr != nil {
				return rec, resolveErr
			}
			rec.commonDir = ""
			if info.GitCommonDir != "" {
				common, commonErr := filepath.EvalSymlinks(info.GitCommonDir)
				if commonErr != nil {
					return rec, commonErr
				}
				rec.commonDir = pathkey.NormalizeVolume(common)
			}
		}
	}
	rec.identityPending = false
	return rec, nil
}

func (mi *MultiIndexer) publishRepoLoadIdentity(before, resolved RepoLoad) {
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		i, ok := newRepoLoadSnapshot(records).find(before.Root)
		if !ok || !records[i].identityPending || records[i].identityToken != before.identityToken {
			return records, false
		}
		rec := &records[i]
		rec.canonicalKey, rec.commonDir, rec.identityPending = resolved.canonicalKey, resolved.commonDir, false
		if strings.HasPrefix(rec.Reason, "identity resolution pending: ") {
			rec.Reason = ""
		}
		if rec.State == RepoLoadQueued && resolved.State == RepoLoadMissing {
			rec.State, rec.Reason, rec.Since = resolved.State, resolved.Reason, time.Now()
		}
		return records, true
	})
}

// RepoIdentityPending is a cheap negative-admission fence, never proof that a
// particular path belongs to a configured repository. Authoritative positive
// scope/checkout/router answers must take precedence. It remains true after an
// uncertain filesystem outcome until warmup resolves identity or registration
// itself proves ownership.
func (mi *MultiIndexer) RepoIdentityPending() bool {
	return mi != nil && mi.repoLoads.load().identityPending
}

// StartRepoLoad marks a queued repository as being worked on by warmup.
func (mi *MultiIndexer) StartRepoLoad(path string) {
	mi.setRepoLoad(path, func(rec *RepoLoad) bool {
		if rec.State != RepoLoadQueued {
			return false
		}
		rec.State, rec.Since = RepoLoadRunning, time.Now()
		return true
	})
}

// FinishRepoLoad records the end of a repository's warmup job. A job that
// registered the repository already moved it to ready; any other ending is
// terminal — missing when the path is gone, failed otherwise — so loading
// never outlives the job that was supposed to end it.
func (mi *MultiIndexer) FinishRepoLoad(path string, jobErr error) {
	if mi == nil {
		return
	}
	root := absRepoRoot(path)
	snap := mi.repoLoads.load()
	i, ok := snap.find(root)
	if !ok || !snap.records[i].State.Loading() {
		return
	}
	rec := snap.records[i]
	// The job may have found the repository registered under another spelling
	// of the same directory, which is a success the registration hook could
	// not attribute to this record.
	if prefix, registered := mi.registeredPrefixForRoot(root); registered {
		mi.markRepoRegistered(prefix, root)
		return
	}
	state, reason := RepoLoadFailed, repoLoadFailureReason(jobErr, rec.Prefix, mi.rootOfPrefix(rec.Prefix))
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		state, reason = RepoLoadMissing, "the configured path does not exist"
	}
	mi.setRepoLoad(root, func(rec *RepoLoad) bool {
		if !rec.State.Loading() {
			return false
		}
		rec.State, rec.Reason, rec.Since = state, reason, time.Now()
		return true
	})
}

// repoLoadFailureReason states why a warmup job ended without registering.
func repoLoadFailureReason(jobErr error, prefix, prefixOwner string) string {
	switch {
	case jobErr != nil:
		return jobErr.Error()
	case prefixOwner != "":
		return "prefix_taken: the prefix " + prefix + " is already registered to " + prefixOwner
	default:
		return "warmup finished without registering this repository"
	}
}

// registeredPrefixForRoot finds a registered repository at root. It takes
// mu, so it is for the warmup worker only, never the admission path.
func (mi *MultiIndexer) registeredPrefixForRoot(root string) (string, bool) {
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && pathkey.EqualPaths(meta.RootPath, root) {
			return prefix, true
		}
	}
	return "", false
}

// rootOfPrefix is the root registered under prefix, or "".
func (mi *MultiIndexer) rootOfPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	if meta := mi.GetMetadata(prefix); meta != nil {
		return meta.RootPath
	}
	return ""
}

func (mi *MultiIndexer) setRepoLoad(path string, mutate func(*RepoLoad) bool) {
	if mi == nil {
		return
	}
	root := absRepoRoot(path)
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		i, ok := newRepoLoadSnapshot(records).find(root)
		if !ok {
			return records, false
		}
		return records, mutate(&records[i])
	})
}

// OnRepoRegistered adds a listener for a repository's first registration in
// this process. Listeners run on their own goroutine, after the repository is
// already visible in the registry and recorded ready, so a listener that
// invalidates cached session scopes can never be overtaken by a resolution
// that still saw the repository as loading.
func (mi *MultiIndexer) OnRepoRegistered(fn func(RepoLoad)) {
	if mi == nil || fn == nil {
		return
	}
	mi.repoLoads.mu.Lock()
	mi.repoLoads.listeners = append(mi.repoLoads.listeners, fn)
	mi.repoLoads.mu.Unlock()
}

// markRepoRegistered records that prefix became visible in the registry at
// root and announces it. Called from the two places a repository enters the
// registry for the first time (cold track and warm reconcile), never from
// the rewrites a re-index or a file save makes to an existing entry: those
// would clear every session's scope and announce "ready" on each edit.
func (mi *MultiIndexer) markRepoRegistered(prefix, root string) {
	if mi == nil || root == "" {
		return
	}
	now := time.Now()
	registered := newRepoLoad(root, prefix, RepoLoadReady, now)
	var announced RepoLoad
	var listeners []func(RepoLoad)
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		listeners = slices.Clone(mi.repoLoads.listeners)
		if i, ok := newRepoLoadSnapshot(records).find(root); ok {
			records[i].State, records[i].Reason, records[i].Since = RepoLoadReady, "", now
			if registered.canonicalKey != "" {
				records[i].canonicalKey = registered.canonicalKey
			}
			records[i].commonDir = registered.commonDir
			records[i].identityPending = registered.identityPending
			records[i].identityToken = registered.identityToken
			if registered.identityPending {
				records[i].Reason = registered.Reason
			}
			if prefix != "" {
				records[i].Prefix = prefix
			}
			announced = records[i]
			return records, true
		}
		announced = registered
		return append(records, announced), true
	})
	if len(listeners) == 0 {
		return
	}
	go func() {
		for _, fn := range listeners {
			fn(announced)
		}
	}()
}

// forgetRepoLoad drops root's record once the repository leaves the registry
// through an untrack, so it reads neither as ready nor as loading.
func (mi *MultiIndexer) forgetRepoLoad(root string) {
	if mi == nil || root == "" {
		return
	}
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		i, ok := newRepoLoadSnapshot(records).find(root)
		if !ok {
			return records, false
		}
		return slices.Delete(records, i, i+1), true
	})
}

// RepoLoads returns every recorded repository, in the order they were
// announced. Lock-free.
func (mi *MultiIndexer) RepoLoads() []RepoLoad {
	if mi == nil {
		return nil
	}
	return slices.Clone(mi.repoLoads.load().records)
}

// ConfiguredRepoRoots includes declarations whose loading finished without
// registration. Family guidance must not forget those roots when the pending
// load table's worktree arm stops applying. This does not grant admission.
func (mi *MultiIndexer) ConfiguredRepoRoots() []string {
	if mi == nil || mi.configMgr == nil {
		return nil
	}
	return mi.configMgr.Global().ConfiguredRepoRoots()
}

// RepoLoadFor returns the record whose root is exactly path. Lock-free; reads
// no store.
func (mi *MultiIndexer) RepoLoadFor(path string) (RepoLoad, bool) {
	if mi == nil {
		return RepoLoad{}, false
	}
	snap := mi.repoLoads.load()
	i, ok := snap.find(absRepoRoot(path))
	if !ok {
		return RepoLoad{}, false
	}
	return snap.records[i], true
}

// PendingRepoForPath returns the configured, not-yet-registered repository
// that path belongs to: path is inside its root, or is a root above it, or is
// inside a linked worktree of a repository that is still loading. A path that
// belongs to no such repository — every configured repository it touches is
// ready, or none is configured — reports false.
//
// It reads only the lock-free snapshot and, for the worktree arm, the
// filesystem: never mu and never the store. Callers on the MCP admission path
// consult it only after the registry has failed to place path.
func (mi *MultiIndexer) PendingRepoForPath(path string) (RepoLoad, bool) {
	if mi == nil || path == "" {
		return RepoLoad{}, false
	}
	return mi.repoLoads.load().matchForPath(path, false)
}

// RepoLoadAdmissionForPath classifies a final negative from one immutable
// snapshot. A matched Ready record is identity proof only: the caller must
// recheck ordinary graph/scope authority or retry, never grant access from it.
// An anonymous identity-pending record names no configured repository.
func (mi *MultiIndexer) RepoLoadAdmissionForPath(path string) (RepoLoad, bool) {
	if mi == nil || path == "" {
		return RepoLoad{}, false
	}
	return mi.repoLoads.load().admissionForPath(path)
}

func (s *repoLoadSnapshot) admissionForPath(path string) (RepoLoad, bool) {
	if rec, matched := s.matchForPath(path, true); matched {
		return rec, true
	}
	if s.identityPending {
		return RepoLoad{State: RepoLoadIdentityPending}, true
	}
	return RepoLoad{}, false
}

func (snap *repoLoadSnapshot) matchForPath(path string, includeReady bool) (RepoLoad, bool) {
	if len(snap.records) == 0 {
		return RepoLoad{}, false
	}
	abs := absRepoRoot(path)
	if rec, ok := snap.enclosing(abs); ok {
		return rec, includeReady || !rec.State.Ready()
	}
	// Until the owner resolves aliases, an unmatched request cannot prove
	// absence. Do not walk its filesystem merely to manufacture a negative.
	if snap.identityPending {
		if rec, ok := snap.contained(abs, includeReady); ok {
			return rec, true
		}
		return RepoLoad{}, false
	}
	if canonical := pathkey.CanonicalPath(abs); canonical != abs {
		if rec, ok := snap.enclosing(canonical); ok {
			return rec, includeReady || !rec.State.Ready()
		}
	}
	if rec, ok := snap.contained(abs, includeReady); ok {
		return rec, true
	}
	if snap.loading == 0 && !includeReady {
		return RepoLoad{}, false
	}
	commonDir := enclosingWorktreeCommonDir(abs)
	if commonDir == "" {
		return RepoLoad{}, false
	}
	var ready RepoLoad
	hasReady := false
	for _, rec := range snap.records {
		if rec.commonDir == "" || !pathkey.EqualPaths(rec.commonDir, commonDir) {
			continue
		}
		if rec.State.Loading() {
			return rec, true
		}
		if includeReady && rec.State.Ready() && !hasReady {
			ready, hasReady = rec, true
		}
	}
	return ready, hasReady
}

// enclosing finds the nearest recorded root at or above path.
func (s *repoLoadSnapshot) enclosing(path string) (RepoLoad, bool) {
	for dir := filepath.Clean(path); ; {
		if i, ok := s.index[pathkey.IdentityKey(dir)]; ok {
			return s.records[i], true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return RepoLoad{}, false
		}
		dir = parent
	}
}

// contained finds a recorded root below path — the shape of an
// agent opened at the directory above its repositories. A loading root wins
// over an unavailable or ready one, because retrying is useful while any
// repository under path can still become ready. The ownership-only public
// lookup excludes ready records; final admission includes them for rechecking.
func (s *repoLoadSnapshot) contained(path string, includeReady bool) (RepoLoad, bool) {
	var found RepoLoad
	ok := false
	for _, rec := range s.records {
		if (!includeReady && rec.State.Ready()) || pathkey.EqualPaths(rec.Root, path) || !pathkey.HasPathPrefix(rec.Root, path) {
			continue
		}
		if rec.State.Loading() {
			return rec, true
		}
		if !ok || (found.State.Ready() && !rec.State.Ready()) {
			found, ok = rec, true
		}
	}
	return found, ok
}

// enclosingWorktreeCommonDir returns the shared git directory of the linked
// worktree path sits in, or "" when it sits in none. The nearest `.git` above
// path decides: a main checkout or a submodule is not a linked worktree.
func enclosingWorktreeCommonDir(path string) string {
	for dir := filepath.Clean(path); ; {
		_, err := os.Lstat(filepath.Join(dir, ".git"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err == nil {
			info, resolveErr := resolveWorktreeChecked(dir)
			if resolveErr != nil {
				return ""
			}
			if info.GitCommonDir == "" {
				return ""
			}
			// Git writes a worktree's gitdir with symlinks resolved while a
			// configured root keeps the spelling it was tracked with, so both
			// sides of the comparison are canonical.
			return pathkey.CanonicalExistingRoot(info.GitCommonDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
