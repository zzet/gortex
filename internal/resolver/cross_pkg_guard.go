package resolver

import (
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Cross-package name-match guard.
//
// The heuristic cascade in resolveFunctionCall / resolveMethodCall ends,
// for calls it can't pin precisely, in a name-only fallback: "the first
// function/method named X in the caller's repo". When the only candidate
// of that name lives in a package the caller never imports, that
// fallback manufactures a false `calls` edge — a JS/TS factory result
// `h.handle()` binding to an unrelated `handle`, or a `ns.foo()`
// namespace call binding to a free `foo` in some other module.
//
// This guard runs once after the main resolution pass. For every edge
// the pass resolved at one of the two weakest confidence tiers
// (text_matched / ast_inferred) it asks a single question: is the
// resolved target import-reachable from the call site? Reachable means
// the target sits in the caller's own directory (same package) or in a
// directory the caller's file imports. When it is not, the edge is
// reverted to its pre-resolution `unresolved::` target so a
// higher-evidence resolver (CrossRepoResolver, or a later LSP-backed
// pass) can have a clean attempt instead of inheriting a wrong binding.
//
// Genuine same-package and imported-target edges are never touched: the
// reachability set always contains the caller's own directory, and an
// imported package contributes its directory to the set. Edges resolved
// at ast_resolved or above are out of scope — those carry structural or
// compiler-grade evidence the name-only fallback never had.

// PendingGuardRevert retains a cross-package guard revert whose reindex or
// dataflow source revert write was refused under writer-gate contention so a
// subsequent resolve pass can finish reverting the weak-tier binding without
// requiring a full file reparse.
type PendingGuardRevert struct {
	RepoPrefix       string
	Reindexes        []graph.EdgeReindex
	SourceReverts    []graph.PlaceholderRepoint
	reindexesApplied bool
	spoolReverts     []lspSpoolRevert
	headDropped      int
}

// ItemCount returns the number of logical guard-revert items retained in p.
func (p PendingGuardRevert) ItemCount() int {
	return max(len(p.Reindexes), max(len(p.SourceReverts), len(p.spoolReverts)))
}

// AppendCoalescedGuardRevert appends entry to dst, coalescing onto an existing
// entry with identical (RepoPrefix, reindexesApplied) whenever their pairing
// invariants match so repeated single-file refusals stay bounded to at most 2
// paired entries per repository. Unpaired synthetic entries stay standalone.
func AppendCoalescedGuardRevert(dst []PendingGuardRevert, entry PendingGuardRevert) []PendingGuardRevert {
	if entry.ItemCount() == 0 {
		return dst
	}
	for i := range dst {
		target := &dst[i]
		if target.RepoPrefix != entry.RepoPrefix || target.reindexesApplied != entry.reindexesApplied {
			continue
		}
		if !entry.reindexesApplied {
			if len(target.Reindexes) == 0 || len(entry.Reindexes) == 0 {
				continue
			}
			srcPaired := len(target.SourceReverts) == len(target.Reindexes) && len(entry.SourceReverts) == len(entry.Reindexes)
			srcEmpty := len(target.SourceReverts) == 0 && len(entry.SourceReverts) == 0
			if !srcPaired && !srcEmpty {
				continue
			}
			spoolPaired := len(target.spoolReverts) == len(target.Reindexes) && len(entry.spoolReverts) == len(entry.Reindexes)
			spoolEmpty := len(target.spoolReverts) == 0 && len(entry.spoolReverts) == 0
			if !spoolPaired && !spoolEmpty {
				continue
			}
			oldReindexes := target.Reindexes
			target.Reindexes = append(target.Reindexes, entry.Reindexes...)
			if len(oldReindexes) == 0 || &target.Reindexes[0] != &oldReindexes[0] {
				target.headDropped = 0
			}
			if srcPaired {
				target.SourceReverts = append(target.SourceReverts, entry.SourceReverts...)
			}
			if spoolPaired {
				target.spoolReverts = append(target.spoolReverts, entry.spoolReverts...)
			}
			return dst
		}
		if len(target.Reindexes) != 0 || len(entry.Reindexes) != 0 {
			continue
		}
		if len(target.SourceReverts) > 0 && len(entry.SourceReverts) > 0 {
			spoolPaired := len(target.spoolReverts) == len(target.SourceReverts) && len(entry.spoolReverts) == len(entry.SourceReverts)
			spoolEmpty := len(target.spoolReverts) == 0 && len(entry.spoolReverts) == 0
			if !spoolPaired && !spoolEmpty {
				continue
			}
			oldSources := target.SourceReverts
			target.SourceReverts = append(target.SourceReverts, entry.SourceReverts...)
			if len(oldSources) == 0 || &target.SourceReverts[0] != &oldSources[0] {
				target.headDropped = 0
			}
			if spoolPaired {
				target.spoolReverts = append(target.spoolReverts, entry.spoolReverts...)
			}
			return dst
		}
		if len(target.SourceReverts) == 0 && len(entry.SourceReverts) == 0 {
			oldSpool := target.spoolReverts
			target.spoolReverts = append(target.spoolReverts, entry.spoolReverts...)
			if len(oldSpool) == 0 || &target.spoolReverts[0] != &oldSpool[0] {
				target.headDropped = 0
			}
			return dst
		}
	}
	return append(dst, entry)
}

// SplitPendingGuardRevertByRepo partitions an untagged PendingGuardRevert into
// per-repository entries using repoForFile, preserving pairing invariants and
// stamping RepoPrefix on every resulting entry and SourceRevert.
func SplitPendingGuardRevertByRepo(entry PendingGuardRevert, repoForFile func(string) string) []PendingGuardRevert {
	if entry.ItemCount() == 0 {
		return nil
	}
	if entry.RepoPrefix != "" || repoForFile == nil {
		if entry.RepoPrefix != "" {
			for i := range entry.SourceReverts {
				if entry.SourceReverts[i].RepoPrefix == "" {
					entry.SourceReverts[i].RepoPrefix = entry.RepoPrefix
				}
			}
		}
		return []PendingGuardRevert{entry}
	}
	byRepo := make(map[string]*PendingGuardRevert)
	var order []string
	bucketFor := func(repo string) *PendingGuardRevert {
		b := byRepo[repo]
		if b == nil {
			b = &PendingGuardRevert{RepoPrefix: repo, reindexesApplied: entry.reindexesApplied}
			byRepo[repo] = b
			order = append(order, repo)
		}
		return b
	}
	if !entry.reindexesApplied && len(entry.Reindexes) > 0 {
		pairedSrc := len(entry.SourceReverts) == len(entry.Reindexes)
		pairedSpool := len(entry.spoolReverts) == len(entry.Reindexes)
		for i, ri := range entry.Reindexes {
			fp := ""
			if ri.Edge != nil {
				fp = ri.Edge.FilePath
			}
			repo := repoForFile(fp)
			b := bucketFor(repo)
			b.Reindexes = append(b.Reindexes, ri)
			if pairedSrc {
				sr := entry.SourceReverts[i]
				if sr.RepoPrefix == "" {
					sr.RepoPrefix = repo
				}
				b.SourceReverts = append(b.SourceReverts, sr)
			}
			if pairedSpool {
				b.spoolReverts = append(b.spoolReverts, entry.spoolReverts[i])
			}
		}
		if !pairedSrc {
			for _, sr := range entry.SourceReverts {
				repo := sr.RepoPrefix
				if repo == "" {
					repo = repoForFile(sr.FilePath)
					sr.RepoPrefix = repo
				}
				b := bucketFor(repo)
				b.SourceReverts = append(b.SourceReverts, sr)
			}
		}
		if !pairedSpool {
			for _, sp := range entry.spoolReverts {
				fp := ""
				if sp.edge != nil {
					fp = sp.edge.FilePath
				}
				b := bucketFor(repoForFile(fp))
				b.spoolReverts = append(b.spoolReverts, sp)
			}
		}
	} else if len(entry.SourceReverts) > 0 {
		pairedSpool := len(entry.spoolReverts) == len(entry.SourceReverts)
		for i, sr := range entry.SourceReverts {
			repo := sr.RepoPrefix
			if repo == "" {
				repo = repoForFile(sr.FilePath)
				sr.RepoPrefix = repo
			}
			b := bucketFor(repo)
			b.reindexesApplied = true
			b.SourceReverts = append(b.SourceReverts, sr)
			if pairedSpool {
				b.spoolReverts = append(b.spoolReverts, entry.spoolReverts[i])
			}
		}
		if !pairedSpool {
			for _, sp := range entry.spoolReverts {
				fp := ""
				if sp.edge != nil {
					fp = sp.edge.FilePath
				}
				b := bucketFor(repoForFile(fp))
				b.reindexesApplied = true
				b.spoolReverts = append(b.spoolReverts, sp)
			}
		}
	} else {
		for _, sp := range entry.spoolReverts {
			fp := ""
			if sp.edge != nil {
				fp = sp.edge.FilePath
			}
			b := bucketFor(repoForFile(fp))
			b.reindexesApplied = true
			b.spoolReverts = append(b.spoolReverts, sp)
		}
	}
	out := make([]PendingGuardRevert, 0, len(order))
	for _, repo := range order {
		out = append(out, *byRepo[repo])
	}
	return out
}

func (r *Resolver) hasPendingGuardReverts() bool {
	return len(r.pendingGuardReverts) > 0
}

func (r *Resolver) appendPendingGuardRevertLocked(entry PendingGuardRevert) {
	if entry.ItemCount() == 0 {
		return
	}
	r.placeholderSrcIdx.guardPruned = false
	r.pendingGuardReverts = AppendCoalescedGuardRevert(r.pendingGuardReverts, entry)
}

func (r *Resolver) pruneDeferredAgainstPendingGuardsLocked() {
	if r.placeholderSrcIdx.guardPruned {
		return
	}
	r.placeholderSrcIdx.pruneSitesMulti(r.pendingGuardReverts)
}

// TakePendingGuardReverts detaches any unapplied guard reverts carried across a
// refused write so coordinated callers (e.g. MultiIndexer) can hand them to the
// retry resolver instance.
func (r *Resolver) TakePendingGuardReverts() []PendingGuardRevert {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pendingGuardReverts) == 0 {
		return nil
	}
	out := r.pendingGuardReverts
	r.pendingGuardReverts = nil
	return out
}

