package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type readbackReferencePlan struct {
	Language               string `json:"language,omitempty"`
	Version                int    `json:"version"`
	Binary                 string `json:"binary"`
	BinarySHA256           string `json:"binary_sha256"`
	BinaryRevision         string `json:"binary_revision"`
	Manifest               string `json:"manifest"`
	Scratch                string `json:"scratch"`
	Output                 string `json:"output"`
	Seed                   int64  `json:"seed"`
	EditMeanNS             int64  `json:"edit_mean_ns"`
	ProductDeadlineSeconds int    `json:"product_deadline_seconds"`
	WarmSeconds            int    `json:"warm_seconds"`
	StopSeconds            int    `json:"stop_seconds"`
	GrantID                string `json:"grant_id,omitempty"`
	ToolSurface            string `json:"tool_surface,omitempty"`
}
type readbackReferenceSchedule struct {
	Cell          readbackReferenceCell `json:"cell"`
	Arrival       []editProbeArrival    `json:"arrival"`
	OfferedTailNS int64                 `json:"offered_tail_ns"`
	PhaseBoundNS  int64                 `json:"phase_bound_ns"`
}
type readbackReferencePreflight struct {
	Plan                    readbackReferencePlan       `json:"plan"`
	PlanSHA256              string                      `json:"plan_sha256"`
	ManifestSHA256          string                      `json:"manifest_sha256"`
	Cells                   []readbackReferenceSchedule `json:"cells"`
	UnderEditScheduledHours float64                     `json:"under_edit_scheduled_hours"`
	NoEditScheduledHours    float64                     `json:"no_edit_scheduled_hours"`
	WarmupBoundHours        float64                     `json:"warmup_bound_hours"`
	StopBoundHours          float64                     `json:"stop_bound_hours"`
	PhaseDrainBoundHours    float64                     `json:"phase_drain_bound_hours"`
	ExecutionState          string                      `json:"execution_state"`
	BaselineReuse           string                      `json:"baseline_reuse"`
	MainComparison          string                      `json:"main_comparison"`
	ToolSurface             string                      `json:"tool_surface"`
}

func readbackReferenceProtocolSchedule(s readbackReferenceSchedule, deadline time.Duration) readbackReferenceSchedule {
	// Keep the nominal acceptance cell at 100. The separate diagnostic report
	// expects one offer and cannot certify that acceptance cell.
	s.Arrival = append([]editProbeArrival(nil), s.Arrival[:1]...)
	s.Arrival[0].DueNS = 0
	s.OfferedTailNS = 0
	s.PhaseBoundNS = int64(2 * deadline)
	return s
}

