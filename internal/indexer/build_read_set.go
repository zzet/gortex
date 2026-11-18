package indexer

import (
	"context"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
)

// prepublishFullResampleEnv, set to 1, makes every working-tree build confirm
// its inputs with a full working-copy sample before it publishes, as it did
// before the read-set confirmation existed.
const prepublishFullResampleEnv = "GORTEX_PREPUBLISH_FULL_RESAMPLE"

// buildReadSetManifests are the files at a read directory or any of its
// ancestors that decide how the files below them parse or bind: module and
// workspace manifests, package and compiler configurations.
var buildReadSetManifests = []string{
	"go.mod", "go.sum", "go.work", "go.work.sum",
	"package.json", "tsconfig.json", "jsconfig.json",
}

type buildReadSetKey struct{}

// buildReadSet is what a build read from its working tree: the files it
// indexed or claims deleted, and the directories whose every file entry its
// semantic pass may have read (the changed files' packages).
type buildReadSet struct {
	files []string
	dirs  []string
}

// withBuildReadSet attaches the read set of a build's plan to the context its
// PrePublish runs under. indexed (the change set and its context), context and
// deleted are repository-relative. Every indexed and deleted file is read. The
// directory of each changed or deleted file is read whole — a semantic pass
// type-checks the changed file's package, which is its directory — while a
// context file is read alone. The manifests at every read directory and each
// of its ancestors are read too, and so are the per-directory ignore files
// there (dirIgnoreFiles): the walk gate reads every ancestor's to admit a
// file.
func withBuildReadSet(ctx context.Context, indexed, context_, deleted []string) context.Context {
	return withBuildReadSetDirs(ctx, indexed, context_, deleted, nil)
}

// withBuildReadSetDirs is withBuildReadSet with directories a reader listed
// or probed (claimed, repository-relative; "." is the root), each read whole.
func withBuildReadSetDirs(ctx context.Context, indexed, context_, deleted, claimed []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	clean := func(p string) (string, bool) {
		p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
		if p == "." || p == "" || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			return "", false
		}
		return p, true
	}
	contextual := make(map[string]struct{}, len(context_))
	for _, p := range context_ {
		if c, ok := clean(p); ok {
			contextual[c] = struct{}{}
		}
	}
	files := make(map[string]struct{}, len(indexed)+len(deleted))
	dirs := make(map[string]struct{})
	parents := make(map[string]struct{})
	add := func(p string, wholeDir bool) {
		c, ok := clean(p)
		if !ok {
			return
		}
		files[c] = struct{}{}
		parents[path.Dir(c)] = struct{}{}
		if wholeDir {
			dirs[path.Dir(c)] = struct{}{}
		}
	}
	for _, p := range indexed {
		c, _ := clean(p)
		_, isContext := contextual[c]
		add(p, !isContext)
	}
	for _, p := range deleted {
		add(p, true)
	}
	for _, d := range claimed {
		if d = path.Clean(strings.ReplaceAll(d, "\\", "/")); d == "." {
			dirs[d] = struct{}{}
			parents[d] = struct{}{}
		} else if c, ok := clean(d); ok {
			dirs[c] = struct{}{}
			parents[c] = struct{}{}
		}
	}
	ancestors := make(map[string]struct{})
	for dir := range parents {
		for d := dir; ; d = path.Dir(d) {
			if _, seen := ancestors[d]; seen {
				break
			}
			ancestors[d] = struct{}{}
			if d == "." {
				break
			}
		}
	}
	for dir := range ancestors {
		for _, name := range buildReadSetManifests {
			files[path.Join(dir, name)] = struct{}{}
		}
		for _, name := range dirIgnoreFiles {
			files[path.Join(dir, name)] = struct{}{}
		}
	}
	set := buildReadSet{files: make([]string, 0, len(files)), dirs: make([]string, 0, len(dirs))}
	for p := range files {
		set.files = append(set.files, p)
	}
	for d := range dirs {
		set.dirs = append(set.dirs, d)
	}
	sort.Strings(set.files)
	sort.Strings(set.dirs)
	return context.WithValue(ctx, buildReadSetKey{}, set)
}

// buildReadSetFrom returns the read set withBuildReadSet attached.
func buildReadSetFrom(ctx context.Context) (buildReadSet, bool) {
	if ctx == nil {
		return buildReadSet{}, false
	}
	set, ok := ctx.Value(buildReadSetKey{}).(buildReadSet)
	return set, ok
}

type prepublishSampleDemandKey struct{}

// withPrepublishSampleDemand attaches a check a working-tree build's
// prepublish fence consults: when it reports true, the fence takes the full
// working-copy sample even though the read set could confirm the build. The
// coordinator uses it for refresh tickets admitted after the build's sample
// began (requests riding the build): only a sample taken after they arrived
// can complete them with this build's publication, and the fence's sample is
// that sample.
func withPrepublishSampleDemand(ctx context.Context, wanted func() bool) context.Context {
	if ctx == nil || wanted == nil {
		return ctx
	}
	return context.WithValue(ctx, prepublishSampleDemandKey{}, wanted)
}

