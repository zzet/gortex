package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// The admitted-input manifest of a working-tree generation, and the pure
// planner that diffs two of them.
//
// A dirty generation's manifest records, per path the checkout sample reported,
// what the sample found there (present with a mode and content hash, absent,
// opaque, or equal to HEAD) and the builder's admission verdict for it. A
// chain root carries the full manifest; a child over a dirty parent carries
// only the paths whose entry differs from the parent's resolved manifest. The
// next build diffs its own sample against the parent's resolved manifest and
// plans only the paths whose entry changed — that is what planDelta computes.
// Nothing here reads the store or the disk: the callers hand in samples,
// stored manifests and the HEAD predicate.

// Fallback reason codes. Each one names why a build over a dirty parent is not
// possible and the build must run direct over the commit generation instead.
// They are reported, never hidden: a fallback is correct but is not an
// incremental success.
const (
	// dirtyChainFallbackCleanCheckout: the sample has no dirty entries. The
	// route takes an empty direct generation; an empty child is never built.
	dirtyChainFallbackCleanCheckout = "clean_checkout"
	// dirtyChainFallbackNoParent: the route has no servable dirty generation,
	// or it fails the ancestry-root predicate.
	dirtyChainFallbackNoParent = "no_parent"
	// dirtyChainFallbackParentManifestMissing: the parent, or a hop of its
	// chain, has no complete manifest of a supported version.
	dirtyChainFallbackParentManifestMissing = "parent_manifest_missing"
	// dirtyChainFallbackPolicyChanged: a policy-identity column or the
	// manifest policy digest differs from the parent's.
	dirtyChainFallbackPolicyChanged = "policy_changed"
	// dirtyChainFallbackDependencyManifestChanged: a path in the diff is a
	// module/build manifest or an ignore/admission-policy input.
	dirtyChainFallbackDependencyManifestChanged = "dependency_manifest_changed"
	// dirtyChainFallbackHeadOrBaseMoved: the route's commit slot is not the
	// parent chain's terminal.
	dirtyChainFallbackHeadOrBaseMoved = "head_or_base_moved"
	// dirtyChainFallbackChainDepthExhausted: the parent already sits at the
	// chain depth bound.
	dirtyChainFallbackChainDepthExhausted = "chain_depth_exhausted"
	// dirtyChainFallbackDeltaNotSmaller: the diff is larger than
	// dirtyChainSmallDelta and no smaller than the sample's dirty set, so
	// rebuilding from the commit is no more work.
	dirtyChainFallbackDeltaNotSmaller = "delta_not_smaller"
	// dirtyChainFallbackClosureTruncatedParent: the parent was published with
	// a truncated closure; nothing is stacked on a knowingly incomplete view.
	dirtyChainFallbackClosureTruncatedParent = "closure_truncated_parent"
	// dirtyChainFallbackSymlinkOrSubmoduleChanged: an opaque entry, a symlink
	// or a gitlink changed; the direct path handles those conservatively.
	dirtyChainFallbackSymlinkOrSubmoduleChanged = "symlink_or_submodule_changed"
	// dirtyChainFallbackFoldUnverified: an inline fold at the chain cap did not
	// reproduce its chain when verified in the background; the next build
	// stands on nothing the chain made.
	dirtyChainFallbackFoldUnverified = "fold_unverified"
)

// dirtyChainFallbackReasons lists every fallback code, for exhaustive tests
// and report validation.
var dirtyChainFallbackReasons = []string{
	dirtyChainFallbackCleanCheckout,
	dirtyChainFallbackNoParent,
	dirtyChainFallbackParentManifestMissing,
	dirtyChainFallbackPolicyChanged,
	dirtyChainFallbackDependencyManifestChanged,
	dirtyChainFallbackHeadOrBaseMoved,
	dirtyChainFallbackChainDepthExhausted,
	dirtyChainFallbackDeltaNotSmaller,
	dirtyChainFallbackClosureTruncatedParent,
	dirtyChainFallbackSymlinkOrSubmoduleChanged,
	dirtyChainFallbackFoldUnverified,
}

// dirtyManifestPolicyTag versions the policy digest's own encoding.
const dirtyManifestPolicyTag = "gortex.indexer.dirty.manifest.policy.v1"

// Git octal modes that are not plain file content.
const (
	manifestSymlinkMode = "120000"
	manifestGitlinkMode = "160000"
)