func decodeReadbackReferencePlan(raw []byte) (readbackReferencePlan, error) {
	var p readbackReferencePlan
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	if d.Decode(new(any)) != io.EOF {
		return p, errors.New("readback plan requires one JSON object")
	}
	if _, err := readbackReferenceLanguageExtension(p.Language); err != nil {
		return p, err
	}
	if _, err := readbackReferenceSurface(p.ToolSurface); err != nil {
		return p, err
	}
	if p.Version != 1 || p.EditMeanNS != int64(editProbeMean) || p.ProductDeadlineSeconds < 1 || p.WarmSeconds < 1 || p.StopSeconds < 1 {
		return p, errors.New("readback plan requires version1, exact15s open-loop edit mean, and positive fixed budgets")
	}
	for _, path := range []string{p.Binary, p.Manifest, p.Scratch, p.Output} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return p, errors.New("readback plan paths must be absolute and canonical")
		}
	}
	if !commitPattern.MatchString(p.BinaryRevision) {
		return p, errors.New("readback binary revision must be fully pinned")
	}
	b, err := hex.DecodeString(p.BinarySHA256)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != p.BinarySHA256 {
		return p, errors.New("readback binary needs canonical SHA256")
	}
	return p, nil
}
func readbackReferenceSchedules(p readbackReferencePlan) ([]readbackReferenceSchedule, error) {
	if _, err := readbackReferenceLanguageExtension(p.Language); err != nil {
		return nil, err
	}
	kinds := make([]string, 0, len(readbackReferenceTools))
	for k := range readbackReferenceTools {
		if k == "relocate" && p.Language != "" && p.Language != "go" {
			continue
		}
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	var out []readbackReferenceSchedule
	for _, agents := range []int{1, 4, 8, 16, 32} {
		for _, mode := range []string{"", "local"} {
			for _, kind := range kinds {
				tools := readbackReferenceTools[kind]
				if len(tools) != 1 {
					return nil, errors.New("reference matrix has one declared mutation adapter per semantic kind")
				}
				cell := readbackReferenceCell{Language: p.Language, Agents: agents, ACKMode: mode, Kind: kind, Tool: tools[0], Offers: readbackReferenceSamples}
				if err := cell.validate(); err != nil {
					return nil, err
				}
				// Identical offered stream per kind and ACK policy enables exact role
				// comparisons. Independent fixed editor streams merge without reseeding.
				arrival, err := editProbeArrivals(p.Seed, cell.Offers, agents, time.Duration(p.EditMeanNS))
				if err != nil {
					return nil, err
				}
				tail := arrival[len(arrival)-1].DueNS
				out = append(out, readbackReferenceSchedule{Cell: cell, Arrival: arrival, OfferedTailNS: tail, PhaseBoundNS: tail + int64(time.Duration(2*p.ProductDeadlineSeconds)*time.Second)})
			}
		}
	}
	return out, nil
}
func readbackReferenceFileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func preflightReadbackReference(p readbackReferencePlan) (readbackReferencePreflight, error) {
	out := readbackReferencePreflight{Plan: p, ExecutionState: "NOT_EXECUTED: no daemon/store/workload launched", BaselineReuse: "No-edit ACK-policy reuse is conditional on exact binary, manifest, fixture-population, offered-read-plan and host identity equality; kind baselines are distinct. Missing equivalence never grants credit.", MainComparison: "Separate same-host main read-only mix at4/8 remains required; no inferred regression pass."}
	var err error
	out.ToolSurface, err = readbackReferenceSurface(p.ToolSurface)
	if err != nil {
		return out, err
	}
	cells, err := readbackReferenceSchedules(p)
	if err != nil {
		return out, err
	}
	out.Cells = cells
	out.PlanSHA256, _ = hashJSON(p)
	corpora, err := loadManifest(p.Manifest)
	if err != nil {
		return out, err
	}
	if len(corpora) != 1 {
		return out, errors.New("readback reference is one explicitly pinned primary checkout")
	}
	if err := validateOutputRoots(corpora, p.Scratch, p.Output); err != nil {
		return out, err
	}
	out.ManifestSHA256, err = readbackReferenceFileSHA(p.Manifest)
	if err != nil {
		return out, err
	}
	actual, err := readbackReferenceFileSHA(p.Binary)
	if err != nil {
		return out, err
	}
	if actual != p.BinarySHA256 {
		return out, errors.New("readback binary SHA256 changed")
	}
	revision, err := binaryRevision(p.Binary, p.BinaryRevision)
	if err != nil || revision != p.BinaryRevision {
		return out, fmt.Errorf("readback binary revision: %w", err)
	}
	var tails, baseline, phaseDrain int64
	for _, s := range cells {
		tails += s.OfferedTailNS
		phaseDrain += s.PhaseBoundNS - s.OfferedTailNS
		if s.Cell.ACKMode == "" {
			baseline += s.OfferedTailNS
		}
	}
	out.UnderEditScheduledHours = float64(tails) / float64(time.Hour)
	out.NoEditScheduledHours = float64(baseline) / float64(time.Hour)
	// Worst-case setup assumes fresh ownership for each role/cell. Grouped
	// setup may only reduce this with an explicit reviewed equivalence proof.
	executions := len(cells) + len(cells)/2
	out.WarmupBoundHours = float64(executions*p.WarmSeconds) / 3600
	out.StopBoundHours = float64(executions*p.StopSeconds) / 3600
	out.PhaseDrainBoundHours = float64(phaseDrain+phaseDrain/2) / float64(time.Hour)
	return out, nil
}