// InheritPendingGuardReverts attaches unapplied guard reverts from a prior
// refused pass on the same store so the next pass completes them.
func (r *Resolver) InheritPendingGuardReverts(reverts []PendingGuardRevert) {
	if len(reverts) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rev := range reverts {
		r.appendPendingGuardRevertLocked(rev)
	}
}

// guardRevertChunkSize stays strictly below SQLite's 5,000-row reindexChunkSize
// so each guard revert batch commits in one atomic transaction paired with its
// exact SourceReverts and spoolReverts.
const (
	guardRevertChunkSize              = 2048
	incrementalFlushGuardRevertBudget = 64
)

func (r *Resolver) flushPendingGuardRevertsLocked(allowedRepo func(string) bool) (int, bool) {
	return r.flushPendingGuardRevertsBoundedLocked(0, allowedRepo, 0)
}

func (r *Resolver) guardEntryScopePartition(
	entry PendingGuardRevert,
	allowedRepo func(string) bool,
	knownSet func() map[string]struct{},
) (eligible PendingGuardRevert, skipped PendingGuardRevert, hasEligible bool) {
	if allowedRepo == nil {
		return entry, PendingGuardRevert{}, entry.ItemCount() > 0
	}
	if entry.RepoPrefix != "" {
		if allowedRepo(entry.RepoPrefix) {
			return entry, PendingGuardRevert{}, entry.ItemCount() > 0
		}
		return PendingGuardRevert{}, entry, false
	}
	known := knownSet()
	if len(known) == 0 {
		if allowedRepo("") {
			return entry, PendingGuardRevert{}, entry.ItemCount() > 0
		}
		return PendingGuardRevert{}, entry, false
	}
	matchFile := func(fp string) bool {
		return allowedRepo(repoForFilePath(fp, known, r.retryRepoPrefix))
	}
	return partitionGuardRevertEntry(entry, matchFile, nil)
}

func partitionGuardRevertEntry(
	entry PendingGuardRevert,
	matchFile func(string) bool,
	matchStubName func(string) bool,
) (matched PendingGuardRevert, unmatched PendingGuardRevert, hasMatched bool) {
	if entry.ItemCount() == 0 {
		return PendingGuardRevert{}, PendingGuardRevert{}, false
	}
	itemMatches := func(fp, sym string) bool {
		if matchFile != nil && matchFile(fp) {
			return true
		}
		if matchStubName != nil && sym != "" && matchStubName(sym) {
			return true
		}
		return false
	}
	if !entry.reindexesApplied && len(entry.Reindexes) > 0 {
		matchCount := 0
		for _, ri := range entry.Reindexes {
			if ri.Edge == nil {
				continue
			}
			if itemMatches(ri.Edge.FilePath, unresolvedBareSymbolName(ri.Edge.To)) {
				matchCount++
			}
		}
		if matchCount == 0 {
			return PendingGuardRevert{}, entry, false
		}
		if matchCount == len(entry.Reindexes) {
			return entry, PendingGuardRevert{}, true
		}
		pairedSrc := len(entry.SourceReverts) == len(entry.Reindexes)
		pairedSpool := len(entry.spoolReverts) == len(entry.Reindexes)
		matched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix}
		unmatched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix}
		var matchedSites map[placeholderSourceSite]struct{}
		if !pairedSrc || !pairedSpool {
			matchedSites = make(map[placeholderSourceSite]struct{}, matchCount)
		}
		for i, ri := range entry.Reindexes {
			isMatch := ri.Edge != nil && itemMatches(ri.Edge.FilePath, unresolvedBareSymbolName(ri.Edge.To))
			if isMatch {
				matched.Reindexes = append(matched.Reindexes, ri)
				if pairedSrc {
					matched.SourceReverts = append(matched.SourceReverts, entry.SourceReverts[i])
				}
				if pairedSpool {
					matched.spoolReverts = append(matched.spoolReverts, entry.spoolReverts[i])
				}
				if matchedSites != nil {
					matchedSites[placeholderSourceSite{from: ri.OldTo, filePath: ri.Edge.FilePath, line: ri.Edge.Line}] = struct{}{}
				}
			} else {
				unmatched.Reindexes = append(unmatched.Reindexes, ri)
				if pairedSrc {
					unmatched.SourceReverts = append(unmatched.SourceReverts, entry.SourceReverts[i])
				}
				if pairedSpool {
					unmatched.spoolReverts = append(unmatched.spoolReverts, entry.spoolReverts[i])
				}
			}
		}
		if !pairedSrc {
			for _, sr := range entry.SourceReverts {
				if _, ok := matchedSites[placeholderSourceSite{from: sr.OldFrom, filePath: sr.FilePath, line: sr.Line}]; ok {
					matched.SourceReverts = append(matched.SourceReverts, sr)
				} else {
					unmatched.SourceReverts = append(unmatched.SourceReverts, sr)
				}
			}
		}
		if !pairedSpool {
			for _, sp := range entry.spoolReverts {
				if sp.edge != nil {
					if _, ok := matchedSites[placeholderSourceSite{from: sp.oldBoundTo, filePath: sp.edge.FilePath, line: sp.edge.Line}]; ok {
						matched.spoolReverts = append(matched.spoolReverts, sp)
						continue
					}
				}
				unmatched.spoolReverts = append(unmatched.spoolReverts, sp)
			}
		}
		return matched, unmatched, true
	}

	if len(entry.SourceReverts) > 0 {
		matchCount := 0
		for _, sr := range entry.SourceReverts {
			if itemMatches(sr.FilePath, unresolvedBareSymbolName(sr.NewFrom)) {
				matchCount++
			}
		}
		if matchCount == 0 {
			return PendingGuardRevert{}, entry, false
		}
		if matchCount == len(entry.SourceReverts) {
			return entry, PendingGuardRevert{}, true
		}
		pairedSpool := len(entry.spoolReverts) == len(entry.SourceReverts)
		matched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix, reindexesApplied: true}
		unmatched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix, reindexesApplied: true}
		var matchedSites map[placeholderSourceSite]struct{}
		if !pairedSpool && len(entry.spoolReverts) > 0 {
			matchedSites = make(map[placeholderSourceSite]struct{}, matchCount)
		}
		for i, sr := range entry.SourceReverts {
			if itemMatches(sr.FilePath, unresolvedBareSymbolName(sr.NewFrom)) {
				matched.SourceReverts = append(matched.SourceReverts, sr)
				if pairedSpool {
					matched.spoolReverts = append(matched.spoolReverts, entry.spoolReverts[i])
				}
				if matchedSites != nil {
					matchedSites[placeholderSourceSite{from: sr.OldFrom, filePath: sr.FilePath, line: sr.Line}] = struct{}{}
				}
			} else {
				unmatched.SourceReverts = append(unmatched.SourceReverts, sr)
				if pairedSpool {
					unmatched.spoolReverts = append(unmatched.spoolReverts, entry.spoolReverts[i])
				}
			}
		}
		if !pairedSpool {
			for _, sp := range entry.spoolReverts {
				if sp.edge != nil {
					if _, ok := matchedSites[placeholderSourceSite{from: sp.oldBoundTo, filePath: sp.edge.FilePath, line: sp.edge.Line}]; ok {
						matched.spoolReverts = append(matched.spoolReverts, sp)
						continue
					}
				}
				unmatched.spoolReverts = append(unmatched.spoolReverts, sp)
			}
		}
		return matched, unmatched, true
	}

	// Only spoolReverts remain.
	matchCount := 0
	for _, sp := range entry.spoolReverts {
		if sp.edge != nil && itemMatches(sp.edge.FilePath, unresolvedBareSymbolName(sp.edge.To)) {
			matchCount++
		}
	}
	if matchCount == 0 {
		return PendingGuardRevert{}, entry, false
	}
	if matchCount == len(entry.spoolReverts) {
		return entry, PendingGuardRevert{}, true
	}
	matched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix, reindexesApplied: true}
	unmatched = PendingGuardRevert{RepoPrefix: entry.RepoPrefix, reindexesApplied: true}
	for _, sp := range entry.spoolReverts {
		if sp.edge != nil && itemMatches(sp.edge.FilePath, unresolvedBareSymbolName(sp.edge.To)) {
			matched.spoolReverts = append(matched.spoolReverts, sp)
		} else {
			unmatched.spoolReverts = append(unmatched.spoolReverts, sp)
		}
	}
	return matched, unmatched, true
}

