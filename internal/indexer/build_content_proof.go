package indexer

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// buildContentProof is what a sample-pinned working-tree build proves about
// the bytes it read: for each file the per-file pass parsed (and each file
// the shared-row emitter scan read), the content identity
// (gitstate.BlobSHA256) of the bytes that went into the payload.
//
// The prepublish fence (confirmDirtyBuildInputs) accepts a read file that was
// saved again after the build's sample when the bytes the build parsed are
// the sample's content for it (gitstate.ConfirmReadSetContent): the payload
// describes the sample for that file, whatever the file holds now, so the
// build publishes under the sample's fingerprint and the newer save is the
// next build's change set. The per-file pass uses the same test for a read
// receipt the save made stale (recordFileReadVersionsBatched): bytes equal to
// the sample's are kept rather than torn.
//
// The proof also refutes: bytes the build read that are not the sample's
// content for a file the sample holds, a read that found absent a file the
// sample holds, or two reads of one file that disagree, mean the payload is
// not the sample's whatever the working copy holds by the fence, and the
// build is torn (contradiction) — even when an undo has since put the
// sample's bytes back.
//
// The fence can confirm only a read set that holds every working-copy read
// the build made between its sample and the fence, so each reader the delta
// reaches is covered one of three ways:
//
//   - recorded with the content identity of the bytes it took, or as absent
//     (record, recordRead): every Indexer.readFileWithVersion (the parse and
//     the re-reads of a file whose speculative parse went stale), every
//     Indexer.readFileContent (the root manifests the per-save module relink
//     reads, contract sources), the shared-row emitter scan, the deletion
//     walk's second extraction of the changed files, every manifest-tree
//     file read (provenManifestTree), the per-directory ignore files the walk
//     gate reads (excludes.Hierarchical.ObserveReads), the root manifests the
//     project-name detection reads, each CODEOWNERS location the ownership
//     pass tries (Indexer.loadCodeownersRules: .github/CODEOWNERS,
//     CODEOWNERS, docs/CODEOWNERS, until one reads), the body-facts cache's
//     handler-source reads, the scoped walk's finding
//     that an explicit path is gone, and every change the plan demoted to a
//     deletion because the working copy no longer held it;
//   - recorded as a read of unknown bytes, held to its change stamp: each
//     tsconfig / jsconfig a cached alias scope came from (holdAliasConfigs),
//     when the process-wide alias cache answered the build;
//   - recorded as an existence probe with its answer (probe): the deletion
//     walk's checks of the files it plans and of the root manifests, the
//     importer scan's check of a deleted file's importers, and a tsconfig
//     alias's multi-target probes (tsAliasProbes), from a cached alias
//     collection too;
//   - claimed by name at every directory a read file lies under
//     (withBuildReadSet): the module and package manifests and the
//     per-directory ignore files the walk gate reads;
//   - claimed as a whole directory (claimDir): a manifest-tree probe, glob
//     or listing, whose answer is recorded too (noteAnswer).
//
// A reader whose reads cannot be enumerated — the enrichment stage, which
// loads whole packages and their imports (or hands the checkout to a
// language server); a whole-tree tsconfig scan; a workspace-glob expansion —
// makes the proof unbounded (noteUnboundedReader): the fence takes a full
// sample and publishes only when the working copy is still the sample's.
// The legacy contract path (no contract-core runtime) keeps the read files'
// directories claimed whole, as before the proof (noteUnscopedReader).
//
// The sample decides the paths it reports (present with their bytes, or
// absent): a read or a probe that disagrees with it is a contradiction. A
// path it does not decide — a clean file, a path it never saw — is decided
// by the working copy: when the fence's full sample finds the working copy in
// the sampled state, every such read must still hold there (readMoved), or
// the build read the path while it held another state and the payload is
// torn, even though the fingerprints agree. A read the proof holds no bytes
// to judge by — read with unknown bytes, a changed path no reader took, or a
// manifest or ignore file (claimedByName: readers beyond the proof take
// those, so one reader's recorded bytes never stand for them) — is proven
// there, and by the read set's confirmation, by its change stamp alone: it
// must not have moved since the build's sample began; a manifest-tree answer
// (noteAnswer) must be the one the working copy gives now (unrecordedMoved).
// The walk gate's ignore files and the project-name detection's root
// manifests are read through the proof (recordRead). A build's partial
// reads of a file (the language sniff's prefix) are recorded as reads of
// unknown bytes, so the file is held to its stamp even when a whole-file
// read of it recorded the sample's bytes.
//
// Only a delta build over a checkout sample carries one: a build through the
// sparse closure builder, an import batch and an import-lane build disable it
// and keep the stamp-only fence.
//
// The lists above are the contract a new working-copy reader of the delta
// must join: record its reads, probe through the proof, claim what it lists,
// or note itself unbounded. What the proof deliberately does not cover —
// each accepted, not overlooked:
//
//   - A symlinked ancestor directory swapped to another target and swapped
//     back between the sample and the fence. The fence re-reads and stats a
//     file through the path as it resolves now, the restored link, so a read
//     taken through the other target is invisible to it. Guarding it means
//     an Lstat of every ancestor of every read at the fence, for a layout
//     (a symlinked directory inside a checkout, swapped twice mid-build)
//     checkouts rarely hold.
//   - Process-wide caches that hold bytes read before the build's sample:
//     the C/C++ include directories from compile_commands.json (keyed by the
//     database's mtime; the lookup's probes are claimed), and the
//     tsconfig / jsconfig alias cache beyond the configs its scopes came
//     from (a config that parsed to no scope is not held; the first load is
//     a whole-tree scan, unbounded). Their staleness predates the proof and
//     is the same for every build of the process, main's included; the proof
//     holds what the build itself read, and their invalidation is theirs.
//   - The legacy unscoped contract path (no contract-core runtime, which the
//     daemon never builds without): it reads cross-file handler sources and
//     their directories beyond what the proof records, so on the equal-sample
//     branch it keeps main's full-sample fence (unrecordedMoved returns
//     early; noteUnscopedReader).
//   - A filesystem without trusted change stamps, or a realtime clock that
//     jumped: nothing held by its stamp can be proven, so the fence keeps
//     main's full-sample decision (ConfirmReadSetSettled reports unproven);
//     recorded bytes are still re-hashed (readMoved).
//   - The move-and-restore guard test moves the fixture's own paths and the
//     named root files every build reads (ignore files, module manifests,
//     CODEOWNERS at each location), not arbitrary paths a future reader
//     might take: a new reader is covered only once it joins the lists above.
type buildContentProof struct {
	root string
	// sample is the sample's content identity of each path it holds as a
	// regular file's bytes, keyed by repository-relative slash path.
	sample map[string]string

	mu  sync.Mutex
	off bool
	// unscoped is set once the read files' directories must be claimed
	// whole (noteUnscopedReader); unbounded once a reader whose reads the
	// read set cannot hold ran (noteUnboundedReader). Either leaves the
	// proof refuting but never confirming.
	unscoped  bool
	unbounded bool
	parsed    map[string]parsedContent
	// claimed are the repository-relative directories a reader listed or
	// probed (claimDir): the fence claims them whole.
	claimed map[string]struct{}
	// probed are the repository-relative paths a reader only checked for
	// existence (probe), with what it found: the fence confirms them by
	// their stamps (or their absence) without disturbing a read of their
	// bytes, and refutes a probe whose answer the sample, or the working copy
	// the full sample found back in the sampled state, contradicts.
	probed map[string]probeSeen
	// answers are the manifest-tree answers readers took (noteAnswer), and
	// plan the changed and deleted paths of the build (readSet): the fence
	// re-asks the one and holds the other to its stamps (unrecordedMoved).
	answers []recordedAnswer
	plan    []string
}

