package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEditProbeSixRepositoryRoutesAreExplicitAndAffectPlanIdentity(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "6", "--worktrees", "0", "--edit-count", "100", "--seed", "42", "--edit-repos", "6"}
	o, err := parseRun(args)
	if err != nil {
		t.Fatalf("explicit six-repository edit cell refused: %v", err)
	}
	corpora := editProbeRoutingCorpora()
	report, err := newRunReport(o, corpora, strings.Repeat("a", 40), Host{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report.Workload.EditProbe)
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Routes []struct {
			ID     string `json:"id"`
			Repo   string `json:"repo"`
			Commit string `json:"commit"`
			File   string `json:"file"`
			Callee string `json:"callee"`
		} `json:"routes"`
		Steps []struct {
			Ordinal    int     `json:"ordinal"`
			Editor     int     `json:"editor"`
			Route      string  `json:"route"`
			DueNS      int64   `json:"due_ns"`
			Marker     string  `json:"marker"`
			MutationID string  `json:"mutation_id"`
			Request    request `json:"request"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 6 || len(plan.Steps) != 100 {
		t.Fatalf("selected route/offer denominator lost: routes=%d steps=%d", len(plan.Routes), len(plan.Steps))
	}
	for i, route := range plan.Routes {
		c := corpora[i]
		if route.ID != c.Label || route.Repo != c.Label || route.Commit != c.Commit || route.File != c.File || route.Callee != strings.TrimPrefix(c.Symbol, c.File+"::") {
			t.Fatalf("route%d lost its pinned fixture identity: %+v", i, route)
		}
	}
	original, err := makeEditProbeTrafficPlan(corpora[0], 42, 100, 6)
	if err != nil {
		t.Fatal(err)
	}
	seenRoutes, seenMutations := map[string]bool{}, map[string]bool{}
	for ordinal, step := range plan.Steps {
		if step.Editor < 0 || step.Editor >= 6 {
			t.Fatalf("invalid fixed editor: %+v", step)
		}
		c := corpora[step.Editor]
		replacement, _ := step.Request.Arguments["replacement"].(string)
		if step.Ordinal != ordinal || step.Route != c.Label || step.DueNS != original.Steps[ordinal].DueNS || !strings.Contains(replacement, strings.TrimPrefix(c.Symbol, c.File+"::")+"()") || step.Marker == "" || step.MutationID == "" || seenMutations[step.MutationID] {
			t.Fatalf("offer was rerouted, reshaped or lost ownership: %+v", step)
		}
		seenRoutes[step.Route], seenMutations[step.MutationID] = true, true
	}
	if len(seenRoutes) != 6 {
		t.Fatal("selected repository has no offered edits", seenRoutes)
	}
	// A secondary repository's pin belongs to the offered workload identity;
	// it cannot be ignored merely because the primary fixture is unchanged.
	changed := append([]Corpus(nil), corpora...)
	changed[3].Commit = strings.Repeat("b", 40)
	other, err := newRunReport(o, changed, report.GitSHA, Host{}, t.TempDir())
	if err != nil || other.RequestPlan == report.RequestPlan {
		t.Fatalf("secondary route pin absent from plan identity: %v", err)
	}
}

func TestEditProbeSelectedCheckoutsKeepProberSessionsOnTheirEditorRoute(t *testing.T) {
	for _, sharedPrefix := range []bool{false, true} {
		name := "distinct_repositories"
		if sharedPrefix {
			name = "shared_prefix_checkouts"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cancel)
			p := &privateRun{Root: t.TempDir()}
			for i := 0; i < 2; i++ {
				c := editProbeCorpus()
				c.Label = fmt.Sprintf("route-%d", i)
				root := filepath.Join(p.Root, "repos", fmt.Sprintf("route-%d", i))
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				content := fmt.Sprintf("package sample\n// route%d\nfunc Example() int { return %d }\n%s\n", i, i, c.EditAnchor)
				if err := os.WriteFile(filepath.Join(root, c.File), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				prefix := c.Label
				if sharedPrefix {
					prefix, c.Label = "fixture", "fixture"
					if i > 0 {
						c.Label = fmt.Sprintf("linked-%d", i)
					}
				}
				p.Repos = append(p.Repos, privateRepo{Path: root, File: filepath.Join(root, c.File), Prefix: prefix, Corpus: c})
			}
			p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				target, _ := args["target"].(map[string]any)
				return stubContent(map[string]any{"cwd": frame.Cwd, "symbol": target["symbol"]}), nil
			})
			var editors []*socketClient
			for _, repo := range p.Repos {
				client, err := openMCP(ctx, p.Socket, repo.Path)
				if err != nil {
					t.Fatal(err)
				}
				editors = append(editors, client)
			}
			t.Cleanup(func() { closeClients(editors) })
			phase := &Phase{Metrics: map[string]Metric{}, Gate: map[string]json.RawMessage{}}
			if sharedPrefix {
				view, err := json.Marshal(map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "repo_prefix": "fixture", "checkout_id": "socket-linked-checkout"}})
				if err != nil {
					t.Fatal(err)
				}
				phase.Gate["linked-1.view"] = view
			}
			options := runOptions{Seed: 42, EditCount: 100, Editors: 2, Deadline: time.Second}
			if sharedPrefix {
				options.Worktrees = 1 // one linked checkout in addition to primary
			}
			runtime, probers, err := prepareEditProbe(ctx, options, p, nil, editors, phase)
			if runtime != nil {
				t.Cleanup(func() { _ = runtime.close() }) // prepared edits intentionally never run
			}
			t.Cleanup(func() { closeClients(probers) })
			if err != nil || runtime == nil || len(probers) != 100 {
				t.Fatalf("selected route setup unavailable: probers=%d err=%v", len(probers), err)
			}
			cwd := func(client *socketClient) string {
				t.Helper()
				raw, err := client.call(ctx, "tools/call", map[string]any{"name": "read", "arguments": map[string]any{"operation": "source"}})
				if err != nil {
					t.Fatal(err)
				}
				body, err := decodeEditProbeResult(raw)
				if err != nil {
					t.Fatal(err)
				}
				value, _ := body["cwd"].(string)
				return value
			}
			for _, state := range runtime.states {
				want := p.Repos[state.step.Editor].Path
				if actual := cwd(editors[state.step.Editor]); actual != want {
					t.Fatalf("editor control is on the wrong checkout: got=%s want=%s", actual, want)
				}
				if actual := cwd(state.client); actual != want {
					t.Fatalf("edit%d prober fell back to the primary checkout: editor=%d got=%s want=%s", state.step.Ordinal, state.step.Editor, actual, want)
				}
				repo := p.Repos[state.step.Editor]
				req := request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": repo.Corpus.Symbol}}}
				raw, _, err := runtime.perform(ctx, state, state.client, req, "route_source", "probe", 0)
				if err != nil {
					t.Fatal(err)
				}
				body, err := decodeEditProbeResult(raw)
				if err != nil || body["cwd"] != want || body["symbol"] != repo.Prefix+"/"+repo.Corpus.Symbol {
					t.Fatalf("same-view prober was dispatched with another repository's target: edit%d body=%v err=%v", state.step.Ordinal, body, err)
				}
			}
		})
	}
}

func TestEditProbeUnknownEditorCheckoutNeverFallsBackToPrimary(t *testing.T) {
	run := &editProbeRuntime{
		p:       &privateRun{Repos: []privateRepo{{Path: "/owned/primary", Prefix: "shared"}, {Path: "/owned/linked", Prefix: "shared"}}},
		editors: []*socketClient{{cwd: "/owned/primary"}, {cwd: "/owned/linked"}, {cwd: "/foreign"}, nil},
	}
	for _, editor := range []int{-1, 2, 3, 4} {
		if repo, err := run.repoFor(editProbeStep{Editor: editor}); err == nil {
			t.Fatalf("unknown editor %d inherited route %+v", editor, repo)
		}
	}
	for editor := range 2 {
		repo, err := run.repoFor(editProbeStep{Editor: editor})
		if err != nil || repo.Path != run.editors[editor].cwd {
			t.Fatalf("registered shared-prefix checkout lost: editor=%d repo=%+v err=%v", editor, repo, err)
		}
	}
	state := &editProbeState{step: editProbeStep{Editor: 1}}
	if _, _, err := run.perform(t.Context(), state, run.editors[0], request{}, "status", "probe", 0); err == nil {
		t.Fatal("foreign primary session accepted for linked-checkout evidence")
	}
	for i, label := range []string{"primary", "linked"} {
		run.p.Repos[i].Corpus = Corpus{Label: label, Commit: fmt.Sprintf("%040x", i+1), File: "main.go"}
		c := run.p.Repos[i].Corpus
		run.plan.Routes = append(run.plan.Routes, editProbeRoute{ID: label, Repo: label, Commit: c.Commit, File: c.File})
	}
	for _, route := range []string{"", "unknown", "primary"} {
		if _, err := run.repoFor(editProbeStep{Editor: 1, Route: route}); err == nil {
			t.Fatalf("invalid offered route %q inherited a checkout", route)
		}
	}
	if _, err := run.repoFor(editProbeStep{Editor: 1, Route: "linked"}); err != nil {
		t.Fatal(err)
	}
	run.plan.Routes[1].Commit = strings.Repeat("f", 40)
	if _, err := run.repoFor(editProbeStep{Editor: 1, Route: "linked"}); err == nil {
		t.Fatal("foreign pin inherited registered checkout")
	}
}

func editProbeRoutingCorpora() []Corpus {
	corpora := make([]Corpus, 6)
	for i := range corpora {
		c := editProbeCorpus()
		c.Label = fmt.Sprintf("route-%d", i)
		c.Source = fmt.Sprintf("/private/route-%d", i)
		c.Commit = fmt.Sprintf("%040x", i+1)
		callee := fmt.Sprintf("Example%d", i)
		c.Symbol = c.File + "::" + callee
		c.EditAnchor = "func Caller() int { return " + callee + "() }"
		c.EditTemplate = "\nfunc {{marker}}() int { return " + callee + "() }\n"
		corpora[i] = c
	}
	return corpora
}

func TestEditProbeRoutedPlansRejectMissingOrForeignOwnership(t *testing.T) {
	o := runOptions{Scenario: editProbeScenario, Seed: 42, EditCount: 100, Editors: 6, EditRepos: 6}
	for _, name := range []string{"empty", "too_few", "extra", "duplicate_label", "duplicate_source", "too_few_editors", "readers", "worktrees", "native", "cli"} {
		t.Run(name, func(t *testing.T) {
			corpora, options := editProbeRoutingCorpora(), o
			switch name {
			case "empty":
				corpora = nil
			case "too_few":
				corpora = corpora[:5]
			case "extra":
				corpora = append(corpora, corpora[0])
			case "duplicate_label":
				corpora[1].Label = corpora[0].Label
			case "duplicate_source":
				corpora[1].Source = corpora[0].Source
			case "too_few_editors":
				options.Editors = 5
			case "readers":
				options.Agents = 1
			case "worktrees":
				options.Worktrees = 1
			case "native":
				corpora[1].EditSender = editProbeShellAfterTool
			case "cli":
				corpora[1].EditOutput = "cli"
			}
			if _, err := planEditProbeRoutes(corpora, options); err == nil {
				t.Fatal("invalid route selection accepted")
			}
		})
	}
	for _, name := range []string{"missing", "unknown", "foreign_editor", "foreign_pin", "missing_metadata"} {
		t.Run(name, func(t *testing.T) {
			corpora := editProbeRoutingCorpora()
			plan, err := planEditProbeRoutes(corpora, o)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing":
				plan.Steps[0].Route = ""
			case "unknown":
				plan.Steps[0].Route = "unknown"
			case "foreign_editor":
				plan.Steps[0].Route = plan.Routes[(plan.Steps[0].Editor+1)%len(plan.Routes)].ID
			case "foreign_pin":
				corpora[1].Commit = strings.Repeat("f", 40)
			case "missing_metadata":
				plan.Routes = nil
			}
			if _, err := editProbeRouteOperations(plan, corpora); err == nil {
				t.Fatal("missing or foreign route inherited primary operations")
			}
		})
	}
}

func TestEditProbeRoutedPhaseRefusesUnsupportedTrafficBeforeAllocatingOwner(t *testing.T) {
	corpora := editProbeRoutingCorpora()
	corpora[1].EditOutput = "cli"
	o := runOptions{Scenario: editProbeScenario, EditRepos: 6, Editors: 6, EditCount: 100, Seed: 42}
	phase, _, artifacts, _, _, err := runPhase(t.Context(), o, editProbeScenario, corpora, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported") || artifacts["private_root"] != "" || phase.editProbeRuntime != nil {
		t.Fatalf("unsupported routed traffic allocated an execution owner: err=%v artifacts=%v", err, artifacts)
	}
}