func (r *Resolver) flushGuardRevertEntryLocked(
	entry PendingGuardRevert,
	maxItems int,
	remainingBudget *int,
	maxWait time.Duration,
) (reverted int, remainders []PendingGuardRevert, ok bool) {
	if entry.ItemCount() == 0 {
		return 0, nil, true
	}
	if !entry.reindexesApplied && len(entry.Reindexes) > 0 {
		take := len(entry.Reindexes)
		if maxItems > 0 && take > *remainingBudget {
			take = *remainingBudget
		}
		if maxItems > 0 {
			*remainingBudget -= take
		}
		paired := len(entry.SourceReverts) == len(entry.Reindexes)
		pairedSpool := len(entry.spoolReverts) == len(entry.Reindexes)
		headReindexes := entry.Reindexes[:take]

		sites := make([]graph.EdgeSite, 0, len(headReindexes))
		for _, ri := range headReindexes {
			if ri.Edge == nil {
				continue
			}
			sites = append(sites, graph.EdgeSite{From: ri.Edge.From, Line: ri.Edge.Line, Kind: ri.Edge.Kind})
		}
		candidates := r.graph.GetEdgeCandidates(nil, sites)
		var live []graph.EdgeReindex
		var liveSources []graph.PlaceholderRepoint
		var liveSpool []lspSpoolRevert
		liveSites := make(map[placeholderSourceSite]struct{}, len(headReindexes))
		for idx, ri := range headReindexes {
			if ri.Edge == nil {
				continue
			}
			for _, cur := range candidates.Site(ri.Edge.From, ri.Edge.Line, ri.Edge.Kind) {
				if cur == nil || cur.To != ri.OldTo || cur.FilePath != ri.Edge.FilePath {
					continue
				}
				// Never clobber an edge that was promoted to LSP or AST-resolved
				// since the weak-tier revert was queued.
				origin := cur.Origin
				if origin == "" {
					origin = graph.DefaultOriginFor(cur.Kind, cur.Confidence, "")
				}
				if origin != graph.OriginTextMatched && origin != graph.OriginASTInferred {
					continue
				}
				live = append(live, ri)
				if paired {
					liveSources = append(liveSources, entry.SourceReverts[idx])
				}
				if pairedSpool {
					liveSpool = append(liveSpool, entry.spoolReverts[idx])
				}
				liveSites[placeholderSourceSite{from: ri.OldTo, filePath: ri.Edge.FilePath, line: ri.Edge.Line}] = struct{}{}
				break
			}
		}

		var tailReindexes []graph.EdgeReindex
		var tailSources []graph.PlaceholderRepoint
		var tailSpool []lspSpoolRevert
		nextHeadDropped := 0
		if take < len(entry.Reindexes) {
			clear(entry.Reindexes[:take])
			tailReindexes = entry.Reindexes[take:]
			nextHeadDropped = entry.headDropped + take
			if paired {
				clear(entry.SourceReverts[:take])
				tailSources = entry.SourceReverts[take:]
			} else if len(live) == 0 {
				tailSources = entry.SourceReverts
			} else {
				for _, sr := range entry.SourceReverts {
					if _, matched := liveSites[placeholderSourceSite{from: sr.OldFrom, filePath: sr.FilePath, line: sr.Line}]; matched {
						liveSources = append(liveSources, sr)
					} else {
						tailSources = append(tailSources, sr)
					}
				}
			}
			if pairedSpool {
				clear(entry.spoolReverts[:take])
				tailSpool = entry.spoolReverts[take:]
			} else if len(live) == 0 {
				tailSpool = entry.spoolReverts
			} else {
				for _, sp := range entry.spoolReverts {
					if sp.edge != nil {
						if _, matched := liveSites[placeholderSourceSite{from: sp.oldBoundTo, filePath: sp.edge.FilePath, line: sp.edge.Line}]; matched {
							liveSpool = append(liveSpool, sp)
							continue
						}
					}
					tailSpool = append(tailSpool, sp)
				}
			}
			if nextHeadDropped >= 256 && nextHeadDropped >= len(tailReindexes) {
				tailReindexes = slices.Clone(tailReindexes)
				if paired {
					tailSources = slices.Clone(tailSources)
				}
				if pairedSpool {
					tailSpool = slices.Clone(tailSpool)
				}
				nextHeadDropped = 0
			}
		} else {
			if !paired && len(live) > 0 {
				for _, sr := range entry.SourceReverts {
					if _, matched := liveSites[placeholderSourceSite{from: sr.OldFrom, filePath: sr.FilePath, line: sr.Line}]; matched {
						liveSources = append(liveSources, sr)
					}
				}
			}
			if !pairedSpool && len(live) > 0 {
				for _, sp := range entry.spoolReverts {
					if sp.edge == nil {
						continue
					}
					if _, matched := liveSites[placeholderSourceSite{from: sp.oldBoundTo, filePath: sp.edge.FilePath, line: sp.edge.Line}]; matched {
						liveSpool = append(liveSpool, sp)
					}
				}
			}
		}

		appendTail := func(dst []PendingGuardRevert) []PendingGuardRevert {
			if len(tailReindexes) == 0 && len(tailSources) == 0 && len(tailSpool) == 0 {
				return dst
			}
			return append(dst, PendingGuardRevert{
				RepoPrefix:       entry.RepoPrefix,
				Reindexes:        tailReindexes,
				SourceReverts:    tailSources,
				reindexesApplied: len(tailReindexes) == 0,
				spoolReverts:     tailSpool,
				headDropped:      nextHeadDropped,
			})
		}

		for start := 0; start < len(live); start += guardRevertChunkSize {
			end := min(start+guardRevertChunkSize, len(live))
			if !r.applyReindexEdgesWithWindow(live[start:end], maxWait) {
				rem := PendingGuardRevert{
					RepoPrefix: entry.RepoPrefix,
					Reindexes:  slices.Clip(live[start:]),
				}
				if len(liveSources) == len(live) {
					rem.SourceReverts = slices.Clip(liveSources[start:])
				} else {
					rem.SourceReverts = slices.Clip(liveSources)
				}
				if len(liveSpool) == len(live) {
					rem.spoolReverts = slices.Clip(liveSpool[start:])
				} else {
					rem.spoolReverts = slices.Clip(liveSpool)
				}
				remainders = append(remainders, rem)
				remainders = appendTail(remainders)
				return reverted, remainders, false
			}
			reverted += end - start
			var chunkSources []graph.PlaceholderRepoint
			if len(liveSources) == len(live) {
				chunkSources = slices.Clip(liveSources[start:end])
			} else if end == len(live) {
				chunkSources = slices.Clip(liveSources)
			}
			var chunkSpool []lspSpoolRevert
			if len(liveSpool) == len(live) {
				chunkSpool = slices.Clip(liveSpool[start:end])
			} else if end == len(live) {
				chunkSpool = slices.Clip(liveSpool)
			}
			if len(chunkSources) > 0 && os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") != "0" {
				if _, applied := graph.ReconcilePlaceholderSourcesTryApplied(r.graph, chunkSources, maxWait); !applied {
					r.notePassWriteRefused()
					remainders = append(remainders, PendingGuardRevert{
						RepoPrefix:       entry.RepoPrefix,
						SourceReverts:    chunkSources,
						spoolReverts:     chunkSpool,
						reindexesApplied: true,
					})
					if end < len(live) {
						rem := PendingGuardRevert{
							RepoPrefix: entry.RepoPrefix,
							Reindexes:  slices.Clip(live[end:]),
						}
						if len(liveSources) == len(live) {
							rem.SourceReverts = slices.Clip(liveSources[end:])
						}
						if len(liveSpool) == len(live) {
							rem.spoolReverts = slices.Clip(liveSpool[end:])
						}
						remainders = append(remainders, rem)
					}
					remainders = appendTail(remainders)
					return reverted, remainders, false
				}
			}
			if len(chunkSpool) > 0 && r.lspDeferredSpool != nil {
				if err := r.lspDeferredSpool.refreshRevertedEdges(chunkSpool); err != nil {
					r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
				}
			}
		}
		remainders = appendTail(remainders)
		return reverted, remainders, true
	}

	// reindexesApplied == true (or !reindexesApplied with len(Reindexes) == 0).
	if len(entry.SourceReverts) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		if len(entry.spoolReverts) > 0 && r.lspDeferredSpool != nil {
			if err := r.lspDeferredSpool.refreshRevertedEdges(entry.spoolReverts); err != nil {
				r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
			}
		}
		return 0, nil, true
	}

	take := len(entry.SourceReverts)
	if maxItems > 0 && take > *remainingBudget {
		take = *remainingBudget
	}
	if maxItems > 0 {
		*remainingBudget -= take
	}
	pairedSpool := len(entry.spoolReverts) == len(entry.SourceReverts)
	if _, applied := graph.ReconcilePlaceholderSourcesTryApplied(r.graph, entry.SourceReverts[:take], maxWait); !applied {
		r.notePassWriteRefused()
		entry.reindexesApplied = true
		return 0, []PendingGuardRevert{entry}, false
	}
	var tailSpool []lspSpoolRevert
	if pairedSpool {
		if r.lspDeferredSpool != nil {
			if err := r.lspDeferredSpool.refreshRevertedEdges(entry.spoolReverts[:take]); err != nil {
				r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
			}
		}
		clear(entry.spoolReverts[:take])
		tailSpool = entry.spoolReverts[take:]
	} else if take == len(entry.SourceReverts) {
		if len(entry.spoolReverts) > 0 && r.lspDeferredSpool != nil {
			if err := r.lspDeferredSpool.refreshRevertedEdges(entry.spoolReverts); err != nil {
				r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
			}
		}
	} else {
		tailSpool = entry.spoolReverts
	}
	clear(entry.SourceReverts[:take])
	tailSources := entry.SourceReverts[take:]
	if len(tailSources) == 0 {
		return 0, nil, true
	}
	nextHeadDropped := entry.headDropped + take
	if nextHeadDropped >= 256 && nextHeadDropped >= len(tailSources) {
		tailSources = slices.Clone(tailSources)
		if pairedSpool {
			tailSpool = slices.Clone(tailSpool)
		}
		nextHeadDropped = 0
	}
	return 0, []PendingGuardRevert{{
		RepoPrefix:       entry.RepoPrefix,
		SourceReverts:    tailSources,
		spoolReverts:     tailSpool,
		reindexesApplied: true,
		headDropped:      nextHeadDropped,
	}}, true
}