// probeSeen is what the existence probes of one path found.
type probeSeen struct {
	// found is whether the path held a file: a non-directory entry, its
	// symlink followed when follow is set.
	found, follow bool
	// conflict is set when two probes of the build found different answers.
	conflict bool
}

// absentContent is the content identity recordRead gives a read that found
// no file: the fence confirms it by the file's absence (or refutes it when
// the sample holds the file).
const absentContent = gitstate.ReadAbsent

// parsedContent is one file the build read.
type parsedContent struct {
	// sha256 is the bytes' content identity, empty when unknown or when two
	// reads disagreed.
	sha256 string
	// conflict is set when two reads of the build took different bytes.
	conflict bool
}

// newBuildContentProof starts the proof of a build of before, rooted at the
// checkout root the build reads.
func newBuildContentProof(root string, before gitstate.DirtySnapshot) *buildContentProof {
	p := &buildContentProof{root: root, sample: make(map[string]string, len(before.Contents)),
		parsed: map[string]parsedContent{}, claimed: map[string]struct{}{}, probed: map[string]probeSeen{}}
	for _, content := range before.Contents {
		switch {
		case content.State == gitstate.DirtyContentAbsent:
			// The sample saw nothing there: a read that took bytes from it,
			// or a probe that found it, read another state.
			p.sample[path.Clean(content.Path)] = absentContent
		case content.State == gitstate.DirtyContentPresent && (content.Mode == "100644" || content.Mode == "100755"):
			p.sample[path.Clean(content.Path)] = content.SHA256
		}
	}
	return p
}

