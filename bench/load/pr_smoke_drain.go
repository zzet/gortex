package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// PRSmokeDrain is optional. Its absence preserves frozen report-v1 JSON and
// workload hashes; it is never inferred from a shorter duration alone.
type PRSmokeDrain struct {
	Version         int     `json:"schema_version"`
	OfferedSeconds  float64 `json:"offered_seconds"`
	MaxDrainSeconds float64 `json:"max_drain_seconds"`
}

type prDrainObservation struct {
	Version                   int                `json:"schema_version"`
	Started                   time.Time          `json:"started"`
	OfferDeadline             time.Time          `json:"offer_deadline"`
	DrainDeadline             time.Time          `json:"drain_deadline"`
	Finished                  time.Time          `json:"finished"`
	ElapsedSeconds            float64            `json:"elapsed_seconds"`
	AfterOfferDeadlineSeconds float64            `json:"after_offer_deadline_seconds"`
	AdmittedCalls             int                `json:"admitted_calls"`
	NotOffered                int                `json:"not_offered_after_window"`
	AdmittedEdits             int                `json:"admitted_edits,omitempty"`
	VisibilityProbeAttempts   int                `json:"visibility_probe_attempts,omitempty"`
	Admissions                []prDrainAdmission `json:"admissions,omitempty"`
}

type prDrainAdmission struct {
	Agent     int       `json:"agent"`
	Step      int       `json:"step"`
	Slot      int       `json:"slot"`
	Tool      string    `json:"tool"`
	OfferedAt time.Time `json:"offered_at"`
}

func prSmokeDrainVersion(o runOptions) int {
	if o.PRSmokeDrainVersion == 0 {
		return 1 // Existing programmatic read callers retain version 1.
	}
	return o.PRSmokeDrainVersion
}

func validatePRSmokeDrain(o runOptions) error {
	if o.PRSmokeDrain < 0 {
		return errors.New("offered-window drain must be nonnegative")
	}
	if o.PRSmokeDrain == 0 {
		return nil
	}
	version := prSmokeDrainVersion(o)
	if (version != 1 || o.Scenario != "read" || o.Editors != 0) && (version != 2 || o.Scenario != "edit" || o.Editors < 1) || o.Host != "native" || o.Profile != "agent" || o.Worktrees != 0 || o.CPUProfile != 0 || o.KeepState {
		return errors.New("offered-window/drain supports standalone native agent reads (version 1) or explicit edits with visibility probes (version 2) only")
	}
	if o.Duration != 20*time.Second && o.Duration != 30*time.Second {
		return errors.New("offered-window drain needs an explicit 20s or 30s offered duration")
	}
	if version == 2 && (o.Duration != 20*time.Second || o.Deadline != 120*time.Second) {
		return errors.New("version 2 edit drain requires the original 20s offers and 120s product budget")
	}
	if o.PRSmokeDrain < o.Deadline || o.PRSmokeDrain > 2*time.Minute {
		return errors.New("offered-window drain must cover the original product deadline and be at most two minutes")
	}
	return nil
}

// Offer timeout is deliberately not the context of an admitted call. Work
// retains the original product budget, bounded by drain and parent deadlines.
func runOfferedWindow(parent context.Context, offerFor, drainFor time.Duration, work func(context.Context, context.Context) error) (prDrainObservation, error) {
	started := time.Now()
	observation := prDrainObservation{Version: 1, Started: started, OfferDeadline: started.Add(offerFor), DrainDeadline: started.Add(offerFor + drainFor)}
	offers, stopOffers := context.WithDeadline(parent, observation.OfferDeadline)
	defer stopOffers()
	drain, stopDrain := context.WithDeadline(parent, observation.DrainDeadline)
	defer stopDrain()
	observation.OfferDeadline, _ = offers.Deadline()
	observation.DrainDeadline, _ = drain.Deadline()
	err := work(offers, drain)
	observation.Finished = time.Now()
	observation.ElapsedSeconds = observation.Finished.Sub(started).Seconds()
	if observation.Finished.After(observation.OfferDeadline) {
		observation.AfterOfferDeadlineSeconds = observation.Finished.Sub(observation.OfferDeadline).Seconds()
	}
	return observation, errors.Join(err, drain.Err(), parent.Err())
}

