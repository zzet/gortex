package hermes

import (
	"io"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
	"github.com/zzet/gortex/internal/profiles"
)

// SyncSkills reconciles the installed routing skills with the active
// instruction profile's subset, the same way every other host does.
//
// Hermes was the first adapter to reuse the Claude skill bodies and the
// last to learn about profiles, so `gortex instructions switch
// localization` used to leave all twenty routing skills in place while
// shrinking Claude Code's to three — the profile was honoured on one
// host and silently ignored on another.
//
// Two Hermes shapes the shared reconciler has to be told about:
//
//   - skills live at skills/<category>/<name>/SKILL.md, not directly
//     under the root, so the directory is resolved through DirFor;
//   - the native master skill (SkillName) is not a routing skill and is
//     absent from RoutingSkills, which is what keeps it out of the
//     subset comparison. It must survive every profile, because it is
//     the skill that teaches Hermes what Gortex is at all.
//
// A machine with no Hermes skills root is a no-op rather than an
// install: a profile switch must not conjure a surface the user never
// asked `gortex install` to create.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := filepath.Join(hermesDir(home), "skills")
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	return skillpack.Sync(w, routingSkillSpec(home), allowed, opts)
}

// routingSkillSpec is the routing pack under home, shared by the profile
// switch and the post-upgrade refresh.
func routingSkillSpec(home string) skillpack.SyncSpec {
	rendered := RoutingSkills()
	return skillpack.SyncSpec{
		Dir:         filepath.Join(hermesDir(home), "skills"),
		Rendered:    rendered,
		KnownHashes: skillpack.AddWorktreeV1Hashes(rendered, profiles.WorktreeBranchRoutingPolicy, skillpack.PreWorktreeHermesSkillHashes()),
		DirFor:      func(id string) string { return filepath.Dir(skillPath(home, id)) },
	}
}

// OwnedFiles makes the routing skills part of the post-upgrade refresh. The
// native master skill is left out: it is not part of the shared pack.
func (a *Adapter) OwnedFiles(env agents.Env) []agents.OwnedFile {
	if env.Home == "" {
		return nil
	}
	return skillpack.OwnedFiles(routingSkillSpec(env.Home))
}