// active reports whether the build is still recording.
func (p *buildContentProof) active() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.off
}

// disable ends the proof: the fence confirms by change stamps alone.
func (p *buildContentProof) disable() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.off = true
	p.parsed = nil
	p.mu.Unlock()
}

// noteUnscopedReader records that a reader took files of the working copy
// the proof records only as stage one did, by their directories: from now on
// the proof refutes but never confirms, and the fence claims every read
// file's directory whole.
func (p *buildContentProof) noteUnscopedReader() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.unscoped = true
	p.mu.Unlock()
}

// noteUnboundedReader records that a reader took the working copy beyond
// any read set the fence can confirm: the fence takes a full sample instead,
// and the proof refutes but never confirms.
func (p *buildContentProof) noteUnboundedReader() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.unscoped, p.unbounded = true, true
	p.mu.Unlock()
}

// isUnbounded reports whether a reader the read set cannot hold ran.
func (p *buildContentProof) isUnbounded() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.off && p.unbounded
}

// recordRead records one whole-file read of abs: the content identity of the
// bytes when it succeeded, absentContent when it found no file, and unknown
// (the file stays read, unproven) for any other failure — a read that took
// bytes and then failed among them.
func (p *buildContentProof) recordRead(abs string, data []byte, err error) {
	if !p.active() {
		return
	}
	sum := ""
	switch {
	case err == nil:
		sum = gitstate.BlobSHA256(data)
	case data == nil && errors.Is(err, fs.ErrNotExist):
		sum = absentContent
	}
	p.record(abs, sum)
}

// abs is the absolute path of the repository-relative slash path rel.
func (p *buildContentProof) abs(rel string) string {
	return filepath.Join(p.root, filepath.FromSlash(rel))
}

// probe records that a reader checked whether the path at abs holds a file,
// and what it found: a non-directory entry (its symlink followed when follow
// is set).
func (p *buildContentProof) probe(abs string, found, follow bool) {
	if !p.active() {
		return
	}
	rel, ok := p.key(abs)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off {
		return
	}
	prior, seen := p.probed[rel]
	switch {
	case !seen:
		p.probed[rel] = probeSeen{found: found, follow: follow}
	case prior.found != found:
		prior.conflict = true
		p.probed[rel] = prior
	}
}

// probeHolds reports whether the path at abs answers a probe now as seen
// found it.
func probeHolds(abs string, seen probeSeen) bool {
	stat := os.Lstat
	if seen.follow {
		stat = os.Stat
	}
	info, err := stat(abs)
	return (err == nil && !info.IsDir()) == seen.found
}