func validatePRDrainObservation(r Report) error {
	if r.Workload.PRSmokeDrain == nil || !r.Complete {
		return nil
	}
	if len(r.Phases) != 1 || r.Phases[0].Name != r.Workload.Scenario {
		return errors.New("complete PR drain report needs its single standalone phase")
	}
	phase := r.Phases[0]
	data, ok := phase.Gate["pr_smoke_drain"]
	if !ok {
		return errors.New("complete PR drain report lacks observed offering/drain receipts")
	}
	if err := requireFields(data, prDrainObservation{}); err != nil {
		return err
	}
	var observed prDrainObservation
	if err := json.Unmarshal(data, &observed); err != nil {
		return err
	}
	config := r.Workload.PRSmokeDrain
	phaseEnd := phase.Started.Add(time.Duration(phase.DurationSec*float64(time.Second)) + time.Microsecond)
	if observed.Version != config.Version || observed.Started.IsZero() || observed.Started.Before(phase.Started) || observed.Finished.After(phaseEnd) || observed.Finished.Before(observed.OfferDeadline) || observed.Finished.After(observed.DrainDeadline) || observed.AdmittedCalls < 1 || observed.NotOffered < 0 {
		return errors.New("offered-window drain has invalid phase timing or admission receipts")
	}
	// This tolerance concerns JSON timestamp/float round trips, not a budget.
	if math.Abs(observed.OfferDeadline.Sub(observed.Started).Seconds()-config.OfferedSeconds) > 1e-6 || math.Abs(observed.DrainDeadline.Sub(observed.OfferDeadline).Seconds()-config.MaxDrainSeconds) > 1e-6 || math.Abs(observed.ElapsedSeconds-observed.Finished.Sub(observed.Started).Seconds()) > 1e-6 || math.Abs(observed.AfterOfferDeadlineSeconds-observed.Finished.Sub(observed.OfferDeadline).Seconds()) > 1e-6 || phase.DurationSec+1e-6 < observed.ElapsedSeconds {
		return errors.New("offered-window drain elapsed/admission receipts do not match the offered and product-drain profile")
	}
	logical := map[prDrainCall]bool{}
	for _, attempt := range phase.Attempts {
		if attempt.Role == "workload" {
			logical[prDrainCall{attempt.Agent, attempt.Step, attempt.Slot, attempt.Tool}] = true
		}
	}
	if len(logical) != observed.AdmittedCalls {
		return errors.New("offered-window drain admitted-call census differs from retained logical workload calls")
	}
	if config.Version == 2 {
		return validatePREditDrainObservation(phase, observed, r.Workload.DeadlineSec)
	}
	return nil
}

type prDrainCall struct {
	agent, step, slot int
	tool              string
}

func validatePREditDrainObservation(phase Phase, observed prDrainObservation, deadlineSeconds float64) error {
	if observed.AdmittedEdits < 1 || observed.AdmittedEdits != len(phase.Edits) || len(observed.Admissions) != observed.AdmittedCalls {
		return errors.New("edit drain is missing its edit/admission census")
	}
	admissions := map[prDrainCall]time.Time{}
	edits := map[[2]int]time.Time{}
	for _, admission := range observed.Admissions {
		key := prDrainCall{admission.Agent, admission.Step, admission.Slot, admission.Tool}
		if _, duplicate := admissions[key]; duplicate || admission.OfferedAt.Before(observed.Started) || !admission.OfferedAt.Before(observed.OfferDeadline) {
			return errors.New("edit drain has duplicate or late logical admissions")
		}
		admissions[key] = admission.OfferedAt
		if admission.Tool == "edit.file" {
			edits[[2]int{admission.Agent, admission.Step}] = admission.OfferedAt
		}
	}
	if len(edits) != observed.AdmittedEdits {
		return errors.New("edit drain's admitted edits differ from workload admissions")
	}
	started := map[prDrainCall]bool{}
	visibleProbes := map[[2]int]bool{}
	probes := 0
	for _, attempt := range phase.Attempts {
		key := prDrainCall{attempt.Agent, attempt.Step, attempt.Slot, attempt.Tool}
		if attempt.Started.After(observed.Finished) {
			return errors.New("edit drain attempt starts after phase completion")
		}
		switch attempt.Role {
		case "workload":
			admitted, ok := admissions[key]
			if !ok || attempt.Started.Before(admitted) {
				return errors.New("edit drain workload attempt lacks a preceding logical admission")
			}
			if attempt.Retry == 0 {
				if started[key] {
					return errors.New("edit drain logical admission executed more than once")
				}
				started[key] = true
			}
		case "probe":
			editKey := [2]int{attempt.Agent, attempt.Step}
			admitted, ok := edits[editKey]
			if !ok || attempt.Tool != "search.symbols" || attempt.Started.Before(admitted) {
				return errors.New("edit drain visibility probe lacks an admitted edit")
			}
			if attempt.Outcome == "success" {
				visibleProbes[editKey] = true
			}
			probes++
		default:
			return errors.New("edit drain has an unexpected attempt role")
		}
	}
	if len(started) != len(admissions) || probes != observed.VisibilityProbeAttempts {
		return errors.New("edit drain workload/probe execution census differs from its admissions")
	}
	for key, admitted := range edits {
		if !visibleProbes[key] {
			return errors.New("complete edit drain lacks an admitted edit's successful visibility probe")
		}
		found := false
		for _, edit := range phase.Edits {
			if edit.Marker == fmt.Sprintf("GortexLoadMarkerA%dS%d", key[0], key[1]) && !edit.Started.Before(admitted) && !edit.Started.After(observed.Finished) {
				if edit.Outcome != "visible" || edit.VisibilityMS.Value == nil || *edit.VisibilityMS.Value > deadlineSeconds*1000 {
					return errors.New("complete edit drain has an unfinished or expired visibility observation")
				}
				found = true
				break
			}
		}
		if !found {
			return errors.New("edit drain lacks an admitted edit's marker observation")
		}
	}
	return nil
}

