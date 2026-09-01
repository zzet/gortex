package codex

// skills.go installs the Gortex playbooks as Codex skills.
//
// # The discovery root is `.agents/skills`, and only that
//
// Codex (0.147.0) looks for skills in exactly three places: the
// `.agents/skills` directory of the current working directory and of
// every parent up to the repository root, `$HOME/.agents/skills`, and
// `/etc/codex/skills`.
//
// `~/.codex` is Codex's *configuration* home — config.toml, AGENTS.md,
// session state — and holds no skills. A SKILL.md written to
// `~/.codex/skills` or `<repo>/.codex/skills` produces no error and no
// warning: Codex simply never looks there, so the installer reports a
// clean success and the user gets nothing. That is the one mistake this
// file exists to avoid, and why skills_test.go asserts the `.codex`
// paths stay empty rather than only asserting the `.agents` paths fill.
//
// # One directory per skill, never nested
//
// Codex documents the layout as one directory per skill directly under
// `.agents/skills`, each containing a `SKILL.md`. Recursion into deeper
// subdirectories (`.agents/skills/generated/foo/SKILL.md`) is not
// documented, so nothing here nests — the generated community skills go
// in as direct children like everything else.

import (
	"io"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
	"github.com/zzet/gortex/internal/profiles"
)

// skillFileName is the file Codex loads inside each skill directory.
const skillFileName = "SKILL.md"

// codexSkillsRoot is the skills directory Codex scans under base —
// `$HOME/.agents/skills` for the user-level pack, `<repo>/.agents/skills`
// for the per-repo one. See the package-level note above for why this is
// not derived from `.codex`.
func codexSkillsRoot(base string) string {
	return filepath.Join(base, ".agents", "skills")
}

// Skills returns the curated Gortex playbook pack in host-neutral form.
//
// The bodies are single-sourced in internal/agents/claudecode and each
// host re-wraps them in its own frontmatter dialect; skillpack does the
// stripping. Reading claudecode's map directly from an adapter is the
// established direction (hermes does the same) — claudecode never
// imports another adapter, so there is no cycle to trip over.
func Skills() ([]skillpack.Skill, error) {
	return skillpack.ParseAll(claudecode.GlobalSkills)
}

// renderSkill puts Codex's frontmatter envelope in front of a neutral
// skill body.
//
// Codex reads exactly two frontmatter fields — `name` and `description`
// — and ignores everything else. The envelope is held to those two on
// purpose: `$HOME/.agents/skills` is a shared root (see
// installCuratedSkills), so every extra key is one more chance for two
// adapters to disagree byte-for-byte about a file they both write.
func renderSkill(s skillpack.Skill) string {
	return skillpack.RenderFrontmatter([][2]string{
		{"name", s.Name},
		{"description", s.Description},
	}) + "\n" + s.Body
}

// applySkills installs whichever skill set belongs to this Env's mode.
// The two sets never overlap: the curated pack is machine-wide and the
// generated community pack is repo-specific.
func applySkills(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if env.Mode == agents.ModeGlobal {
		return installCuratedSkills(env, opts)
	}
	return installGeneratedSkills(env, opts), nil
}

// installCuratedSkills writes the curated pack to
// `$HOME/.agents/skills/<id>/SKILL.md`.
//
// ModeGlobal only, mirroring claudecode — whose SyncGlobalSkills sits
// inside applyGlobal for the same reason. The playbooks are codebase-
// and host-agnostic, so a `gortex init` that dropped 21 markdown files
// into a repo would be handing the user's teammates a pile of unrelated
// files to review for no benefit.
//
// `$HOME/.agents/skills` is also one of OpenCode's global skill roots,
// so this single write serves both hosts. That convergence is
// deliberate: the vendors agreed on a root, and duplicating the same
// bodies under a second host-specific directory would only create two
// copies to drift apart. The consequence is that one `gortex install`
// run can reach this path twice, once per adapter, so the write must be
// byte-stable — whichever adapter runs second sees identical bytes and
// reports ActionSkip instead of churning the file.
//
// An existing file that differs from the shipped body is the user's:
// it is left alone (ActionSkip "customised"), the same never-clobber
// posture as claudecode's writeAgentArtifact.
//
// `allowed` is nil here — install materialises the whole pack, and
// `gortex instructions switch` narrows it afterwards through SyncSkills.
// Both go through the same reconciler so an install and a profile switch
// can never disagree about what belongs on disk.
// An empty Home is refused rather than defaulted. codexSkillsRoot joins
// onto it, so an unresolved home turns "$HOME/.agents/skills" into the
// relative ".agents/skills" and the curated pack lands in whatever
// directory the process happens to be in — most often the user's repo,
// which is exactly where this pack must never go. planSkills already
// guards for it; this is the same guard on the write side.
func installCuratedSkills(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if env.Home == "" {
		return nil, nil
	}
	return syncCuratedSkills(env.Stderr, codexSkillsRoot(env.Home), nil, opts)
}