// claimDir claims the directory at abs whole: a reader listed it, or probed
// an entry of it.
func (p *buildContentProof) claimDir(abs string) {
	if !p.active() {
		return
	}
	rel, ok := p.key(abs)
	if !ok && filepath.Clean(abs) == filepath.Clean(p.root) {
		rel, ok = ".", true
	}
	if !ok {
		return
	}
	p.mu.Lock()
	if !p.off {
		p.claimed[rel] = struct{}{}
	}
	p.mu.Unlock()
}

// key is abs's repository-relative slash path under the proof's root.
func (p *buildContentProof) key(abs string) (string, bool) {
	rel, err := filepath.Rel(p.root, abs)
	if err != nil {
		return "", false
	}
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// sampleSays reports what the sample holds for the file at abs: its content
// identity, and whether the sample holds it as a regular file's bytes.
func (p *buildContentProof) sampleSays(abs string) (string, bool) {
	if !p.active() {
		return "", false
	}
	rel, ok := p.key(abs)
	if !ok {
		return "", false
	}
	want, ok := p.sample[rel]
	return want, ok
}

// holdsSample reports whether sum is the sample's content of the file at abs:
// bytes read from it describe the build's sample however it moved since.
func (p *buildContentProof) holdsSample(abs, sum string) bool {
	want, ok := p.sampleSays(abs)
	return ok && sum != "" && want == sum
}

// contradicts reports whether sum, the bytes a read took from the file at abs,
// is other than the sample's content for it: a payload built from them is
// not the sample's.
func (p *buildContentProof) contradicts(abs, sum string) bool {
	want, ok := p.sampleSays(abs)
	return ok && sum != "" && want != sum
}

// record notes that the build read bytes with content identity sum from the
// file at abs. An empty sum leaves the file unproven; a sum another read of
// the same file disagrees with is a conflict (contradiction).
func (p *buildContentProof) record(abs, sum string) {
	if p == nil {
		return
	}
	rel, ok := p.key(abs)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off {
		return
	}
	prior, seen := p.parsed[rel]
	switch {
	case !seen:
		p.parsed[rel] = parsedContent{sha256: sum}
	case prior.conflict || prior.sha256 == sum:
	case prior.sha256 != "" && sum != "":
		p.parsed[rel] = parsedContent{conflict: true}
	default:
		// One of the reads is unknown: the file stays read, unproven.
		p.parsed[rel] = parsedContent{}
	}
}

// contradiction is the first read file (in path order) whose recorded bytes
// are not the sample's content for it, or whose reads disagreed: the payload
// is not the sample's, whatever the working copy holds by now. Empty when
// the proof is off or refutes nothing.
func (p *buildContentProof) contradiction() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off {
		return ""
	}
	var out []string
	for rel, parsed := range p.parsed {
		want, sampled := p.sample[rel]
		if parsed.conflict || (sampled && parsed.sha256 != "" && parsed.sha256 != want) {
			out = append(out, rel)
		}
	}
	for rel, seen := range p.probed {
		want, sampled := p.sample[rel]
		if seen.conflict || (sampled && seen.found != (want != absentContent)) {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		return ""
	}
	sort.Strings(out)
	return out[0]
}

// readMoved is the first path whose recorded read the working copy refutes
// now, among the paths the sample does not decide (contradiction judges
// those): bytes, or an absence, other than what the path holds, or a probe
// answered otherwise (gitstate.ConfirmReadsHold). The fence asks it after a
// full sample found the working copy in the sampled state: such a read was
// taken while the path held another state and restored before that sample,
// so the payload describes a state that never existed. Empty when the proof
// is off or every read holds.
func (p *buildContentProof) readMoved(ctx context.Context, sampler *gitstate.DirtySampler, before gitstate.DirtySnapshot) (string, error) {
	if p == nil {
		return "", nil
	}
	p.mu.Lock()
	if p.off {
		p.mu.Unlock()
		return "", nil
	}
	reads := make(map[string]string, len(p.parsed))
	for rel, parsed := range p.parsed {
		if _, sampled := p.sample[rel]; !sampled && parsed.sha256 != "" && !parsed.conflict {
			reads[rel] = parsed.sha256
		}
	}
	probes := make([]string, 0, len(p.probed))
	probed := make(map[string]probeSeen, len(p.probed))
	for rel, seen := range p.probed {
		if _, sampled := p.sample[rel]; !sampled {
			probes = append(probes, rel)
			probed[rel] = seen
		}
	}
	p.mu.Unlock()
	sort.Strings(probes)
	var moved []string
	for _, rel := range probes {
		if seen := probed[rel]; seen.conflict || !probeHolds(p.abs(rel), seen) {
			moved = append(moved, rel)
			break
		}
	}
	var (
		read string
		err  error
	)
	if sampler != nil {
		read, err = sampler.ConfirmReadsHold(ctx, before, reads)
	} else {
		read, err = gitstate.ConfirmReadsHoldAt(ctx, p.root, reads)
	}
	if err != nil {
		return "", err
	}
	if read != "" {
		moved = append(moved, read)
	}
	if len(moved) == 0 {
		return "", nil
	}
	sort.Strings(moved)
	return moved[0], nil
}

// unrecordedMoved is the first read of the build that the proof holds no
// bytes, absence or existence answer to judge by and that moved since the
// build's own sample began; the fence asks it with readMoved, after a full
// sample found the working copy in the sampled state. Such a read, taken
// while its path held another state restored since, built a payload of a
// state that never existed, and only what the working copy answers now can
// still show it:
//
//   - a read of unknown bytes (a prefix read, a manifest read that failed
//     part way, two reads that disagreed), a manifest or ignore file read
//     with bytes (claimedByName: readers beyond the proof take those too),
//     and a changed or deleted path of the plan the build holds no read of
//     (a gate dropped it) are held to their change stamps
//     (gitstate.ConfirmReadSetSettled);
//   - a manifest-tree answer (a probe, a glob, the top-level listing) must be
//     what the working copy answers now (noteAnswer): the reader took the
//     answer the state it describes gives.
//
// A path nothing read — an absent manifest the read set claims by name only,
// so the read set's own confirmation can prove it by stamps — is not held:
// its stamp, or its parent's, moves with every save beside it. Empty when
// the proof is off, when every such read held, and when stamps cannot
// decide (no sampler, a sample this sampler did not take, a filesystem
// without change stamps): the full sample alone decides then, as it does for
// a build without a proof. The legacy contract path (an unscoped proof that
// is not unbounded) keeps that stage-one fence too: its semantic pass reads
// the read files' directories whole, beyond what the proof records.
func (p *buildContentProof) unrecordedMoved(ctx context.Context, sampler *gitstate.DirtySampler, before gitstate.DirtySnapshot) (string, error) {
	if p == nil {
		return "", nil
	}
	p.mu.Lock()
	if p.off || (p.unscoped && !p.unbounded) {
		p.mu.Unlock()
		return "", nil
	}
	judged := func(rel string) bool {
		if parsed, read := p.parsed[rel]; read {
			return parsed.sha256 != "" && !parsed.conflict &&
				(parsed.sha256 == absentContent || !claimedByName(rel))
		}
		seen, probed := p.probed[rel]
		return probed && !seen.conflict
	}
	var files []string
	for rel := range p.parsed {
		if !judged(rel) {
			files = append(files, rel)
		}
	}
	for _, rel := range p.plan {
		if !judged(rel) {
			files = append(files, rel)
		}
	}
	answers := append([]recordedAnswer(nil), p.answers...)
	p.mu.Unlock()
	for _, answer := range answers {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !answer.holds() {
			return answer.what, nil
		}
	}
	if sampler == nil {
		return "", nil
	}
	moved, proven, err := sampler.ConfirmReadSetSettled(ctx, before, files, nil, false, nil)
	if err != nil || !proven {
		return "", err
	}
	return moved, nil
}

// recordedAnswer is one manifest-tree answer a reader of the build took:
// what was asked, and whether the working copy gives the same answer now.
type recordedAnswer struct {
	what  string
	holds func() bool
}

// noteAnswer records an answer a reader took from the working copy beyond a
// whole-file read: holds re-asks the question and reports whether the answer
// is unchanged.
func (p *buildContentProof) noteAnswer(what string, holds func() bool) {
	if !p.active() {
		return
	}
	p.mu.Lock()
	if !p.off {
		p.answers = append(p.answers, recordedAnswer{what: what, holds: holds})
	}
	p.mu.Unlock()
}

// readSet is the read set the fence confirms a delta that proves its reads
// against (withBuildReadSet): the change set and every other file the build
// read (re-derived dependents, importers, scanned emitters, manifests and
// sources read whole among them), the directories a reader listed or probed,
// and — by name, at every directory those lie under — the manifests and
// ignore files. The directories of the read files are claimed whole only
// once an unscoped reader ran: without one nothing else listed a directory,
// so a file saved beside the change set is the next build's change, not a
// tear of this one.
func (p *buildContentProof) readSet(ctx context.Context, indexed, deleted []string) context.Context {
	if !p.active() {
		return withBuildReadSet(ctx, indexed, nil, deleted)
	}
	p.mu.Lock()
	parsed := make([]string, 0, len(p.parsed)+len(p.probed))
	for rel := range p.parsed {
		parsed = append(parsed, rel)
	}
	for rel := range p.probed {
		parsed = append(parsed, rel)
	}
	claimed := make([]string, 0, len(p.claimed))
	for rel := range p.claimed {
		claimed = append(claimed, rel)
	}
	p.plan = appendUniqueSorted(append([]string(nil), indexed...), deleted...)
	unscoped := p.unscoped
	p.mu.Unlock()
	files := appendUniqueSorted(append([]string(nil), indexed...), parsed...)
	var alone []string
	if !unscoped {
		alone = files
	}
	return withBuildReadSetDirs(ctx, files, alone, deleted, claimed)
}

// proven is the content identity of every read file the proof covers, keyed
// by repository-relative slash path; nil when the proof is off or an unscoped
// reader ran, so the fence confirms by change stamps alone.
func (p *buildContentProof) proven() map[string]string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off || p.unscoped {
		return nil
	}
	out := make(map[string]string, len(p.parsed))
	for rel, parsed := range p.parsed {
		if parsed.sha256 != "" && parsed.sha256 != absentContent && !claimedByName(rel) {
			out[rel] = parsed.sha256
		}
	}
	return out
}

