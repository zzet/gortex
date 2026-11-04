package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type smokeTaskReceipt struct {
	Scenario string    `json:"scenario"`
	Role     string    `json:"role"`
	Repeat   int       `json:"repeat"`
	Seed     int64     `json:"seed"`
	Report   string    `json:"report"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Error    string    `json:"error,omitempty"`
}

// smokeCommand is deliberately fixed to two fresh owned stores and three
// rotated five-step sequences. It accepts no borrowed path or resume API.
func smokeCommand(ctx context.Context, args []string) (result error) {
	f := flag.NewFlagSet("load smoke", flag.ContinueOnError)
	mainBinary := f.String("main-binary", "", "exact-main daemon")
	mainRevision := f.String("main-revision", "", "full exact-main revision")
	candidateBinary := f.String("candidate-binary", "", "clean candidate daemon")
	candidateRevision := f.String("candidate-revision", "", "full candidate revision")
	manifest := f.String("manifest", "", "one pinned contemporary corpus")
	out := f.String("out", "", "NEW owned stores/report directory outside source")
	editACK := f.String("edit-ack", "", "explicit local or full owned edit policy; omission preserves the product default")
	retainFailures := f.Bool("retain-read-source-failures", false, "opt-in private bounded raw wire evidence for failed read.source attempts")
	offer := f.Duration("offer", 20*time.Second, "version 2 PR smoke offered window: 20s")
	deadline := f.Duration("deadline", 120*time.Second, "original product deadline")
	warm := f.Duration("warm-timeout", 5*time.Minute, "each ready/enriched/public-analysis gate budget")
	stop := f.Duration("stop-timeout", 30*time.Second, "owned normal-stop budget")
	sweep := f.Duration("sweep-duration", 2*time.Minute, "all 21 operations, one repetition")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *editACK != "" && *editACK != "local" && *editACK != "full" {
		return errors.New("smoke edit ACK mode must be local or full when explicit")
	}
	if f.NArg() != 0 || *manifest == "" || *out == "" || *offer != 20*time.Second || *deadline != 120*time.Second || *warm <= 0 || *stop <= 0 || *stop > 2*time.Minute || *sweep <= 0 {
		return errors.New("version 2 smoke requires fixed two-build inputs, one corpus, original 20s offers/120s product budgets and bounded positive safety budgets")
	}
	builds := map[string]buildSpec{"main": {label: "main", binary: *mainBinary, sha: *mainRevision}, "candidate": {label: "candidate", binary: *candidateBinary, sha: *candidateRevision}}
	for role, build := range builds {
		if build.binary == "" || !commitPattern.MatchString(build.sha) {
			return fmt.Errorf("smoke lacks pinned %s binary", role)
		}
		actual, err := binaryRevision(build.binary, build.sha)
		if err != nil {
			return fmt.Errorf("smoke %s binary differs from pinned revision: %w", role, err)
		}
		if actual != build.sha {
			return fmt.Errorf("smoke %s binary differs from pinned revision", role)
		}
	}
	corpora, err := loadManifest(*manifest)
	if err != nil {
		return err
	}
	if len(corpora) != 1 {
		return errors.New("PR smoke requires exactly one pinned corpus")
	}
	if err := validateOutputRoots(corpora, *out); err != nil {
		return err
	}
	if err := os.Mkdir(*out, 0o700); err != nil {
		return err
	}
	host, group, err := inspectHost("native", *out)
	if err != nil {
		return err
	}
	states := map[string]*privateRun{}
	markers := map[string][]string{}
	var tasks []smokeTaskReceipt
	started := time.Now().UTC()
	defer func() {
		starts := map[string]int{}
		for _, role := range []string{"main", "candidate"} {
			p := states[role]
			if p == nil {
				continue
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), *stop)
			_, forced, stopErr := p.stop(stopCtx)
			cancel()
			if forced {
				stopErr = errors.Join(stopErr, errors.New("final smoke cleanup force-killed an owned daemon"))
			}
			starts[role] = p.startCount
			result = errors.Join(result, stopErr, os.RemoveAll(p.SocketDir), p.removeState())
		}
		value := map[string]any{"schema_version": 1, "started": started, "finished": time.Now().UTC(), "tasks": tasks, "fresh_store_count": len(states), "process_starts": starts, "complete": result == nil}
		if *editACK != "" {
			value["edit_ack_mode"] = *editACK
		}
		if result != nil {
			value["error"] = result.Error()
		}
		data, marshalErr := json.MarshalIndent(value, "", "  ")
		result = errors.Join(result, marshalErr)
		if marshalErr == nil {
			result = errors.Join(result, os.WriteFile(filepath.Join(*out, "sequence.json"), append(data, '\n'), 0o600))
		}
	}()
	for _, role := range []string{"main", "candidate"} {
		p, err := newPrivateRun(ctx, *out, builds[role].binary, corpora)
		if err != nil {
			return err
		}
		states[role] = p
	}
	steps := []struct {
		name, wire       string
		readers, editors int
	}{{"load-16", "read", 16, 0}, {"loadedit-4", "edit", 0, 4}, {"tools-single-gortex", "sweep", 1, 0}, {"warm-restart", "restart", 1, 0}, {"idle-stop", "idle-stop", 0, 0}}
	for repeat := 1; repeat <= 3; repeat++ {
		roles := []string{"main", "candidate"}
		if repeat%2 == 0 {
			slices.Reverse(roles)
		}
		for _, role := range roles {
			for _, step := range steps {
				if err := ctx.Err(); err != nil {
					return errors.Join(result, err)
				}
				root := filepath.Join(*out, fmt.Sprintf("%s-%03d-%s", step.name, repeat, role))
				if err := os.Mkdir(root, 0o700); err != nil {
					return errors.Join(result, err)
				}
				build := builds[role]
				o := runOptions{RetainReadSourceFailures: *retainFailures, EditACKMode: *editACK, Binary: build.binary, GitSHA: build.sha, Manifest: *manifest, Scratch: *out, Output: filepath.Join(root, "report.json"), Profile: "agent", Host: "native", Scenario: step.wire, Agents: step.readers, Editors: step.editors, Seed: int64(repeat), Duration: *offer, Deadline: *deadline, WarmTimeout: *warm, StopTimeout: *stop, SweepBudget: *sweep, SweepRepetitions: 1, smokeState: states[role], smokeMarkers: markers[role]}
				if step.wire == "read" {
					o.PRSmokeDrain = *deadline
				}
				if step.wire == "edit" {
					o.PRSmokeDrain, o.PRSmokeDrainVersion = *deadline, 2
				}
				if err := validateRunOptions(o); err != nil {
					return errors.Join(result, err)
				}
				tr := smokeTaskReceipt{Scenario: step.name, Role: role, Repeat: repeat, Seed: int64(repeat), Started: time.Now().UTC()}
				report, err := newRunReport(o, corpora, build.sha, host, root)
				if err != nil {
					return errors.Join(result, err)
				}
				phase, sweepRows, artifacts, crashes, version, runErr := runPhase(withReadSourceFailureEvidence(ctx, *retainFailures), o, step.wire, corpora, group, root)
				addPhase(&report, phase, sweepRows, artifacts, crashes, version, runErr)
				runErr = errors.Join(runErr, finishReport(&report, 1))
				path := filepath.Join(root, "report.json")
				runErr = errors.Join(runErr, writeReport(path, report))
				tr.Report, _ = filepath.Rel(*out, path)
				tr.Finished = time.Now().UTC()
				if runErr != nil {
					tr.Error = runErr.Error()
					result = errors.Join(result, runErr)
				}
				tasks = append(tasks, tr)
				if step.wire == "edit" {
					for _, edit := range phase.Edits {
						markers[role] = append(markers[role], edit.Marker)
					}
					if err := restoreSmokeClone(ctx, states[role]); err != nil {
						// No subsequent step may see un-restored source.
						return errors.Join(result, err)
					}
				}
			}
		}
	}
	return result
}

func validateSmokeOwner(p *privateRun, binary string, corpora []Corpus) error {
	if p.Binary != binary || p.process != nil || len(p.Repos) != len(corpora) {
		return errors.New("smoke phase cannot borrow another build's store or an active process")
	}
	for i, repo := range p.Repos {
		if repo.Corpus != corpora[i] || !inside(p.Root, repo.Path) {
			return errors.New("smoke phase store/corpus ownership differs")
		}
	}
	return nil
}

func restoreSmokeClone(ctx context.Context, p *privateRun) error {
	if p.process != nil {
		return errors.New("smoke restoration requires the owned daemon to be joined")
	}
	for _, repo := range p.Repos {
		if !inside(p.Root, repo.Path) {
			return errors.New("smoke restoration escaped the owned clone")
		}
		expected, err := p.git(ctx, repo.Path, "show", repo.Corpus.Commit+":"+repo.Corpus.File)
		if err != nil {
			return err
		}
		if _, err := p.git(ctx, repo.Path, "reset", "--hard", repo.Corpus.Commit); err != nil {
			return err
		}
		if _, err := p.git(ctx, repo.Path, "clean", "-fd"); err != nil {
			return err
		}
		actual, err := os.ReadFile(filepath.Join(repo.Path, filepath.FromSlash(repo.Corpus.File)))
		if err != nil || !bytes.Equal(actual, expected) {
			return errors.Join(err, errors.New("smoke restoration did not restore exact pinned file bytes"))
		}
		dirty, err := p.git(ctx, repo.Path, "status", "--porcelain", "--untracked-files=normal")
		if err != nil || len(dirty) != 0 {
			return errors.Join(err, errors.New("smoke restoration left changed or untracked clone files"))
		}
	}
	return nil
}

func awaitSmokeRestoration(ctx context.Context, o runOptions, p *privateRun, client *socketClient, phase *Phase) error {
	for _, marker := range o.smokeMarkers {
		for {
			r := request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": marker}}
			answer, rows, err := callTool(ctx, client, r, dispatchArgs(r, p.Repos[0]), "restoration_probe", 0, 0, 0, o.Deadline, waitContext)
			phase.Attempts = append(phase.Attempts, rows...)
			if err != nil {
				return err
			}
			payload, err := textPayload(answer, "content")
			if err != nil {
				return err
			}
			if !containsIdentity(payload, marker) {
				break
			}
			if err := waitContext(ctx, 250*time.Millisecond); err != nil {
				return fmt.Errorf("smoke restoration still exposes pending marker %s: %w", marker, err)
			}
		}
	}
	data, err := json.Marshal(map[string]any{"restored_markers": len(o.smokeMarkers), "ready_and_enriched": true})
	phase.Gate["pr_smoke_restoration"] = data
	return err
}

func measureIdleStop(ctx context.Context, p *privateRun, clients []*socketClient, phase *Phase, sampler processSampler, group string) error {
	var closeErr error
	for _, client := range clients {
		closeErr = errors.Join(closeErr, client.Close())
	}
	if closeErr != nil {
		return closeErr
	}
	phase.Started = time.Now().UTC()
	monitor := startMonitor(p, sampler, group)
	err := waitContext(ctx, 2*time.Second)
	phase.DurationSec = time.Since(phase.Started).Seconds()
	monitor.finish(phase)
	data, marshalErr := json.Marshal(map[string]any{"started": phase.Started, "finished": time.Now().UTC(), "closed_mcp_sessions": len(clients), "required_idle_seconds": 2, "complete": err == nil})
	phase.Gate["idle_stop"] = data
	return errors.Join(err, marshalErr)
}