func (r *Resolver) flushFrontierGuardRevertsLocked(
	matchFile func(string) bool,
	matchStubName func(string) bool,
	allowedRepo func(string) bool,
	maxWait time.Duration,
) (int, bool) {
	if len(r.pendingGuardReverts) == 0 {
		return 0, true
	}
	var cachedKnown map[string]struct{}
	knownLoaded := false
	knownSet := func() map[string]struct{} {
		if !knownLoaded {
			cachedKnown = r.knownRepoPrefixesSet()
			knownLoaded = true
		}
		return cachedKnown
	}

	reverted := 0
	var kept []PendingGuardRevert
	for i := range r.pendingGuardReverts {
		entry := r.pendingGuardReverts[i]
		eligible, skipped, hasEligible := r.guardEntryScopePartition(entry, allowedRepo, knownSet)
		if !hasEligible {
			if skipped.ItemCount() > 0 {
				kept = append(kept, skipped)
			}
			continue
		}
		matched, unmatched, hasMatched := partitionGuardRevertEntry(eligible, matchFile, matchStubName)
		if !hasMatched {
			kept = append(kept, entry)
			continue
		}
		if skipped.ItemCount() > 0 {
			kept = append(kept, skipped)
		}
		var unusedBudget int
		n, remainders, ok := r.flushGuardRevertEntryLocked(matched, 0, &unusedBudget, maxWait)
		reverted += n
		if !ok {
			kept = append(kept, remainders...)
			if unmatched.ItemCount() > 0 {
				kept = append(kept, unmatched)
			}
			kept = append(kept, r.pendingGuardReverts[i+1:]...)
			r.pendingGuardReverts = kept
			return reverted, false
		}
		if unmatched.ItemCount() > 0 {
			kept = append(kept, unmatched)
		}
	}
	if len(kept) == 0 {
		r.pendingGuardReverts = nil
	} else {
		r.pendingGuardReverts = kept
	}
	return reverted, true
}

func (r *Resolver) flushPendingGuardRevertsBoundedLocked(
	maxItems int,
	allowedRepo func(string) bool,
	maxWait time.Duration,
) (int, bool) {
	if len(r.pendingGuardReverts) == 0 {
		return 0, true
	}
	var cachedKnown map[string]struct{}
	knownLoaded := false
	knownSet := func() map[string]struct{} {
		if !knownLoaded {
			cachedKnown = r.knownRepoPrefixesSet()
			knownLoaded = true
		}
		return cachedKnown
	}

	reverted := 0
	remainingBudget := maxItems
	var kept []PendingGuardRevert
	for i := range r.pendingGuardReverts {
		if maxItems > 0 && remainingBudget <= 0 {
			kept = append(kept, r.pendingGuardReverts[i:]...)
			break
		}
		entry := r.pendingGuardReverts[i]
		eligible, skipped, hasEligible := r.guardEntryScopePartition(entry, allowedRepo, knownSet)
		if !hasEligible {
			if skipped.ItemCount() > 0 {
				kept = append(kept, skipped)
			}
			continue
		}
		if skipped.ItemCount() > 0 {
			kept = append(kept, skipped)
		}
		n, remainders, ok := r.flushGuardRevertEntryLocked(eligible, maxItems, &remainingBudget, maxWait)
		reverted += n
		if !ok {
			kept = append(kept, remainders...)
			kept = append(kept, r.pendingGuardReverts[i+1:]...)
			r.pendingGuardReverts = kept
			return reverted, false
		}
		kept = append(kept, remainders...)
	}
	if len(kept) == 0 {
		r.pendingGuardReverts = nil
	} else {
		r.pendingGuardReverts = kept
	}
	return reverted, true
}

// drainGuardSpoolToPendingLocked evaluates committed weak-tier jobs already
// spooled during a ResolveAll pass that is aborting early on write refusal.
// It runs read-only (zero store writes) and queues every unreachable revert
// onto r.pendingGuardReverts so retry finishes guarding them before they vanish
// from the unresolved stream. Caller holds r.mu.
func (r *Resolver) drainGuardSpoolToPendingLocked(guardSpool *resolveGuardSpool, guardRepos map[string]struct{}) {
	if guardSpool == nil || guardSpool.count == 0 || guardSpool.readAt >= guardSpool.count {
		return
	}
	var guardClosure map[string]map[string]struct{}
	if len(r.scope) == 0 {
		guardClosure = r.buildImportClosure()
	} else {
		guardClosure = r.buildImportClosureFiltered(guardRepos)
	}
	if len(guardClosure) == 0 {
		return
	}
	refused := true
	defer r.trackPassWriteRefused(&refused)()
	for done := false; !done; {
		records, exhausted, err := guardSpool.nextPage(resolvePendingPageRows)
		if err != nil {
			r.logger.Error("resolver: drain guard spool on write refusal", zap.Error(err))
			break
		}
		jobs := guardJobsFromRecords(r.graph, records)
		r.clearLookupCache()
		r.warmGuardLookupCache(jobs)
		r.guardCrossPackageCallEdges(jobs, guardClosure)
		r.clearLookupCache()
		done = exhausted
	}
}

