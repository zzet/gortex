package antigravity

import (
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
	"github.com/zzet/gortex/internal/profiles"
)

const (
	skillsDirName = "skills"
	skillFileName = "SKILL.md"
	agentsDirName = ".agents"
)

// CuratedSkillNames returns the curated pack's skill IDs sorted for
// deterministic planning and rendering.
func CuratedSkillNames() []string {
	names := make([]string, 0, len(agents.GlobalSkills))
	for name := range agents.GlobalSkills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CuratedSkills renders agents.GlobalSkills into Antigravity's SKILL.md
// envelope (`name` + `description` frontmatter), keyed by skill ID.
func CuratedSkills() map[string]string {
	out := make(map[string]string, len(agents.GlobalSkills))
	for id, sharedBody := range agents.GlobalSkills {
		skill, err := skillpack.Parse(id, sharedBody)
		if err != nil {
			continue
		}
		out[id] = antigravitySkillFromShared(skill)
	}
	return out
}

func antigravitySkillFromShared(skill skillpack.Skill) string {
	return skillpack.RenderFrontmatter([][2]string{
		{"name", skill.ID},
		{"description", skill.Description},
	}) + skill.Body
}

func curatedSkillsRoot(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "config", skillsDirName)
}

func curatedSkillPath(home, id string) string {
	root := curatedSkillsRoot(home)
	if root == "" {
		return ""
	}
	return filepath.Join(root, id, skillFileName)
}

func generatedSkillPath(root, dirName string) string {
	return filepath.Join(root, agentsDirName, skillsDirName, dirName, skillFileName)
}

func installCuratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	out, err := syncCuratedSkills(env.Stderr, curatedSkillsRoot(env.Home), nil, opts)
	if err != nil {
		internalutil.Warnf(env.Stderr, "antigravity skills: %v", err)
	}
	return out
}

// SyncSkills reshapes an already-installed ~/.gemini/config/skills tree
// to the allowed subset (nil = every shipped skill). Driven by
// `gortex instructions switch`.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := curatedSkillsRoot(home)
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	return syncCuratedSkills(w, root, allowed, opts)
}

func syncCuratedSkills(w io.Writer, root string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if root == "" {
		return nil, nil
	}
	return skillpack.Sync(w, skillpack.SyncSpec{
		Dir:         root,
		FileName:    skillFileName,
		Rendered:    CuratedSkills(),
		KnownHashes: skillpack.AddWorktreeV1Hashes(CuratedSkills(), profiles.WorktreeBranchRoutingPolicy, skillpack.PreWorktreeClaudeSkillHashes()),
	}, allowed, opts)
}

func installGeneratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			internalutil.Warnf(env.Stderr, "skipping generated skill %q: Antigravity requires a lowercase, hyphenated name", s.DirName)
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
