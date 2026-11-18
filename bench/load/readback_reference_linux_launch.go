package main

import (
	"errors"
	"time"
)

// A cell keeps the existing inner setup/phase/stop bounds. The extra30 seconds
// for the test client's report and60 for the outer's resource proof are the
// same margins as the reviewed2100/2160-second one-off diagnostic. The reviewed
// S2 host controller retains its separate120-second completion margin.
type readbackReferenceLinuxCellLaunch struct {
	Index                int                       `json:"index"`
	Schedule             readbackReferenceSchedule `json:"schedule"`
	PlannedArrivalSHA256 string                    `json:"planned_arrival_sha256"`
	ExpectedOffers       int                       `json:"expected_offers"`
	ClientTimeoutSeconds int                       `json:"client_timeout_seconds"`
	WorkCapSeconds       int                       `json:"work_cap_seconds"`
	HostTotalCapSeconds  int                       `json:"host_total_cap_seconds"`
}

type readbackReferenceLinuxCatalog struct {
	Version                int                                `json:"version"`
	State                  string                             `json:"state"`
	Seed                   int64                              `json:"seed"`
	EditMeanNS             int64                              `json:"edit_mean_ns"`
	ProductDeadlineSeconds int                                `json:"product_deadline_seconds"`
	WarmSeconds            int                                `json:"warm_seconds"`
	StopSeconds            int                                `json:"stop_seconds"`
	ToolSurface            string                             `json:"tool_surface"`
	Cells                  []readbackReferenceLinuxCellLaunch `json:"cells"`
}

// This is a metadata-only export of the already frozen Go90 matrix. It cannot
// launch a daemon or turn a planned arrival hash into actual offered evidence.
func readbackReferenceLinuxLaunchCatalog(p readbackReferencePlan) (readbackReferenceLinuxCatalog, error) {
	if p.Version != 1 || p.Language != "" || p.Seed != 1 || p.EditMeanNS != int64(15*time.Second) || p.ProductDeadlineSeconds != 120 || p.WarmSeconds != 1800 || p.StopSeconds != 30 || p.ToolSurface != readbackReferenceCompatibilitySurface {
		return readbackReferenceLinuxCatalog{}, errors.New("linux acceptance binding fixes the existing Go90/N100/seed1/15s/all-defer schedule and budgets")
	}
	schedules, err := readbackReferenceSchedules(p)
	if err != nil {
		return readbackReferenceLinuxCatalog{}, err
	}
	out := readbackReferenceLinuxCatalog{Version: 1, State: "UNGRANTED_METADATA_ONLY_NO_RUNTIME", Seed: p.Seed, EditMeanNS: p.EditMeanNS, ProductDeadlineSeconds: p.ProductDeadlineSeconds, WarmSeconds: p.WarmSeconds, StopSeconds: p.StopSeconds, ToolSurface: p.ToolSurface}
	for index, schedule := range schedules {
		if schedule.Cell.Offers != 100 || len(schedule.Arrival) != 100 {
			return readbackReferenceLinuxCatalog{}, errors.New("full reference binding cannot shorten or enlarge the offered population")
		}
		arrivalSHA, err := hashJSON(schedule.Arrival)
		if err != nil {
			return readbackReferenceLinuxCatalog{}, err
		}
		phaseSeconds := int((schedule.PhaseBoundNS + int64(time.Second) - 1) / int64(time.Second))
		client := p.WarmSeconds + phaseSeconds + p.StopSeconds + 30
		work := client + 60
		out.Cells = append(out.Cells, readbackReferenceLinuxCellLaunch{Index: index, Schedule: schedule, PlannedArrivalSHA256: arrivalSHA, ExpectedOffers: 100, ClientTimeoutSeconds: client, WorkCapSeconds: work, HostTotalCapSeconds: work + 120})
	}
	return out, nil
}
