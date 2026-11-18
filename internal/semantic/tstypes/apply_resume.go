package tstypes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// A streamed apply pass writes every page to the store as it goes, and the
// pass deadline for a large repository can fall well short of the whole
// apply. Without a record of how far it got, the next run replayed the
// committed pages (idempotent, but paying the full per-page preload again)
// and was cut at the same page: the pass could never complete. The resume
// point records the last committed page so the next pass continues after it.
//
// Skipping committed work is sound only because it is the same work: the
// point is keyed to the revision, to this engine's build, and is honoured
// only under the manager's resume scope (clean tree, no whole-repo re-parse,
// no forced re-enrich). Supers and metas pages are skipped outright; their
// edges and stamps are in the store, and later phases read them from there.
// The aliases phase writes only to the pass's temp spool, so it re-runs
// whenever the calls phase is reached again.
//
// A file re-parsed since then lost what its committed pages applied, and so
// did every file whose facts the type engine bound into it: importers, and
// files reaching it through inheritance, return types or repo-unique names.
// No bounded set names them all, so a pass does not resume over a re-parse.
// The indexer leaves a re-parsed file on the per-file ledger
// (graph.MetaReparsePendingEnrichment) until a whole pass completes, and
// stamps each re-parse, and each clearing of the marker, with a new
// graph.MetaReparseEpoch. Every point carries the digest of the ledger
// its pass listed its files from (ledgerDigest), and a pass resumes only a
// point whose digest matches its own, checked when it opens and again right
// before it skips the committed pages: any re-parse of a ledger-eligible
// file, a skip that replaces a file's facts with a synthetic node, or a file
// added or removed, since the point changes the digest and the pass applies
// from the first page. Resuming is kept for passes cut by
// the deadline alone.

// applyResumeFormat versions what a resume point means: the fact classes,
// their keyset page order and the phase sequence. Bump it when any of those
// change. The build identity below also invalidates points across upgrades.
const applyResumeFormat = 1

// applyResumeVersion is empty when the build carries neither a VCS revision
// nor a released module version: two such binaries could apply facts
// differently under one key, so a build that cannot name itself never
// resumes or records.
var applyResumeVersion = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return resumeVersionFor(info)
})

func resumeVersionFor(info *debug.BuildInfo) string {
	identified := info.Main.Version != "" && info.Main.Version != "(devel)"
	v := "tstypes/" + strconv.Itoa(applyResumeFormat) + "/" + info.Main.Version
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			identified = identified || setting.Value != ""
			v += "/" + setting.Value
		case "vcs.modified":
			v += "/" + setting.Value
		}
	}
	if !identified {
		return ""
	}
	return v
}

type applyResume struct {
	store  graph.EnrichmentResumeStore
	point  graph.EnrichmentResumePoint
	record bool
	logger *zap.Logger
	// resumed says the pass starts at point rather than the first page.
	resumed bool
}

// openApplyResume decides where a pass starts. It runs before the pass parses
// or writes anything, so a point it discards can never be read again and a
// point it honours describes state committed by an earlier pass; passes for
// one (repo, provider) never overlap, so nothing else writes the row
// meanwhile. digest computes the ledger digest over the file nodes the pass
// lists its files from (digestLedger), only once the pass can read and
// record points; revalidate checks a resume against the ledger again before
// the apply. A nil result means the pass neither reads nor records points.
func (p *Provider) openApplyResume(ctx context.Context, g graph.Store, repoPrefix string, digest func() string) *applyResume {
	scope, ok := semantic.EnrichResumeFrom(ctx)
	if !ok {
		return nil
	}
	store, ok := g.(graph.EnrichmentResumeStore)
	if !ok {
		return nil
	}
	current := digest()
	r := &applyResume{
		store: store,
		point: graph.EnrichmentResumePoint{
			RepoPrefix: repoPrefix,
			Provider:   p.Name(),
			IndexedSHA: scope.SHA,
			Version:    p.resumeVersion,
			// Every point this pass records covers the ledger it started
			// with: a pass from the first page applies every ledger file
			// after its re-parse, and a resumed pass continues one that
			// started over the same ledger.
			LedgerDigest: current,
		},
		record: scope.Record && p.resumeVersion != "",
		logger: p.logger,
	}
	prior, found, err := store.GetEnrichmentResume(repoPrefix, p.Name())
	if err != nil {
		r.logger.Warn("tstypes: read resume point failed; applying from the first page",
			zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix), zap.Error(err))
		found = true // still try to discard whatever is there
	}
	if !found {
		return r
	}
	if why := resumeRefusal(err, scope, prior, r.point.Version, current); why != "" {
		if err == nil {
			// Say why: a point recorded by another build or revision is
			// discarded by design, but silently it reads as a resume that
			// never happened.
			r.logger.Info(resumeRefusedMsg,
				zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
				zap.String("reason", why), zap.Int("phase", prior.Phase),
				zap.String("recorded_sha", prior.IndexedSHA), zap.String("sha", scope.SHA),
				zap.String("recorded_version", prior.Version), zap.String("version", r.point.Version))
		}
	} else {
		r.point.Phase, r.point.After, r.point.Covered = prior.Phase, prior.After, prior.Covered
		r.resumed = true
		r.logger.Info("tstypes: apply resuming after committed pages",
			zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
			zap.Int("phase", prior.Phase), zap.String("after", prior.After))
		return r
	}
	if err := store.ClearEnrichmentResume(repoPrefix, p.Name()); err != nil {
		// A stale point this pass cannot remove must not outlive it either:
		// record nothing, so the next pass sees the same stale point and
		// discards it again.
		r.record = false
		r.logger.Warn("tstypes: discard stale resume point failed",
			zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix), zap.Error(err))
	}
	return r
}