// manifestAdmitFunc is the builder's admission verdict for one present path.
type manifestAdmitFunc func(path string) store_sqlite.InputManifestAdmission

// admittedManifest turns one checkout sample into a full admitted-input
// manifest: one entry per distinct path the sample reported, sorted, with the
// sample's own content identity and the builder's admission verdict.
//
// admit is consulted only for paths with bytes on disk (present, and
// present-and-HEAD-equal); an absent or opaque path has nothing to admit and is
// recorded not_applicable. A nil admit records every present path admitted.
// policy is the build's policy digest (dirtyManifestPolicyDigest).
func admittedManifest(
	snap gitstate.DirtySnapshot,
	admit manifestAdmitFunc,
	policy string,
) (store_sqlite.InputManifestMeta, []store_sqlite.InputManifestEntry) {
	entries := make([]store_sqlite.InputManifestEntry, 0, len(snap.Contents))
	seen := make(map[string]struct{}, len(snap.Contents))
	for _, content := range snap.Contents {
		if content.Path == "" {
			continue
		}
		clean := path.Clean(filepath.ToSlash(content.Path))
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		entry := store_sqlite.InputManifestEntry{
			FilePath:  clean,
			Admission: store_sqlite.InputManifestNotApplicable,
		}
		switch content.State {
		case gitstate.DirtyContentPresent:
			entry.State = store_sqlite.InputManifestPresent
			if content.HeadEqual {
				entry.State = store_sqlite.InputManifestHeadEqual
			}
			entry.Mode = content.Mode
			entry.ContentSHA256 = content.SHA256
			entry.Admission = store_sqlite.InputManifestAdmitted
			if admit != nil {
				entry.Admission = admit(clean)
			}
		case gitstate.DirtyContentAbsent:
			entry.State = store_sqlite.InputManifestAbsent
			if content.HeadEqual {
				// Absent on disk and absent from HEAD: nothing differs.
				entry.State = store_sqlite.InputManifestHeadEqual
			}
		case gitstate.DirtyContentOpaque:
			entry.State = store_sqlite.InputManifestOpaque
			entry.ContentSHA256 = content.SHA256
		default:
			// An unknown sampler state must never be read as "unchanged".
			entry.State = store_sqlite.InputManifestOpaque
			entry.ContentSHA256 = "unknown-state:" + string(content.State)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].FilePath < entries[j].FilePath })
	return store_sqlite.InputManifestMeta{
		ManifestVersion: store_sqlite.InputManifestVersion,
		IsFull:          true,
		EntryCount:      len(entries),
		PolicyDigest:    policy,
	}, entries
}

// resolvedManifest is a chain's manifest folded newest-wins into one map.
//
// Only paths whose working-tree state differs from HEAD are kept: an entry that
// equals HEAD (head_equal, or a path that left the sample) is the committed
// state, and the committed state is what a path absent from the map means.
type resolvedManifest struct {
	// policy is the policy digest every link was admitted under.
	policy string
	// entries maps canonical path to its dirty entry.
	entries map[string]store_sqlite.InputManifestEntry
}

// dirtyCount is the size of the dirty set the manifest describes: the number
// of paths a direct build over the commit would plan.
func (m resolvedManifest) dirtyCount() int { return len(m.entries) }

// entry returns p's dirty entry, or false when p is in its committed state.
func (m resolvedManifest) entry(p string) (store_sqlite.InputManifestEntry, bool) {
	e, ok := m.entries[p]
	return e, ok
}

// resolveChainManifest folds manifests oldest-first into one resolved map; a
// later entry for a path replaces an earlier one (newest wins). A head_equal
// entry returns its path to the committed state.
func resolveChainManifest(entries ...[]store_sqlite.InputManifestEntry) resolvedManifest {
	out := resolvedManifest{entries: make(map[string]store_sqlite.InputManifestEntry)}
	for _, link := range entries {
		for _, e := range link {
			if e.State == store_sqlite.InputManifestHeadEqual {
				delete(out.entries, e.FilePath)
				continue
			}
			out.entries[e.FilePath] = e
		}
	}
	return out
}

// chainManifestLink is one generation's stored manifest as InputManifest
// returned it.
type chainManifestLink struct {
	Meta    store_sqlite.InputManifestMeta
	Entries []store_sqlite.InputManifestEntry
	// OK is InputManifest's bool: false when the generation has no manifest.
	OK bool
}

