package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// advanceFixture is a tracked dedicated repository with a published committed
// base and a Git watcher over its working copy.
//
// It is assembled in the order a daemon assembles it: the publication runtime
// is installed BEFORE any owner is registered (SetDedicatedBaseCleanupRuntime
// refuses a late installation), the repository is tracked, the initial base is
// published, and only then is the watcher built over the same Indexer the
// MultiWatcher would hand it.
type advanceFixture struct {
	*lifecycleFixture
	root      string
	prefix    string
	graphID   string
	publisher *InitialBasePublisher
	trigger   *DedicatedBaseAdvanceTrigger
	watcher   *GitWatcher
	baseGen   int64
}

func newAdvanceFixture(t *testing.T, name string) *advanceFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	installStartupPublisherRuntime(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := f.gitRepo(name)
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	require.NotEmpty(t, registered.Prefix)

	publisher := startupPublisher(t, f)
	initial := publisher.PublishRepo(ctx, registered.Prefix)
	require.NoError(t, initial.Err)
	require.Empty(t, initial.Skipped)
	require.Positive(t, initial.GenerationID)

	out := &advanceFixture{
		lifecycleFixture: f,
		root:             root,
		prefix:           registered.Prefix,
		graphID:          GraphIDFor(registered.Prefix),
		publisher:        publisher,
		trigger:          publisher.AdvanceTrigger(),
		baseGen:          initial.GenerationID,
	}
	require.NotNil(t, out.trigger)
	out.watcher = out.newWatcher(t)
	return out
}

// newWatcher builds the Git watcher exactly as MultiWatcher does — over the
// checkout root and the registered Indexer — and seeds lastSHA with the commit
// the base was published at, which is what a watcher that came up on a warmed
// repository has.
func (f *advanceFixture) newWatcher(t *testing.T) *GitWatcher {
	t.Helper()
	idx := f.mi.GetIndexer(f.prefix)
	require.NotNil(t, idx, "the tracked repository has no registered indexer")
	gw, err := NewGitWatcher(f.root, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Stop() })
	gw.mu.Lock()
	gw.lastSHA = gitHead(t, f.root)
	gw.mu.Unlock()
	return gw
}

// commit makes one commit and returns its SHA.
func (f *advanceFixture) commit(t *testing.T, file, body, message string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.root, file), body)
	runGit(t, f.root, "add", ".")
	runGit(t, f.root, "commit", "-q", "-m", message)
	return gitHead(t, f.root)
}