// guardCrossPackageCallEdges inspects the edges mutated by the just-
// completed resolution pass and reverts any weak-tier call/reference
// edge whose resolved target is not import-reachable from the caller.
// jobs are the reindexJob records produced by ResolveAll's worker
// phase; each carries the edge's pre-resolution target in oldTo, so a
// reverted edge is restored exactly. closure is the import-reachability
// map from buildImportClosure. Returns the number of edges reverted.
func (r *Resolver) guardCrossPackageCallEdges(jobs []reindexJob, closure map[string]map[string]struct{}) int {
	if len(jobs) == 0 {
		return 0
	}
	var liveJobs resolveJobLiveness
	if r.validateLiveness {
		edges := make([]*graph.Edge, 0, len(jobs))
		for i := range jobs {
			edges = append(edges, jobs[i].edge)
		}
		liveJobs = loadEdgeLiveness(r.graph, edges)
	}
	// Collect both mutation lists across the whole pass and apply them
	// via the batched Store methods at the end. Per-edge
	// SetEdgeProvenance + ReindexEdge in the body would otherwise pay
	// two ACID round-trips per reverted edge against disk backends —
	// catastrophic on a 30k-job pass.
	var provBatch []graph.EdgeProvenanceUpdate
	var reindexBatch []graph.EdgeReindex
	var spoolReverts []lspSpoolRevert
	var sourceReverts []graph.PlaceholderRepoint
	var repoPrefixes []string
	for i := range jobs {
		j := &jobs[i]
		// A concurrent edit during a chunked ResolveAll yield may have evicted
		// this edge since it resolved; reverting + reindexing it would
		// half-resurrect it. Skip — it is no longer in the graph.
		if r.validateLiveness && !liveJobs.containsEdge(j.edge) {
			continue
		}
		// The deferred LSP batch may have re-bound (or confirmed) this edge
		// after the heuristic job was recorded, stamping it OriginLSPResolved —
		// compiler-grade evidence the name-only fallback this guard polices
		// never had. j.origin still holds the stale heuristic tier, so trust
		// the live edge: never revert an LSP-owned binding. (The batch now
		// overrides confident heuristic binds, so a recorded job's target can
		// be LSP-owned; before, the batch only touched heuristic-unresolved
		// edges, disjoint from these jobs, and this never fired.)
		if j.edge.Origin == graph.OriginLSPResolved {
			continue
		}
		if !isCallLikeEdge(j.kind) {
			continue
		}
		// A parked reference that bound back to its own prior target is the
		// binding the whole index already judged with the extractor's
		// target; re-judging it on the restub's bare placeholder would give
		// the per-save path a different answer from the same tree.
		if j.restubRoundTrip {
			continue
		}
		// Only the two weakest tiers — a name-only guess — are in scope.
		// DefaultOriginFor backfills the tier for edges whose Origin the
		// resolver left unset (the heuristic fallbacks never stamp it).
		origin := j.origin
		if origin == "" {
			origin = graph.DefaultOriginFor(j.kind, j.confidence, "")
		}
		if origin != graph.OriginTextMatched && origin != graph.OriginASTInferred {
			continue
		}
		// The pre-resolution target must be a bare-name placeholder —
		// `unresolved::Foo` (function call) or `unresolved::*.foo`
		// (member call). Anything else carries evidence the name-only
		// fallback never had and is out of scope: `extern::` pins an
		// import path, `grpc::` / `pyrel::` / `import::` are owned by
		// dedicated passes, and a non-`unresolved::` target was never a
		// guess to begin with.
		if !isBareNameCallTarget(j.oldTo) {
			continue
		}
		callerFile := r.edgeCallerFile(j.edge)
		callerNode := r.cachedGetNode(j.edge.From)
		target := r.cachedGetNode(j.newTo)
		if callerFile == "" || target == nil {
			continue
		}
		if r.targetImportReachable(callerFile, callerNode, target, closure) {
			continue
		}
		// A member call whose only in-repo definition of the name is this
		// target is not a cross-package mis-guess — there is nowhere else the
		// call could bind. A method call carries its receiver, so it needs no
		// import of the method's package, and inherited / indirectly-typed
		// receivers (owner.foo() → BaseType.foo two packages up) never name the
		// declaring package, so the import closure structurally misses them.
		// Keep the resolution.
		if r.loneMemberDefnKeep(target, j.edge, j.oldTo, callerFile) {
			continue
		}
		// A C# extension-method bind is corroborated by namespace
		// visibility, not imports: the extension enters scope via the
		// caller's enclosing namespace or a project-scoped `global using`
		// declared in a sibling file (or `using static` of the declaring
		// class) — none of which leaves an import edge on the calling
		// file for the reachability closure to see. Re-check the same
		// visibility evidence the bind used and keep it when it holds.
		if r.csharpExtensionGuardKeep(j.edge, callerFile, target) {
			continue
		}
		// Same shape for a receiverless call bound through `using static`:
		// the directive is the scope evidence, never an import edge.
		if r.csharpUsingStaticGuardKeep(j.edge, callerFile, target) {
			continue
		}
		// Not reachable — revert to the unresolved placeholder and
		// re-index against the resolved target we are abandoning.
		// SetEdgeProvenance("") drops the resolution provenance so
		// the reverted edge's identity change is counted; the target
		// revert + re-bucket follows. Both go in their respective
		// batches so the whole pass commits in two chunks instead of
		// 2×N per-edge transactions.
		oldResolved := j.edge.To
		// Source reconciliation may already have lifted an arg_of/value_flow
		// placeholder at this exact call site. Reverting only the call would
		// leave that guessed producer bound. Restore the caller's source
		// namespace, as applyRepoPrefix does, without touching other sites that
		// legitimately use the same definition.
		repoPrefix := r.retryRepoPrefix
		if callerNode != nil && callerNode.RepoPrefix != "" {
			repoPrefix = callerNode.RepoPrefix
		} else if repoPrefix == "" && target.RepoPrefix != "" {
			repoPrefix = target.RepoPrefix
		}
		placeholder := j.oldTo
		if callerNode != nil && callerNode.RepoPrefix != "" {
			placeholder = callerNode.RepoPrefix + "/" + placeholder
		}
		sourceReverts = append(sourceReverts, graph.PlaceholderRepoint{
			OldFrom: oldResolved, FilePath: j.edge.FilePath, Line: j.edge.Line, NewFrom: placeholder, RepoPrefix: repoPrefix,
		})
		provBatch = append(provBatch, graph.EdgeProvenanceUpdate{Edge: j.edge, NewOrigin: ""})
		j.edge.To = j.oldTo
		j.edge.Confidence = 0
		// The label travels with the confidence — agents act on it, and an
		// unresolved edge advertising the abandoned bind's EXTRACTED label
		// would out-rank evidence that actually survived.
		j.edge.ConfidenceLabel = ""
		// Revert stamp: persists on the reverted unresolved edge across passes
		// so CrossRepoResolver never resurrects an unimported same-repo Go
		// decoy and guard verdicts remain visible while the edge stays
		// unresolved. Binding the edge to a concrete target clears the stamp,
		// and the guard re-stamps it if a later restub triggers another
		// unreachable guess.
		if j.edge.Meta == nil {
			j.edge.Meta = map[string]any{}
		}
		j.edge.Meta["guard_reverted"] = true
		// The abandoned bind's provenance must not shadow a later
		// rebind — the receiver gate exempts resolution shapes like
		// extension_method, and a stale tag would carry that exemption
		// to whatever the edge binds to next.
		delete(j.edge.Meta, "resolution")
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: j.edge, OldTo: oldResolved})
		spoolReverts = append(spoolReverts, lspSpoolRevert{edge: j.edge, oldBoundTo: oldResolved})
		repoPrefixes = append(repoPrefixes, repoPrefix)
	}
	if len(sourceReverts) > 0 {
		// Drop any unapplied deferred placeholder move targeting the binding
		// this guard just rejected so retry never lifts From onto it.
		r.placeholderSrcIdx.pruneSites(sourceReverts)
	}
	clearInMemoryProvenance := func(updates []graph.EdgeProvenanceUpdate) {
		for _, u := range updates {
			if u.Edge == nil || u.Edge.Origin == u.NewOrigin {
				continue
			}
			u.Edge.Origin = u.NewOrigin
			if u.Edge.Tier != "" {
				u.Edge.Tier = graph.ResolvedBy(u.NewOrigin)
			}
		}
	}
	queueGuardRevertsByRepo := func(
		reindexes []graph.EdgeReindex,
		sources []graph.PlaceholderRepoint,
		spools []lspSpoolRevert,
		repos []string,
		reindexesApplied bool,
	) {
		n := len(repos)
		if n == 0 {
			return
		}
		uniform := true
		first := repos[0]
		for _, p := range repos[1:] {
			if p != first {
				uniform = false
				break
			}
		}
		if uniform {
			r.appendPendingGuardRevertLocked(PendingGuardRevert{
				RepoPrefix:       first,
				Reindexes:        slices.Clip(reindexes),
				SourceReverts:    slices.Clip(sources),
				reindexesApplied: reindexesApplied,
				spoolReverts:     slices.Clip(spools),
			})
			return
		}
		byRepo := make(map[string]*PendingGuardRevert)
		var order []string
		for idx, repo := range repos {
			b := byRepo[repo]
			if b == nil {
				b = &PendingGuardRevert{RepoPrefix: repo, reindexesApplied: reindexesApplied}
				byRepo[repo] = b
				order = append(order, repo)
			}
			if !reindexesApplied && idx < len(reindexes) {
				b.Reindexes = append(b.Reindexes, reindexes[idx])
			}
			if idx < len(sources) {
				b.SourceReverts = append(b.SourceReverts, sources[idx])
			}
			if idx < len(spools) {
				b.spoolReverts = append(b.spoolReverts, spools[idx])
			}
		}
		for _, repo := range order {
			r.appendPendingGuardRevertLocked(*byRepo[repo])
		}
	}
	if r.writeRefused() {
		// An earlier write in this pass was already refused under writer-gate
		// contention. Clear in-memory provenance and queue this revert batch
		// without firing another write into the contended gate.
		clearInMemoryProvenance(provBatch)
		if len(reindexBatch) > 0 || len(sourceReverts) > 0 {
			queueGuardRevertsByRepo(reindexBatch, sourceReverts, spoolReverts, repoPrefixes, false)
		}
		return 0
	}
	reverted := 0
	for start := 0; start < len(reindexBatch); start += guardRevertChunkSize {
		end := min(start+guardRevertChunkSize, len(reindexBatch))
		if end <= len(provBatch) && start < end {
			if r.passReindexMaxWait <= 0 {
				r.graph.SetEdgeProvenanceBatch(provBatch[start:end])
			}
			// The reindex below persists each edge struct as it stands. The
			// in-memory store's SetEdgeProvenance clears Origin on this very
			// pointer, but a store that answers reads with copies (the per-file
			// delta writer) may leave the caller's copy untouched, and the
			// reverted row would then keep the abandoned bind's origin — a row a
			// whole index (which reverts in memory) never writes. Apply the same
			// clear to the struct, with SetEdgeProvenance's Tier rule; on the
			// in-memory store it is already done (and on bounded opportunistic
			// passes it avoids an unbounded provenance write).
			clearInMemoryProvenance(provBatch[start:end])
		}
		if !r.applyReindexEdges(reindexBatch[start:end]) {
			if end < len(provBatch) {
				clearInMemoryProvenance(provBatch[end:])
			}
			queueGuardRevertsByRepo(reindexBatch[start:], sourceReverts[start:], spoolReverts[start:], repoPrefixes[start:], false)
			return reverted
		}
		reverted += end - start
		chunkSources := slices.Clip(sourceReverts[start:end])
		chunkSpool := slices.Clip(spoolReverts[start:end])
		chunkRepos := repoPrefixes[start:end]
		if len(chunkSources) > 0 && os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") != "0" {
			if _, ok := graph.ReconcilePlaceholderSourcesTryApplied(r.graph, chunkSources, r.passReindexMaxWait); !ok {
				r.notePassWriteRefused()
				if end < len(provBatch) {
					clearInMemoryProvenance(provBatch[end:])
				}
				queueGuardRevertsByRepo(nil, chunkSources, chunkSpool, chunkRepos, true)
				if end < len(reindexBatch) {
					queueGuardRevertsByRepo(reindexBatch[end:], sourceReverts[end:], spoolReverts[end:], repoPrefixes[end:], false)
				}
				return reverted
			}
		}
		// A reverted edge may still hold a deferred LSP verify-record spooled
		// while it was heuristically bound. Re-snapshot those records to the
		// post-revert edge state (after both batches, so the struct fields are
		// authoritative on either backend) — otherwise the next pass's exact
		// liveness matching declares them stale and deletes the queued retry.
		if len(chunkSpool) > 0 && r.lspDeferredSpool != nil {
			if err := r.lspDeferredSpool.refreshRevertedEdges(chunkSpool); err != nil {
				r.logger.Error("resolver: refresh reverted LSP spool records", zap.Error(err))
			}
		}
	}
	return reverted
}

