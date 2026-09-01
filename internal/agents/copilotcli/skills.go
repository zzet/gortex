package copilotcli

import (
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// skills.go installs Agent Skills for the Copilot CLI. The CLI adopted
// Anthropic's Agent Skills spec verbatim — a `<name>/SKILL.md` with
// `name` + `description` frontmatter — so the bodies are Claude Code's,
// reused through skillpack with a minimal envelope back on.
//
// Where those files may live is the part a maintainer will get wrong,
// because the CLI has *removed* discovery roots over its life:
//
//   - `~/.copilot/skills/` — the only user-level root that still works.
//   - `~/.claude/skills/` — loaded until v1.0.36, ignored since.
//   - `~/.agents/skills/` — loaded until v1.0.66, ignored since.
//
// Repo-relative `.claude/skills/`, `.github/skills/` and `.agents/skills/`
// all still load, but this adapter writes only `.github/skills/`: the
// repo-local Claude directories belong to the claudecode adapter, and two
// adapters materialising the same generated skill under different roots
// would double every community skill in the picker.
//
// Docs: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-skills

const (
	// skillsDirName is the leaf directory at both levels.
	skillsDirName = "skills"
	// skillFileName is fixed by the Agent Skills spec.
	skillFileName = "SKILL.md"
	// githubDirName holds every repo-level Copilot surface.
	githubDirName = ".github"
)

// CuratedSkillNames returns the curated pack's skill IDs, sorted, so a
// Plan, an install report and a golden test all see the same order —
// Go randomises map iteration, and claudecode.GlobalSkills is a map.
func CuratedSkillNames() []string {
	names := make([]string, 0, len(claudecode.GlobalSkills))
	for name := range claudecode.GlobalSkills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CuratedSkills renders claudecode.GlobalSkills into the Copilot
// envelope, keyed by skill ID.
//
// The envelope is `name` + `description` and nothing else. The CLI also
// accepts `license`, `allowed-tools`, `argument-hint` and
// `disable-model-invocation`, but every one of them either narrows what
// the skill may do or hides it from model invocation — emitting a guess
// for any of them would silently disable a playbook, so the two required
// keys are all that is written.
func CuratedSkills() map[string]string {
	out := make(map[string]string, len(claudecode.GlobalSkills))
	for id, claudeBody := range claudecode.GlobalSkills {
		skill, err := skillpack.Parse(id, claudeBody)
		if err != nil {
			// The Claude bodies are compile-time constants, so this can
			// only fire if that package is edited into a shape skillpack
			// rejects. Skipping beats installing a skill whose frontmatter
			// we could not rewrite; skillpack's own test over
			// claudecode.GlobalSkills turns the mistake into a red build.
			continue
		}
		out[id] = copilotSkillFromClaude(skill)
	}
	return out
}

// copilotSkillFromClaude keeps the body byte-for-byte and puts the
// Copilot frontmatter in front of it. `name` is the skill ID rather than
// the parsed frontmatter name: the CLI requires a lowercase, hyphenated
// name and matches it against the containing directory, and the ID is
// the value that is guaranteed (by skillpack.ValidID) to be both.
func copilotSkillFromClaude(skill skillpack.Skill) string {
	return skillpack.RenderFrontmatter([][2]string{
		{"name", skill.ID},
		{"description", skill.Description},
	}) + skill.Body
}

// curatedSkillsRoot is ~/.copilot/skills — the only user-level skills
// root the CLI still honours (see the file header for the two it
// dropped). Empty when the Env has no resolvable config home.
func curatedSkillsRoot(env agents.Env) string {
	home := copilotConfigHome(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, skillsDirName)
}

// curatedSkillPath is ~/.copilot/skills/<id>/SKILL.md.
func curatedSkillPath(env agents.Env, id string) string {
	root := curatedSkillsRoot(env)
	if root == "" {
		return ""
	}
	return filepath.Join(root, id, skillFileName)
}

// generatedSkillPath is <root>/.github/skills/<DirName>/SKILL.md — flat,
// with no `generated/` level. Claude Code needs that level because its
// curated pack shares .claude/skills/ with the generated one; here the
// curated pack is user-level, so the repo root holds generated skills
// only and DirName already carries the `gortex-` prefix that keeps them
// distinguishable from a team's own skills.
func generatedSkillPath(root, dirName string) string {
	return filepath.Join(root, githubDirName, skillsDirName, dirName, skillFileName)
}

// installCuratedSkills writes the curated pack to ~/.copilot/skills/.
//
// ModeGlobal only, matching claudecode: the pack is codebase-agnostic, so
// installing it per-repo would commit 21 identical SKILL.md files into
// every repository `gortex init` touches.
//
// An existing file is never overwritten — a user who tuned a playbook's
// description or trimmed its steps keeps that work across re-installs.
//
// `allowed` is nil here: install materialises the whole pack, and
// `gortex instructions switch` narrows it afterwards through SyncSkills.
// Both go through the same reconciler so an install and a profile switch
// can never disagree about what belongs on disk.
func installCuratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	out, err := syncCuratedSkills(env.Stderr, curatedSkillsRoot(env), nil, opts)
	if err != nil {
		// Partial progress is still reported: a single unreadable skill
		// must not hide the ones that did land.
		internalutil.Warnf(env.Stderr, "copilot-cli skills: %v", err)
	}
	return out
}

// SyncSkills reshapes an ALREADY-INSTALLED ~/.copilot/skills tree to the
// allowed subset (nil = every shipped skill). This is the entry point
// `gortex instructions switch` drives.
//
// A missing skills root is a silent no-op, not an install: switching a
// profile must never conjure a Copilot CLI surface on a machine that
// never ran `gortex install`, and most machines carry only one or two of
// the supported hosts.
//
// home is taken rather than an agents.Env so every host's switch entry
// point has the same shape; it is wrapped back into an Env because
// $COPILOT_HOME may relocate the whole config directory, and that
// override is only honoured for the machine's real home.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := curatedSkillsRoot(agents.Env{Home: home, Stderr: w})
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	return syncCuratedSkills(w, root, allowed, opts)
}

// syncCuratedSkills is the one reconciler both entry points reach. The
// deletion rule (never remove a customised file) lives in skillpack, not
// here, so all four skill-aware hosts enforce it identically.
func syncCuratedSkills(w io.Writer, root string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if root == "" {
		return nil, nil
	}
	return skillpack.Sync(w, skillpack.SyncSpec{
		Dir:         root,
		FileName:    skillFileName,
		Rendered:    CuratedSkills(),
		KnownHashes: skillpack.PreWorktreeClaudeSkillHashes(),
	}, allowed, opts)
}

// installGeneratedSkills writes the per-community skills the caller
// generated from the graph to <root>/.github/skills/.
//
// ModeProject only, and owned end-to-end (regenerated every run) because
// their content tracks the current graph — unlike the curated pack,
// there is no stable body for a user to have customised.
func installGeneratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			// The CLI rejects a non-kebab-case skill name at load time and
			// says nothing — the skill simply never appears. Warn here so
			// the generator's mistake is visible at install time instead.
			internalutil.Warnf(env.Stderr, "skipping generated skill %q: Copilot CLI requires a lowercase, hyphenated name", s.DirName)
			continue
		}
		path := generatedSkillPath(env.Root, s.DirName)
		action, err := agents.WriteOwnedFile(env.Stderr, path, s.Content, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not write generated skill %s: %v", s.DirName, err)
			continue
		}
		out = append(out, action)
	}
	return out
}