// holdAliasConfigs records, for a build whose tsconfig / jsconfig alias scopes
// came from the process-wide cache (tsAliasCollectionForTree), every config
// file a cached scope was parsed from as a read of unknown bytes: the build
// read none of them, but its payload is built on what they said, so each is
// held to its change stamp (unrecordedMoved, and the read set's own
// confirmation). Only a config present now is held: an absent one is
// decided by its directory's stamp, which every save beside it moves.
func (p *buildContentProof) holdAliasConfigs(root string, coll *tsalias.Collection) {
	if !p.active() {
		return
	}
	for _, scope := range coll.Maps() {
		for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
			abs := filepath.Join(root, filepath.FromSlash(scope.DirPrefix), name)
			if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
				p.record(abs, "")
			}
		}
	}
}

// claimedByName reports whether rel is a file the read set claims by name at
// every directory a read file lies under: a module or package manifest, a
// per-directory ignore file. Readers beyond the proof take them too (the walk
// gate's ignore matcher, the project-name detection's root manifests), so a
// recorded read of one does not stand for every read of it: it is proven by
// its change stamp, never by the bytes one reader recorded.
func claimedByName(rel string) bool {
	base := path.Base(rel)
	for _, name := range buildReadSetManifests {
		if base == name {
			return true
		}
	}
	for _, name := range dirIgnoreFiles {
		if base == name {
			return true
		}
	}
	return false
}

