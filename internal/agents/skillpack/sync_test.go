// sync_test.go stays in the external test package for the same reason
// skillpack_test.go does: skillpack must never depend on an adapter, and
// keeping the tests outside the package means an adapter that imports
// skillpack can still be imported here without closing a cycle.
package skillpack_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/hermes"
	"github.com/zzet/gortex/internal/agents/skillpack"
	"github.com/zzet/gortex/internal/profiles"
)

const skillFile = "SKILL.md"

// threeSkills is the rendered pack the table below reconciles: small
// enough to assert on exhaustively, big enough that a subset is a real
// subset.
func threeSkills() map[string]string {
	return map[string]string{
		"gortex-explore": "---\nname: gortex-explore\n---\nexplore\n",
		"gortex-guide":   "---\nname: gortex-guide\n---\nguide\n",
		"gortex-debug":   "---\nname: gortex-debug\n---\ndebug\n",
	}
}

func specFor(dir string, rendered map[string]string) skillpack.SyncSpec {
	return skillpack.SyncSpec{Dir: dir, FileName: skillFile, Rendered: rendered}
}

// seedSkill materialises one skill on disk and returns its path.
func seedSkill(t *testing.T, dir, id, content string) string {
	t.Helper()
	path := filepath.Join(dir, id, skillFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// installedIDs lists the skill directories that still hold a SKILL.md.
// Directory presence alone is not the question a profile switch asks —
// the skill is what the host loads.
func installedIDs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), skillFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// actionsByID keys a Sync result by the skill directory name, which is
// the only stable identity across hosts with different roots.
func actionsByID(actions []agents.FileAction) map[string]agents.FileAction {
	out := make(map[string]agents.FileAction, len(actions))
	for _, a := range actions {
		out[filepath.Base(filepath.Dir(a.Path))] = a
	}
	return out
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestSyncReconciliation is the table for the whole decision matrix.
// Each case seeds a tree, runs one Sync, and asserts both the reported
// action and what is left on disk — a reconciler that reports the right
// action while leaving the wrong bytes is the failure mode that matters.
func TestSyncReconciliation(t *testing.T) {
	rendered := threeSkills()
	const customised = "---\nname: gortex-guide\n---\nMY OWN STEPS\n"

	for _, tc := range []struct {
		name string
		// seed runs before Sync and returns nothing; the tree it builds
		// is the precondition under test.
		seed       func(t *testing.T, dir string)
		allowed    []string
		opts       agents.ApplyOpts
		wantAction map[string]agents.ActionKind
		wantOnDisk []string
		// wantBytes pins files whose content must survive verbatim.
		wantBytes map[string]string
	}{
		{
			name:    "install when missing",
			seed:    func(*testing.T, string) {},
			allowed: nil,
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionCreate,
				"gortex-guide":   agents.ActionCreate,
				"gortex-debug":   agents.ActionCreate,
			},
			wantOnDisk: []string{"gortex-debug", "gortex-explore", "gortex-guide"},
		},
		{
			name: "skip when identical",
			seed: func(t *testing.T, dir string) {
				for id, body := range rendered {
					seedSkill(t, dir, id, body)
				}
			},
			allowed: nil,
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionSkip,
				"gortex-guide":   agents.ActionSkip,
				"gortex-debug":   agents.ActionSkip,
			},
			wantOnDisk: []string{"gortex-debug", "gortex-explore", "gortex-guide"},
		},
		{
			name: "delete when outside profile and pristine",
			seed: func(t *testing.T, dir string) {
				for id, body := range rendered {
					seedSkill(t, dir, id, body)
				}
			},
			allowed: []string{"gortex-explore"},
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionSkip,
				"gortex-guide":   agents.ActionDelete,
				"gortex-debug":   agents.ActionDelete,
			},
			wantOnDisk: []string{"gortex-explore"},
		},
		{
			// The load-bearing case: enforcing a profile is worth
			// deleting files Gortex wrote, never a playbook the user
			// rewrote.
			name: "keep when outside profile and customised",
			seed: func(t *testing.T, dir string) {
				for id, body := range rendered {
					seedSkill(t, dir, id, body)
				}
				seedSkill(t, dir, "gortex-guide", customised)
			},
			allowed: []string{"gortex-explore"},
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionSkip,
				"gortex-guide":   agents.ActionSkip,
				"gortex-debug":   agents.ActionDelete,
			},
			wantOnDisk: []string{"gortex-explore", "gortex-guide"},
			wantBytes:  map[string]string{"gortex-guide": customised},
		},
		{
			name: "keep customised inside profile",
			seed: func(t *testing.T, dir string) {
				seedSkill(t, dir, "gortex-guide", customised)
			},
			allowed: nil,
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionCreate,
				"gortex-guide":   agents.ActionSkip,
				"gortex-debug":   agents.ActionCreate,
			},
			wantOnDisk: []string{"gortex-debug", "gortex-explore", "gortex-guide"},
			wantBytes:  map[string]string{"gortex-guide": customised},
		},
		{
			// A skill that was never installed cannot be pruned, and the
			// attempt must not create the directory it is refusing to
			// keep.
			name:    "outside profile and never installed",
			seed:    func(*testing.T, string) {},
			allowed: []string{"gortex-explore"},
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionCreate,
				"gortex-guide":   agents.ActionSkip,
				"gortex-debug":   agents.ActionSkip,
			},
			wantOnDisk: []string{"gortex-explore"},
		},
		{
			name:    "dry run plans an install without writing",
			seed:    func(*testing.T, string) {},
			allowed: nil,
			opts:    agents.ApplyOpts{DryRun: true},
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionWouldCreate,
				"gortex-guide":   agents.ActionWouldCreate,
				"gortex-debug":   agents.ActionWouldCreate,
			},
			wantOnDisk: nil,
		},
		{
			name: "dry run plans a prune without deleting",
			seed: func(t *testing.T, dir string) {
				for id, body := range rendered {
					seedSkill(t, dir, id, body)
				}
			},
			allowed: []string{"gortex-explore"},
			opts:    agents.ApplyOpts{DryRun: true},
			wantAction: map[string]agents.ActionKind{
				"gortex-explore": agents.ActionSkip,
				"gortex-guide":   agents.ActionWouldDelete,
				"gortex-debug":   agents.ActionWouldDelete,
			},
			wantOnDisk: []string{"gortex-debug", "gortex-explore", "gortex-guide"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "skills")
			tc.seed(t, dir)

			var log strings.Builder
			actions, err := skillpack.Sync(&log, specFor(dir, rendered), tc.allowed, tc.opts)
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if len(actions) != len(rendered) {
				t.Fatalf("Sync reported %d actions, want one per shipped skill (%d)", len(actions), len(rendered))
			}

			got := actionsByID(actions)
			for id, want := range tc.wantAction {
				if got[id].Action != want {
					t.Errorf("%s: action = %q (reason %q), want %q", id, got[id].Action, got[id].Reason, want)
				}
			}

			onDisk := installedIDs(t, dir)
			if strings.Join(onDisk, ",") != strings.Join(tc.wantOnDisk, ",") {
				t.Errorf("installed = %v, want %v", onDisk, tc.wantOnDisk)
			}
			for id, want := range tc.wantBytes {
				if got := readFileString(t, filepath.Join(dir, id, skillFile)); got != want {
					t.Errorf("%s bytes changed:\n%s", id, got)
				}
			}
		})
	}
}

