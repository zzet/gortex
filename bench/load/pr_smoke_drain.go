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
	Version                   int       `json:"schema_version"`
	Started                   time.Time `json:"started"`
	OfferDeadline             time.Time `json:"offer_deadline"`
	DrainDeadline             time.Time `json:"drain_deadline"`
	Finished                  time.Time `json:"finished"`
	ElapsedSeconds            float64   `json:"elapsed_seconds"`
	AfterOfferDeadlineSeconds float64   `json:"after_offer_deadline_seconds"`
	AdmittedCalls             int       `json:"admitted_calls"`
	NotOffered                int       `json:"not_offered_after_window"`
}

func validatePRSmokeDrain(o runOptions) error {
	if o.PRSmokeDrain < 0 {
		return errors.New("offered-window drain must be nonnegative")
	}
	if o.PRSmokeDrain == 0 {
		return nil
	}
	if o.Scenario != "read" || o.Host != "native" || o.Profile != "agent" || o.Editors != 0 || o.Worktrees != 0 || o.CPUProfile != 0 || o.KeepState {
		return errors.New("offered-window/drain currently supports standalone native agent reads only; edit/edit-and-probe and other scenarios are unsupported")
	}
	if o.Duration != 20*time.Second && o.Duration != 30*time.Second {
		return errors.New("offered-window drain needs an explicit 20s or 30s offered duration")
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
	if len(r.Phases) != 1 || r.Phases[0].Name != "read" {
		return errors.New("complete PR drain report needs one standalone read phase")
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
	if observed.Version != 1 || observed.Started.IsZero() || observed.Started.Before(phase.Started) || observed.Finished.After(phaseEnd) || observed.Finished.Before(observed.OfferDeadline) || observed.Finished.After(observed.DrainDeadline) || observed.AdmittedCalls < 1 || observed.NotOffered < 0 {
		return errors.New("offered-window drain has invalid phase timing or admission receipts")
	}
	// This tolerance concerns JSON timestamp/float round trips, not a budget.
	if math.Abs(observed.OfferDeadline.Sub(observed.Started).Seconds()-config.OfferedSeconds) > 1e-6 || math.Abs(observed.DrainDeadline.Sub(observed.OfferDeadline).Seconds()-config.MaxDrainSeconds) > 1e-6 || math.Abs(observed.ElapsedSeconds-observed.Finished.Sub(observed.Started).Seconds()) > 1e-6 || math.Abs(observed.AfterOfferDeadlineSeconds-observed.Finished.Sub(observed.OfferDeadline).Seconds()) > 1e-6 || phase.DurationSec+1e-6 < observed.ElapsedSeconds {
		return errors.New("offered-window drain elapsed/admission receipts do not match the offered and product-drain profile")
	}
	logical := map[[3]int]bool{}
	for _, attempt := range phase.Attempts {
		if attempt.Role == "workload" {
			logical[[3]int{attempt.Agent, attempt.Step, attempt.Slot}] = true
		}
	}
	if len(logical) != observed.AdmittedCalls {
		return errors.New("offered-window drain admitted-call census differs from retained logical workload calls")
	}
	return nil
}

func canOfferPRRead(offers, drain context.Context) bool {
	deadline, ok := offers.Deadline()
	return ok && time.Now().Before(deadline) && offers.Err() == nil && drain.Err() == nil
}

func runPRSmokeReads(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, phase *Phase) error {
	if err := validatePRSmokeDrain(o); err != nil {
		return err
	}
	if len(clients) != o.Agents || len(p.Repos) == 0 {
		return errors.New("offered-window reads need one owned session per reading agent and a pinned repository")
	}
	var mu sync.Mutex
	admitted, notOffered := 0, 0
	observation, err := runOfferedWindow(parent, o.Duration, o.PRSmokeDrain, func(offers, drain context.Context) error {
		var workers sync.WaitGroup
		var failures []error
		for agent := range o.Agents {
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
				pace := newPacing(agentProfile, uint64(o.Seed)+uint64(agent))
				for step := 0; canOfferPRRead(offers, drain); step++ {
					if waitContext(offers, pace.think()) != nil {
						return
					}
					var burst sync.WaitGroup
					for slot := range pace.burst() {
						burst.Go(func() {
							if !canOfferPRRead(offers, drain) {
								mu.Lock()
								notOffered++
								mu.Unlock()
								return
							}
							mu.Lock()
							admitted++
							mu.Unlock()
							r := plan[(agent+step*5+slot)%len(plan)]
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
	observation.AdmittedCalls, observation.NotOffered = admitted, notOffered
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