func canOfferPRRead(offers, drain context.Context) bool {
	deadline, ok := offers.Deadline()
	return ok && time.Now().Before(deadline) && offers.Err() == nil && drain.Err() == nil
}

func runPRSmokeAgents(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, editing bool, phase *Phase) error {
	if err := validatePRSmokeDrain(o); err != nil {
		return err
	}
	if editing != (prSmokeDrainVersion(o) == 2) {
		return errors.New("offered-window phase differs from its versioned read/edit identity")
	}
	count := o.Agents
	if editing {
		count += o.Editors
	}
	if len(clients) != count || len(p.Repos) != 1 {
		return errors.New("offered-window work needs one owned session per agent and one pinned repository")
	}
	return runPRSmokeWindow(parent, o, p, clients, editing, phase)
}

func runPRSmokeWindow(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, editing bool, phase *Phase) error {
	var mu sync.Mutex
	admitted, notOffered, edits := 0, 0, 0
	var admissions []prDrainAdmission
	observation, err := runOfferedWindow(parent, o.Duration, o.PRSmokeDrain, func(offers, drain context.Context) error {
		admit := func(agent, step, slot int, tool string) bool {
			mu.Lock()
			defer mu.Unlock()
			at := time.Now()
			deadline, ok := offers.Deadline()
			if !ok || !at.Before(deadline) || offers.Err() != nil || drain.Err() != nil {
				notOffered++
				return false
			}
			admitted++
			if editing {
				admissions = append(admissions, prDrainAdmission{Agent: agent, Step: step, Slot: slot, Tool: tool, OfferedAt: at})
			}
			return true
		}
		var workers sync.WaitGroup
		var failures []error
		for agent := range len(clients) {
			workers.Go(func() {
				repo := p.Repos[agent%len(p.Repos)]
				client := clients[agent]
				plan, planErr := readingPlan(repo.Corpus)
				if planErr != nil {
					mu.Lock()
					failures = append(failures, planErr)
					mu.Unlock()
					return
				}
				pace := newPacing(profile(o.Profile), uint64(o.Seed)+uint64(agent))
				for step := 0; canOfferPRRead(offers, drain); step++ {
					if waitContext(offers, pace.think()) != nil {
						return
					}
					if editing && agent >= o.Agents {
						if !admit(agent, step, 0, "edit.file") {
							return
						}
						mu.Lock()
						edits++
						mu.Unlock()
						edit, rows := editAndObserve(drain, o, client, repo, agent, step)
						mu.Lock()
						phase.Edits = append(phase.Edits, edit)
						phase.Attempts = append(phase.Attempts, rows...)
						if edit.Outcome != "visible" {
							failures = append(failures, fmt.Errorf("editor %d step %d: %s", agent, step, edit.Outcome))
						}
						mu.Unlock()
					}
					var burst sync.WaitGroup
					for slot := range pace.burst() {
						burst.Go(func() {
							r := plan[(agent+step*5+slot)%len(plan)]
							if !admit(agent, step, slot, r.name()) {
								return
							}
							_, rows, callErr := callTool(drain, client, r, dispatchArgs(r, repo), "workload", agent, step, slot, o.Deadline, waitContext)
							mu.Lock()
							phase.Attempts = append(phase.Attempts, rows...)
							if callErr != nil {
								failures = append(failures, fmt.Errorf("reader %d step %d slot %d: %w", agent, step, slot, callErr))
							}
							mu.Unlock()
						})
					}
					burst.Wait()
				}
			})
		}
		workers.Wait()
		return errors.Join(failures...)
	})
	observation.Version = prSmokeDrainVersion(o)
	observation.AdmittedCalls, observation.NotOffered = admitted, notOffered
	observation.AdmittedEdits = edits
	observation.Admissions = admissions
	if editing {
		for _, attempt := range phase.Attempts {
			if attempt.Role == "probe" {
				observation.VisibilityProbeAttempts++
			}
		}
	}
	data, marshalErr := json.Marshal(observation)
	if phase.Gate == nil {
		phase.Gate = map[string]json.RawMessage{}
	}
	if marshalErr == nil {
		phase.Gate["pr_smoke_drain"] = data
	}
	finishEditMetrics(phase)
	return errors.Join(err, marshalErr)
}