// TestSyncActionsAreIDOrdered pins the ordering the install report, the
// plan and every golden test depend on. Go randomises map iteration, so
// without an explicit sort this passes on most runs and fails on some.
func TestSyncActionsAreIDOrdered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	actions, err := skillpack.Sync(nil, specFor(dir, threeSkills()), nil, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	want := []string{"gortex-debug", "gortex-explore", "gortex-guide"}
	for i, a := range actions {
		if got := filepath.Base(filepath.Dir(a.Path)); got != want[i] {
			t.Fatalf("action %d = %s, want %s (actions must be ID-sorted)", i, got, want[i])
		}
	}
}

// TestSyncIsIdempotent: `gortex install` re-runs after every upgrade and
// `instructions switch` is re-runnable by hand, so a second pass must
// report nothing but skips and touch no file.
func TestSyncIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	rendered := threeSkills()
	allowed := []string{"gortex-explore"}

	if _, err := skillpack.Sync(nil, specFor(dir, rendered), allowed, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	before := installedIDs(t, dir)

	actions, err := skillpack.Sync(nil, specFor(dir, rendered), allowed, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	for _, a := range actions {
		if a.Action != agents.ActionSkip {
			t.Errorf("re-run reported %s for %s, want skip", a.Action, a.Path)
		}
	}
	if after := installedIDs(t, dir); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("re-run changed the tree: %v -> %v", before, after)
	}
}

