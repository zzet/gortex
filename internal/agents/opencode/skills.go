package opencode

// skills.go installs the Gortex playbooks as OpenCode skills and the
// Gortex slash-command pack as OpenCode commands.
//
// # `name` must equal the directory, or the skill is dropped
//
// OpenCode (1.18.18) globs `{skill,skills}/**/SKILL.md`, reads the
// frontmatter, and rejects the skill outright when `name` is absent, is
// not `^[a-z0-9]+(-[a-z0-9]+)*$`, or does not equal the name of the
// directory holding the SKILL.md. The rejection is silent — the skill
// simply never appears — so this file derives both halves from the same
// value (skillpack.Skill.ID) rather than trusting the frontmatter that
// arrived from the Claude source, and skills_test.go asserts the
// equality over the whole corpus rather than a sample.
//
// # Only five frontmatter keys are recognised
//
// `name`, `description`, `license`, `compatibility`, `metadata`. Anything
// else is ignored, so the envelope is held to `name` + `description`:
// every extra key is one more thing to keep in sync for no behaviour.
//
// # Plural directory spellings
//
// The loader accepts both `skill/` and `skills/` (and `command/` /
// `commands/`); the vendor docs use the plural, so we write the plural.
// The one exception is the plugin, which goes in `plugin/` — see
// plugin.go for why that one is singular.
//
// # Global root: ~/.config/opencode
//
// OpenCode also reads `~/.claude/skills` and `~/.agents/skills`, and the
// codex adapter already fills the latter. We still write our own copy
// under `~/.config/opencode/skills`: the `.agents` pack only exists when
// Codex was detected on this machine, so relying on it would make the
// OpenCode install silently depend on an unrelated agent being present.
// The rendered bytes are identical to codex's (same two frontmatter keys,
// same body), so a user with both hosts sees two byte-identical copies of
// each skill rather than two that can drift.
//
// The `.config` path is joined onto Env.Home rather than resolved through
// XDG_CONFIG_HOME, matching this adapter's own Detect and every other
// adapter in this tree.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// skillFileName is the file OpenCode loads inside each skill directory.
const skillFileName = "SKILL.md"

// commandFileExt is the extension OpenCode globs for commands. The file's
// base name *is* the command name — `gortex-explore.md` is invoked as
// `/gortex-explore` — so nothing inside the file names it.
const commandFileExt = ".md"

// globalConfigDir is OpenCode's user-level config root.
func globalConfigDir(home string) string {
	return filepath.Join(home, ".config", "opencode")
}

func globalSkillsDir(home string) string   { return filepath.Join(globalConfigDir(home), "skills") }
func globalCommandsDir(home string) string { return filepath.Join(globalConfigDir(home), "commands") }

// projectSkillsDir is the repo-local skills root. Generated community
// skills land here because they describe *this* repo and are worthless
// machine-wide.
func projectSkillsDir(root string) string {
	return filepath.Join(root, ".opencode", "skills")
}

// Skills returns the curated Gortex playbook pack in host-neutral form.
//
// The bodies are single-sourced in internal/agents/claudecode and each
// host re-wraps them in its own frontmatter dialect; skillpack does the
// stripping. Reading claudecode's map directly from an adapter is the
// established direction (codex and hermes do the same) — claudecode never
// imports another adapter, so there is no cycle to trip over.
func Skills() ([]skillpack.Skill, error) {
	return skillpack.ParseAll(claudecode.GlobalSkills)
}

// Commands returns the slash-command pack in host-neutral form, keyed by
// the command name (the file's base name, minus `.md`).
//
// The source is claudecode.GlobalSkills rather than
// claudecode.SlashCommands wherever both carry the same id: the two hold
// the same body, but only the skill map wraps it in frontmatter, and that
// frontmatter is where the description lives. A command with no skill
// twin falls back to its bare body and ships without a description.
func Commands() ([]skillpack.Skill, error) {
	raw := make(map[string]string, len(claudecode.SlashCommands))
	for file, body := range claudecode.SlashCommands {
		id := strings.TrimSuffix(file, commandFileExt)
		if described, ok := claudecode.GlobalSkills[id]; ok {
			body = described
		}
		raw[id] = body
	}
	return skillpack.ParseAll(raw)
}

// renderSkill puts OpenCode's frontmatter envelope in front of a neutral
// skill body.
//
// `name` is taken from the ID, not from Skill.Name: the ID is also the
// directory this file is written into, and OpenCode drops any skill whose
// frontmatter name and containing directory disagree. Deriving both from
// one value makes that mismatch unrepresentable.
func renderSkill(s skillpack.Skill) string {
	return skillpack.RenderFrontmatter([][2]string{
		{"name", s.ID},
		{"description", s.Description},
	}) + "\n" + s.Body
}

// renderCommand puts OpenCode's command frontmatter in front of a neutral
// body. `description` is the only key we set — `agent`, `model`,
// `variant` and `subtask` all pin a command to a specific model or
// sub-agent configuration, which is the user's call, not ours.
//
// A command with no description gets no frontmatter at all rather than an
// empty `description:` line, which OpenCode would render as a blank entry
// in the command picker.
func renderCommand(c skillpack.Skill) string {
	if c.Description == "" {
		return c.Body
	}
	return skillpack.RenderFrontmatter([][2]string{
		{"description", c.Description},
	}) + "\n" + c.Body
}