// reconcileAndWait drives the production HEAD-change path and joins whatever
// publication it queued.
func (f *advanceFixture) reconcileAndWait(t *testing.T) {
	t.Helper()
	f.watcher.reconcile("test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, f.publisher.Wait(ctx))
}

// dispatchAndWait drives ONE observed HEAD movement through the trigger's
// production entry point and joins the publication it queued, returning the
// advance the trigger recorded.
//
// It replaces the trigger's old exported AdvanceRepo, which published
// synchronously outside the shared queue and outside the memo pre-check. Going
// through HeadChanged means a test asserts on the path the Git watcher actually
// takes.
//
// The join is the FIFO barrier, not InitialBasePublisher.Wait alone. Wait is
// the publisher's own accounting — it returns when every queued publication has
// been ATTEMPTED — and the trigger records the advance in a completion callback
// the worker runs afterwards, so a test that read Advances() straight after
// Wait was racing the callback it was there to observe. The barrier is a
// request enqueued behind this one whose own callback cannot run until every
// earlier request's has returned; see settlePublisher.
func (f *advanceFixture) dispatchAndWait(t *testing.T, root, commitOID string) DedicatedBaseAdvance {
	t.Helper()
	before := len(f.trigger.Advances())
	f.trigger.HeadChanged(f.prefix, root, commitOID)
	f.settlePublisher(t)
	advances := f.trigger.Advances()
	require.Greater(t, len(advances), before,
		"the production dispatch recorded no advance for %s", shortCommit(commitOID))
	return advances[len(advances)-1]
}

// generations lists this fixture's committed generations, oldest first.
func (f *advanceFixture) generations(t *testing.T) []store_sqlite.ViewGeneration {
	t.Helper()
	return dedicatedGenerations(t, f.lifecycleFixture, f.graphID)
}

func (f *advanceFixture) activeGeneration(t *testing.T) int64 {
	t.Helper()
	return f.familyOf(f.prefix).ActiveGenerationID
}

// gitTree resolves a commit's tree the way the trigger does.
func gitTree(t *testing.T, root, commitOID string) string {
	t.Helper()
	tree, err := dedicatedBaseCommitTree(context.Background(), root, commitOID)
	require.NoError(t, err)
	return tree
}

// TestGitWatcherHeadChangeAdvancesTheCommittedBase is the acceptance case for
// this item: a HEAD movement observed by the running daemon publishes exactly
// one new committed generation, adopted, describing the tree HEAD now names.
//
// It is driven through GitWatcher.reconcile — the production entry point the
// fsnotify ref event reaches — and not through the trigger, so it fails if the
// dispatch is removed from finalizeReconcile even though the trigger itself
// still works.
func TestGitWatcherHeadChangeAdvancesTheCommittedBase(t *testing.T) {
	f := newAdvanceFixture(t, "advance")
	before := f.generations(t)
	require.Len(t, before, 1, "the fixture starts from one published base")

	newSHA := f.commit(t, "advanced.go", "package a\n\nfunc Advanced() {}\n", "advance the head")
	newTree := gitTree(t, f.root, newSHA)
	require.NotEqual(t, before[0].TreeOID, newTree, "the commit did not move the tree")

	f.reconcileAndWait(t)

	after := f.generations(t)
	require.Len(t, after, 2, "a HEAD change publishes exactly one committed generation")
	var published store_sqlite.ViewGeneration
	for _, row := range after {
		if row.GenerationID != before[0].GenerationID {
			published = row
		}
	}
	require.Positive(t, published.GenerationID)
	require.Equal(t, published.GenerationID, f.activeGeneration(t),
		"the new committed generation was not adopted")
	require.Equal(t, store_sqlite.ViewGenerationReady, published.State)
	require.Equal(t, newTree, published.TreeOID, "the base does not describe the tree HEAD names")
	require.Equal(t, newSHA, published.ProvenanceCommitOID)
	require.Equal(t, before[0].GenerationID, published.BaseGenerationID,
		"an advance whose frozen inputs are unchanged extends the chain: it is a delta over the "+
			"previous base, not a second full root")
	require.NotEmpty(t, published.LayerID)
	require.NotEmpty(t, published.LowerViewFingerprint)
	require.NotEmpty(t, published.DependencyRevision)
	require.False(t, strings.HasPrefix(published.DependencyRevision, DependencyRevisionDegradedPrefix),
		"a tracked, indexed repository describes a real cohort: %s", published.DependencyRevision)

	// The replaced base is retained, never retired: an old coherent route stays
	// available until its readers are gone.
	require.Contains(t,
		[]store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded},
		rowByID(t, after, before[0].GenerationID).State)

	// Publication is not activation: the owning repository's own request route
	// is untouched (the declared W4.5 limitation).
	_, routed, err := f.catalog.GetCheckoutRoute(context.Background(), f.checkoutOf(f.prefix).CheckoutID)
	require.NoError(t, err)
	require.False(t, routed, "the dedicated owner must not be routed by publication")
}

// newestGeneration returns the row a fresh advance just allocated.
func newestGeneration(t *testing.T, rows []store_sqlite.ViewGeneration) store_sqlite.ViewGeneration {
	t.Helper()
	require.NotEmpty(t, rows)
	newest := rows[0]
	for _, row := range rows[1:] {
		if row.GenerationID > newest.GenerationID {
			newest = row
		}
	}
	return newest
}

// TestSuccessiveCommitsExtendTheChainRatherThanReRooting is the whole point of
// the incremental committed base, and it was broken by the cohort's own scope
// rule until this item.
//
// A repository's OWN corpus tree was an input to its OWN dependency revision
// (dependencyCohortSource.sourceIdentities named every in-scope roster member,
// target included, and graphBase resolves the target's tree from its active
// generation). So the target's dependency revision moved on essentially every
// commit, and a moved revision ROOTS a new chain rather than extending one
// (dedicatedBaseParentForAdvance). The measured shape was: commit 1 a delta,
// commit 2 a FULL committed-tree index, commit 3 a FULL committed-tree index —
// a full re-index of the whole repository on nearly every commit, on a branch
// whose measured target is write amplification.
//
// Three successive commits, driven through the Git watcher's production
// dispatch, must produce one root and then two deltas, each naming the previous
// generation as its base, all four under ONE unchanged dependency revision.
func TestSuccessiveCommitsExtendTheChainRatherThanReRooting(t *testing.T) {
	f := newAdvanceFixture(t, "chain")
	rows := f.generations(t)
	require.Len(t, rows, 1, "the fixture starts from one published base")
	root := rows[0]
	require.Zero(t, root.BaseGenerationID, "the first committed generation is a full root")
	require.True(t, strings.HasPrefix(root.DependencyRevision, DependencyRevisionEncodingVersion+":"),
		"the fixture's base carries no certified cohort: %q", root.DependencyRevision)

	parent := root
	for i, name := range []string{"chain-one.go", "chain-two.go", "chain-three.go"} {
		sha := f.commit(t, name,
			"package a\n\nfunc Chain"+string(rune('A'+i))+"() {}\n", "chain "+name)
		f.reconcileAndWait(t)

		rows = f.generations(t)
		require.Lenf(t, rows, i+2, "commit %d published the wrong number of generations", i+1)
		published := newestGeneration(t, rows)
		require.Equalf(t, published.GenerationID, f.activeGeneration(t),
			"commit %d did not adopt what it published", i+1)
		require.Equalf(t, gitTree(t, f.root, sha), published.TreeOID,
			"commit %d published the wrong tree", i+1)
		require.Equalf(t, parent.DependencyRevision, published.DependencyRevision,
			"commit %d moved the repository's OWN dependency revision; its own corpus tree is an "+
				"input to its own cohort again, so every advance re-roots", i+1)
		require.Equalf(t, parent.GenerationID, published.BaseGenerationID,
			"commit %d re-rooted (base=%d) instead of extending the chain over generation %d; "+
				"that is a full committed-tree index per commit",
			i+1, published.BaseGenerationID, parent.GenerationID)
		require.NotEmptyf(t, published.LayerID, "commit %d published a root, not a delta", i+1)
		require.NotEmptyf(t, published.LowerViewFingerprint,
			"commit %d published a delta with no lower", i+1)
		parent = published
	}
}