// TestSyncCustomisedFileSurvivesEveryPass is the guard against the worst
// outcome this code can produce: silently eating a user's work over
// repeated upgrades. One pass keeping the file is not enough — the bug
// class is a reconciler that keeps it once and prunes it on the next run
// because it now compares against different bytes.
func TestSyncCustomisedFileSurvivesEveryPass(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	rendered := threeSkills()
	const mine = "---\nname: gortex-debug\n---\nmine, hands off\n"
	path := seedSkill(t, dir, "gortex-debug", mine)

	for pass := range 3 {
		if _, err := skillpack.Sync(nil, specFor(dir, rendered), []string{"gortex-explore"}, agents.ApplyOpts{}); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := readFileString(t, path); got != mine {
			t.Fatalf("pass %d rewrote a customised skill:\n%s", pass, got)
		}
	}
}

// TestSyncWarnsWhenKeepingCustomisedSkill: the file surviving is only
// half the contract. A user who asked for a three-skill profile and got
// four needs to be told which one refused to go and why.
func TestSyncWarnsWhenKeepingCustomisedSkill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	rendered := threeSkills()
	seedSkill(t, dir, "gortex-guide", "mine\n")

	var log strings.Builder
	if _, err := skillpack.Sync(&log, specFor(dir, rendered), []string{"gortex-explore"}, agents.ApplyOpts{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !strings.Contains(log.String(), "gortex-guide") || !strings.Contains(log.String(), "customised") {
		t.Fatalf("expected a warning naming the kept skill, got:\n%s", log.String())
	}
}

// TestSyncKnownHashesTreatAnOldBodyAsOurs covers the migration seam: a
// SKILL.md left over from an earlier release differs from what this
// build renders, but it is still Gortex's file — refreshable in profile,
// prunable out of it. Without this, every upgrade would classify the
// whole pack as "customised" and freeze it forever.
func TestSyncKnownHashesTreatAnOldBodyAsOurs(t *testing.T) {
	rendered := threeSkills()
	const oldBody = "---\nname: gortex-guide\n---\nthe v1 wording\n"
	oldHash := fmt.Sprintf("%x", sha256.Sum256([]byte(oldBody)))

	spec := func(dir string) skillpack.SyncSpec {
		s := specFor(dir, rendered)
		s.KnownHashes = map[string][]string{"gortex-guide": {"unused", oldHash}}
		return s
	}

	t.Run("in profile: refreshed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		path := seedSkill(t, dir, "gortex-guide", oldBody)
		if _, err := skillpack.Sync(nil, spec(dir), nil, agents.ApplyOpts{}); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if got := readFileString(t, path); got != rendered["gortex-guide"] {
			t.Fatalf("a previous release's body was not refreshed:\n%s", got)
		}
	})

	t.Run("outside profile: pruned", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		seedSkill(t, dir, "gortex-guide", oldBody)
		if _, err := skillpack.Sync(nil, spec(dir), []string{"gortex-explore"}, agents.ApplyOpts{}); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if installed := installedIDs(t, dir); len(installed) != 1 || installed[0] != "gortex-explore" {
			t.Fatalf("installed = %v, want [gortex-explore]", installed)
		}
	})

	t.Run("one byte customization is preserved", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		custom := oldBody + "x"
		path := seedSkill(t, dir, "gortex-guide", custom)
		actions, err := skillpack.Sync(nil, spec(dir), nil, agents.ApplyOpts{})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if got := readFileString(t, path); got != custom {
			t.Fatalf("customized body overwritten: %q", got)
		}
		if got := actionsByID(actions)["gortex-guide"].Reason; got != "customised" {
			t.Fatalf("reason = %q, want customised", got)
		}
	})
}

