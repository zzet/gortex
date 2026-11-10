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
// A file re-parsed since then lost what its committed pages applied. The
// indexer leaves such a file on the per-file ledger
// (graph.MetaReparsePendingEnrichment) until a whole pass completes, so a
// resumed pass applies those files again in every phase behind its cursor.
// Their direct importers are applied again too: an importer's calls and
// supers that the type engine bound into a re-parsed file went with it.
// Everything else pointing into a re-parsed file is left to the indexer:
// its restub pass parks incoming reference edges before eviction and
// restores their resolved provenance when the structural resolver rebinds
// them to the same target. Only an engine whose import hints are
// authoritative (LangSpec.ImportHintsAuthoritative) re-applies this way:
// elsewhere even a direct reference can bind across files with no import
// naming the file (same-package and wildcard references). Even there the
// importers are the direct dependents only. Files that bound into a
// re-parsed file by a route no recorded import reveals are not re-applied:
// ambient, global and script-mode names, specifiers that are not literals,
// type queries such as import("m").T, and binding through an intermediate
// module (a factory's return type, a member inherited through a middle
// class).
//
// Re-applying is paid once per ledger, not once per cut run. Every point
// carries the digest of the ledger its pages cover (each file's path and
// on-disk version): a pass from the first page covers the ledger present at
// its start, and a resumed pass covers the set it re-applied, which all runs
// before its first forward page commits. A resumed pass whose ledger
// digest matches the point's re-applies nothing. When it differs and the
// re-apply cannot be bounded — the engine's import hints are not
// authoritative, or the set is over half the staged files — the pass starts
// again from the first page instead, which covers the new ledger outright.

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
	store    graph.EnrichmentResumeStore
	point    graph.EnrichmentResumePoint
	record   bool
	resumed  bool
	reparsed []string // ledger-marked files a resumed pass re-applies
	// uncommitted is set from the moment a resumed pass starts re-applying
	// files until it records a forward page: the stored point still carries
	// the old ledger digest, so a pass cut in between must not leave it
	// behind. It stays clear before the apply phase starts, when nothing
	// was re-applied and the point is still the one to resume.
	uncommitted bool
	logger      *zap.Logger
}