// TestARealCohortChangeStillRootsANewChain is the other direction of the same
// rule, and the reason the fix is "exclude SELF" rather than "stop digesting
// source identities".
//
// A workspace sibling's committed tree IS a cross-repository input: a build of
// this repository may resolve against it. When one joins the target's workspace
// the cohort genuinely moved, so the next advance must NOT be spliced under a
// base that was built against the older cohort — it roots a new chain.
func TestARealCohortChangeStillRootsANewChain(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	root := f.gitRepo("cohort-target")
	target, err := f.lc.Register(ctx, config.RepoEntry{
		Path: root, Name: "cohort-target", Workspace: cohortTestWorkspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, target.CatalogErr)

	publisher := startupPublisher(t, f)
	initial := publisher.PublishRepo(ctx, target.Prefix)
	require.NoError(t, initial.Err)
	require.Empty(t, initial.Skipped)
	require.Positive(t, initial.GenerationID)

	graphID := GraphIDFor(target.Prefix)
	idx := f.mi.GetIndexer(target.Prefix)
	require.NotNil(t, idx)
	require.Equal(t, cohortTestWorkspace, idx.WorkspaceID(),
		"the target is not in a workspace, so no sibling can be an input to it")
	gw, err := NewGitWatcher(root, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Stop() })
	gw.mu.Lock()
	gw.lastSHA = gitHead(t, root)
	gw.mu.Unlock()

	advance := func(t *testing.T, file, body, message string) store_sqlite.ViewGeneration {
		t.Helper()
		writeFile(t, filepath.Join(root, file), body)
		runGit(t, root, "add", ".")
		runGit(t, root, "commit", "-q", "-m", message)
		gw.reconcile("test")
		require.NoError(t, publisher.Wait(ctx))
		return newestGeneration(t, dedicatedGenerations(t, f, graphID))
	}

	// Baseline: with the cohort unchanged, an advance extends.
	base := newestGeneration(t, dedicatedGenerations(t, f, graphID))
	extended := advance(t, "before.go", "package a\n\nfunc Before() {}\n", "before the sibling")
	require.Equal(t, base.GenerationID, extended.BaseGenerationID,
		"an advance under an unchanged cohort did not extend the chain")
	require.Equal(t, base.DependencyRevision, extended.DependencyRevision)

	// A second repository is tracked into the SAME workspace and indexed. That
	// is a real change to what this repository's resolution can see.
	siblingRoot := f.gitRepo("cohort-input")
	sibling, err := f.lc.Register(ctx, config.RepoEntry{
		Path: siblingRoot, Name: "cohort-input", Workspace: cohortTestWorkspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, sibling.CatalogErr)

	rooted := advance(t, "after.go", "package a\n\nfunc After() {}\n", "after the sibling")
	require.NotEqual(t, extended.DependencyRevision, rooted.DependencyRevision,
		"a repository joined the target's workspace and its cohort did not move; "+
			"excluding SELF from the cohort must not exclude siblings")
	require.True(t, strings.HasPrefix(rooted.DependencyRevision, DependencyRevisionEncodingVersion+":"),
		"the advance after the sibling joined fell back to a degraded revision: %q",
		rooted.DependencyRevision)
	require.Zero(t, rooted.BaseGenerationID,
		"an advance under a CHANGED cohort was spliced under a base built for the old one")
	require.Empty(t, rooted.LayerID, "a re-root published a sparse layer")
}

func rowByID(t *testing.T, rows []store_sqlite.ViewGeneration, id int64) store_sqlite.ViewGeneration {
	t.Helper()
	for _, row := range rows {
		if row.GenerationID == id {
			return row
		}
	}
	t.Fatalf("generation %d is gone", id)
	return store_sqlite.ViewGeneration{}
}

// TestGitWatcherSameTreeCommitPublishesNothing is gate 2's no-op clause on the
// live path: a commit whose TREE is identical to the active base's — an amend
// with no content change, a message-only rewrite — must perform no catalog DML
// and run no build, however many times it happens.
//
// The commit SHA moves, so the watcher does reconcile and does dispatch; what
// must not move is the corpus. The write audit is installed around the
// publication alone because the generation-0 freshness restamp that precedes it
// is a legitimate write.
func TestGitWatcherSameTreeCommitPublishesNothing(t *testing.T) {
	f := newAdvanceFixture(t, "sametree")
	base := f.generations(t)
	require.Len(t, base, 1)

	// An amend with no content change: a new commit object over the same tree.
	runGit(t, f.root, "commit", "-q", "--amend", "-m", "amended once")
	amended := gitHead(t, f.root)
	require.Equal(t, base[0].TreeOID, gitTree(t, f.root, amended),
		"the amend was supposed to leave the tree alone")

	// The production entry point first, unaudited: the watcher's reconcile
	// restamps generation-0 freshness on its way to the dispatch, which is a
	// legitimate write, so the audit cannot span it.
	f.reconcileAndWait(t)
	require.Len(t, f.generations(t), 1,
		"the watcher's own dispatch allocated a generation for a same-tree commit")
	require.Equal(t, base[0].GenerationID, f.activeGeneration(t))

	// The same fact with the catalog instrumented. A SECOND same-tree amend,
	// because the trigger memoises the commit that just landed and would drop a
	// repeat of it before reaching the publisher at all.
	runGit(t, f.root, "commit", "-q", "--amend", "-m", "amended twice")
	again := gitHead(t, f.root)
	require.NotEqual(t, amended, again, "the second amend produced no new commit object")
	require.Equal(t, base[0].TreeOID, gitTree(t, f.root, again),
		"the second amend was supposed to leave the tree alone")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	audit, err := installDedicatedWriteAudit(ctx, f.dbPath)
	require.NoError(t, err)

	advance := f.dispatchAndWait(t, f.root, again)

	require.NoError(t, audit(), "a same-tree commit wrote the catalog")
	require.NoError(t, advance.Err)
	require.Empty(t, advance.Skipped)
	require.False(t, advance.Published, "a same-tree commit published a new generation")
	require.True(t, advance.Coalesced, "a same-tree commit ran a physical build")
	require.Equal(t, base[0].GenerationID, advance.GenerationID)
	require.Len(t, f.generations(t), 1, "a same-tree commit allocated a generation")
	require.Equal(t, base[0].GenerationID, f.activeGeneration(t))
}

// TestDedicatedBaseAdvanceCoalescesToTheNewestTarget pins the burst behaviour:
// ten commits landing while one publication is in flight cost one follow-up
// publication of the TENTH tree, not ten publications of ten trees.
//
// The worker is neutralised rather than raced: consuming the publisher's
// worker Once leaves the queue observable, which is the only way to assert
// "these all collapsed into one slot" without depending on scheduler timing.
func TestDedicatedBaseAdvanceCoalescesToTheNewestTarget(t *testing.T) {
	f := newAdvanceFixture(t, "burst")
	// Nothing may drain while the burst is queued.
	f.publisher.worker.Do(func() {})

	commits := make([]string, 0, 3)
	for i, name := range []string{"one.go", "two.go", "three.go"} {
		commits = append(commits, f.commit(t, name,
			"package a\n\nfunc Burst"+string(rune('A'+i))+"() {}\n", "burst "+name))
	}
	for _, sha := range commits {
		f.trigger.HeadChanged(f.prefix, f.root, sha)
	}

	f.publisher.mu.Lock()
	order := append([]string(nil), f.publisher.pendingOrder...)
	queued := f.publisher.queued
	request := f.publisher.pendingReq[f.prefix]
	f.publisher.mu.Unlock()

	require.Equal(t, []string{f.prefix}, order, "a burst queued more than one slot")
	require.Equal(t, 1, queued, "a burst counted more than one queued publication")
	require.Equal(t, commits[len(commits)-1], request.target.CommitOID,
		"the queued request does not name the newest observed commit")
	require.True(t, request.live)

	// Draining that one slot publishes the newest tree, and exactly one
	// generation for the whole burst.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f.publisher.mu.Lock()
	popped, ok := f.publisher.popLocked()
	f.publisher.mu.Unlock()
	require.True(t, ok)
	outcome := f.publisher.publish(ctx, popped)
	require.NoError(t, outcome.Err)
	require.Equal(t, gitTree(t, f.root, commits[len(commits)-1]), outcome.TreeOID)
	require.Len(t, f.generations(t), 2, "a coalesced burst published more than one generation")
	require.Equal(t, outcome.GenerationID, f.activeGeneration(t))
}

// TestDedicatedBaseAdvanceIgnoresARepeatedObservation pins the memo: the same
// commit observed twice (a watcher restart, a poller and a watcher both seeing
// the same transition) costs nothing the second time. A fresh observation is
// not free — it takes a daemon-wide roster lease and reads one catalog row per
// cohort member — so a repeat must not pay for it.
func TestDedicatedBaseAdvanceIgnoresARepeatedObservation(t *testing.T) {
	f := newAdvanceFixture(t, "repeat")
	sha := f.commit(t, "repeat.go", "package a\n\nfunc Repeat() {}\n", "repeat")

	first := f.dispatchAndWait(t, f.root, sha)
	require.NoError(t, first.Err)
	require.True(t, first.Published)

	// queued only ever grows, so comparing it across the repeat is exact even
	// with the publisher's worker still running: an accepted dispatch has
	// already incremented it by the time HeadChanged returns.
	f.publisher.mu.Lock()
	before := f.publisher.queued
	f.publisher.mu.Unlock()

	f.trigger.HeadChanged(f.prefix, f.root, sha)

	f.publisher.mu.Lock()
	queued := f.publisher.queued
	f.publisher.mu.Unlock()
	require.Equal(t, before, queued, "a repeat of the commit that already landed was queued again")
	require.Len(t, f.generations(t), 2, "a repeat observation published a second generation")
}

// TestDedicatedBaseAdvanceRefusesAnObservationFromAnotherWorkingCopy is the
// ownership fence on the live path.
//
// A watcher observes ONE working copy. A linked worktree tracked as its own
// repository has its own HEAD, and its commits are not the dedicated owner's:
// publishing the owner's base at a sibling's commit would stamp the owner with
// a tree it never had.
func TestDedicatedBaseAdvanceRefusesAnObservationFromAnotherWorkingCopy(t *testing.T) {
	f := newAdvanceFixture(t, "owner")
	sibling := f.worktreeOf(f.root, "owner-sibling")
	runGit(t, sibling, "add", ".")
	runGit(t, sibling, "commit", "-q", "-m", "sibling commit")
	siblingSHA := gitHead(t, sibling)

	advance := f.dispatchAndWait(t, sibling, siblingSHA)
	require.NoError(t, advance.Err)
	require.Equal(t, "observed root is not the dedicated owner", advance.Skipped)
	require.Len(t, f.generations(t), 1, "a sibling's commit advanced the owner's base")
	require.Equal(t, f.baseGen, f.activeGeneration(t))
}

// TestGitWatcherHeadChangeInvalidatesTheWorkspaceCohort closes the one cohort
// source W2.4b's wiring deliberately left open: a workspace member's committed
// tree moving is not a lifecycle event, so nothing but the ref-transition
// observer can tell a cached cohort it has stopped being current.
//
// Without it a certified dependency revision names a sibling tree OID that has
// since moved, and every consumer keyed on it keeps answering from a
// certificate that stopped being true.
func TestGitWatcherHeadChangeInvalidatesTheWorkspaceCohort(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The member whose HEAD will move, and a sibling in the same workspace
	// whose coordinator caches a cohort that names the member's bytes.
	memberRoot := f.gitRepo("cohort-member")
	member, err := f.lc.Register(ctx, config.RepoEntry{
		Path: memberRoot, Name: "cohort-member", Workspace: cohortTestWorkspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, member.CatalogErr)

	siblingRoot := f.gitRepo("cohort-observer")
	f.worktreeOf(siblingRoot, "cohort-observer-wt")
	sibling, err := f.lc.Register(ctx, config.RepoEntry{
		Path: siblingRoot, Name: "cohort-observer", Workspace: cohortTestWorkspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, sibling.CatalogErr)
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)

	checkouts, err := f.catalog.ListCheckouts(ctx, sibling.FamilyID)
	require.NoError(t, err)
	var automatic store_sqlite.Checkout
	for i := range checkouts {
		if checkouts[i].CheckoutID != sibling.CheckoutID {
			automatic = checkouts[i]
		}
	}
	require.NotEmpty(t, automatic.CheckoutID, "the linked worktree got no identity")
	f.activateAndWait(automatic.CheckoutID)
	coordinator := settledCoordinator(t, f.lc, automatic.CheckoutID)
	require.Equal(t, cohortTestWorkspace, coordinator.workspaceID)

	publisher := startupPublisher(t, f)
	idx := f.mi.GetIndexer(member.Prefix)
	require.NotNil(t, idx)
	gw, err := NewGitWatcher(memberRoot, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Stop() })
	gw.mu.Lock()
	gw.lastSHA = gitHead(t, memberRoot)
	gw.mu.Unlock()

	writeFile(t, filepath.Join(memberRoot, "moved.go"), "package a\n\nfunc Moved() {}\n")
	runGit(t, memberRoot, "add", ".")
	runGit(t, memberRoot, "commit", "-q", "-m", "the member's tree moved")

	gw.reconcile("test")
	require.NoError(t, publisher.Wait(ctx))

	require.True(t, cohortStale(coordinator),
		"a workspace member's committed tree moved and the sibling coordinator's cached cohort "+
			"stayed settled; its next cycle keys layers on a revision that names a tree that is gone")
}

// TestPollerHeadFinalizeAllocatesNoCommittedGeneration is gate 2's other half:
// selection and polling must never allocate a payload generation.
//
// The filesystem poller has its own HEAD observation (poller.go's
// observeGitHead/finalizeGitHead) — the degraded fallback for a repository
// fsnotify cannot watch. It restamps generation-0 freshness and nothing else,
// and it must stay that way: a timer-driven path that allocates is a
// generation per tick.
func TestPollerHeadFinalizeAllocatesNoCommittedGeneration(t *testing.T) {
	f := newAdvanceFixture(t, "poll")
	idx := f.mi.GetIndexer(f.prefix)
	require.NotNil(t, idx)
	poller := newPoller(nil, idx, zap.NewNop())
	poller.mu.Lock()
	poller.lastSHA = gitHead(t, f.root)
	poller.mu.Unlock()

	// An empty commit is a HEAD movement with no changed path, so the poller's
	// Git path reaches its freshness finalization without a watcher batch.
	runGit(t, f.root, "commit", "-q", "--allow-empty", "-m", "empty")
	require.NotEqual(t, poller.lastSHA, gitHead(t, f.root))

	poller.pollGitHead()

	require.Len(t, f.generations(t), 1, "a poll tick allocated a committed generation")
	require.Equal(t, f.baseGen, f.activeGeneration(t), "a poll tick moved the active base")
}

// TestWorkingCopyReindexAllocatesNoCommittedGeneration is requirement (5): the
// non-Git watcher path writes generation 0 and nothing else.
//
// Both of its entry points funnel into IncrementalReindexPaths — watcher.go's
// overflow reconcile and incremental_watcher_batch.go's storm batch — and both
// describe UNCOMMITTED working-copy edits, which have no committed tree to
// publish. A committed base that moved because somebody saved a buffer would be
// a committed identity reading the live worktree.
func TestWorkingCopyReindexAllocatesNoCommittedGeneration(t *testing.T) {
	f := newAdvanceFixture(t, "dirty")
	dirty := filepath.Join(f.root, "dirty.go")
	writeFile(t, dirty, "package a\n\nfunc Dirty() {}\n")

	_, err := f.mi.GetIndexer(f.prefix).IncrementalReindexPaths(f.root, []string{dirty})
	require.NoError(t, err)

	require.Len(t, f.generations(t), 1, "a working-copy reindex allocated a committed generation")
	require.Equal(t, f.baseGen, f.activeGeneration(t), "a working-copy reindex moved the active base")
	require.Equal(t, f.generations(t)[0].TreeOID, f.checkoutOf(f.prefix).HeadTree,
		"the committed base stopped describing a committed tree")
}

// TestOnlyTheGitWatcherHeadFinalizeDispatchesAdvancement is the standing guard
// on the trigger's blast radius.
//
// Every other candidate source is a timer: the 15 s checkout-coordinator poll,
// the hourly janitor, the filesystem poller, the storm batch. Wiring any of
// them would allocate a payload generation for the passage of time, which is
// exactly what gate 2 forbids — and it is a one-line mistake to make. This test
// fails the moment a second production dispatch appears.
func TestOnlyTheGitWatcherHeadFinalizeDispatchesAdvancement(t *testing.T) {
	census := func(t *testing.T, patterns ...string) map[string]int {
		t.Helper()
		entries, err := os.ReadDir(".")
		require.NoError(t, err)
		callers := map[string]int{}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			body, err := os.ReadFile(name)
			require.NoError(t, err)
			for _, line := range strings.Split(string(body), "\n") {
				code := strings.TrimSpace(line)
				if strings.HasPrefix(code, "//") {
					continue
				}
				for _, pattern := range patterns {
					if strings.Contains(code, pattern) {
						callers[name]++
						break
					}
				}
			}
		}
		return callers
	}

	require.Equal(t, map[string]int{
		// the dispatch itself
		"git_watcher.go": 1,
		// HeadChanged's own enqueue
		"dedicated_base_advance_trigger.go": 1,
	}, census(t, ".HeadChanged(", ".enqueueAdvance("),
		"committed advancement gained a production dispatch site outside the "+
			"git watcher's HEAD-change finalize path")

	// The queue-bypass census. A dispatch site is only half the blast radius:
	// the trigger used to carry an exported AdvanceRepo that published
	// SYNCHRONOUSLY, outside the shared pending list and outside the memo's
	// pre-check, and the pattern set above was blind to it (W5/W4.3-verify,
	// minor 3). Every call of the publisher's own publish must therefore be one
	// of the two the design admits: the worker draining the queue, and
	// PublishRepo, the deliberate synchronous entry point for a caller that
	// owns its own waiting.
	//
	// The pattern is the bare selector, not "p.publish(". The bypass this
	// census exists for was spelled t.publisher.publish(, and "p.publish(" is
	// not a substring of it — the byte before .publish( is an r — so the guard
	// matched only a bypass that happened to be written on a receiver named p.
	// Matching the call rather than the receiver's spelling is what makes this
	// a census of shape.
	require.Equal(t, map[string]int{"dedicated_base_startup.go": 2},
		census(t, ".publish("),
		"a committed-base publication path appeared outside InitialBasePublisher.run "+
			"and InitialBasePublisher.PublishRepo; a synchronous bypass skips the single "+
			"pending list that keeps one committed build running at a time")
}

