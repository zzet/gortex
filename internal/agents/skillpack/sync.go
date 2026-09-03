package skillpack

// sync.go reconciles one host's installed skill tree with the subset an
// instruction profile allows. Every skill-aware host installs the same
// playbooks under a different root in a different frontmatter dialect,
// and every one of them needs the same three-way decision on each file:
// install it, leave it alone, or remove it. Writing that decision once
// is the point — it is the highest blast-radius rule in the installer,
// because getting it wrong deletes a file off a user's machine.
//
// # Import direction: adapters -> skillpack, never the reverse
//
// This file relaxes the package's original stdlib-only rule to
// internal/agents (Env/ApplyOpts/FileAction, and the atomic writers) and
// internal/agents/internalutil (the shared log prefixes). Both are safe
// because neither imports skillpack, and neither may start: internal/
// agents is the parent every adapter depends on, so an
// agents -> skillpack edge plus the adapters' skillpack -> agents edge
// would be a cycle Go rejects.
//
// The rule that still holds without exception is the original one:
// skillpack MUST NOT import an adapter package
// (internal/agents/claudecode, internal/agents/codex, …). Adapters call
// in; skillpack never calls out. That is what lets four hosts share this
// reconciler without any of them depending on each other.
//
// # Why the rule is duplicated rather than shared with claudecode
//
// internal/agents/claudecode has its own copy of this logic
// (SyncGlobalSkills + writeAgentArtifact), carrying migration hashes for
// artifacts only Claude Code ever shipped. The behaviour here is a
// faithful port of that rule — deliberately, byte-decision for
// byte-decision — so the two cannot disagree about what gets deleted.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// defaultSkillFileName is the file the Agent Skills spec fixes inside
// each skill directory. Used when a SyncSpec leaves FileName empty.
const defaultSkillFileName = "SKILL.md"

// SyncSpec describes one host's installed skill tree: where it lives,
// what this host would write into it, and which bodies count as ours.
type SyncSpec struct {
	// Dir is the host's skills root. Each skill occupies
	// <Dir>/<id>/<FileName>, and pruning removes <Dir>/<id> whole —
	// a skill directory may carry auxiliary files (scripts, reference
	// docs) that are meaningless once the SKILL.md is gone.
	//
	// An empty Dir makes Sync a no-op rather than an error: a host with
	// no resolvable home is simply not installed on this machine.
	Dir string

	// Rendered maps skill ID -> the exact bytes THIS host installs.
	// Per-host rather than a single shared body because each host
	// demands its own frontmatter dialect, and the byte comparison that
	// separates "still ours" from "the user edited it" is only
	// meaningful against the bytes this host would have written.
	Rendered map[string]string

	// FileName is the file inside each skill directory. Every host
	// supported today uses SKILL.md, but it stays a field so a host that
	// spells it differently does not have to fork the reconciler.
	// Empty means SKILL.md.
	FileName string

	// KnownHashes maps skill ID -> sha256 hex digests of bodies an
	// EARLIER Gortex release shipped for that ID. A file matching one is
	// still Gortex's, not the user's, so it may be refreshed in place or
	// pruned. Optional: a pack with no superseded rendering leaves this
	// nil and relies on the byte compare alone.
	KnownHashes map[string][]string

	// DirFor overrides where one skill's directory lives, for a host
	// that groups skills below the root instead of placing them
	// directly under it — Hermes files each skill under a category
	// (skills/<category>/<id>/). Optional; nil means <Dir>/<id>.
	//
	// It returns the directory, not the file, because pruning removes
	// the directory whole and the reconciler must not have to guess how
	// far up the tree the skill's own scope ends.
	DirFor func(id string) string
}

// skillDir resolves one skill's directory under this spec.
func (s SyncSpec) skillDir(id string) string {
	if s.DirFor != nil {
		return s.DirFor(id)
	}
	return filepath.Join(s.Dir, id)
}