// isBareNameCallTarget reports whether an unresolved edge target is a
// bare-name call placeholder — `unresolved::Foo` for a free-function
// call or `unresolved::*.foo` for a member call. These are the only
// shapes the name-only resolution fallback acts on. Targets that embed
// further structure (`unresolved::extern::path::sym`, `grpc::`,
// `pyrel::`, `import::`) carry evidence the fallback never had and are
// resolved by other code paths, so the guard leaves them alone.
func isBareNameCallTarget(target string) bool {
	rest, ok := strings.CutPrefix(target, unresolvedPrefix)
	if !ok || rest == "" {
		return false
	}
	rest = strings.TrimPrefix(rest, "*.")
	if rest == "" {
		return false
	}
	// A remaining `::` means the placeholder is one of the structured
	// forms (extern::, grpc::, pyrel::, import::), not a bare name.
	return !strings.Contains(rest, "::")
}

// isCallLikeEdge reports whether an edge kind is one the guard polices.
// EdgeCalls is the obvious case; EdgeReferences is included because the
// resolver promotes a call-shaped EdgeReads to EdgeReferences once it
// learns the target is a function/method, and that promotion runs
// through the very same name-only fallback.
func isCallLikeEdge(k graph.EdgeKind) bool {
	return k == graph.EdgeCalls || k == graph.EdgeReferences
}

// edgeCallerFile returns the file path of the node that owns the edge's
// From end. Empty when the caller node is unknown.
//
// Hot path: called once per cross-package-guarded edge. The pre-warmed
// per-pass cache populated in ResolveAll holds every From ID across the
// pending slice, so this call is a map lookup during a ResolveAll pass
// and a direct store call elsewhere.
func (r *Resolver) edgeCallerFile(e *graph.Edge) string {
	if n := r.cachedGetNode(e.From); n != nil && n.FilePath != "" {
		return n.FilePath
	}
	return e.FilePath
}

// targetImportReachable reports whether target sits in a package the
// caller's file can see: the caller's own directory (same package), or
// a directory present in the caller's import closure.
func (r *Resolver) targetImportReachable(callerFile string, callerNode, target *graph.Node, closure map[string]map[string]struct{}) bool {
	if target.FilePath == "" {
		// A target with no file (synthetic / external stub) can't be
		// shown unreachable — leave the edge alone.
		return true
	}
	callerDir := filePathDir(callerFile)
	targetDir := filePathDir(target.FilePath)
	if targetDir == callerDir {
		return true
	}
	// Same source package across different directories is reachable without
	// an import edge. Maven splits one package across src/main/java and
	// src/test/java, and JVM same-package callers import nothing — so a
	// directory-only closure reports a false "unreachable" for every
	// test→production same-package call. scope_pkg is stamped only on JVM
	// member nodes, so this never fires for directory-scoped ecosystems.
	if sameScopePackage(callerNode, target) {
		return true
	}
	dirs, ok := closure[callerFile]
	if !ok {
		// No closure entry for the caller (its file node or imports were
		// not indexed). Be conservative: without evidence of isolation
		// we keep the edge rather than risk dropping a real one.
		return true
	}
	_, reachable := dirs[targetDir]
	return reachable
}

// scopePkgOf returns a node's stamped source package (scope_pkg Meta),
// empty when absent. Only JVM extractors (Java / Kotlin) stamp it.
func scopePkgOf(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	if p, ok := n.Meta["scope_pkg"].(string); ok {
		return p
	}
	return ""
}

// sameScopePackage reports whether two nodes belong to the same source
// package of the same language. Empty package on either side is never a
// match, so directory-scoped ecosystems (no scope_pkg) never qualify.
func sameScopePackage(a, b *graph.Node) bool {
	if a == nil || b == nil {
		return false
	}
	pa := scopePkgOf(a)
	if pa == "" {
		return false
	}
	return pa == scopePkgOf(b) && a.Language == b.Language
}

