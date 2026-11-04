package main

import "errors"

// nightlyWorkloadSeed describes an opt-in workload fault, never a replacement
// product revision or a modification of observations or policy thresholds.
// Versioning prevents an old collector from silently interpreting a new fault.
type nightlyWorkloadSeed struct {
	Version           int    `json:"schema_version"`
	Kind              string `json:"kind"`
	CandidateRevision string `json:"candidate_revision"`
}

func validateNightlySeed(c laneConfig) error {
	if c.NightlySeed == nil {
		return nil
	}
	seed := c.NightlySeed
	if seed.Version != 1 || seed.Kind != "search-delay-v1" || c.Lane != "nightly" || c.EditGateSeed != "" || seed.CandidateRevision != c.Builds["candidate"].Revision || !revisionPattern.MatchString(seed.CandidateRevision) {
		return errors.New("nightly_workload_seed requires schema1 search-delay-v1, full nightly collection, no edit seed and the exact candidate revision")
	}
	return nil
}

// Raw fault identity is separate from the normalized offered workload. It must
// agree with candidate/loadedit-4, whose search latency has an absolute budget;
// MAIN/tag and other cells stay clean.
func validateNightlySeedReport(c inputs, input runInput, r report) error {
	expected := ""
	if c.NightlySeed != nil {
		if err := validateNightlySeed(laneConfig{Lane: "nightly", NightlySeed: c.NightlySeed, Builds: map[string]laneBuild{"candidate": {Revision: c.CandidateRevision}}}); err != nil {
			return err
		}
		if input.Role == "candidate" && input.Scenario == "loadedit-4" {
			expected = c.NightlySeed.Kind
		}
	}
	if r.NightlySeed != expected {
		return errors.New("missing or foreign nightly workload fault provenance")
	}
	observed := false
	for _, p := range r.Phases {
		for _, a := range p.Attempts {
			seeded := expected != "" && a.Tool == "search.symbols" && a.Role == "workload"
			if seeded {
				if a.NightlySeed != expected || a.NightlyDelayNS < 1000000000 {
					return errors.New("nightly search fault lacks actual one-second raw delay")
				}
				observed = true
			} else if a.NightlySeed != "" || a.NightlyDelayNS != 0 {
				return errors.New("foreign nightly workload fault attempt")
			}
		}
	}
	if expected != "" && !observed {
		return errors.New("nightly search fault did not reach an actual workload request")
	}
	return nil
}