// provenManifestTree is the working-copy manifest tree of a build that proves
// its reads (Indexer.manifestTreeWithRef): it answers exactly as
// diskManifestTree does and puts every answer in the proof — a file read with
// the content identity of its bytes (or as absent), a probe by its parent
// directory, a glob by every directory it lists or probes, and the top-level
// listing by the root and each directory it reported.
type provenManifestTree struct {
	diskManifestTree
	proof *buildContentProof
}

var _ manifestTree = provenManifestTree{}

func (t provenManifestTree) readFile(rel string) ([]byte, bool) {
	data, ok := t.diskManifestTree.readFile(rel)
	if t.rootPath == "" || rel == "" {
		return data, ok
	}
	abs := joinPath(t.rootPath, rel)
	if ok {
		t.proof.recordRead(abs, data, nil)
		return data, ok
	}
	_, err := os.Lstat(abs)
	if err == nil {
		err = errUnreadManifest
	}
	t.proof.recordRead(abs, nil, err)
	return data, ok
}

// errUnreadManifest is a manifest-tree read that failed although the file
// exists: recorded as a read of unknown bytes.
var errUnreadManifest = errors.New("manifest present but not read")

func (t provenManifestTree) isFile(rel string) bool {
	t.claimParent(rel)
	answer := t.diskManifestTree.isFile(rel)
	t.proof.noteAnswer("is "+rel+" a file", func() bool { return t.diskManifestTree.isFile(rel) == answer })
	return answer
}