func BenchmarkSyncTwentyOneUnchangedSkills(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "skills")
	rendered := make(map[string]string, 21)
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("gortex-skill-%02d", i)
		rendered[id] = fmt.Sprintf("---\nname: %s\n---\nbody\n", id)
		path := filepath.Join(dir, id, skillFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rendered[id]), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	spec := specFor(dir, rendered)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := skillpack.Sync(nil, spec, nil, agents.ApplyOpts{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHistoricalHashSetsAreDefensiveAndComplete(t *testing.T) {
	for name, hashes := range map[string]map[string][]string{
		"agent":  skillpack.PreWorktreeAgentSkillHashes(),
		"claude": skillpack.PreWorktreeClaudeSkillHashes(),
		"hermes": skillpack.PreWorktreeHermesSkillHashes(),
	} {
		want := 21
		if name == "hermes" {
			want = 20
		}
		if len(hashes) != want {
			t.Errorf("%s hashes = %d, want %d", name, len(hashes), want)
		}
	}
	first := skillpack.PreWorktreeAgentSkillHashes()
	first["gortex-guide"][0] = "mutated"
	if skillpack.PreWorktreeAgentSkillHashes()["gortex-guide"][0] == "mutated" {
		t.Fatal("historical hashes leaked mutable package state")
	}
}

func TestAddWorktreeV1HashesAddsOwnershipWithoutMutatingPriorCatalog(t *testing.T) {
	prior := skillpack.PreWorktreeClaudeSkillHashes()
	rendered := map[string]string{"gortex-guide": "before\n" + profiles.WorktreeBranchRoutingPolicy + "after\n"}
	got := skillpack.AddWorktreeV1Hashes(rendered, profiles.WorktreeBranchRoutingPolicy, prior)
	if len(got["gortex-guide"]) != 2 {
		t.Fatalf("hashes = %v, want pre-worktree + worktree-v1", got["gortex-guide"])
	}
	if len(prior["gortex-guide"]) != 1 {
		t.Fatal("prior catalog was mutated")
	}
	if got["gortex-guide"][1] != skillpack.WorktreeV1BodyHash(rendered["gortex-guide"], profiles.WorktreeBranchRoutingPolicy) {
		t.Fatal("catalog and single-body worktree-v1 hashes disagree")
	}
}

func TestPreWorktreeHashesMatchImmediatelyPriorShippedBodies(t *testing.T) {
	policy := "\n## Worktree and branch routing\n\n" + profiles.WorktreeBranchRoutingPolicy
	assertHashes := func(name string, current map[string]string, hashes map[string][]string) {
		t.Helper()
		for id, body := range current {
			old := strings.Replace(body, policy, "", 1)
			if old == body {
				t.Errorf("%s/%s: policy block not found", name, id)
				continue
			}
			got := fmt.Sprintf("%x", sha256.Sum256([]byte(old)))
			if len(hashes[id]) != 1 || hashes[id][0] != got {
				t.Errorf("%s/%s: historical hash = %v, want %s", name, id, hashes[id], got)
			}
		}
	}
	assertHashes("claude", claudecode.GlobalSkills, skillpack.PreWorktreeClaudeSkillHashes())
	assertHashes("hermes", hermes.RoutingSkills(), skillpack.PreWorktreeHermesSkillHashes())
	agent := make(map[string]string)
	for id, raw := range claudecode.GlobalSkills {
		s, err := skillpack.Parse(id, raw)
		if err != nil {
			t.Fatal(err)
		}
		agent[id] = skillpack.RenderFrontmatter([][2]string{{"name", s.Name}, {"description", s.Description}}) + "\n" + s.Body
	}
	assertHashes("agent", agent, skillpack.PreWorktreeAgentSkillHashes())
}

// TestSyncEmptyAllowedRemovesEverything separates "nil = no filtering"
// from "empty = keep none". They are one typo apart at every call site,
// and confusing them either wipes a pack or ignores a profile.
func TestSyncEmptyAllowedRemovesEverything(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	rendered := threeSkills()
	for id, body := range rendered {
		seedSkill(t, dir, id, body)
	}
	if _, err := skillpack.Sync(nil, specFor(dir, rendered), []string{}, agents.ApplyOpts{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if installed := installedIDs(t, dir); len(installed) != 0 {
		t.Fatalf("installed = %v, want none", installed)
	}
}

// TestSyncPruneRemovesTheWholeSkillDirectory: a skill directory may hold
// reference files beside its SKILL.md, and leaving those behind would
// keep the host advertising a half-deleted skill.
func TestSyncPruneRemovesTheWholeSkillDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	rendered := threeSkills()
	seedSkill(t, dir, "gortex-guide", rendered["gortex-guide"])
	extra := filepath.Join(dir, "gortex-guide", "reference.md")
	if err := os.WriteFile(extra, []byte("aux\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := skillpack.Sync(nil, specFor(dir, rendered), []string{"gortex-explore"}, agents.ApplyOpts{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gortex-guide")); !os.IsNotExist(err) {
		t.Fatalf("skill directory survived the prune (stat err = %v)", err)
	}
}

// TestSyncWithoutADirIsANoOp: a host with no resolvable home is simply
// not installed, and a profile switch must not turn that into an error.
func TestSyncWithoutADirIsANoOp(t *testing.T) {
	actions, err := skillpack.Sync(nil, skillpack.SyncSpec{Rendered: threeSkills()}, nil, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
}

// TestSyncDefaultsToSkillMd: FileName is a field so a future host can
// rename the file, but every host today ships the Agent Skills spelling
// and should not have to say so.
func TestSyncDefaultsToSkillMd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	spec := skillpack.SyncSpec{Dir: dir, Rendered: threeSkills()}
	if _, err := skillpack.Sync(nil, spec, nil, agents.ApplyOpts{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gortex-explore", skillFile)); err != nil {
		t.Fatalf("default file name is not %s: %v", skillFile, err)
	}
}