// openApplyResume decides where a pass starts. It runs before the pass parses
// or writes anything, so a point it discards can never be read again and a
// point it honours describes state committed by an earlier pass; passes for
// one (repo, provider) never overlap, so nothing else writes the row
// meanwhile. reparsed names the candidate files on the per-file ledger and
// digest their current versions; a resumed pass applies them again unless
// the point already covers that digest. A nil result means the pass neither
// reads nor records points.
func (p *Provider) openApplyResume(ctx context.Context, g graph.Store, repoPrefix string, reparsed []string, digest string) *applyResume {
	scope, ok := semantic.EnrichResumeFrom(ctx)
	if !ok {
		return nil
	}
	store, ok := g.(graph.EnrichmentResumeStore)
	if !ok {
		return nil
	}
	r := &applyResume{
		store: store,
		point: graph.EnrichmentResumePoint{
			RepoPrefix: repoPrefix,
			Provider:   p.Name(),
			IndexedSHA: scope.SHA,
			Version:    p.resumeVersion,
			// Every point this pass records covers the current ledger: a
			// pass from the first page reaches every ledger file after its
			// re-parse, and a resumed pass re-applies them before its first
			// forward page commits.
			LedgerDigest: digest,
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
	if err == nil && scope.Resume && r.point.Version != "" &&
		prior.IndexedSHA == scope.SHA && prior.Version == r.point.Version && resumablePhase(prior.Phase) {
		reapply := len(reparsed) > 0 && prior.LedgerDigest != digest
		if !reapply || p.spec.ImportHintsAuthoritative {
			r.point.Phase, r.point.After, r.point.Covered = prior.Phase, prior.After, prior.Covered
			r.resumed = true
			if reapply {
				r.reparsed = reparsed
			}
			r.logger.Info("tstypes: apply resuming after committed pages",
				zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
				zap.Int("phase", prior.Phase), zap.String("after", prior.After),
				zap.Int("reparsed_files", len(r.reparsed)))
			return r
		}
		r.logger.Info("tstypes: re-parsed files changed and this engine's import hints do not name all their dependents; applying from the first page",
			zap.String("provider", p.Name()), zap.String("repo_prefix", repoPrefix),
			zap.Int("reparsed_files", len(reparsed)))
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

// Aliases resolve into the pass's temp spool only, so no point is recorded
// in that phase and a point naming it is not honoured.
func resumablePhase(phase int) bool {
	switch stagedFactPhase(phase) {
	case stagedSupers, stagedMetas, stagedCalls:
		return true
	}
	return false
}

func (r *applyResume) reapplying() bool { return r != nil && r.resumed && len(r.reparsed) > 0 }

// restart turns a resumed pass back into one from the first page and
// discards the point it resumed, which no longer describes this pass.
func (r *applyResume) restart() {
	if r == nil || !r.resumed {
		return
	}
	r.resumed, r.reparsed, r.uncommitted = false, nil, false
	r.point.Phase, r.point.After, r.point.Covered = 0, "", 0
	if err := r.store.ClearEnrichmentResume(r.point.RepoPrefix, r.point.Provider); err != nil {
		r.record = false
		r.logger.Warn("tstypes: discard resume point failed",
			zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix), zap.Error(err))
	}
}

func (r *applyResume) reapplyFiles() []string { return r.reparsed }

// beginReapply marks the start of the re-apply, before the importers are
// looked up: a cut from here until the first forward commit would repeat
// the same work on every later pass, so it discards the point instead.
func (r *applyResume) beginReapply() {
	if r.reapplying() {
		r.uncommitted = true
	}
}

func (r *applyResume) start() (stagedFactPhase, string, int, bool) {
	if r == nil || !r.resumed {
		return stagedSupers, "", 0, false
	}
	return stagedFactPhase(r.point.Phase), r.point.After, r.point.Covered, true
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
		return
	}
	r.uncommitted = false
}

// abandonUncommitted discards the point of a resumed pass that started
// re-applying files and ends, cut or failed, before it records a forward
// page.
// Re-applied pages never move the cursor, so the stored point would keep the
// old ledger digest and cursor, and every later pass would repeat the same
// re-apply and be cut at the same place. The next pass applies from the
// first page instead and records the new digest as it moves forward. Callers
// defer it once the point is opened; it does nothing before the re-apply
// starts, after a forward page commits, after restart, or after complete.
func (r *applyResume) abandonUncommitted() {
	if r == nil || !r.uncommitted {
		return
	}
	r.uncommitted = false
	r.logger.Info("tstypes: apply ended while re-applying re-parsed files, before it moved past its resume point; discarding the point so the next pass applies from the first page",
		zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix),
		zap.Int("reparsed_files", len(r.reparsed)))
	if err := r.store.ClearEnrichmentResume(r.point.RepoPrefix, r.point.Provider); err != nil {
		r.logger.Warn("tstypes: discard resume point failed",
			zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix), zap.Error(err))
	}
}

// complete removes the point once a pass has applied every page.
func (r *applyResume) complete() {
	if r == nil {
		return
	}
	r.uncommitted = false
	if err := r.store.ClearEnrichmentResume(r.point.RepoPrefix, r.point.Provider); err != nil {
		r.logger.Warn("tstypes: clear resume point failed",
			zap.String("provider", r.point.Provider), zap.String("repo_prefix", r.point.RepoPrefix), zap.Error(err))
	}
}

// ledgerFiles lists the candidate files carrying the indexer's
// re-parsed-without-enrichment marker, by graph path, and digests them with
// their on-disk versions. The marker alone cannot tell a second re-parse of
// a file from the first; the version can, since a re-parse that evicts a
// file's enrichment follows a write to it. The digest is "" when no file is
// marked.
func ledgerFiles(files []fileRef) ([]string, string) {
	type entry struct {
		path, version string
	}
	var entries []entry
	for _, ref := range files {
		if ref.node == nil || ref.node.Meta == nil {
			continue
		}
		if pending, _ := ref.node.Meta[graph.MetaReparsePendingEnrichment].(bool); pending {
			version := "missing"
			if info, err := os.Stat(ref.absPath); err == nil {
				version = strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10)
			}
			entries = append(entries, entry{ref.node.FilePath, version})
		}
	}
	if len(entries) == 0 {
		return nil, ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	paths := make([]string, len(entries))
	h := sha256.New()
	for i, e := range entries {
		paths[i] = e.path
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(e.version))
		h.Write([]byte{'\n'})
	}
	return paths, hex.EncodeToString(h.Sum(nil))
}
