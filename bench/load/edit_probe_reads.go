package main

import (
	"context"
	"sync"
	"time"
)

const editProbeNoEditWindow = 30 * time.Second
const editProbeMaxReaders = 32

// Pure readers run the ordinary reading plan beside the edits. Each call is
// classed when it is issued: before the edit origin it is the same run's
// no-edit baseline, afterwards it is under edit while any offered edit is sent
// but not yet finished, and between edits otherwise. An edit sent while the
// read is in flight does not reclass it.
type editProbeReadStats struct {
	Calls    int                `json:"calls"`
	Busy     int                `json:"busy_refusals"`
	Errors   int                `json:"errors"`
	Censored int                `json:"censored"`
	Latency  editProbeQuantiles `json:"latency_ms"`
}

// Read stats per operation, then per class.
type editProbeReads map[string]map[string]editProbeReadStats

type editProbeRead struct {
	operation, class, outcome string
	latency                   float64
}

func (r *editProbeRuntime) readClass(at time.Time) string {
	switch {
	case at.Before(r.origin):
		return "no_edit_baseline"
	case r.editsInFlight.Load() > 0:
		return "under_edit"
	default:
		return "between_edits"
	}
}

func (r *editProbeRuntime) startReaders(ctx context.Context, readers *sync.WaitGroup) {
	repo := r.p.Repos[0]
	plan, _ := readingPlan(repo.Corpus)
	if len(plan) == 0 {
		return
	}
	for reader, client := range r.readers {
		readers.Go(func() {
			pace := newPacing(profile(r.options.Profile), uint64(r.options.Seed)+uint64(reader))
			for step := 0; ctx.Err() == nil; step++ {
				if waitContext(ctx, pace.think()) != nil {
					return
				}
				var burst sync.WaitGroup
				for slot := range pace.burst() {
					burst.Go(func() {
						req := plan[(reader+step*5+slot)%len(plan)]
						class := r.readClass(time.Now())
						_, rows, _ := callTool(ctx, client, req, dispatchArgs(req, repo), "reader", reader, step, slot, r.options.Deadline, waitContext)
						r.recordRead(req.name(), class, rows)
					})
				}
				burst.Wait()
			}
		})
	}
}

func (r *editProbeRuntime) recordRead(operation, class string, rows []Attempt) {
	read := editProbeRead{operation: operation, class: class, outcome: "error"}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		read.outcome, read.latency = last.Outcome, last.LogicalMS
	}
	r.mu.Lock()
	r.phase.Attempts = append(r.phase.Attempts, rows...)
	r.reads = append(r.reads, read)
	r.mu.Unlock()
}

// Only successful calls enter a latency sample; busy, failed and censored
// calls stay counted beside it, never as a zero latency.
func summarizeEditProbeReads(reads []editProbeRead, seed int64) editProbeReads {
	if len(reads) == 0 {
		return nil
	}
	type cell struct{ operation, class string }
	samples := map[cell][]float64{}
	stats := map[cell]*editProbeReadStats{}
	for _, read := range reads {
		key := cell{read.operation, read.class}
		if stats[key] == nil {
			stats[key] = &editProbeReadStats{}
		}
		s := stats[key]
		s.Calls++
		switch read.outcome {
		case "success":
			samples[key] = append(samples[key], read.latency)
		case "busy":
			s.Busy++
		case "phase_censored":
			s.Censored++
		default:
			s.Errors++
		}
	}
	out := editProbeReads{}
	for key, s := range stats {
		s.Latency = editProbeQuantilesOf(samples[key], s.Calls-len(samples[key]), "ms", seed, "reads."+key.operation+"."+key.class)
		if out[key.operation] == nil {
			out[key.operation] = map[string]editProbeReadStats{}
		}
		out[key.operation][key.class] = *s
	}
	return out
}