// loneMemberDefnKeep reports whether a to-be-reverted member-call edge should
// survive the cross-package guard because its target is the sole in-repo
// definition of the method name. A name with exactly one candidate cannot be a
// cross-package mis-guess: a method call carries its receiver, so it needs no
// import of the method's package, and the import closure structurally misses
// inherited / indirectly-typed receivers (owner.foo() where owner came from a
// return value the caller's file never imports the type of). Restricted to the
// languages listed in loneMemberLang, and gated on the receiver, when known,
// naming an in-repo type so an external-typed receiver (a logging facade's
// `logger.info`) still reverts rather than latching onto an unrelated
// same-named local method. C# demands one more corroboration: the
// candidate's namespace must be visible from the calling file — see the
// BCL-shadowing note in the body.
func (r *Resolver) loneMemberDefnKeep(target *graph.Node, e *graph.Edge, oldTo, callerFile string) bool {
	if target == nil || !loneMemberLang(target.Language) {
		return false
	}
	bareName := graph.UnresolvedName(oldTo)
	memberCall := strings.HasPrefix(bareName, "*.")
	// Go has free functions that DO need their package imported, so only a
	// member call (`x.foo()` — the receiver carries the type, no import of the
	// method's package needed) is kept; a bare free-function call to an
	// un-imported package must still revert. Java has no free functions, so its
	// bare calls are static-member dispatch and keep too.
	if target.Language != "java" && !memberCall {
		return false
	}
	name := strings.TrimPrefix(bareName, "*.")
	if name == "" {
		return false
	}
	repo := r.callerRepoPrefix(e)
	if rt := edgeReceiverType(e); rt != "" && !r.hasInRepoType(rt, repo) {
		return false
	}
	// C#: a lone definition is no corroboration when the name shadows a
	// BCL member (`Where`, `Select`, `Parse`) — the true target is
	// external, so the sole indexed homonym is a decoy for every call the
	// tenv could not type. Demand the candidate's namespace be visible
	// from the calling file. Narrowing-only, like the extension-
	// visibility policy: a file with NO recorded usings is stale or
	// partial data, not evidence of absence — the keep is lost only when
	// using evidence exists and the namespace is not in it (nor an
	// enclosing one). For the member shapes that can bind cross-namespace
	// without the file naming the type — extensions and class-qualified
	// statics — visibility is the language rule, not a heuristic; for
	// var-typed instance receivers it trades a rare silent miss for an
	// honest unresolved.
	if target.Language == "csharp" {
		if ns, _ := target.Meta["scope_ns"].(string); ns != "" {
			visible := r.csharpFileNamespaceSet(callerFile)
			_, enc := visible.enclosing[ns]
			_, imp := visible.imported[ns]
			if len(visible.imported) > 0 && !enc && !imp {
				return false
			}
		}
	}
	n := 0
	for _, c := range r.cachedFindNodesByNameInRepo(name, repo) {
		if c.Language != target.Language {
			continue
		}
		// A member call can only bind to a method; count functions too only
		// for Java's static-member model (its bare calls).
		if c.Kind == graph.KindMethod || (target.Language == "java" && c.Kind == graph.KindFunction) {
			if n++; n > 1 {
				return false
			}
		}
	}
	return n == 1
}

// loneMemberLang reports whether a lone in-repo method definition is safe to
// keep against the cross-package guard for the given language.
//
// The bar is not static typing as such — loneMemberDefnKeep already excludes
// bare free-function calls and receivers typed as something the repo does not
// define, and only fires when the name has exactly ONE in-repo method
// definition, which is the case duck typing cannot confuse. The bar is whether
// a class-based member call is the language's dispatch model. PHP qualifies:
// its calls carry a receiver, and since PHP call sites stamp receiver_type the
// external-receiver gate above screens them more effectively than it does for
// most of this list. GDScript qualifies for the same reason and needs it more
// than any of them: a script IS a class, `class_name` registers it
// project-globally, and the language has no import statement at all — so the
// import closure this guard consults is structurally empty for every
// cross-directory call and would revert all of them. TS / Python / JS stay
// out — an object literal or a dynamically attached method makes a same-name
// coincidence likelier there, and their guard revert is load-bearing.
func loneMemberLang(lang string) bool {
	switch lang {
	case "java", "go", "rust", "csharp", "kotlin", "scala", "php", "gdscript":
		return true
	}
	return false
}

// hasInRepoType reports whether the repo defines a type/interface named
// typeName — the gate that keeps javaLoneMemberDefnKeep from latching a
// call on an external-typed receiver onto an unrelated in-repo method.
func (r *Resolver) hasInRepoType(typeName, repo string) bool {
	for _, c := range r.cachedFindNodesByNameInRepo(typeName, repo) {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			return true
		}
	}
	return false
}

// buildImportClosure maps each caller file path to the set of directories
// it can reach by import. The set is seeded with the file's own directory
// and extended with the directory of every node its resolved EdgeImports
// edges point at. It is built from the post-resolution graph — by the
// time the guard runs, import edges have been resolved to real file /
// package nodes, so this closure captures JS/TS relative-file imports
// that the pre-resolution reachability index (keyed on directory-shaped
// import paths) structurally misses.
func (r *Resolver) buildImportClosure() map[string]map[string]struct{} {
	return r.buildImportClosureFiltered(nil)
}

// buildImportClosureFiltered is buildImportClosure restricted to a set of repo
// prefixes: it seeds the closure only for files owned by those repos and only
// walks import edges whose caller sits in one of them. Each import edge
// contributes solely to its own caller's closure entry, so a caller in the set
// gets the same reachable-dir set it would in the whole-graph build — the guard
// queries the closure only for those callers, so its verdicts are unchanged.
// Re-export edges stay unfiltered: a caller in the set may import a barrel that
// re-exports from a repo outside it, and the transitive barrel walk must still
// reach it. A nil repos set builds the whole-graph closure.
func (r *Resolver) buildImportClosureFiltered(repos map[string]struct{}) map[string]map[string]struct{} {
	inScope := func(id string) bool {
		if repos == nil {
			return true
		}
		_, ok := repos[graph.RepoPrefixOfID(id)]
		return ok
	}
	closure := make(map[string]map[string]struct{})
	add := func(file, dir string) {
		if file == "" || dir == "" {
			return
		}
		set := closure[file]
		if set == nil {
			set = make(map[string]struct{})
			closure[file] = set
		}
		set[dir] = struct{}{}
	}
	var repoPrefixes []string
	if repos != nil {
		repoPrefixes = make([]string, 0, len(repos))
		for prefix := range repos {
			repoPrefixes = append(repoPrefixes, prefix)
		}
		sort.Strings(repoPrefixes)
	}
	for file := range graph.FileNodeIdentitiesSeq(r.graph, repoPrefixes) {
		if file.FilePath != "" && inScope(file.ID) {
			add(file.FilePath, filePathDir(file.FilePath))
		}
	}
	// Materialise metadata-free import projections and batch-load only the
	// endpoint placement fields needed by the closure. A per-edge point lookup
	// here would be a query round-trip per import on a disk backend.
	//
	// Re-export edges ride the same batch: an import that lands on a
	// barrel (`import { persist } from 'zustand/middleware'` resolving to
	// src/middleware.ts, which `export { persist } from
	// './middleware/persist.ts'`) must make the re-exported module's
	// directory reachable too — the consumer names the barrel, but the
	// symbol it calls lives behind the re-export hop. Without this, the
	// guard reverts every legitimate barrel-mediated call as
	// "not import-reachable".
	skipTarget := importClosureSkipsTarget
	var imports, reexports []*graph.Edge
	ids := make(map[string]struct{})
	collect := func(e *graph.Edge) {
		if e.From != "" {
			ids[e.From] = struct{}{}
		}
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeImports) {
		// Skip imports still pointing at an unresolved placeholder or an
		// out-of-repo stub — neither names an in-repo directory that a
		// name-only call candidate could legitimately live in.
		if skipTarget(e.To) {
			continue
		}
		// An import edge only extends its own caller's closure entry, so on a
		// scoped build we need just the edges whose caller is in scope.
		if !inScope(e.From) {
			continue
		}
		imports = append(imports, e)
		collect(e)
	}
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeReExports) {
		if skipTarget(e.To) {
			continue
		}
		reexports = append(reexports, e)
		collect(e)
	}
	return r.assembleImportClosure(closure, imports, reexports, ids, nil)
}

// importClosureSkipsTarget reports an import / re-export target that names no
// in-repo directory: a still-unresolved placeholder or an out-of-repo stub.
func importClosureSkipsTarget(to string) bool {
	return strings.HasPrefix(to, unresolvedPrefix) ||
		strings.HasPrefix(to, "external::") ||
		graph.IsStdlibStub(to) ||
		strings.HasPrefix(to, "dep::")
}