func prepublishSampleWanted(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	wanted, _ := ctx.Value(prepublishSampleDemandKey{}).(func() bool)
	return wanted != nil && wanted()
}

// readSetConfirmations counts prepublish fences decided by the read set
// (confirmed) and by a full sample (fallback), for tests and diagnostics.
var readSetConfirmations struct {
	confirmed, fallback atomic.Uint64
	// contentProven counts the read-set confirmations (either count) that
	// confirmed a moved read file by the bytes the build parsed
	// (buildContentProof).
	contentProven atomic.Uint64
	// afterSample counts the fences whose full sample found the tree moved
	// and whose read set then proved the payload is the build's sample: the
	// build published a state the working copy had left. They are neither
	// confirmed (a full sample was taken) nor fallback.
	afterSample atomic.Uint64
	// refuted counts the fences that tore a build whose own reads
	// contradicted its sample (buildContentProof.contradiction).
	refuted atomic.Uint64
	// unbounded counts the fences of builds that ran a reader no read set
	// holds (buildContentProof.noteUnboundedReader): the full sample decided.
	unbounded atomic.Uint64
}

// prepublishReadSetSeen is a test seam: when set, the prepublish fence hands
// it the read set it was given, and whether that read set is a complete
// proof (the content proof is bounded and scoped) the fence may confirm by
// parsed bytes.
var prepublishReadSetSeen atomic.Pointer[func(set buildReadSet, complete bool)]