// resolveChainLinks resolves a physical chain's manifests, oldest (the root,
// which must carry a full manifest) first. It refuses with
// parent_manifest_missing when any link has no manifest, an unsupported
// version, or when the chain does not start from a full manifest, and with
// policy_changed when the links disagree on their policy digest. A full link
// in the middle restarts the fold, since it describes the whole state itself.
func resolveChainLinks(links ...chainManifestLink) (resolvedManifest, string) {
	if len(links) == 0 {
		return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
	}
	policy := ""
	start := -1
	for i, link := range links {
		if !link.OK || link.Meta.ManifestVersion != store_sqlite.InputManifestVersion || link.Meta.PolicyDigest == "" {
			return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
		}
		if link.Meta.EntryCount != len(link.Entries) {
			return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
		}
		if i == 0 {
			policy = link.Meta.PolicyDigest
		} else if link.Meta.PolicyDigest != policy {
			return resolvedManifest{}, dirtyChainFallbackPolicyChanged
		}
		if link.Meta.IsFull {
			start = i
		}
	}
	if start < 0 {
		return resolvedManifest{}, dirtyChainFallbackParentManifestMissing
	}
	folded := make([][]store_sqlite.InputManifestEntry, 0, len(links)-start)
	for _, link := range links[start:] {
		folded = append(folded, link.Entries)
	}
	out := resolveChainManifest(folded...)
	out.policy = policy
	return out, ""
}

// resolvedFromFull resolves one full manifest (a build's own sample).
func resolvedFromFull(meta store_sqlite.InputManifestMeta, entries []store_sqlite.InputManifestEntry) resolvedManifest {
	out := resolveChainManifest(entries)
	out.policy = meta.PolicyDigest
	return out
}

// manifestEntriesEqual compares the four fields that make two entries the same
// admitted input.
func manifestEntriesEqual(a, b store_sqlite.InputManifestEntry) bool {
	return a.State == b.State && a.Mode == b.Mode && a.ContentSHA256 == b.ContentSHA256 && a.Admission == b.Admission
}