// TestFiveCommitsAllocateFiveGenerationsAndTimersAllocateNone is the plan's
// named W4.3 verification, at the size the test budget affords: N commits
// allocate exactly N committed generations, and the two timer-driven paths that
// could plausibly allocate one — the checkout coordinator's 15 s poll and the
// hourly reconcile janitor — allocate none.
//
// It is the budget claim W8 will freeze, stated as a count rather than inferred
// from a single-commit test: one root plus five deltas for five commits, no
// generation per tick, and nothing re-rooted in between.
func TestFiveCommitsAllocateFiveGenerationsAndTimersAllocateNone(t *testing.T) {
	f := newAdvanceFixture(t, "budget")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	require.Len(t, f.generations(t), 1, "the fixture starts from one published base")

	const commits = 5
	for i := 0; i < commits; i++ {
		name := "budget-" + string(rune('a'+i)) + ".go"
		f.commit(t, name, "package a\n\nfunc Budget"+string(rune('A'+i))+"() {}\n", "budget "+name)
		f.reconcileAndWait(t)
	}

	rows := f.generations(t)
	require.Len(t, rows, 1+commits,
		"%d commits did not allocate exactly %d committed generations", commits, commits)
	roots := 0
	for _, row := range rows {
		if row.BaseGenerationID == 0 {
			roots++
		}
	}
	require.Equal(t, 1, roots,
		"%d commits produced %d full committed-tree roots; the chain re-rooted instead of extending",
		commits, roots)
	settled := len(rows)
	active := f.activeGeneration(t)
	require.Equal(t, newestGeneration(t, rows).GenerationID, active)

	// The janitor tick: the checkout sweep plus the whole-workspace reconcile,
	// exactly what startReconcileJanitor runs every hour. Both do real work —
	// the reconcile re-reads every tracked repository — and neither is an
	// observed HEAD movement, so neither may allocate.
	for i := 0; i < 2; i++ {
		_, err := f.lc.Sweep(ctx)
		require.NoError(t, err)
		f.mi.ReconcileAll()
	}
	require.Len(t, f.generations(t), settled,
		"a janitor tick allocated a committed generation for the passage of time")
	require.Equal(t, active, f.activeGeneration(t), "a janitor tick moved the active base")

	// The checkout coordinator's poll. It is built after the commits so the
	// fan-out it reacts to is settled before the count is taken.
	f.worktreeOf(f.root, "budget-wt")
	_, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	automatic := f.automaticCheckoutID(f.familyOf(f.prefix).FamilyID, "budget-wt")
	f.activateAndWait(automatic)
	coordinator := settledCoordinator(t, f.lc, automatic)

	// One real cycle first, so the polls below have routed layers to settle on
	// and the no-allocation claim is not vacuous. It builds this checkout's own
	// layers — which are NOT committed generations of the dedicated graph.
	routed := coordinator.reconcile(ctx)
	require.NoError(t, routed.Err)
	require.NotZerof(t, routed.CommitGenerationID, "the coordinator routed no layers: %+v", routed)

	before := len(f.generations(t))
	for i := 0; i < 5; i++ {
		_, ok := coordinator.settledWithoutBuild(ctx)
		require.Truef(t, ok, "poll %d did not settle on the layers the coordinator had routed; "+
			"the no-allocation claim below would be vacuous", i)
	}
	// And a whole poll cycle, the body the 15 s timer runs.
	polled := coordinator.reconcile(ctx)
	require.NoError(t, polled.Err)
	require.Falsef(t, polled.CommitBuilt || polled.DirtyBuilt,
		"an idle poll rebuilt the checkout's layers: %+v", polled)

	require.Len(t, f.generations(t), before,
		"a checkout-coordinator poll allocated a committed generation")
	require.Equal(t, active, f.activeGeneration(t), "a coordinator poll moved the active base")
}