// confirmDirtyBuildInputs is a working-tree build's prepublish fence: it
// proves the payload describes before, the state the build sampled, and
// refuses the publish otherwise.
//
// The proof is proportional to the build: gitstate's ConfirmReadSet checks the
// change stamps of exactly what the build read (its read set, attached by the
// builder) against the sample, with no git process. A change elsewhere in the
// checkout does not make this payload wrong for the fingerprint it names; the
// next cycle's sample sees it and builds it. Whenever the read set cannot
// prove it — a read file moved, HEAD's files moved, the read set is unknown,
// the filesystem gives no change stamps — the fence is the full re-sample
// (confirmDirtySnapshotWith), which is what decides. So it is when a refresh
// ticket waits that only a new sample can complete (withPrepublishSampleDemand).
//
// A delta that proves its reads (proof, buildContentProof) is also confirmed
// by the bytes it parsed (ConfirmReadSetContent): a read file saved again
// after the sample still confirms when the build parsed the sample's bytes
// for it. The payload then describes before, a state the checkout really was
// in, and is published under its fingerprint even though the working copy
// has moved on; outpaced reports that the fence saw it move (a parsed file
// stamped past the sample, or a full sample that differs), and the
// coordinator builds the newer state next. The read set does not see a move
// confined to files the build never read: that build is confirmed with
// outpaced false, a truthful publication of its sample, and the newer state
// is left to the signal the move raised (the watcher's, a poll's, a ticket's). A refresh ticket's full sample
// comes first, as before: when it finds the tree moved, the content proof is
// what spares the build, and the ticket waits for the next one. Deleted,
// renamed and unparsed read paths, read directories and HEAD's files are
// still confirmed by their stamps, and GORTEX_PREPUBLISH_FULL_RESAMPLE=1
// turns the content proof off with the read set. Only a proof whose read set
// holds every working-copy read of the build (buildContentProof) confirms by
// parsed bytes or publishes over a full sample that differs. A build that
// ran a reader no read set can hold — its enrichment stage among them — is
// unbounded (buildContentProof.noteUnboundedReader): it skips the read set,
// and the full sample alone decides.
//
// Before anything is sampled, bytes the build read that are not the
// sample's (buildContentProof.contradiction) tear it: the payload is another
// state's, even if the working copy has since returned to the sample's. The
// sample decides only the paths it reports; a full sample equal to before
// does not prove a read of any other path (a clean file, a path the sample
// never saw), which may have been taken while that path held another state
// and restored since. Such reads must still hold in the working copy the
// full sample found (buildContentProof.readMoved), every read the proof
// holds no bytes for — a read of unknown bytes, a manifest or ignore file,
// a changed path the build never read — must not have moved since the
// build's own sample began, and every manifest-tree answer a reader took must
// be what the working copy answers now (buildContentProof.unrecordedMoved),
// or the build is torn. The read set's own confirmation applies the same rule
// to a dirty file the build holds no bytes for: its moved stamp refuses,
// where a build without a proof re-hashes it.
func (b *SparseGenerationBuilder) confirmDirtyBuildInputs(
	ctx context.Context,
	sampler *gitstate.DirtySampler,
	root string,
	generationID int64,
	before gitstate.DirtySnapshot,
	proof *buildContentProof,
) (outpaced bool, err error) {
	if refuted := proof.contradiction(); refuted != "" {
		readSetConfirmations.refuted.Add(1)
		if b.Logger != nil {
			b.Logger.Info("indexer: working-tree build read bytes other than its sample's",
				zap.Int64("generation", generationID), zap.String("path", refuted))
		}
		return false, b.tearDirtySnapshot(ctx, root, generationID, before.Fingerprint, "bytes other than the sample's in "+refuted)
	}
	set, haveSet := buildReadSetFrom(ctx)
	unbounded := proof.isUnbounded()
	readSet := haveSet && sampler != nil && os.Getenv(prepublishFullResampleEnv) != "1" && !unbounded
	parsed := proof.proven()
	if unbounded {
		readSetConfirmations.unbounded.Add(1)
		if b.Logger != nil {
			b.Logger.Info("indexer: working-tree inputs need a full sample",
				zap.Int64("generation", generationID), zap.String("reason", "a reader no read set holds took the working copy"))
		}
	}
	if seen := prepublishReadSetSeen.Load(); haveSet && seen != nil {
		(*seen)(set, readSet && parsed != nil)
	}
	confirm := func() (gitstate.ReadSetConfirmation, error) {
		if parsed != nil {
			return sampler.ConfirmReadSetContent(ctx, before, set.files, set.dirs, parsed)
		}
		return sampler.ConfirmReadSet(ctx, before, set.files, set.dirs)
	}
	confirmed := func(verdict gitstate.ReadSetConfirmation, started time.Time, afterSample bool) {
		if afterSample {
			readSetConfirmations.afterSample.Add(1)
		} else {
			readSetConfirmations.confirmed.Add(1)
		}
		if verdict.ContentProven > 0 {
			readSetConfirmations.contentProven.Add(1)
		}
		if b.Logger == nil {
			return
		}
		log := b.Logger.Debug
		if verdict.ContentProven > 0 {
			log = b.Logger.Info
		}
		log("indexer: working-tree inputs confirmed by read set",
			zap.Int64("generation", generationID),
			zap.Int("files", verdict.Files), zap.Int("dirs", verdict.Dirs),
			zap.Int("rehashed", verdict.Rehashed), zap.Int("content_proven", verdict.ContentProven),
			zap.Duration("elapsed", time.Since(started)))
	}
	triedReadSet := false
	if readSet && !prepublishSampleWanted(ctx) {
		triedReadSet = true
		started := time.Now()
		verdict, err := confirm()
		if err != nil {
			return false, err
		}
		if verdict.Confirmed {
			confirmed(verdict, started, false)
			return verdict.ContentProven > 0, nil
		}
		readSetConfirmations.fallback.Add(1)
		if b.Logger != nil {
			// Info, not Debug: the full sample is a git process through the
			// shared limiter, hundreds of milliseconds to seconds of an
			// edit's publication on a busy daemon, and the reason is what
			// says whether the read set could have spared it.
			b.Logger.Info("indexer: working-tree inputs need a full sample",
				zap.Int64("generation", generationID), zap.String("reason", verdict.Reason),
				zap.Duration("elapsed", time.Since(started)))
		}
	}
	if b.Logger != nil && prepublishSampleWanted(ctx) {
		b.Logger.Info("indexer: working-tree inputs sampled for a refresh riding the build",
			zap.Int64("generation", generationID))
	}
	after, err := b.sampleDirtyAfterBuild(ctx, sampler, root, generationID, before.Fingerprint)
	if err != nil {
		return false, err
	}
	if after.Fingerprint == before.Fingerprint {
		// The working copy is in the sampled state now, which says nothing
		// about a path the sample does not decide that a reader took while
		// it held another state, restored since: the build's recorded reads
		// of those paths must hold now, and every path of its read set it
		// holds no record for must not have moved since its sample began.
		moved, err := proof.readMoved(ctx, sampler, before)
		if err == nil && moved == "" {
			moved, err = proof.unrecordedMoved(ctx, sampler, before)
		}
		if err != nil || moved == "" {
			return false, err
		}
		readSetConfirmations.refuted.Add(1)
		if b.Logger != nil {
			b.Logger.Info("indexer: working-tree build read a path in a state other than its sample's",
				zap.Int64("generation", generationID), zap.String("path", moved))
		}
		return false, b.tearDirtySnapshot(ctx, root, generationID, before.Fingerprint, "a read of "+moved+" the working copy no longer answers")
	}
	if readSet && parsed != nil && !triedReadSet {
		started := time.Now()
		verdict, err := confirm()
		if err != nil {
			return false, err
		}
		// The full sample already says the tree moved, so a confirmed read
		// set publishes a state the working copy has left (outpaced) even
		// when the move was only in files the build never read.
		if verdict.Confirmed {
			confirmed(verdict, started, true)
			return true, nil
		}
		if b.Logger != nil {
			b.Logger.Info("indexer: working-tree inputs moved past the sample",
				zap.Int64("generation", generationID), zap.String("reason", verdict.Reason),
				zap.Duration("elapsed", time.Since(started)))
		}
	}
	return false, b.tearDirtySnapshot(ctx, root, generationID, before.Fingerprint, after.Fingerprint)
}