// resumeRefusedMsg is logged once, with its reason, when a stored point is
// discarded rather than resumed.
const resumeRefusedMsg = "tstypes: resume point not honoured; applying from the first page"

// resumeRefusal names why a stored point cannot be resumed, or returns ""
// when it can. Every refusal discards the point.
func resumeRefusal(err error, scope semantic.EnrichResumeScope, prior graph.EnrichmentResumePoint, version, digest string) string {
	switch {
	case err != nil:
		return "read failed"
	case !scope.Resume:
		return "this pass may not resume (dirty tree, forced re-enrich, or a re-parse this run the ledger does not cover)"
	case version == "":
		return "this build has no identity"
	case prior.IndexedSHA != scope.SHA:
		return "recorded at another revision"
	case prior.Version != version:
		return "recorded by another build"
	case !resumablePhase(prior.Phase):
		return "recorded in a phase that is not resumable"
	case prior.LedgerDigest != digest:
		return "a file was re-parsed, added or removed since it was recorded"
	}
	return ""
}

// Aliases resolve into the pass's temp spool only, so no point is recorded
// in that phase and a point naming it is not honoured.
func resumablePhase(phase int) bool {
	switch stagedFactPhase(phase) {
	case stagedSupers, stagedMetas, stagedCalls:
		return true
	}
	return false
}

func (r *applyResume) start() (stagedFactPhase, string, int, bool) {
	if r == nil || !r.resumed {
		return stagedSupers, "", 0, false
	}
	return stagedFactPhase(r.point.Phase), r.point.After, r.point.Covered, true
}

// revalidate drops the resume when current, the ledger digest now, differs
// from the one it was decided over. The decision is made before the pass
// stages its facts, and a covered file re-parsed meanwhile lost facts the
// pages behind the point applied. Callers invoke it right before the apply
// skips those pages. Re-parses do not take the resolve mutex the apply
// holds, so a file can still be re-parsed after this check, during the
// apply. That is safe because every point the pass records keeps the digest
// it started with while the re-parse moves the file's epoch, so the next
// pass refuses those points, and the indexer discharges the ledger after a
// completed pass only when no change was queued during it
// (clearPendingEnrich's generation check).
//
// A refused resume keeps the digest the pass opened with, so the next pass
// refuses every point this one records: its staged facts may predate the
// re-parse. Recording current instead would be unsound.
func (r *applyResume) revalidate(current func() string) {
	if r == nil || !r.resumed || current() == r.point.LedgerDigest {
		return
	}
	r.logger.Info(resumeRefusedMsg,
		zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix),
		zap.String("reason", "a file was re-parsed while the pass staged"),
		zap.Int("phase", r.point.Phase), zap.String("after", r.point.After))
	r.point.Phase, r.point.After, r.point.Covered = int(stagedSupers), "", 0
	r.resumed = false
}

// commit records that every page of phase up to and including after is in
// the store. Callers invoke it only after the page's writes returned.
func (r *applyResume) commit(phase stagedFactPhase, after string, covered int) {
	if r == nil || !r.record {
		return
	}
	r.point.Phase, r.point.After, r.point.Covered = int(phase), after, covered
	r.point.UpdatedAt = time.Now().Unix()
	if err := r.store.SetEnrichmentResume(r.point); err != nil {
		// Progress tracking is an optimisation; stop trying for this pass.
		r.record = false
		r.logger.Warn("tstypes: record resume point failed",
			zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix), zap.Error(err))
	}
}

// complete removes the point once a pass has applied every page.
func (r *applyResume) complete() {
	if r == nil {
		return
	}
	if err := r.store.ClearEnrichmentResume(r.point.RepoPrefix, r.point.Provider); err != nil {
		r.logger.Warn("tstypes: clear resume point failed",
			zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix), zap.Error(err))
	}
}

// ledgerDigest digests the repository's file nodes of the spec's languages
// by graph path, with each file's re-parse epoch and, while it carries the
// indexer's re-parsed-without-enrichment marker, its on-disk version. It
// reads every such node, not only the files the pass analyzes: the C# and
// Kotlin engines bind cross-file types by repo-unique name, and that lookup
// counts vendored, over-cap and missing files' nodes too. The path set
// moves when a file is added or removed. The epoch tells a second re-parse
// of a file from the first, including one with no write to the file (the
// importer of a deleted file) and a skip that replaced its facts with a
// synthetic node, and is replaced rather than removed when the marker is
// cleared. The on-disk version covers markers written before epochs were.
// It is "" when there are no files.
func ledgerDigest(g graph.Store, spec *LangSpec, repoPrefix, repoRoot string) string {
	return digestLedger(repoFileNodes(g, spec, repoPrefix), repoRoot)
}

// digestLedger is ledgerDigest over nodes already read by repoFileNodes.
func digestLedger(nodes []*graph.Node, repoRoot string) string {
	type entry struct {
		path, version string
	}
	var entries []entry
	for _, n := range nodes {
		version, _ := n.Meta[graph.MetaReparseEpoch].(string)
		if pending, _ := n.Meta[graph.MetaReparsePendingEnrichment].(bool); pending {
			if info, err := os.Stat(fileAbsPath(n, repoRoot)); err == nil {
				version += ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10)
			} else {
				version += ":missing"
			}
		}
		entries = append(entries, entry{n.FilePath, version})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(e.version))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