func (t provenManifestTree) isDir(rel string) bool {
	t.claimParent(rel)
	answer := t.diskManifestTree.isDir(rel)
	t.proof.noteAnswer("is "+rel+" a directory", func() bool { return t.diskManifestTree.isDir(rel) == answer })
	return answer
}

func (t provenManifestTree) matchFiles(glob string) []string {
	t.claimGlob(glob)
	answer := t.diskManifestTree.matchFiles(glob)
	t.proof.noteAnswer("files matching "+glob, func() bool { return slices.Equal(t.diskManifestTree.matchFiles(glob), answer) })
	return answer
}

func (t provenManifestTree) matchDirs(glob string) []string {
	t.claimGlob(glob)
	answer := t.diskManifestTree.matchDirs(glob)
	t.proof.noteAnswer("directories matching "+glob, func() bool { return slices.Equal(t.diskManifestTree.matchDirs(glob), answer) })
	return answer
}

func (t provenManifestTree) topLevelDirs(exts ...string) map[string]bool {
	out := t.diskManifestTree.topLevelDirs(exts...)
	if t.rootPath == "" {
		return out
	}
	t.proof.claimDir(t.rootPath)
	for name := range out {
		t.proof.claimDir(filepath.Join(t.rootPath, name))
	}
	answer := maps.Clone(out)
	t.proof.noteAnswer("the top-level directories", func() bool { return maps.Equal(t.diskManifestTree.topLevelDirs(exts...), answer) })
	return out
}

// claimParent claims the directory that decides whether rel exists and what
// it is.
func (t provenManifestTree) claimParent(rel string) {
	if t.rootPath == "" || rel == "" {
		return
	}
	t.proof.claimDir(filepath.Dir(filepath.Join(t.rootPath, filepath.FromSlash(rel))))
}

// claimGlob claims every directory filepath.Glob may list or probe to expand
// glob: each directory along a literal prefix and every directory a wildcard
// segment matches on the way down. The claim is taken from the directories as
// they are now; a directory that matched differently when Glob ran moved a
// claimed ancestor's stamp, which the fence refuses.
func (t provenManifestTree) claimGlob(glob string) {
	if t.rootPath == "" || glob == "" {
		return
	}
	dirs := []string{t.rootPath}
	segments := strings.Split(path.Clean(glob), "/")
	for i, segment := range segments {
		var next []string
		for _, dir := range dirs {
			t.proof.claimDir(dir)
			if i == len(segments)-1 {
				continue
			}
			if !strings.ContainsAny(segment, `*?[\`) {
				next = append(next, filepath.Join(dir, segment))
				continue
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if matched, _ := path.Match(segment, entry.Name()); !matched {
					continue
				}
				candidate := filepath.Join(dir, entry.Name())
				if info, err := os.Stat(candidate); err == nil && info.IsDir() {
					next = append(next, candidate)
				}
			}
		}
		dirs = next
	}
}
