package skillpack_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

type specLister skillpack.SyncSpec

func (s specLister) OwnedFiles(agents.Env) []agents.OwnedFile {
	return skillpack.OwnedFiles(skillpack.SyncSpec(s))
}

// The refresh treats a skill as Gortex's on Sync's terms: a body an earlier
// release shipped is refreshed, an edited one is kept with the note that
// install would keep it too, and an absent one is not installed. DirFor
// (Hermes' category folders) is honoured.
func TestOwnedFilesFollowSyncOwnership(t *testing.T) {
	dir := t.TempDir()
	category := filepath.Join(dir, "category")
	rendered := threeSkills()
	old := "---\nname: gortex-explore\n---\nan older release's explore\n"
	spec := specFor(dir, rendered)
	spec.KnownHashes = map[string][]string{"gortex-explore": {fmt.Sprintf("%x", sha256.Sum256([]byte(old)))}}
	spec.DirFor = func(id string) string { return filepath.Join(category, id) }

	shipped := seedSkill(t, category, "gortex-explore", old)
	edited := seedSkill(t, category, "gortex-guide", "my own guide\n")
	absent := filepath.Join(category, "gortex-debug", skillFile)

	entries, _, err := agents.RefreshOwnedFiles([]agents.NamedLister{{Name: "host", Lister: specLister(spec)}},
		agents.Env{BinaryVersion: "v2"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]agents.RefreshEntry{}
	for _, e := range entries {
		got[e.Path] = e
	}
	if len(entries) != 2 || got[shipped].Status != agents.RefreshRewritten || got[edited].Status != agents.RefreshUnrecognized {
		t.Fatalf("entries = %+v, want the shipped skill refreshed and the edited one kept", entries)
	}
	if readFileString(t, shipped) != rendered["gortex-explore"] {
		t.Fatal("shipped skill was not brought up to date")
	}
	if readFileString(t, edited) != "my own guide\n" || got[edited].Note != skillpack.KeptSkillNote {
		t.Fatalf("edited skill: %+v", got[edited])
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("refresh installed a skill that was not there: %v", err)
	}
}