// Sync reconciles a host's installed skill tree with the allowed
// subset. A nil allowed means "every shipped skill" — the install path
// — and is not the same as an empty slice, which means "keep none".
//
// Per skill, in ID order (map order is randomised, and an install
// report, a plan and a golden test all need the same sequence):
//
//   - in the subset and missing on disk -> installed;
//   - in the subset and byte-identical -> left alone (ActionSkip
//     "unchanged");
//   - in the subset, differing, but matching a KnownHashes digest ->
//     refreshed, because those bytes are a previous release's, not the
//     user's;
//   - in the subset and otherwise differing -> KEPT with a warning
//     (ActionSkip "customised"): a re-install must never discard an
//     edit the user made;
//   - OUTSIDE the subset and still matching a shipped body -> deleted;
//   - OUTSIDE the subset and customised -> KEPT with a warning.
//
// That last pair is the load-bearing rule. Enforcing a profile is worth
// removing files Gortex itself wrote; it is never worth removing a
// playbook a user rewrote. When the two goals conflict, the user's
// bytes win and the profile stays slightly wider than requested.
//
// Under DryRun nothing touches disk and removals report
// ActionWouldDelete.
//
// Returned actions are partial on error: a caller that reports progress
// should still consume what came back before handling the error.
func Sync(w io.Writer, spec SyncSpec, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if spec.Dir == "" || len(spec.Rendered) == 0 {
		return nil, nil
	}
	fileName := spec.FileName
	if fileName == "" {
		fileName = defaultSkillFileName
	}

	// A nil set means "no filtering at all"; an empty non-nil set means
	// "nothing is allowed". Keeping those distinct is what lets the
	// install path share this function with the profile switch.
	var allowedSet map[string]bool
	if allowed != nil {
		allowedSet = make(map[string]bool, len(allowed))
		for _, id := range allowed {
			allowedSet[id] = true
		}
	}

	out := make([]agents.FileAction, 0, len(spec.Rendered))
	for _, id := range sortedIDs(spec.Rendered) {
		content := spec.Rendered[id]
		dir := spec.skillDir(id)
		path := filepath.Join(dir, fileName)

		var (
			action agents.FileAction
			err    error
		)
		if allowedSet != nil && !allowedSet[id] {
			action, err = pruneSkill(w, dir, path, content, spec.KnownHashes[id], opts)
		} else {
			action, err = installSkill(w, path, content, spec.KnownHashes[id], opts)
		}
		if err != nil {
			return out, fmt.Errorf("skillpack: skill %s: %w", id, err)
		}
		out = append(out, action)
	}
	return out, nil
}

// installSkill writes content at path unless something is already there
// that Gortex did not write.
func installSkill(w io.Writer, path, content string, knownHashes []string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agents.WriteIfNotExists(w, path, content, opts)
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}
	if string(existing) == content {
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "unchanged"}, nil
	}
	if matchesKnownHash(existing, knownHashes) {
		// A previous release's body: ours to replace, so an upgrade
		// actually delivers the new playbook.
		return agents.WriteOwnedFile(w, path, content, opts)
	}
	internalutil.Warnf(w, "keeping customised skill %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customised"}, nil
}

// pruneSkill removes a skill the active profile excludes, but only when
// the installed bytes are still one Gortex shipped.
func pruneSkill(w io.Writer, dir, path, content string, knownHashes []string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		// Not installed, or unreadable — either way there is nothing to
		// prune. Deliberately not an error even for a permission
		// failure: a profile switch must not fail because one host's
		// tree is inaccessible, and refusing to delete is the safe
		// direction for a file we cannot even read.
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "outside-profile"}, nil
	}
	if !isShippedBody(existing, content, knownHashes) {
		internalutil.Warnf(w, "keeping customised skill %s (outside the active profile)", path)
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customised"}, nil
	}
	if opts.DryRun {
		return agents.FileAction{Path: path, Action: agents.ActionWouldDelete, Keys: []string{"skill"}}, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return agents.FileAction{}, err
	}
	internalutil.Logf(w, "[gortex] removed skill %s (outside the active profile)", path)
	return agents.FileAction{Path: path, Action: agents.ActionDelete, Keys: []string{"skill"}}, nil
}

// isShippedBody reports whether existing is a body Gortex wrote: either
// the one this build renders, or one a previous release shipped.
func isShippedBody(existing []byte, current string, knownHashes []string) bool {
	return string(existing) == current || matchesKnownHash(existing, knownHashes)
}

func matchesKnownHash(existing []byte, knownHashes []string) bool {
	if len(knownHashes) == 0 {
		return false
	}
	hash := bodyHash(existing)
	for _, knownHash := range knownHashes {
		if knownHash != "" && hash == knownHash {
			return true
		}
	}
	return false
}

// bodyHash fingerprints an on-disk body for comparison against
// SyncSpec.KnownHashes.
func bodyHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

// sortedIDs orders a rendered map's keys. Go randomises map iteration,
// so without this the same install would emit its actions — and its log
// lines — in a different order every run.
func sortedIDs(rendered map[string]string) []string {
	ids := make([]string, 0, len(rendered))
	for id := range rendered {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