// buildImportClosureForCallerFiles is buildImportClosure restricted to the
// given caller files: the entry of every listed file is exactly the entry the
// whole-graph build gives it, and no other file has one. An entry depends only
// on the file's own file node, the import edges whose caller is the file, and
// the re-export edges reachable from those imports' targets, so the build reads
// just those: the files' file nodes, the import edges out of their nodes, and a
// breadth-first walk of re-export edges out of the imported (barrel) files. The
// guard consults the closure only for the caller files of the jobs it checks,
// so an incremental resolve pays for its own callers instead of a scan of
// every file node and import edge of the store.
//
// One shape is read differently: an import edge whose source node is missing
// or has no file path is attributed to its own file_path by the whole-graph
// build, and is found here only when the store answers the file projection
// from edge provenance. Such edges do not occur in an indexed graph (eviction
// removes a node's edges with it).
func (r *Resolver) buildImportClosureForCallerFiles(files []string) map[string]map[string]struct{} {
	closure := make(map[string]map[string]struct{})
	callers := make(map[string]struct{}, len(files))
	wanted := make([]string, 0, len(files))
	for _, file := range files {
		if file == "" {
			continue
		}
		if _, dup := callers[file]; !dup {
			callers[file] = struct{}{}
			wanted = append(wanted, file)
		}
	}
	if len(wanted) == 0 {
		return closure
	}
	sort.Strings(wanted)
	seed := func(filePath string) {
		dir := filePathDir(filePath)
		if dir == "" {
			return
		}
		set := closure[filePath]
		if set == nil {
			set = make(map[string]struct{})
			closure[filePath] = set
		}
		set[dir] = struct{}{}
	}
	// A caller file the pass's directory index lists is a file node already
	// read; only the others are read here (a file-scoped projection composed
	// through a delta's layers costs seconds per call).
	unknown := wanted
	if r.dirIndex != nil {
		unknown = nil
		for _, file := range wanted {
			listed := false
			for _, identity := range r.dirIndex[filePathDir(file)] {
				if identity.FilePath == file {
					listed = true
					break
				}
			}
			if listed {
				seed(file)
			} else {
				unknown = append(unknown, file)
			}
		}
	}
	if len(unknown) > 0 {
		for node := range graph.NodesInScopeSeq(r.graph, nil, unknown, graph.KindFile) {
			if node == nil || node.FilePath == "" {
				continue
			}
			if _, ok := callers[node.FilePath]; !ok {
				continue
			}
			seed(node.FilePath)
		}
	}
	var imports, reexports []*graph.Edge
	ids := make(map[string]struct{})
	collect := func(e *graph.Edge) {
		if e.From != "" {
			ids[e.From] = struct{}{}
		}
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	targets := make(map[string]struct{})
	addImport := func(e *graph.Edge) {
		if e == nil || importClosureSkipsTarget(e.To) {
			return
		}
		imports = append(imports, e)
		collect(e)
		if e.To != "" {
			targets[e.To] = struct{}{}
		}
	}
	// The import projection answers a caller file's direct import targets
	// (the same rows, from the file's nodes) and is kept per stack by a
	// delta; the file-scoped edge projection is the fallback.
	projected, complete := map[string][]string(nil), false
	if projector, ok := r.graph.(graph.ImportAdjacencyProjector); ok {
		projected, complete = projector.ProjectImportAdjacency(wanted)
	}
	if complete {
		for _, file := range wanted {
			for _, to := range projected[file] {
				addImport(&graph.Edge{From: file, To: to, Kind: graph.EdgeImports, FilePath: file})
			}
		}
	} else {
		for row := range graph.EdgesInScopeSeq(r.graph, nil, wanted, graph.EdgeImports) {
			addImport(row.Edge)
		}
	}
	if len(imports) == 0 {
		return closure
	}
	// Re-export hops: walk barrel files breadth-first from the imported files.
	visited := make(map[string]struct{})
	nextFiles := func(ids map[string]struct{}) []string {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		var out []string
		for _, placement := range graph.NodePlacementsByIDs(r.graph, list) {
			if placement.FilePath == "" || !mayCarryReExports(placement.FilePath) {
				continue
			}
			if _, seen := visited[placement.FilePath]; seen {
				continue
			}
			visited[placement.FilePath] = struct{}{}
			out = append(out, placement.FilePath)
		}
		sort.Strings(out)
		return out
	}
	for frontier := nextFiles(targets); len(frontier) > 0; {
		hop := make(map[string]struct{})
		for row := range graph.EdgesInScopeSeq(r.graph, nil, frontier, graph.EdgeReExports) {
			e := row.Edge
			if e == nil || importClosureSkipsTarget(e.To) {
				continue
			}
			reexports = append(reexports, e)
			collect(e)
			if e.To != "" {
				hop[e.To] = struct{}{}
			}
		}
		frontier = nextFiles(hop)
	}
	return r.assembleImportClosure(closure, imports, reexports, ids, callers)
}

// assembleImportClosure folds import and re-export edges into closure (already
// seeded with each caller file's own directory): every non-placeholder import
// adds its target's directory, plus every directory reachable from that target
// through re-export hops. ids are the edges' endpoint IDs. callers, when
// non-nil, keeps only imports whose caller file is in the set (the scoped
// build fetches a superset of their import edges).
func (r *Resolver) assembleImportClosure(
	closure map[string]map[string]struct{},
	imports, reexports []*graph.Edge,
	ids map[string]struct{},
	callers map[string]struct{},
) map[string]map[string]struct{} {
	if len(imports) == 0 {
		return closure
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	placements := graph.NodePlacementsByIDs(r.graph, idList)
	add := func(file, dir string) {
		if file == "" || dir == "" {
			return
		}
		set := closure[file]
		if set == nil {
			set = make(map[string]struct{})
			closure[file] = set
		}
		set[dir] = struct{}{}
	}

	// Collapse symbol-level re-export edges to unique file targets.
	reexpTargets := make(map[string]map[string]struct{})
	for _, e := range reexports {
		barrel := e.FilePath
		if from, ok := placements[e.From]; ok && from.FilePath != "" {
			barrel = from.FilePath
		}
		if to, ok := placements[e.To]; ok && to.FilePath != "" && barrel != "" {
			if reexpTargets[barrel] == nil {
				reexpTargets[barrel] = make(map[string]struct{})
			}
			reexpTargets[barrel][to.FilePath] = struct{}{}
		}
	}

	// Schedule imported descendants first so overlapping roots can reuse
	// completed closures. Cyclic members still require independent full
	// walks: recursive partial cache entries would be order-dependent.
	barrelDirCache := make(map[string][]string)
	if len(reexpTargets) > 0 {
		var roots []string
		seenRoots := make(map[string]struct{})
		for _, e := range imports {
			if target, ok := placements[e.To]; ok && len(reexpTargets[target.FilePath]) > 0 {
				if _, seen := seenRoots[target.FilePath]; !seen {
					seenRoots[target.FilePath] = struct{}{}
					roots = append(roots, target.FilePath)
				}
			}
		}
		for _, root := range importRootOrder(roots, reexpTargets) {
			barrelDirCache[root] = importReachableDirs(root, reexpTargets, barrelDirCache)
		}
	}
	for _, e := range imports {
		callerFile := e.FilePath
		if from, ok := placements[e.From]; ok && from.FilePath != "" {
			callerFile = from.FilePath
		}
		if callers != nil {
			if _, wanted := callers[callerFile]; !wanted {
				continue
			}
		}
		if target, ok := placements[e.To]; ok && target.FilePath != "" && callerFile != "" {
			add(callerFile, filePathDir(target.FilePath))
			if len(reexpTargets[target.FilePath]) == 0 {
				continue
			}
			dirs, ok := barrelDirCache[target.FilePath]
			if !ok {
				dirs = importReachableDirs(target.FilePath, reexpTargets, barrelDirCache)
				barrelDirCache[target.FilePath] = dirs
			}
			for _, dir := range dirs {
				add(callerFile, dir)
			}
		}
	}
	return closure
}

// reExportFreeExtensions are source extensions whose extractors never emit a
// re-export edge (only the JavaScript / TypeScript extractors and the formats
// that embed them do). A file with one of them is never a barrel, so the
// re-export walk does not read its edges.
var reExportFreeExtensions = map[string]struct{}{
	".go": {}, ".py": {}, ".java": {}, ".kt": {}, ".kts": {}, ".scala": {},
	".rs": {}, ".rb": {}, ".php": {}, ".cs": {}, ".swift": {}, ".dart": {},
	".c": {}, ".h": {}, ".cc": {}, ".cpp": {}, ".cxx": {}, ".hpp": {}, ".hh": {},
	".m": {}, ".mm": {}, ".ex": {}, ".exs": {}, ".erl": {}, ".hs": {}, ".lua": {},
	".sh": {}, ".bash": {}, ".sql": {}, ".proto": {}, ".md": {}, ".yaml": {}, ".yml": {},
	".json": {}, ".toml": {},
}

// mayCarryReExports reports whether filePath can hold re-export edges: any
// file except one whose extension belongs to a language that never emits them
// (an unknown or missing extension may be a content-detected script).
func mayCarryReExports(filePath string) bool {
	_, free := reExportFreeExtensions[jsTSExt(filePath)]
	return !free
}