// TestCommittedAdvancementRequiresTheSharedLeaseDomain pins the trigger to the
// same invariant the publisher carries: a driver over a private
// graphview.LeaseManager advances generations the retirement sweep cannot see
// are in use. The refusal is at publisher construction, so the trigger must not
// come into existence at all when it is refused.
func TestCommittedAdvancementRequiresTheSharedLeaseDomain(t *testing.T) {
	t.Run("a private lease domain leaves no trigger", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		private, err := NewDedicatedBaseRuntime(f.store, graphview.NewLeaseManager())
		require.NoError(t, err)
		require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(private))

		publisher, err := NewInitialBasePublisher(f.lc)
		require.Error(t, err)
		require.Nil(t, publisher)
		require.Nil(t, dedicatedBaseAdvanceTriggerFor(f.mi),
			"a refused publisher still registered a live advancement trigger")
	})

	t.Run("the shared domain registers one", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		runtime := installStartupPublisherRuntime(t, f)
		publisher, err := NewInitialBasePublisher(f.lc)
		require.NoError(t, err)
		defer publisher.Close()
		trigger := dedicatedBaseAdvanceTriggerFor(f.mi)
		require.NotNil(t, trigger)
		require.Same(t, publisher.AdvanceTrigger(), trigger)
		require.Same(t, f.lc.ViewLeases(), runtime.ViewLeases(),
			"the trigger publishes through the lifecycle's own lease domain")
	})

	t.Run("closing the publisher unbinds the watchers", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		installStartupPublisherRuntime(t, f)
		publisher, err := NewInitialBasePublisher(f.lc)
		require.NoError(t, err)
		require.NotNil(t, dedicatedBaseAdvanceTriggerFor(f.mi))
		publisher.Close()
		require.Nil(t, dedicatedBaseAdvanceTriggerFor(f.mi),
			"a closed publisher left its trigger reachable by this process's watchers")
		_, leaked := dedicatedBaseAdvanceRegistry.Load(f.mi)
		require.False(t, leaked,
			"a closed publisher left its whole lifecycle reachable from the process-wide registry")
	})

	t.Run("closing publisher admission stops the trigger", func(t *testing.T) {
		// The daemon's shutdown does NOT call InitialBasePublisher.Close: the
		// first thing CheckoutLifecycle.Close does is close publisher
		// admission on the runtime, and the watchers keep running until
		// MultiWatcher.Stop joins them. A HEAD change inside that window must
		// find no trigger rather than one whose publications can never run.
		f := newLifecycleFixture(t)
		defer f.close()
		runtime := installStartupPublisherRuntime(t, f)
		publisher, err := NewInitialBasePublisher(f.lc)
		require.NoError(t, err)
		defer publisher.Close()
		trigger := publisher.AdvanceTrigger()
		require.NotNil(t, dedicatedBaseAdvanceTriggerFor(f.mi))

		<-runtime.CloseDedicatedBaseAdmission()

		require.Nil(t, dedicatedBaseAdvanceTriggerFor(f.mi),
			"publisher admission closed and a watcher could still reach the trigger")
		trigger.HeadChanged("any", "/nowhere", "0123456789abcdef")
		require.Zero(t, publisher.Pending(), "a stopped trigger queued an advance")

		// And the registry ENTRY is gone, not merely dead. This is the daemon's
		// real shutdown path — `state.shared.Close()` runs the stack cleanup
		// chain, whose CheckoutLifecycle.Close closes publisher admission — and
		// nothing on it calls InitialBasePublisher.Close. Before the trigger
		// bound its registration to the publisher's context, the process-wide
		// sync.Map retained trigger -> publisher -> lifecycle -> *MultiIndexer
		// for the life of the process (W5/W4.3-verify, minor 5).
		require.Eventually(t, func() bool {
			_, leaked := dedicatedBaseAdvanceRegistry.Load(f.mi)
			return !leaked
		}, 10*time.Second, 5*time.Millisecond,
			"closing publisher admission left the whole lifecycle reachable from the "+
				"process-wide advancement registry")
	})
}

