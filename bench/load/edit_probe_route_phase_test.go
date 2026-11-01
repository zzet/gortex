package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is startup/layout admission followed by an intentional pre-edit stop.
// Every offered edit stays censored; no live mutation/capacity claim is made.
func TestEditProbeMultiRootPhaseAdmitsOnlyItsPlannedOwnedCheckouts(t *testing.T) {
	for _, cell := range []struct {
		name                   string
		repos, linked, editors int
	}{
		{"six_repositories", 6, 0, 6}, {"four_total_worktrees", 1, 3, 4},
	} {
		t.Run(cell.name, func(t *testing.T) {
			t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
			var corpora []Corpus
			var originals []string
			for i := range cell.repos {
				c := gitFixture(t)
				c.Label = fmt.Sprintf("route-%d", i)
				c.EditAnchor = "func Caller() int { return Example() }"
				c.EditTemplate = "\nfunc {{marker}}() int { return Example() }\n"
				content := fmt.Sprintf("package sample\n// owner-%d\nfunc Example() int { return 1 }\n%s\n", i, c.EditAnchor)
				if err := os.WriteFile(filepath.Join(c.Source, c.File), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"add", c.File}, {"commit", "--quiet", "-m", "routing fixture"}, {"rev-parse", "HEAD"}} {
					cmd := exec.Command("git", append([]string{"-C", c.Source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
					data, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v %s", args, err, data)
					}
					if args[0] == "rev-parse" {
						c.Commit = strings.TrimSpace(string(data))
					}
				}
				corpora = append(corpora, c)
				originals = append(originals, content)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			o := runOptions{Binary: binary, Profile: "agent", Host: "native", Editors: cell.editors, EditCount: 100, EditRepos: cell.repos, Worktrees: cell.linked, Seed: 42, Duration: time.Second, Deadline: time.Second, WarmTimeout: 10 * time.Second, StopTimeout: 5 * time.Second}
			stop := errors.New("layout admitted; intentional diagnostic stop before edits")
			called := false
			o.pairedDriver = func(_ context.Context, p *privateRun, _, _ []*socketClient, phase *Phase) error {
				called = true
				run := phase.editProbeRuntime
				total := cell.repos + cell.linked
				if run == nil || len(p.Repos) != total || len(run.plan.Routes) != total || len(run.originals) != total {
					return fmt.Errorf("planned checkout denominator lost: private=%d runtime=%v", len(p.Repos), run != nil)
				}
				data, err := os.ReadFile(filepath.Join(p.Root, "config", "gortex", "config.yaml"))
				if err != nil {
					return err
				}
				var config struct {
					Repos []json.RawMessage `json:"repos"`
				}
				if err := json.Unmarshal(data, &config); err != nil {
					return err
				}
				expectedBoot := cell.repos
				if len(config.Repos) != expectedBoot {
					return fmt.Errorf("boot configured %d roots, want%d", len(config.Repos), expectedBoot)
				}
				for _, step := range run.plan.Steps {
					repo, err := run.repoFor(step)
					if err != nil {
						return err
					}
					expected := p.Repos[step.Editor%len(p.Repos)]
					if repo.Path != expected.Path || run.editorFor(step).cwd != expected.Path || run.states[step.Ordinal].client.cwd != expected.Path {
						return fmt.Errorf("ordinal%d fell back from its owned checkout", step.Ordinal)
					}
				}
				for i, repo := range p.Repos {
					current, err := os.ReadFile(repo.File)
					if err != nil || string(run.originals[repo.Path]) != string(current) {
						return fmt.Errorf("checkout%d original capture missing: %v", i, err)
					}
					if cell.linked > 0 && i > 0 {
						var receipt struct {
							View struct {
								Kind   string `json:"kind"`
								Exact  bool   `json:"exact"`
								Prefix string `json:"repo_prefix"`
							} `json:"view"`
						}
						if err := json.Unmarshal(phase.Gate[repo.Corpus.Label+".view"], &receipt); err != nil || !receipt.View.Exact || receipt.View.Kind != "worktree" || receipt.View.Prefix != repo.Prefix {
							return fmt.Errorf("checkout%d has no exact public worktree admission", i)
						}
					}
				}
				return stop
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			phase, _, artifacts, _, _, err := runPhase(ctx, o, editProbeScenario, corpora, "", t.TempDir())
			if !called || !errors.Is(err, stop) {
				t.Fatalf("owned routed phase was not admitted: called=%v err=%v", called, err)
			}
			if phase.EditProbe == nil || len(phase.EditProbe.Records) != 100 {
				t.Fatal("diagnostic stop lost offered denominator")
			}
			for _, record := range phase.EditProbe.Records {
				if record.Outcome != "phase_censored" || record.Ack.Value != nil {
					t.Fatal("pre-edit layout diagnostic fabricated work", record)
				}
			}
			forced := phase.Metrics["force_killed"]
			if forced.Value == nil || *forced.Value != 0 {
				t.Fatal("layout fixture failed normal owned stop", phase.Metrics)
			}
			if _, err := os.Stat(filepath.Join(artifacts["private_root"], "repos")); !os.IsNotExist(err) {
				t.Fatal("private checkout handles/state retained", err)
			}
			for i, c := range corpora {
				actual, err := os.ReadFile(filepath.Join(c.Source, c.File))
				if err != nil || string(actual) != originals[i] {
					t.Fatal("source fixture was mutated", i, err)
				}
			}
		})
	}
}

func TestEditProbeSourceRepositoryIdentityDoesNotCountLinkedViewsTwice(t *testing.T) {
	primary := gitFixture(t)
	before, err := os.ReadFile(filepath.Join(primary.Source, primary.File))
	if err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(t.TempDir(), "linked-source")
	cmd := exec.CommandContext(t.Context(), "git", "-C", primary.Source, "-c", "core.hooksPath="+os.DevNull, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "worktree", "add", "--detach", linkedPath, primary.Commit)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("own source worktree: %v %s", err, data)
	}
	linked := primary
	linked.Source, linked.Label = linkedPath, "linked-source"
	if err := validateEditProbeSourceRepos(t.Context(), []Corpus{primary, linked}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "distinct source repositories") {
		t.Fatalf("two source views masqueraded as independent repositories: %v", err)
	}
	independent := gitFixture(t)
	if err := validateEditProbeSourceRepos(t.Context(), []Corpus{primary, independent}, t.TempDir()); err != nil {
		t.Fatalf("independent source repositories refused: %v", err)
	}
	for _, c := range []Corpus{primary, linked} {
		after, err := os.ReadFile(filepath.Join(c.Source, c.File))
		if err != nil || string(after) != string(before) {
			t.Fatalf("read-only identity admission mutated source bytes: %v", err)
		}
	}
}
