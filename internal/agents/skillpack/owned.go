package skillpack

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
)

// KeptSkillNote tells the user how to get the shipped body of a skill the
// refresh kept: install never overwrites a skill it does not recognise, so
// the file has to go first.
const KeptSkillNote = "`gortex install` keeps an edited skill, so delete it before reinstalling."

// OwnedFiles lists spec's skills for the post-upgrade refresh (#1052). A
// skill is Gortex's on the same terms Sync uses: the body this build renders
// or one KnownHashes says an earlier release shipped. Absent skills are
// skipped by the refresh, so it never installs one the profile left out.
func OwnedFiles(spec SyncSpec) []agents.OwnedFile {
	if spec.Dir == "" || len(spec.Rendered) == 0 {
		return nil
	}
	fileName := spec.FileName
	if fileName == "" {
		fileName = defaultSkillFileName
	}
	out := make([]agents.OwnedFile, 0, len(spec.Rendered))
	for _, id := range sortedIDs(spec.Rendered) {
		content, known := spec.Rendered[id], spec.KnownHashes[id]
		out = append(out, agents.OwnedFile{
			Path: filepath.Join(spec.skillDir(id), fileName),
			Render: func(existing []byte) (string, bool) {
				return content, isShippedBody(existing, content, known)
			},
			Note: KeptSkillNote,
		})
	}
	return out
}