// TestGitWatcherDispatchesThroughTheRegisteredIndexer proves the resolution
// path the dispatch depends on: a Git watcher holds no catalog, no lifecycle
// and no publication runtime, and reaches the trigger through the MultiIndexer
// its Indexer already carries. A watcher over a repository whose process has no
// publisher simply reconciles generation 0, as it did before this item.
func TestGitWatcherDispatchesThroughTheRegisteredIndexer(t *testing.T) {
	f := newAdvanceFixture(t, "resolve")
	idx := f.mi.GetIndexer(f.prefix)
	require.NotNil(t, idx)
	require.Same(t, f.mi, idx.repositoryMutationOwner,
		"the registered indexer does not carry the MultiIndexer the trigger is keyed on")
	require.Same(t, f.trigger, dedicatedBaseAdvanceTriggerFor(idx.repositoryMutationOwner))

	// No publisher: the same dispatch is a no-op rather than a failure.
	bare := newLifecycleFixture(t)
	defer bare.close()
	root := bare.gitRepo("unpublished")
	registered, err := bare.lc.Register(context.Background(), config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	bareIdx := bare.mi.GetIndexer(registered.Prefix)
	require.NotNil(t, bareIdx)
	require.Nil(t, dedicatedBaseAdvanceTriggerFor(bareIdx.repositoryMutationOwner))
	gw, err := NewGitWatcher(root, bareIdx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = gw.Stop() })
	gw.dispatchDedicatedBaseAdvance(gitHead(t, root))
}