// SyncSkills reshapes an ALREADY-INSTALLED `$HOME/.agents/skills` tree
// to the allowed subset (nil = every shipped skill). This is the entry
// point `gortex instructions switch` drives.
//
// A missing skills root is a silent no-op, not an install: switching a
// profile must never conjure a Codex surface on a machine that never ran
// `gortex install`, and most machines carry only one or two of the
// supported hosts. That is the one behavioural difference from
// installCuratedSkills, which shares the reconciler below.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := codexSkillsRoot(home)
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	return syncCuratedSkills(w, root, allowed, opts)
}

// syncCuratedSkills is the one reconciler both entry points reach. The
// deletion rule (never remove a customised file) lives in skillpack, not
// here, so all four skill-aware hosts enforce it identically.
func syncCuratedSkills(w io.Writer, root string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	skills, err := Skills()
	if err != nil {
		return nil, err
	}
	rendered := make(map[string]string, len(skills))
	for _, s := range skills {
		rendered[s.ID] = renderSkill(s)
	}
	return skillpack.Sync(w, skillpack.SyncSpec{
		Dir:         root,
		FileName:    skillFileName,
		Rendered:    rendered,
		KnownHashes: skillpack.AddWorktreeV1Hashes(rendered, profiles.WorktreeBranchRoutingPolicy, skillpack.PreWorktreeAgentSkillHashes()),
	}, allowed, opts)
}

// installGeneratedSkills materialises the per-community skills the
// skills generator produced, at `<root>/.agents/skills/<DirName>/SKILL.md`.
//
// Codex is the first non-Claude adapter to consume Env.GeneratedSkills —
// until now only claudecode did, under `.claude/skills/generated/`.
// There is no `generated/` grouping level here: Codex only documents
// one directory per skill directly under `.agents/skills`, and DirName
// already carries a `gortex-` prefix, so it cannot collide with the
// curated pack even without a grouping directory.
//
// A DirName that is not kebab-case is skipped rather than written. The
// same generated fixture flows on to OpenCode and Copilot, which refuse
// a non-kebab skill name at load time — and refuse it silently, so the
// skill just never appears in the picker. A loud warning here beats
// three hosts quietly ignoring the file.
//
// WriteOwnedFile (not the never-clobber writer used for the curated
// pack) because these track the current graph and must be regenerated
// on every init; it reports ActionSkip "unchanged" when the content is
// byte-identical, which is what keeps a re-run idempotent.
func installGeneratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	root := codexSkillsRoot(env.Root)
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			internalutil.Warnf(env.Stderr, "skipping generated skill %q: Codex skill directories must be kebab-case", s.DirName)
			continue
		}
		path := filepath.Join(root, s.DirName, skillFileName)
		action, err := agents.WriteOwnedFile(env.Stderr, path, s.Content, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not write generated skill %s: %v", s.DirName, err)
			continue
		}
		out = append(out, action)
	}
	return out
}

// planSkills enumerates every SKILL.md this adapter would write for env.
//
// Plan is what `gortex doctor` and `--print-config` read — they never
// call Apply — so a path that Apply writes but Plan omits is invisible
// to both, and a user debugging a missing skill has nothing to check
// against.
func planSkills(env agents.Env) ([]agents.FileAction, error) {
	if env.Mode == agents.ModeGlobal {
		if env.Home == "" {
			return nil, nil
		}
		skills, err := Skills()
		if err != nil {
			return nil, err
		}
		root := codexSkillsRoot(env.Home)
		out := make([]agents.FileAction, 0, len(skills))
		for _, s := range skills {
			out = append(out, agents.FileAction{
				Path:   filepath.Join(root, s.ID, skillFileName),
				Action: agents.ActionWouldCreate,
			})
		}
		return out, nil
	}
	if env.Root == "" {
		return nil, nil
	}
	root := codexSkillsRoot(env.Root)
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			continue
		}
		out = append(out, agents.FileAction{
			Path:   filepath.Join(root, s.DirName, skillFileName),
			Action: agents.ActionWouldCreate,
		})
	}
	return out, nil
}