// applySkills installs whichever artifact set belongs to this Env's mode.
// The sets never overlap: the curated pack is machine-wide, the generated
// community pack is repo-specific.
func applySkills(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if env.Mode == agents.ModeGlobal {
		return installCuratedPack(env, opts)
	}
	return installGeneratedSkills(env, opts), nil
}

// installCuratedPack writes the playbooks to
// `~/.config/opencode/skills/<id>/SKILL.md` and the slash commands to
// `~/.config/opencode/commands/<id>.md`.
//
// ModeGlobal only, mirroring claudecode and codex: the playbooks are
// codebase-agnostic, so a `gortex init` that dropped 41 markdown files
// into a repo would hand the user's teammates a pile of unrelated files
// to review for no benefit.
//
// An existing file that differs from the shipped body is the user's and
// is left alone (ActionSkip "customised"), the same never-clobber posture
// as claudecode's writeAgentArtifact.
//
// The skills half goes through the shared reconciler with a nil allowed
// set — install materialises the whole pack, and `gortex instructions
// switch` narrows it afterwards through SyncSkills, so the two can never
// disagree about what belongs on disk.
func installCuratedPack(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out, err := syncCuratedSkills(env.Stderr, globalSkillsDir(env.Home), nil, opts)
	if err != nil {
		return out, err
	}
	commands, err := Commands()
	if err != nil {
		return out, err
	}

	commandsRoot := globalCommandsDir(env.Home)
	for _, c := range commands {
		path := filepath.Join(commandsRoot, c.ID+commandFileExt)
		action, err := writeCuratedFile(env.Stderr, path, renderCommand(c), opts)
		if err != nil {
			return out, fmt.Errorf("command %s: %w", c.ID, err)
		}
		out = append(out, action)
	}
	return out, nil
}

// SyncSkills reshapes an ALREADY-INSTALLED `~/.config/opencode/skills`
// tree to the allowed subset (nil = every shipped skill). This is the
// entry point `gortex instructions switch` drives.
//
// A missing skills root is a silent no-op, not an install: switching a
// profile must never conjure an OpenCode surface on a machine that never
// ran `gortex install`, and most machines carry only one or two of the
// supported hosts.
//
// Only skills are reconciled, not the command pack — matching Claude
// Code, whose profile subset governs `~/.claude/skills` and leaves
// `~/.claude/commands` whole. A slash command is invoked deliberately by
// name; a skill is auto-selected into the model's context, which is the
// cost an instruction profile exists to control.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := globalSkillsDir(home)
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
		KnownHashes: skillpack.PreWorktreeAgentSkillHashes(),
	}, allowed, opts)
}

// installGeneratedSkills materialises the per-community skills the skills
// generator produced, at `<root>/.opencode/skills/<DirName>/SKILL.md`.
//
// Flat, with no `generated/` grouping level, even though the glob would
// recurse into one: the directory name is what OpenCode matches the
// frontmatter `name` against, so a `generated/` parent buys nothing and
// DirName already carries a `gortex-` prefix that keeps it clear of the
// curated pack.
//
// A DirName that is not kebab-case is skipped with a warning rather than
// written: OpenCode refuses such a name at load time, and refuses it
// silently, so the file would sit on disk looking installed forever.
//
// WriteOwnedFile (not the never-clobber writer used for the curated pack)
// because these track the current graph and must be regenerated on every
// init; it reports ActionSkip "unchanged" for byte-identical content,
// which is what keeps a re-run idempotent.
func installGeneratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	root := projectSkillsDir(env.Root)
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			internalutil.Warnf(env.Stderr, "skipping generated skill %q: OpenCode skill directories must be kebab-case", s.DirName)
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

// planSkills enumerates every skill / command file this adapter would
// write for env.
//
// Plan is what `gortex doctor` and `--print-config` read — they never
// call Apply — so a path Apply writes but Plan omits is invisible to
// both, and a user debugging a missing skill has nothing to check
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
		commands, err := Commands()
		if err != nil {
			return nil, err
		}
		out := make([]agents.FileAction, 0, len(skills)+len(commands))
		for _, s := range skills {
			out = append(out, agents.FileAction{
				Path:   filepath.Join(globalSkillsDir(env.Home), s.ID, skillFileName),
				Action: agents.ActionWouldCreate,
			})
		}
		for _, c := range commands {
			out = append(out, agents.FileAction{
				Path:   filepath.Join(globalCommandsDir(env.Home), c.ID+commandFileExt),
				Action: agents.ActionWouldCreate,
			})
		}
		return out, nil
	}
	if env.Root == "" {
		return nil, nil
	}
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			continue
		}
		out = append(out, agents.FileAction{
			Path:   filepath.Join(projectSkillsDir(env.Root), s.DirName, skillFileName),
			Action: agents.ActionWouldCreate,
		})
	}
	return out, nil
}

// writeCuratedFile installs content at path only when nothing is there,
// and never overwrites a file the user has edited.
//
// Replicated from codex's writeSkillFile rather than shared: both are
// small, and hoisting them into the agents package would make a
// never-clobber policy that each host should own its own copy of into a
// cross-adapter contract nobody asked for.
func writeCuratedFile(w io.Writer, path, content string, opts agents.ApplyOpts) (agents.FileAction, error) {
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
	internalutil.Warnf(w, "keeping customised %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customised"}, nil
}
