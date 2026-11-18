package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Sweep every advertised domain/operation explicitly. Unsupported prerequisites
// are evidence, not successful measurements or a claim of complete coverage.
func discoverSweep(ctx context.Context, client *socketClient, corpus Corpus) ([]request, []SweepEntry, error) {
	known, err := benchmarkPlan(corpus)
	if err != nil {
		return nil, nil, err
	}
	builders := make(map[string]request)
	for _, r := range known {
		builders[r.name()] = r
	}
	var selected []request
	var inventory []SweepEntry
	seen := make(map[string]bool)
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := client.call(ctx, "tools/list", params)
		if err != nil {
			return nil, inventory, err
		}
		var result struct {
			Tools []struct {
				Name   string `json:"name"`
				Schema struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
			Next string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, inventory, err
		}
		for _, tool := range result.Tools {
			ops := tool.Schema.Properties["operation"].Enum
			if tool.Name == "analyze" {
				ops = tool.Schema.Properties["kind"].Enum
			}
			if len(ops) == 0 {
				ops = []string{""}
			}
			for _, op := range ops {
				r := request{Tool: tool.Name, Operation: op}
				if seen[r.name()] {
					return nil, inventory, errors.New("duplicate advertised sweep operation")
				}
				seen[r.name()] = true
				entry := SweepEntry{Tool: r.Tool, Operation: op, Status: "outside_scope", Reason: "outside the documented 21-operation benchmark contract"}
				if build, ok := builders[r.name()]; ok {
					selected = append(selected, build)
					entry.Status, entry.Reason = "planned", ""
				}
				inventory = append(inventory, entry)
			}
		}
		if result.Next == "" {
			break
		}
		if result.Next == cursor || pages == 99 {
			return nil, inventory, errors.New("invalid tool inventory pagination")
		}
		cursor = result.Next
	}
	for _, r := range known {
		if !seen[r.name()] {
			inventory = append(inventory, SweepEntry{Tool: r.Tool, Operation: r.Operation, Status: "missing", Reason: "planned operation not advertised by this binary"})
		}
	}
	slices.SortFunc(selected, func(a, b request) int {
		if a.name() < b.name() {
			return -1
		}
		if a.name() > b.name() {
			return 1
		}
		return 0
	})
	// Both operations take the daemon's mutually-exclusive expensive-analysis
	// guard. Keep them at opposite ends, hence distinct 3–5-call bursts. Do
	// not hide ordinary workload refusals behind a new retry classification.
	var ordered []request
	for _, r := range selected {
		if r.name() == "analyze.dead_code" {
			ordered = append(ordered, r)
		}
	}
	for _, r := range selected {
		if r.name() != "analyze.dead_code" && r.name() != "analyze.hotspots" {
			ordered = append(ordered, r)
		}
	}
	for _, r := range selected {
		if r.name() == "analyze.hotspots" {
			ordered = append(ordered, r)
		}
	}
	return ordered, inventory, nil
}

func sweepBurst(remaining int, p *pacing) int {
	if remaining <= 5 {
		return remaining
	}
	size := p.burst()
	if tail := remaining - size; tail > 0 && tail < 3 {
		size = remaining - 3
	}
	return size
}

func runSweep(ctx context.Context, o runOptions, clients []*socketClient, repo privateRepo, requests []request, inventory []SweepEntry, phase *Phase, sleep func(context.Context, time.Duration) error) error {
	if len(clients) != len(requests) {
		return errors.New("sweep requires one preopened isolated session per operation")
	}
	if o.SweepRepetitions < 1 {
		return errors.New("sweep requires a positive repetition count")
	}
	pace := newPacing(profile(o.Profile), uint64(o.Seed))
	var mu sync.Mutex
	step := 0
	for range o.SweepRepetitions {
		for index := 0; index < len(requests); step++ {
			if err := sleep(ctx, pace.think()); err != nil {
				return err
			}
			size := sweepBurst(len(requests)-index, pace)
			var burst sync.WaitGroup
			for slot := range size {
				r := requests[index+slot]
				client := clients[index+slot]
				burst.Go(func() {
					_, rows, err := callTool(ctx, client, r, dispatchArgs(r, repo), "workload", 0, step, slot, o.Deadline, sleep)
					mu.Lock()
					defer mu.Unlock()
					phase.Attempts = append(phase.Attempts, rows...)
					for i := range inventory {
						if inventory[i].Tool == r.Tool && inventory[i].Operation == r.Operation {
							if inventory[i].Status != "error" {
								inventory[i].Status = "measured"
							}
							if err != nil {
								inventory[i].Status = "error"
								if inventory[i].Reason != "" {
									inventory[i].Reason += "; "
								}
								inventory[i].Reason += err.Error()
							}
						}
					}
				})
			}
			burst.Wait()
			index += size
		}
	}
	return nil
}

func finalizeSweepInventory(inventory []SweepEntry, attempts []Attempt, repetitions int, phase *Phase) {
	metrics := summarize(attempts, phase.DurationSec, "workload")
	for i := range inventory {
		entry := &inventory[i]
		if entry.Status == "outside_scope" {
			continue
		}
		name := entry.Tool
		if entry.Operation != "" {
			name += "." + entry.Operation
		}
		row := metrics[name]
		if entry.Status != "missing" {
			if row.Successes == repetitions && len(row.Errors) == 0 && row.ProductExpired == 0 && row.PhaseCensored == 0 {
				entry.Status, entry.Reason = "measured", ""
			} else {
				entry.Status = "error"
				entry.Reason = fmt.Sprintf("expected%d successes, observed%d; errors/censoring retained in tool metrics", repetitions, row.Successes)
			}
		}
		if entry.Status != "measured" {
			phase.Errors = append(phase.Errors, "tool sweep "+name+": "+entry.Status+": "+entry.Reason)
		}
	}
}
