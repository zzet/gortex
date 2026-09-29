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
// of its ancestors are read too.
func withBuildReadSet(ctx context.Context, indexed, context_, deleted []string) context.Context {
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
}

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
func (b *SparseGenerationBuilder) confirmDirtyBuildInputs(
	ctx context.Context,
	sampler *gitstate.DirtySampler,
	root string,
	generationID int64,
	before gitstate.DirtySnapshot,
) error {
	if set, ok := buildReadSetFrom(ctx); ok && sampler != nil && os.Getenv(prepublishFullResampleEnv) != "1" && !prepublishSampleWanted(ctx) {
		started := time.Now()
		verdict, err := sampler.ConfirmReadSet(ctx, before, set.files, set.dirs)
		if err != nil {
			return err
		}
		if verdict.Confirmed {
			readSetConfirmations.confirmed.Add(1)
			if b.Logger != nil {
				b.Logger.Debug("indexer: working-tree inputs confirmed by read set",
					zap.Int64("generation", generationID),
					zap.Int("files", verdict.Files), zap.Int("dirs", verdict.Dirs),
					zap.Int("rehashed", verdict.Rehashed), zap.Duration("elapsed", time.Since(started)))
			}
			return nil
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
	return b.confirmDirtySnapshotWith(ctx, sampler, root, generationID, before.Fingerprint)
}