// manifestDeltaEntries is the delta manifest a child over parent writes to
// describe next: every path whose resolved entry differs, with a path that
// returned to its committed state written head_equal. Resolving the parent's
// chain plus this delta yields next exactly.
func manifestDeltaEntries(parent, next resolvedManifest) []store_sqlite.InputManifestEntry {
	var out []store_sqlite.InputManifestEntry
	for p, n := range next.entries {
		if pe, ok := parent.entries[p]; ok && manifestEntriesEqual(pe, n) {
			continue
		}
		out = append(out, n)
	}
	for p := range parent.entries {
		if _, ok := next.entries[p]; ok {
			continue
		}
		out = append(out, store_sqlite.InputManifestEntry{
			FilePath:  p,
			State:     store_sqlite.InputManifestHeadEqual,
			Admission: store_sqlite.InputManifestNotApplicable,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FilePath < out[j].FilePath })
	return out
}

// planDelta derives the child's change set from its parent's resolved
// manifest and its own. headHolds reports whether the commit (HEAD tree) holds
// a path; it is consulted only for paths whose resolved state is committed on
// one side.
//
// Per path p in parent ∪ next, with "committed" meaning absent from a resolved
// map (head_equal, or not reported by the sample):
//
//	parent            next                         change
//	equal             equal                        none (reused)
//	any               present, differs             modified if the parent view holds p, else added
//	dirty             committed, HEAD holds p      modified (partial undo: committed bytes re-read from disk)
//	dirty present     committed, HEAD lacks p      deleted (undo of a dirty add)
//	present/committed absent (deleted on disk)     deleted when the parent view holds p
//
// A rename arrives as its two halves (delete of the old path, add of the new).
// The parent view holds p when p is dirty-present in the parent, or committed
// there and HEAD holds it.
//
// A non-empty reason means the delta must not be used and the build falls
// back direct: policy_changed, dependency_manifest_changed,
// symlink_or_submodule_changed, or delta_not_smaller (a change set of more
// than dirtyChainSmallDelta paths that is no smaller than the sample's dirty
// set, or any change over a parent that describes a clean working tree). clean_checkout is returned when next
// has no dirty entries at all. An empty change set with no reason means next
// resolves to exactly the parent's state.
func planDelta(parent, next resolvedManifest, headHolds func(string) bool) ([]LayerPathChange, string) {
	if parent.policy != next.policy {
		return nil, dirtyChainFallbackPolicyChanged
	}
	if next.dirtyCount() == 0 {
		return nil, dirtyChainFallbackCleanCheckout
	}
	holds := func(p string) bool { return headHolds != nil && headHolds(p) }
	paths := make(map[string]struct{}, len(parent.entries)+len(next.entries))
	for p := range parent.entries {
		paths[p] = struct{}{}
	}
	for p := range next.entries {
		paths[p] = struct{}{}
	}
	var changes []LayerPathChange
	reason := ""
	for p := range paths {
		pe, pDirty := parent.entry(p)
		ne, nDirty := next.entry(p)
		if pDirty && nDirty && manifestEntriesEqual(pe, ne) {
			continue
		}
		if !pDirty && !nDirty {
			continue
		}
		// Any path that changes is in the diff; classify the fallbacks first.
		if manifestEntryOpaque(pe, pDirty) || manifestEntryOpaque(ne, nDirty) {
			reason = worseChainReason(reason, dirtyChainFallbackSymlinkOrSubmoduleChanged)
		}
		if dependencyManifestPath(p) {
			reason = worseChainReason(reason, dirtyChainFallbackDependencyManifestChanged)
		}
		parentViewHolds := (pDirty && pe.State == store_sqlite.InputManifestPresent) || (!pDirty && holds(p))
		var kind LayerChangeKind
		switch {
		case nDirty && ne.State == store_sqlite.InputManifestPresent:
			kind = LayerPathAdded
			if parentViewHolds {
				kind = LayerPathModified
			}
		case nDirty && ne.State == store_sqlite.InputManifestAbsent:
			if !parentViewHolds {
				continue
			}
			kind = LayerPathDeleted
		case nDirty:
			// Opaque on the next side; already a fallback above.
			continue
		default: // next is committed, parent was dirty
			if holds(p) {
				kind = LayerPathModified
			} else {
				if !parentViewHolds {
					continue
				}
				kind = LayerPathDeleted
			}
		}
		changes = append(changes, LayerPathChange{Path: p, Kind: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if reason != "" {
		return nil, reason
	}
	// The delta is judged by its own size. An edit of at most
	// dirtyChainSmallDelta paths always chains, however small the dirty set
	// it lands in: a one-file dirty set edited again is one file over the
	// previous generation, not a direct rebuild of the same file over the
	// commit (which forfeits the parent's resolved rows and the reuse paths).
	// Only a large delta that is no smaller than the direct build it would
	// replace goes direct.
	if len(changes) > dirtyChainSmallDelta && len(changes) >= next.dirtyCount() {
		return nil, dirtyChainFallbackDeltaNotSmaller
	}
	// A parent that describes a clean working tree carries nothing to reuse:
	// the delta over it IS the direct build, one generation shallower.
	if len(changes) > 0 && parent.dirtyCount() == 0 {
		return nil, dirtyChainFallbackDeltaNotSmaller
	}
	return changes, ""
}

// dirtyChainSmallDelta is the largest delta that always chains over the
// previous working-tree generation. A variable only so a fixture can reach
// the large-delta rule with a few files.
var dirtyChainSmallDelta = 32

// worseChainReason keeps the first fallback found in a fixed precedence, so a
// diff that trips two rules reports the same code every time regardless of map
// order.
func worseChainReason(current, candidate string) string {
	rank := func(r string) int {
		switch r {
		case dirtyChainFallbackDependencyManifestChanged:
			return 2
		case dirtyChainFallbackSymlinkOrSubmoduleChanged:
			return 1
		default:
			return 0
		}
	}
	if rank(candidate) > rank(current) {
		return candidate
	}
	return current
}

// manifestEntryOpaque reports whether a dirty entry is content the planner
// does not diff: an opaque directory, a symlink or a gitlink.
func manifestEntryOpaque(e store_sqlite.InputManifestEntry, dirty bool) bool {
	if !dirty {
		return false
	}
	return e.State == store_sqlite.InputManifestOpaque || e.Mode == manifestSymlinkMode || e.Mode == manifestGitlinkMode
}

// dependencyManifestPath reports whether p is an input that changes how other
// paths are resolved or admitted rather than just its own payload: a module or
// build manifest the indexer reads (rootManifests, at any depth, since nested
// modules and workspace members read theirs too), a Go checksum/workspace
// file, a TypeScript/JavaScript path-alias config, a compile database, an
// ignore file, or the repository's gortex config. A change to any of them
// sends the build direct.
func dependencyManifestPath(p string) bool {
	base := path.Base(p)
	for _, m := range rootManifests() {
		if base == path.Base(m.path) {
			return true
		}
	}
	for _, name := range dirIgnoreFiles {
		if base == name {
			return true
		}
	}
	switch base {
	case "go.mod", "go.sum", "go.work", "go.work.sum",
		"composer.lock", "compile_commands.json",
		".gitignore", ".gitattributes", ".gitmodules",
		".gortex.yaml", ".gortex.yml":
		return true
	}
	if strings.HasSuffix(base, ".json") && (strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, "jsconfig")) {
		return true
	}
	return false
}

// dirtyManifestPolicyDigest is the policy half of a manifest's identity: every
// input that decides how a path's bytes become payload without being a path
// of the sample itself. Two manifests are diffable only when their digests are
// equal.
//
// It covers the parser/extractor policy (ExtractorVersions, ConfigHash), the
// resolver version, the dependency read-set (DependencyRevision, plus the HEAD
// tree, which names every committed module/build manifest's bytes — dirty
// manifests are sample paths and trip dependency_manifest_changed instead),
// the producers the build declares (embedder presence and dimensions, whether
// semantic enrichment is enabled and which providers serve which languages),
// and the Go toolchain the build runs under.
func (b *SparseGenerationBuilder) dirtyManifestPolicyDigest(identity GenerationIdentity) string {
	var parts []string
	add := func(k, v string) { parts = append(parts, k+"="+v) }
	add("tag", dirtyManifestPolicyTag)
	add("config", identity.ConfigHash)
	add("extractors", identity.ExtractorVersions)
	add("resolver", identity.ResolverVersion)
	add("dependencies", identity.DependencyRevision)
	add("head_tree", identity.TreeOID)
	add("go", runtime.Version()+"/"+runtime.GOOS+"/"+runtime.GOARCH)
	add("goflags", os.Getenv("GOFLAGS"))
	embedder := "none"
	if b != nil && b.Embedder != nil {
		embedder = fmt.Sprintf("%T/%d", b.Embedder, b.Embedder.Dimensions())
	}
	add("embedder", embedder)
	semantic := "none"
	if b != nil && b.Semantic != nil {
		var providers []string
		for _, p := range b.Semantic.AllProviders() {
			langs := append([]string(nil), p.Languages()...)
			sort.Strings(langs)
			providers = append(providers, p.Name()+"("+strings.Join(langs, ",")+")")
		}
		sort.Strings(providers)
		semantic = strconv.FormatBool(b.Semantic.Enabled()) + ":" + strings.Join(providers, ";")
	}
	add("semantic", semantic)
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(strconv.Itoa(len(part))))
		sum.Write([]byte{':'})
		sum.Write([]byte(part))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// manifestAdmitter returns the builder's own walk admission for the paths of a
// working-tree sample: the same gate (exclude/ignore rules, language
// detection, size cap) runPass's walk over req.Target applies, evaluated for
// one path at a time under the same installed content source, so the verdict
// cannot differ from the one the pass reaches. Paths the target cannot stat
// for a reason other than absence are unreadable.
func (b *SparseGenerationBuilder) manifestAdmitter(root string, target source.ContentSource) manifestAdmitFunc {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	idx := &Indexer{
		registry:   b.Registry,
		config:     b.Config,
		transforms: newTransformPipeline(b.Config.Transforms, b.Registry, b.Logger),
		logger:     b.Logger,
		rootPath:   absRoot,
	}
	idx.setContentSourceWithManifests(target, target)
	return func(p string) store_sqlite.InputManifestAdmission {
		if target == nil {
			return store_sqlite.InputManifestAdmitted
		}
		meta, statErr := target.Stat(p)
		switch {
		case errors.Is(statErr, source.ErrNotInSource):
			return store_sqlite.InputManifestNotApplicable
		case statErr != nil:
			return store_sqlite.InputManifestUnreadable
		}
		adm := idx.admitWalkEntry(absRoot, filepath.Join(absRoot, filepath.FromSlash(p)), meta.Size, false)
		switch {
		case adm.admit:
			return store_sqlite.InputManifestAdmitted
		case adm.excluded:
			return store_sqlite.InputManifestExcluded
		case adm.oversize:
			return store_sqlite.InputManifestOversized
		default:
			return store_sqlite.InputManifestUnknownLanguage
		}
	}
}
